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
	"strings"
	"sync"
	"time"

	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/peer"

	"github.com/xiaods/k8e/pkg/daemons/config"
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
// The existing cert is reused only while it is still valid (> 30 days) AND its SANs still cover the
// currently configured advertise hostname — so changing --sandbox-advertise-hostname (or
// K8E_SANDBOX_ADVERTISED_HOSTNAME) forces a regeneration instead of silently serving a stale cert.
func ensureServerCert(caKey *ecdsa.PrivateKey, caCert *x509.Certificate, certFile, keyFile, advertiseHostname string) error {
	hostname, _ := os.Hostname()
	sans, err := collectServerSANs(hostname, advertiseHostname)
	if err != nil {
		return err
	}

	if existingCert, err := tls.LoadX509KeyPair(certFile, keyFile); err == nil {
		if len(existingCert.Certificate) > 0 {
			if c, parseErr := x509.ParseCertificate(existingCert.Certificate[0]); parseErr == nil {
				if time.Now().Before(c.NotAfter.Add(-30*24*time.Hour)) && sansCoveredBy(c, sans) {
					return nil // still valid > 30 days and SANs cover the configured names
				}
			}
		}
	}

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return fmt.Errorf("generate server key: %w", err)
	}

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

	// Atomic writes: each file is written to a sibling temp and renamed, so a
	// crash mid-rotation never leaves a truncated cert/key behind (SAN-driven
	// rotations can now fire on any restart after a config change).
	if err := atomicWriteFile(keyFile, pemEncodeECPrivateKey(key), 0600); err != nil {
		return fmt.Errorf("write server key: %w", err)
	}
	if err := atomicWriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		return fmt.Errorf("write server cert: %w", err)
	}

	return nil
}

type serverSANs struct {
	dnsNames []string
	ips      []net.IP
}

// collectServerSANs builds the server certificate SAN set: the machine hostname,
// every non-loopback interface address (in AWS these are the private VPC IPs),
// and the operator-configured external advertise hostname (--sandbox-advertise-hostname,
// merged with the legacy K8E_SANDBOX_ADVERTISED_HOSTNAME env var). A value that parses
// as an IP is added to the IP SANs; anything else is added as a DNS SAN.
//
// A malformed advertise hostname (scheme, host:port, path, whitespace, bad DNS
// label) is a hard error: the gateway must not start with a certificate that
// cannot authenticate the configured endpoint, so the failure is surfaced to
// the caller (gateway startup) instead of being silently omitted.
func collectServerSANs(hostname, advertiseHostname string) (serverSANs, error) {
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

	// First-class flag wins; the env var is merged as a fallback for operators
	// who set it directly in the k8e-server process environment. Either source
	// is validated — an invalid value fails the gateway startup loudly.
	for _, adv := range []string{advertiseHostname, os.Getenv("K8E_SANDBOX_ADVERTISED_HOSTNAME")} {
		adv = strings.TrimSpace(adv)
		if adv == "" {
			continue
		}
		if ip := net.ParseIP(adv); ip != nil {
			if !ip.IsLoopback() && !containsIP(s.ips, ip) {
				s.ips = append(s.ips, ip)
			}
			continue
		}
		if err := config.ValidateAdvertiseHostname(adv); err != nil {
			return serverSANs{}, err
		}
		if !containsString(s.dnsNames, adv) {
			s.dnsNames = append(s.dnsNames, adv)
		}
	}

	return s, nil
}

// atomicWriteFile writes data to a sibling temp file, fsyncs it, then renames
// over path so readers never observe a partially written file.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	tmp := path + ".tmp"
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := f.Write(data); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return err
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, path)
}

// sansCoveredBy reports whether cert already carries every SAN in want. Extra
// (stale) SANs on the cert are tolerated — only a missing desired name/IP forces
// regeneration.
func sansCoveredBy(cert *x509.Certificate, want serverSANs) bool {
	for _, d := range want.dnsNames {
		if !containsString(cert.DNSNames, d) {
			return false
		}
	}
	for _, ip := range want.ips {
		if !containsIP(cert.IPAddresses, ip) {
			return false
		}
	}
	return true
}

func containsString(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

func containsIP(list []net.IP, want net.IP) bool {
	for _, ip := range list {
		if ip.Equal(want) {
			return true
		}
	}
	return false
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
		SerialNumber:          big.NewInt(time.Now().UnixNano()),
		Subject:               pkix.Name{CommonName: commonName, Organization: []string{caOrg}},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(time.Duration(ttlDays) * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
		IsCA:                  false,
		CRLDistributionPoints: []string{"https://k8e.internal/sandbox/crl"},
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
