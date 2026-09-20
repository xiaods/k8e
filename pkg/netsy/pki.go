package netsy

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"time"
)

// Certificate roles and URI SAN scheme understood by Netsy (internal/mtls).
const (
	uriScheme  = "netsy"
	rolePeer   = "peer"
	roleClient = "client"
)

// CertPaths is the on-disk mTLS PKI Netsy and k8e share.
type CertPaths struct {
	CA             string
	ServerCert     string
	ServerKey      string
	PeerClientCert string
	PeerClientKey  string
	// DatastoreCert/DatastoreKey carry the `client` role and are what
	// kube-apiserver and pkg/etcdstorage present to the Netsy client API.
	DatastoreCert string
	DatastoreKey  string
}

const (
	caCertFile       = "ca.crt"
	caKeyFile        = "ca.key"
	serverCertFile   = "server.crt"
	serverKeyFile    = "server.key"
	peerCertFile     = "peer-client.crt"
	peerKeyFile      = "peer-client.key"
	datastoreCert    = "datastore-client.crt"
	datastoreKeyFile = "datastore-client.key"
)

// leafValidity is how long the generated leaf certificates are valid. It is a
// variable so tests can drive the renewal window.
var leafValidity = 365 * 24 * time.Hour

// certRenewBefore is the window before a leaf expires in which EnsurePKI
// regenerates the whole PKI. The datastore clients load their certificates at
// startup only, so a node that restarts inside this window renews them instead
// of later serving a certificate that expires while it is running.
const certRenewBefore = 30 * 24 * time.Hour

// validPKI reports whether the PKI files already exist, chain to the CA, are
// still valid for the given cluster, node and server hosts and are not yet
// inside the renewal window.
func validPKI(paths CertPaths, clusterID, nodeID string, hosts []string) bool {
	caPEM, err := os.ReadFile(paths.CA)
	if err != nil {
		return false
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(caPEM) {
		return false
	}

	// Leaf and CA must be one generation: an interrupted regeneration leaves a
	// new CA next to old leaves, which load and parse fine and only the
	// signature check catches.
	server, ok := validLeaf(paths.ServerCert, paths.ServerKey, roots,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}, clusterID, rolePeer, nodeID)
	if !ok {
		return false
	}
	for _, host := range hosts {
		if err := server.VerifyHostname(host); err != nil {
			return false
		}
	}

	if _, ok := validLeaf(paths.PeerClientCert, paths.PeerClientKey, roots,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, clusterID, rolePeer, nodeID); !ok {
		return false
	}

	if _, ok := validLeaf(paths.DatastoreCert, paths.DatastoreKey, roots,
		[]x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}, clusterID, roleClient, DefaultClientName); !ok {
		return false
	}

	return true
}

// validLeaf loads a leaf and reports whether it is signed by the CA in roots,
// usable for one of the given purposes, carries the expected netsy role and
// identity and is outside the renewal window.
func validLeaf(certFile, keyFile string, roots *x509.CertPool, usages []x509.ExtKeyUsage, clusterID, role, identity string) (*x509.Certificate, bool) {
	leaf, err := loadLeaf(certFile, keyFile)
	if err != nil {
		return nil, false
	}
	if _, err := leaf.Verify(x509.VerifyOptions{Roots: roots, KeyUsages: usages}); err != nil {
		return nil, false
	}
	if err := checkLeaf(leaf, clusterID, role, identity); err != nil {
		return nil, false
	}
	if time.Until(leaf.NotAfter) <= certRenewBefore {
		return nil, false
	}
	return leaf, true
}

func loadLeaf(certFile, keyFile string) (*x509.Certificate, error) {
	pair, err := tls.LoadX509KeyPair(certFile, keyFile)
	if err != nil {
		return nil, err
	}
	if len(pair.Certificate) == 0 {
		return nil, fmt.Errorf("empty certificate chain in %s", certFile)
	}
	leaf, err := x509.ParseCertificate(pair.Certificate[0])
	if err != nil {
		return nil, err
	}
	now := time.Now()
	if now.Before(leaf.NotBefore) || now.After(leaf.NotAfter) {
		return nil, fmt.Errorf("certificate %s is not currently valid", certFile)
	}
	return leaf, nil
}

// checkLeaf verifies the netsy:// URI SAN role/cluster/identity and the subject
// organization, mirroring Netsy's own validation.
func checkLeaf(cert *x509.Certificate, clusterID, role, identity string) error {
	if len(cert.Subject.Organization) == 0 || cert.Subject.Organization[0] != clusterID {
		return fmt.Errorf("certificate organization %v does not match cluster %q", cert.Subject.Organization, clusterID)
	}
	for _, u := range cert.URIs {
		if u.Scheme != uriScheme {
			continue
		}
		if u.Host != clusterID {
			continue
		}
		if u.Path == "/"+role+"/"+identity {
			return nil
		}
	}
	return fmt.Errorf("certificate has no netsy://%s/%s/%s URI SAN", clusterID, role, identity)
}

// EnsurePKI loads the existing PKI from certDir, regenerating it when it is
// missing or no longer matches the cluster, node or server hosts.
func EnsurePKI(certDir, clusterID, nodeID, clientName string, hosts []string) (CertPaths, error) {
	paths := CertPaths{
		CA:             filepath.Join(certDir, caCertFile),
		ServerCert:     filepath.Join(certDir, serverCertFile),
		ServerKey:      filepath.Join(certDir, serverKeyFile),
		PeerClientCert: filepath.Join(certDir, peerCertFile),
		PeerClientKey:  filepath.Join(certDir, peerKeyFile),
		DatastoreCert:  filepath.Join(certDir, datastoreCert),
		DatastoreKey:   filepath.Join(certDir, datastoreKeyFile),
	}
	if validPKI(paths, clusterID, nodeID, hosts) {
		return paths, nil
	}
	return paths, generatePKI(paths, clusterID, nodeID, clientName, hosts)
}

func generatePKI(paths CertPaths, clusterID, nodeID, clientName string, hosts []string) error {
	if err := os.MkdirAll(filepath.Dir(paths.CA), 0700); err != nil {
		return fmt.Errorf("failed to create netsy cert dir: %w", err)
	}

	caKey, caCert, err := newCA(clusterID)
	if err != nil {
		return err
	}

	dnsNames, ips := splitHosts(hosts)

	serverKey, serverCert, err := newLeaf(caCert, caKey, leafSpec{
		clusterID: clusterID,
		role:      rolePeer,
		identity:  nodeID,
		usages:    []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		dnsNames:  dnsNames,
		ips:       ips,
	})
	if err != nil {
		return err
	}
	peerKey, peerCert, err := newLeaf(caCert, caKey, leafSpec{
		clusterID: clusterID,
		role:      rolePeer,
		identity:  nodeID,
		usages:    []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return err
	}
	clientKey, clientCert, err := newLeaf(caCert, caKey, leafSpec{
		clusterID: clusterID,
		role:      roleClient,
		identity:  clientName,
		usages:    []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	})
	if err != nil {
		return err
	}

	files := []struct {
		path string
		perm os.FileMode
		typ  string
		der  []byte
	}{
		{paths.CA, 0644, "CERTIFICATE", caCert.Raw},
		{paths.ServerCert, 0644, "CERTIFICATE", serverCert.Raw},
		{paths.ServerKey, 0600, "EC PRIVATE KEY", mustMarshalKey(serverKey)},
		{paths.PeerClientCert, 0644, "CERTIFICATE", peerCert.Raw},
		{paths.PeerClientKey, 0600, "EC PRIVATE KEY", mustMarshalKey(peerKey)},
		{paths.DatastoreCert, 0644, "CERTIFICATE", clientCert.Raw},
		{paths.DatastoreKey, 0600, "EC PRIVATE KEY", mustMarshalKey(clientKey)},
	}
	for _, f := range files {
		if err := writePEM(f.path, f.perm, f.typ, f.der); err != nil {
			return err
		}
	}
	return nil
}

type leafSpec struct {
	clusterID string
	role      string
	identity  string
	usages    []x509.ExtKeyUsage
	dnsNames  []string
	ips       []net.IP
}

func newCA(clusterID string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate netsy CA key: %w", err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber:          serial,
		Subject:               pkix.Name{CommonName: clusterID + "-ca", Organization: []string{clusterID}},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		IsCA:                  true,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create netsy CA certificate: %w", err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse netsy CA certificate: %w", err)
	}
	return key, cert, nil
}

func newLeaf(caCert *x509.Certificate, caKey *ecdsa.PrivateKey, spec leafSpec) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to generate netsy %s key: %w", spec.role, err)
	}
	serial, err := newSerial()
	if err != nil {
		return nil, nil, err
	}
	now := time.Now()
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:         spec.identity,
			Organization:       []string{spec.clusterID},
			OrganizationalUnit: []string{spec.role},
		},
		NotBefore:             now.Add(-time.Hour),
		NotAfter:              now.Add(leafValidity),
		KeyUsage:              x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:           spec.usages,
		BasicConstraintsValid: true,
		DNSNames:              spec.dnsNames,
		IPAddresses:           spec.ips,
		URIs: []*url.URL{{
			Scheme: uriScheme,
			Host:   spec.clusterID,
			Path:   "/" + spec.role + "/" + spec.identity,
		}},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to create netsy %s certificate: %w", spec.role, err)
	}
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to parse netsy %s certificate: %w", spec.role, err)
	}
	return key, cert, nil
}

func splitHosts(hosts []string) ([]string, []net.IP) {
	var dnsNames []string
	var ips []net.IP
	for _, host := range hosts {
		if ip := net.ParseIP(host); ip != nil {
			ips = append(ips, ip)
			continue
		}
		dnsNames = append(dnsNames, host)
	}
	return dnsNames, ips
}

func newSerial() (*big.Int, error) {
	limit := new(big.Int).Lsh(big.NewInt(1), 128)
	serial, err := rand.Int(rand.Reader, limit)
	if err != nil {
		return nil, fmt.Errorf("failed to generate certificate serial: %w", err)
	}
	return serial, nil
}

func mustMarshalKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		// MarshalECPrivateKey only fails on unsupported curves; P-256 is
		// always supported, so this is unreachable.
		panic(err)
	}
	return der
}

func writePEM(path string, perm os.FileMode, typ string, der []byte) error {
	f, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return fmt.Errorf("failed to create %s: %w", path, err)
	}
	defer f.Close()
	if err := pem.Encode(f, &pem.Block{Type: typ, Bytes: der}); err != nil {
		return fmt.Errorf("failed to write %s: %w", path, err)
	}
	return nil
}
