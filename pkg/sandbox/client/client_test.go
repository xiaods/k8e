package client

import (
	"bytes"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestNewClientWithEndpointRemoteWithoutKeyRequiresCachedCerts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_APIKEY", "")

	_, err := NewClientWithEndpoint("sandbox.example.com:50051", "")
	if err == nil {
		t.Fatal("expected cached-cert error")
	}
	if !strings.Contains(err.Error(), "cached") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestNewClientWithEndpointLoopbackWithoutKeyUsesLocalDiscovery(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_APIKEY", "")

	c, err := NewClientWithEndpoint("127.0.0.1:50051", "")
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
}

func TestSandboxCacheDirPriority(t *testing.T) {
	explicit := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", explicit)
	dir, err := sandboxCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	if dir != explicit {
		t.Fatalf("CERT_DIR should win: got %q want %q", dir, explicit)
	}

	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	dir, err = sandboxCacheDir()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".k8e", "sandbox")
	if dir != want {
		t.Fatalf("home path: got %q want %q", dir, want)
	}
}

func TestInspectClientCertSinglePass(t *testing.T) {
	dir := t.TempDir()
	certFile := filepath.Join(dir, "client.crt")

	// missing
	st := inspectClientCert(certFile)
	if st.valid || st.expiringSoon {
		t.Fatalf("missing cert should be invalid: %+v", st)
	}

	// fresh cert (60d remaining with 30d renew window → not expiring soon)
	writeTestClientCert(t, certFile, time.Now().Add(60*24*time.Hour))
	st = inspectClientCert(certFile)
	if !st.valid {
		t.Fatal("expected valid cert")
	}
	if st.expiringSoon {
		t.Fatal("60d remaining should not be expiring soon with 30d window")
	}

	// near-expiry (20d remaining)
	writeTestClientCert(t, certFile, time.Now().Add(20*24*time.Hour))
	st = inspectClientCert(certFile)
	if !st.valid || !st.expiringSoon {
		t.Fatalf("20d remaining should be valid+expiringSoon: %+v", st)
	}

	// expired
	writeTestClientCert(t, certFile, time.Now().Add(-time.Hour))
	st = inspectClientCert(certFile)
	if st.valid {
		t.Fatal("expired cert must be invalid")
	}
}

func TestAtomicWriteFileAndLoadOrGenerateKey(t *testing.T) {
	dir := t.TempDir()
	keyFile := filepath.Join(dir, "client.key")

	k1, err := loadOrGenerateKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	k2, err := loadOrGenerateKey(keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if k1.D.Cmp(k2.D) != 0 {
		t.Fatal("loadOrGenerateKey should reuse existing key")
	}

	// atomicWriteFile should produce the exact payload after rename
	path := filepath.Join(dir, "ca.crt")
	payload := []byte("-----BEGIN CERTIFICATE-----\nMIIB\n-----END CERTIFICATE-----\n")
	if err := atomicWriteFile(path, payload, 0644); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, payload) {
		t.Fatalf("atomic write mismatch: %q", got)
	}
	// no leftover temp files
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.Contains(e.Name(), ".tmp") {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}

func TestDialErrFriendlyHint(t *testing.T) {
	err := dialErr("sandbox.example.com:50051", &x509.UnknownAuthorityError{})
	if !strings.Contains(err.Error(), "hint:") {
		t.Fatalf("expected recovery hint, got: %v", err)
	}
}

func TestEndpointStampRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if err := writeEndpointStamp(dir, "a.example:50051"); err != nil {
		t.Fatal(err)
	}
	if err := checkEndpointStamp(dir, "a.example:50051"); err != nil {
		t.Fatalf("same endpoint should match: %v", err)
	}
	err := checkEndpointStamp(dir, "b.example:50051")
	if err == nil || !strings.Contains(err.Error(), "cached certs are for") {
		t.Fatalf("expected endpoint mismatch error, got %v", err)
	}
	// missing stamp is ok (legacy installs)
	if err := checkEndpointStamp(t.TempDir(), "any:1"); err != nil {
		t.Fatalf("missing stamp should be tolerated: %v", err)
	}
}

func TestLoginRequestAuditFields(t *testing.T) {
	t.Setenv("K8E_SANDBOX_DEVICE_NAME", "ci-runner-7")
	req := loginRequest("-----BEGIN CERTIFICATE REQUEST-----\nX\n-----END CERTIFICATE REQUEST-----")
	if req.DeviceName != "ci-runner-7" {
		t.Fatalf("device name: got %q", req.DeviceName)
	}
	if req.ClientVersion == "" {
		t.Fatal("expected client version from pkg/version")
	}
	if req.Csr == "" {
		t.Fatal("csr required")
	}
}

func TestLoadMTLSMaterial(t *testing.T) {
	dir := t.TempDir()
	caFile := filepath.Join(dir, "ca.crt")
	certFile := filepath.Join(dir, "client.crt")
	keyFile := filepath.Join(dir, "client.key")

	// Generate a mini CA + leaf so LoadX509KeyPair succeeds.
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	caTmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTmpl, caTmpl, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(caFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER}), 0644); err != nil {
		t.Fatal(err)
	}
	leafKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	leafTmpl := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: "leaf"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	leafDER, err := x509.CreateCertificate(rand.Reader, leafTmpl, caTmpl, &leafKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(certFile, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: leafDER}), 0644); err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(leafKey)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyFile, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}), 0600); err != nil {
		t.Fatal(err)
	}

	mat, err := loadMTLSMaterial(caFile, certFile, keyFile)
	if err != nil {
		t.Fatal(err)
	}
	if mat.pool == nil || len(mat.clientCert.Certificate) == 0 {
		t.Fatal("expected pool and client cert")
	}
}

func writeTestClientCert(t *testing.T, path string, notAfter time.Time) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "test"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     notAfter,
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0644); err != nil {
		t.Fatal(err)
	}
}
