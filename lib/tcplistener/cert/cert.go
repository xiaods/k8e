package cert

import (
	"crypto"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"io/ioutil"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"time"
)

const (
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
		IsCA:                  true,
	}

	certDERBytes, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, key.Public(), key)
	if err != nil {
		return nil, err
	}
	return x509.ParseCertificate(certDERBytes)
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

//Generate cert key
func LoadOrGenerateKeyFile(keyPath string, force bool) (crypto.Signer, error) {
	if !force {
		loadedData, err := ioutil.ReadFile(keyPath)
		if err != nil && !os.IsNotExist(err) {
			return nil, fmt.Errorf("error loading key from %s: %v", keyPath, err)
		}
		sign, err := ParsePrivateKeyPEM(loadedData)
		if err != nil {
			return nil, err
		}
		return sign, nil
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

func EncodeCertPEM(cert *x509.Certificate) []byte {
	block := pem.Block{
		Type:  CertificateBlockType,
		Bytes: cert.Raw,
	}
	return pem.EncodeToMemory(&block)
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

func exists(files ...string) bool {
	for _, file := range files {
		if _, err := os.Stat(file); err != nil {
			return false
		}
	}
	return true
}
