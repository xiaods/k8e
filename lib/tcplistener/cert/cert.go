package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/ioutil"
	"math"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
)

const (
	rsaKeySize = 2048

	// ECPrivateKeyBlockType is a possible value for pem.Block.Type.
	ECPrivateKeyBlockType = "EC PRIVATE KEY"
	// RSAPrivateKeyBlockType is a possible value for pem.Block.Type.
	RSAPrivateKeyBlockType = "RSA PRIVATE KEY"
	// PrivateKeyBlockType is a possible value for pem.Block.Type.
	PrivateKeyBlockType = "PRIVATE KEY"
	// PublicKeyBlockType is a possible value for pem.Block.Type.
	PublicKeyBlockType = "PUBLIC KEY"
	// CertificateBlockType is a possible value for pem.Block.Type.
	CertificateBlockType = "CERTIFICATE"
	// CertificateRequestBlockType is a possible value for pem.Block.Type.
	CertificateRequestBlockType = "CERTIFICATE REQUEST"

	duration365d = time.Hour * 24 * 365

	CertificateRenewDays = 90
)

type AltNames struct {
	DNSNames []string
	IPs      []net.IP
}

type Config struct {
	CommonName   string
	Organization []string
	AltNames     AltNames
	Usages       []x509.ExtKeyUsage
}

// NewPrivateKey creates an RSA private key
func NewPrivateKey() (*rsa.PrivateKey, error) {
	return rsa.GenerateKey(rand.Reader, rsaKeySize)
}

func NewSelfSignedCACert(cfg Config, key crypto.Signer) (*x509.Certificate, error) {
	now := time.Now()
	tmpl := x509.Certificate{
		SerialNumber: new(big.Int).SetInt64(0),
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		NotBefore:             now.UTC(),
		NotAfter:              now.Add(duration365d * 10).UTC(), //10 year cert
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
		IsCA: true,
	}

	certDERBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(certDERBytes)
}

func NewSignedCert(cfg Config, key crypto.Signer, caCert *x509.Certificate, caKey crypto.Signer) (*x509.Certificate, error) {
	serial, err := rand.Int(rand.Reader, new(big.Int).SetInt64(math.MaxInt64))
	if err != nil {
		return nil, err
	}
	if len(cfg.CommonName) == 0 {
		return nil, errors.New("must specify a CommonName")
	}
	if len(cfg.Usages) == 0 {
		return nil, errors.New("must specify at least one ExtKeyUsage")
	}

	certTmpl := x509.Certificate{
		Subject: pkix.Name{
			CommonName:   cfg.CommonName,
			Organization: cfg.Organization,
		},
		DNSNames:     cfg.AltNames.DNSNames,
		IPAddresses:  cfg.AltNames.IPs,
		SerialNumber: serial,
		NotBefore:    caCert.NotBefore,
		NotAfter:     time.Now().Add(duration365d).UTC(),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  cfg.Usages,
	}
	certDERBytes, err := x509.CreateCertificate(rand.Reader, &certTmpl, caCert, key.Public(), caKey)
	if err != nil {
		return nil, err
	}

	parsedCert, err := x509.ParseCertificate(certDERBytes)
	if err == nil {
		logrus.Infof("certificate %s signed by %s: notBefore=%s notAfter=%s",
			parsedCert.Subject, caCert.Subject, parsedCert.NotBefore, parsedCert.NotAfter)
	}
	return parsedCert, err
}

func NewPool(filename string) (*x509.CertPool, error) {
	certs, err := CertsFromFile(filename)
	if err != nil {
		return nil, err
	}
	pool := x509.NewCertPool()
	for _, cert := range certs {
		pool.AddCert(cert)
	}
	return pool, nil
}

func CertsFromFile(file string) ([]*x509.Certificate, error) {
	pemBlock, err := ioutil.ReadFile(file)
	if err != nil {
		return nil, err
	}
	certs, err := ParseCertsPEM(pemBlock)
	if err != nil {
		return nil, fmt.Errorf("error reading %s: %s", file, err)
	}
	return certs, nil
}

func CreateCACertKey(prefix, certFile, keyFile string) (bool, error) {
	if exists(certFile, keyFile) {
		return false, nil
	}
	caKey, err := LoadOrGenerateKeyFile(keyFile, false)
	if err != nil {
		return false, err
	}
	cfg := Config{
		CommonName: fmt.Sprintf("%s-ca@%d", prefix, time.Now().Unix()),
	}
	cert, err := NewSelfSignedCACert(cfg, caKey)
	if err != nil {
		return false, err
	}

	if err := WriteCert(certFile, EncodeCertPEM(cert)); err != nil {
		return false, err
	}
	return true, nil
}

func CreateClientCertKey(regen bool, commonName string, organization []string, altNames *AltNames, extKeyUsage []x509.ExtKeyUsage,
	caCertFile, caKeyFile, certFile, keyFile string) (bool, error) {
	caBytes, err := ioutil.ReadFile(caCertFile)
	if err != nil {
		return false, err
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caBytes)
	//not regenerate check certificate expiration
	if !regen {
		regen = expiredCert(certFile, pool)
	}
	caKeyBytes, err := ioutil.ReadFile(caKeyFile)
	if err != nil {
		return false, err
	}
	caKey, err := ParsePrivateKeyPEM(caKeyBytes)
	if err != nil {
		return false, err
	}
	caCert, err := ParseCertsPEM(caBytes)
	if err != nil {
		return false, err
	}

	key, err := LoadOrGenerateKeyFile(keyFile, regen)
	if err != nil {
		return false, err
	}
	cfg := Config{
		CommonName:   commonName,
		Organization: organization,
		Usages:       extKeyUsage,
	}
	if altNames != nil {
		cfg.AltNames = *altNames
	}
	cert, err := NewSignedCert(cfg, key, caCert[0], caKey)
	if err != nil {
		return false, err
	}

	return true, WriteCert(certFile, append(EncodeCertPEM(cert), EncodeCertPEM(caCert[0])...))
}

//Generate cert key
func LoadOrGenerateKeyFile(keyPath string, force bool) (crypto.Signer, error) {
	if !force {
		loadedData, err := ioutil.ReadFile(keyPath)
		if err == nil {
			sign, err := ParsePrivateKeyPEM(loadedData)
			if err != nil {
				return nil, err
			}
			return sign, nil
		}
		if !os.IsNotExist(err) {
			return nil, fmt.Errorf("error loading key from %s: %v", keyPath, err)
		}
	}
	privateKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, err
	}
	generatedData, err := MakeEllipticPrivateKeyPEM(privateKey)
	if err != nil {
		return nil, fmt.Errorf("error generating key: %v", err)
	}
	if err := WriteKey(keyPath, generatedData); err != nil {
		return nil, fmt.Errorf("error writing key to %s: %v", keyPath, err)
	}
	return privateKey, nil
}

func WriteKey(keyPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(keyPath), os.FileMode(0755)); err != nil {
		return err
	}
	return ioutil.WriteFile(keyPath, data, os.FileMode(0600))
}

func WriteCert(certPath string, data []byte) error {
	if err := os.MkdirAll(filepath.Dir(certPath), os.FileMode(0755)); err != nil {
		return err
	}
	return ioutil.WriteFile(certPath, data, os.FileMode(0644))
}

func MakeEllipticPrivateKeyPEM(privateKey *ecdsa.PrivateKey) ([]byte, error) {
	derBytes, err := x509.MarshalECPrivateKey(privateKey)
	if err != nil {
		return nil, err
	}

	privateKeyPemBlock := &pem.Block{
		Type:  ECPrivateKeyBlockType,
		Bytes: derBytes,
	}
	return pem.EncodeToMemory(privateKeyPemBlock), nil
}

func ParsePrivateKeyPEM(keyData []byte) (crypto.Signer, error) {
	var privateKeyPemBlock *pem.Block

	privateKeyPemBlock, keyData = pem.Decode(keyData)
	if privateKeyPemBlock == nil {
		return nil, fmt.Errorf("data does not contain a valid RSA or ECDSA private key")
	}
	switch privateKeyPemBlock.Type {
	case ECPrivateKeyBlockType:
		// ECDSA Private Key in ASN.1 format
		if key, err := x509.ParseECPrivateKey(privateKeyPemBlock.Bytes); err == nil {
			return key, nil
		}
	case RSAPrivateKeyBlockType:
		// RSA Private Key in PKCS#1 format
		if key, err := x509.ParsePKCS1PrivateKey(privateKeyPemBlock.Bytes); err == nil {
			return key, nil
		}
	case PrivateKeyBlockType:
		// RSA or ECDSA Private Key in unencrypted PKCS#8 format
		if key, err := x509.ParsePKCS8PrivateKey(privateKeyPemBlock.Bytes); err == nil {
			return key.(crypto.Signer), nil
		}
	}
	return nil, fmt.Errorf("data does not contain a valid RSA or ECDSA private key")
}

func ParseCertsPEM(pemCerts []byte) ([]*x509.Certificate, error) {
	ok := false
	certs := []*x509.Certificate{}
	for len(pemCerts) > 0 {
		var block *pem.Block
		block, pemCerts = pem.Decode(pemCerts)
		if block == nil {
			break
		}
		// Only use PEM "CERTIFICATE" blocks without extra headers
		if block.Type != CertificateBlockType || len(block.Headers) != 0 {
			continue
		}

		cert, err := x509.ParseCertificate(block.Bytes)
		if err != nil {
			return certs, err
		}

		certs = append(certs, cert)
		ok = true
	}

	if !ok {
		return certs, errors.New("data does not contain any valid RSA or ECDSA certificates")
	}
	return certs, nil
}

func exists(files ...string) bool {
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			return false
		}
	}
	return true
}

func getCertExpiredDay(certFile string, pool *x509.CertPool) (float64, error) {
	certBytes, err := ioutil.ReadFile(certFile)
	if err != nil {
		return 0, err
	}
	certificates, err := ParseCertsPEM(certBytes)
	if err != nil {
		return 0, err
	}
	_, err = certificates[0].Verify(x509.VerifyOptions{
		Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageAny,
		},
	})
	if err != nil {
		return 0, err
	}
	cert := certificates[0]
	expirationDate := cert.NotAfter
	diffDays := time.Until(expirationDate).Hours() / 24.0
	logrus.Infof("certificate %s will expire in %f days at %s", cert.Subject, diffDays, cert.NotAfter)
	return diffDays, nil
}

func expiredCert(certFile string, pool *x509.CertPool) bool {
	certBytes, err := ioutil.ReadFile(certFile)
	if err != nil {
		return false
	}
	certificates, err := ParseCertsPEM(certBytes)
	if err != nil {
		return false
	}
	_, err = certificates[0].Verify(x509.VerifyOptions{
		Roots: pool,
		KeyUsages: []x509.ExtKeyUsage{
			x509.ExtKeyUsageAny,
		},
	})
	if err != nil {
		return true
	}
	return IsCertExpired(certificates[0], CertificateRenewDays)
}

// IsCertExpired checks if the certificate about to expire
func IsCertExpired(cert *x509.Certificate, days int) bool {
	expirationDate := cert.NotAfter
	diffDays := time.Until(expirationDate).Hours() / 24.0
	if diffDays <= float64(days) {
		logrus.Infof("certificate %s will expire in %f days at %s", cert.Subject, diffDays, cert.NotAfter)
		return true
	}
	return false
}

// EncodePrivateKeyPEM returns PEM-encoded private key data
func EncodePrivateKeyPEM(key *rsa.PrivateKey) []byte {
	block := pem.Block{
		Type:  RSAPrivateKeyBlockType,
		Bytes: x509.MarshalPKCS1PrivateKey(key),
	}
	return pem.EncodeToMemory(&block)
}

func EncodeCertPEM(cert *x509.Certificate) []byte {
	block := pem.Block{
		Type:  CertificateBlockType,
		Bytes: cert.Raw,
	}
	return pem.EncodeToMemory(&block)
}
