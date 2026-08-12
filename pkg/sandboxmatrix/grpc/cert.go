package grpc

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"
)

const caOrg = "K8E Sandbox"

// ensureCA loads the sandbox CA from disk, or generates a new ECDSA P-256 CA.
func ensureCA(caCertFile, caKeyFile string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	if caPEM, err := os.ReadFile(caCertFile); err == nil {
		if keyPEM, err := os.ReadFile(caKeyFile); err == nil {
			cert, key, loadErr := loadCA(caPEM, keyPEM)
			if loadErr == nil {
				return key, cert, nil
			}
		}
	}
	return generateCA(caCertFile, caKeyFile)
}

func loadCA(certPEM, keyPEM []byte) (*x509.Certificate, *ecdsa.PrivateKey, error) {
	block, _ := pem.Decode(certPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("invalid CA cert PEM")
	}
	cert, err := x509.ParseCertificate(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA cert: %w", err)
	}
	block, _ = pem.Decode(keyPEM)
	if block == nil {
		return nil, nil, fmt.Errorf("invalid CA key PEM")
	}
	key, err := x509.ParseECPrivateKey(block.Bytes)
	if err != nil {
		return nil, nil, fmt.Errorf("parse CA key: %w", err)
	}
	return cert, key, nil
}

func generateCA(caCertFile, caKeyFile string) (*ecdsa.PrivateKey, *x509.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return nil, nil, fmt.Errorf("generate CA key: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "K8E Sandbox CA", Organization: []string{caOrg}},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageCRLSign,
		BasicConstraintsValid: true,
		IsCA:                  true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return nil, nil, fmt.Errorf("create CA cert: %w", err)
	}

	cert, err := x509.ParseCertificate(der)
	if err != nil {
		return nil, nil, fmt.Errorf("parse generated CA cert: %w", err)
	}

	if err := os.WriteFile(caKeyFile, pemEncodeECPrivateKey(key), 0600); err != nil {
		return nil, nil, fmt.Errorf("write CA key: %w", err)
	}
	if err := os.WriteFile(caCertFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return nil, nil, fmt.Errorf("write CA cert: %w", err)
	}

	return key, cert, nil
}

func pemEncodeECPrivateKey(key *ecdsa.PrivateKey) []byte {
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		panic("marshal EC private key: " + err.Error())
	}
	return pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: der})
}

// ensureServerCert ensures the gRPC gateway has a valid server certificate signed by the sandbox CA.
// If the existing cert has > 30 days of validity, it is reused.
func ensureServerCert(caKey *ecdsa.PrivateKey, caCert *x509.Certificate, certFile, keyFile string) error {
	if existingCert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		if len(existingCert.Certificate) > 0 {
			if c, parseErr := x509.ParseCertificate(existingCert.Certificate[0]); parseErr == nil {
				if time.Now().Before(c.NotAfter.Add(-30 * 24 * time.Hour)) {
					return nil // still valid > 30 days
				}
			}
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}

	hostname, _ := os.Hostname()
	sans := collectServerSANs(hostname)

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: hostname, Organization: []string{caOrg}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(90 * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     sans.dnsNames,
		IPAddresses:  sans.ips,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, &key.PublicKey, caKey)
	if err != nil {
		return fmt.Errorf("create server cert: %w", err)
	}

	if err := os.WriteFile(keyFile, pemEncodeECPrivateKey(key), 0600); err != nil {
		return fmt.Errorf("write server key: %w", err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return fmt.Errorf("write server cert: %w", err)
	}

	return nil
}

type serverSANs struct {
	dnsNames []string
	ips      []net.IP
}

func collectServerSANs(hostname string) serverSANs {
	var s serverSANs
	if hostname != "" {
		s.dnsNames = append(s.dnsNames, hostname)
	}

	ifaces, _ := net.Interfaces()
	for _, iface := range ifaces {
		addrs, _ := iface.Addrs()
		for _, addr := range addrs {
			if ipnet, ok := addr.(*net.IPNet); ok && !ipnet.IP.IsLoopback() {
				s.ips = append(s.ips, ipnet.IP)
			}
		}
	}

	if adv := os.Getenv("K8E_SANDBOX_ADVERTISED_HOSTNAME"); adv != "" {
		s.dnsNames = append(s.dnsNames, adv)
	}

	return s
}

// signClientCert signs a client CSR with the sandbox CA.
// The CSR's Subject and SANs are ignored — the server controls certificate identity.
func signClientCert(caKey *ecdsa.PrivateKey, caCert *x509.Certificate, csrPEM, commonName string, ttlDays int) (certPEM string, fingerprint string, err error) {
	block, _ := pem.Decode([]byte(csrPEM))
	if block == nil {
		return "", "", fmt.Errorf("invalid CSR PEM")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return "", "", fmt.Errorf("parse CSR: %w", err)
	}
	if err := csr.CheckSignature(); err != nil {
		return "", "", fmt.Errorf("CSR signature invalid: %w", err)
	}

	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(time.Now().UnixNano()),
		Subject:      pkix.Name{CommonName: commonName, Organization: []string{caOrg}},
		NotBefore:    time.Now(),
		NotAfter:     time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		CRLDistributionPoints:  []string{"https://k8e.internal/sandbox/crl"},
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, caCert, csr.PublicKey, caKey)
	if err != nil {
		return "", "", fmt.Errorf("create client cert: %w", err)
	}

	h := sha256.Sum256(der)
	fingerprint = fmt.Sprintf("%x", h)

	return string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), fingerprint, nil
}

// issuedCertRecord tracks a signed client certificate.
type issuedCertRecord struct {
	KeyName     string    `json:"key_name"`
	Fingerprint string    `json:"fingerprint"`
	IssuedAt    time.Time `json:"issued_at"`
	ExpiresAt   time.Time `json:"expires_at"`
}

// issuedCertStore persists issued certificate records to a JSON file.
type issuedCertStore struct {
	mu       sync.Mutex
	records  []issuedCertRecord
	filePath string
}

func newIssuedCertStore(filePath string) *issuedCertStore {
	s := &issuedCertStore{filePath: filePath}
	data, err := os.ReadFile(filePath)
	if err != nil {
		return s
	}
	json.Unmarshal(data, &s.records) // best-effort
	return s
}

func (s *issuedCertStore) Add(keyName, fingerprint string, issuedAt, expiresAt time.Time) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records = append(s.records, issuedCertRecord{
		KeyName: keyName, Fingerprint: fingerprint,
		IssuedAt: issuedAt, ExpiresAt: expiresAt,
	})
	s.save()
}

func (s *issuedCertStore) FindByKeyName(keyName string) []issuedCertRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []issuedCertRecord
	for _, r := range s.records {
		if r.KeyName == keyName {
			out = append(out, r)
		}
	}
	return out
}

func (s *issuedCertStore) PruneExpired() {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	n := 0
	for _, r := range s.records {
		if r.ExpiresAt.After(now) {
			s.records[n] = r
			n++
		}
	}
	// Only rewrite the ledger when something was removed (hot Login path).
	if n == len(s.records) {
		return
	}
	s.records = s.records[:n]
	s.save()
}

func (s *issuedCertStore) save() {
	data, err := json.MarshalIndent(s.records, "", "  ")
	if err != nil {
		return
	}
	// Atomic replace so concurrent readers never see a half-written ledger.
	tmp := s.filePath + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return
	}
	_ = os.Rename(tmp, s.filePath)
}

// RevocationList is an in-memory list of revoked certificate fingerprints.
type RevocationList struct {
	mu      sync.RWMutex
	entries map[string]time.Time // fingerprint → revokedAt
}

func newRevocationList() *RevocationList {
	return &RevocationList{entries: make(map[string]time.Time)}
}

func (rl *RevocationList) Revoke(fingerprint string) {
	rl.mu.Lock()
	rl.entries[fingerprint] = time.Now()
	rl.mu.Unlock()
}

func (rl *RevocationList) IsRevoked(fingerprint string) bool {
	rl.mu.RLock()
	defer rl.mu.RUnlock()
	_, ok := rl.entries[fingerprint]
	return ok
}

func (rl *RevocationList) RevokeByKeyName(store *issuedCertStore, keyName string) {
	for _, r := range store.FindByKeyName(keyName) {
		rl.Revoke(r.Fingerprint)
	}
}

// buildMTLSCreds creates gRPC transport credentials with mTLS (VerifyClientCertIfGiven).
func buildMTLSCreds(caCert *x509.Certificate, serverCert tls.Certificate) (credentials.TransportCredentials, error) {
	pool := x509.NewCertPool()
	pool.AddCert(caCert)

	return credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{serverCert},
		ClientAuth:   tls.VerifyClientCertIfGiven,
		ClientCAs:    pool,
		MinVersion:   tls.VersionTLS12,
	}), nil
}

// peerIdentity extracts the authenticated identity from a gRPC context.
// Returns (commonName, true) for loopback connections without a client cert.
func peerIdentity(ctx context.Context) (keyName string, isLocal bool) {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return "", false
	}

	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok {
		// No TLS — check loopback
		if isLoopbackAddr(p.Addr) {
			return "", true
		}
		return "", false
	}

	if len(tlsInfo.State.PeerCertificates) == 0 {
		if isLoopbackAddr(p.Addr) {
			return "", true
		}
		return "", false
	}

	cert := tlsInfo.State.PeerCertificates[0]
	now := time.Now()
	if now.Before(cert.NotBefore) || now.After(cert.NotAfter) {
		return "", false // expired/invalid — TLS handshake should already reject
	}

	return cert.Subject.CommonName, false
}

func isLoopbackAddr(addr net.Addr) bool {
	if addr == nil {
		return false
	}
	host, _, err := net.SplitHostPort(addr.String())
	if err != nil {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}
