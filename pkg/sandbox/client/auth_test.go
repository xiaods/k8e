package client

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
)

type authGateway struct {
	pb.UnimplementedSandboxServiceServer
	ca        *x509.Certificate
	key       *ecdsa.PrivateKey
	caPEM     string
	calls     atomic.Int32
	malformed atomic.Bool
}

func (s *authGateway) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	n := s.calls.Add(1)
	md, _ := metadata.FromIncomingContext(ctx)
	auth := md.Get("authorization")
	if len(auth) != 1 || auth[0] != "Bearer valid-key" {
		return nil, status.Error(codes.Unauthenticated, "invalid key")
	}
	block, _ := pem.Decode([]byte(req.Csr))
	if block == nil {
		return nil, status.Error(codes.InvalidArgument, "CSR required")
	}
	csr, err := x509.ParseCertificateRequest(block.Bytes)
	if err != nil {
		return nil, err
	}
	if err := csr.CheckSignature(); err != nil {
		return nil, err
	}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(int64(n) + 10), Subject: pkix.Name{CommonName: "test-user"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(60 * 24 * time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth}}
	der, err := x509.CreateCertificate(rand.Reader, leaf, s.ca, csr.PublicKey, s.key)
	if err != nil {
		return nil, err
	}
	resp := &pb.LoginResponse{Cert: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})), CaCert: s.caPEM}
	if s.malformed.Load() {
		resp.Cert = "invalid certificate"
	}
	return resp, nil
}

func startAuthGateway(t *testing.T) (*authGateway, string, string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "test CA"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(365 * 24 * time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	der, err := x509.CreateCertificate(rand.Reader, ca, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	ca, err = x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	s := &authGateway{ca: ca, key: key, caPEM: string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))}
	leaf := &x509.Certificate{SerialNumber: big.NewInt(2), DNSNames: []string{"gateway.internal"}, NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth}}
	der, err = x509.CreateCertificate(rand.Reader, leaf, ca, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(ca)
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{der}, PrivateKey: key}}, ClientAuth: tls.VerifyClientCertIfGiven, ClientCAs: pool})))
	pb.RegisterSandboxServiceServer(server, s)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	go server.Serve(listener)
	t.Cleanup(server.Stop)
	path := filepath.Join(t.TempDir(), "trusted-ca.crt")
	if err := os.WriteFile(path, []byte(s.caPEM), 0600); err != nil {
		t.Fatal(err)
	}
	return s, listener.Addr().String(), path
}

func TestLoginServerTrust(t *testing.T) {
	s, endpoint, ca := startAuthGateway(t)
	_, _, wrongCA := startAuthGateway(t)
	for _, tc := range []struct {
		name, ca     string
		insecure, ok bool
	}{
		{"system-rejects-private-CA", "", false, false},
		{"trusted-loopback", ca, false, true},
		{"wrong-loopback-CA", wrongCA, false, false},
		{"opt-in-does-not-ignore-existing-CA", wrongCA, true, false},
		{"explicit-insecure-bootstrap", "", true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg, err := loginTLSConfig(endpoint, tc.ca, tc.insecure)
			if err != nil {
				t.Fatal(err)
			}
			// Exercise the actual TLS boundary: an untrusted peer cannot receive an API key.
			conn, err := tls.DialWithDialer(&net.Dialer{Timeout: time.Second}, "tcp", endpoint, cfg)
			if conn != nil {
				conn.Close()
			}
			if (err == nil) != tc.ok {
				t.Fatalf("handshake error=%v, want success=%v", err, tc.ok)
			}
		})
	}
	if s.calls.Load() != 0 {
		t.Fatal("handshake test unexpectedly authenticated")
	}
}

func TestExplicitLoginAndFailedResetPreserveCredentials(t *testing.T) {
	s, endpoint, ca := startAuthGateway(t)
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)
	c, err := NewClientWithOptions(endpoint, "valid-key", ConnectOptions{CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	old, err := os.ReadFile(filepath.Join(dir, clientCertFile))
	if err != nil {
		t.Fatal(err)
	}
	c, err = NewClientWithEndpoint(endpoint, "valid-key")
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if s.calls.Load() != 1 {
		t.Fatal("ordinary connection should reuse credentials")
	}
	_, err = NewClientWithOptions(endpoint, "invalid-key", ConnectOptions{ForceLogin: true})
	if status.Code(err) != codes.Unauthenticated {
		t.Fatalf("explicit login must validate new key: %v", err)
	}
	if s.calls.Load() != 2 {
		t.Fatal("explicit login skipped RPC")
	}
	s.malformed.Store(true)
	_, err = NewClientWithOptions(endpoint, "valid-key", ConnectOptions{ResetCerts: true, CAFile: ca})
	if err == nil {
		t.Fatal("invalid response accepted")
	}
	got, err := os.ReadFile(filepath.Join(dir, clientCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, old) {
		t.Fatal("failed authentication replaced old credentials")
	}
	s.malformed.Store(false)
	c, err = NewClientWithOptions(endpoint, "valid-key", ConnectOptions{ForceLogin: true})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	got, err = os.ReadFile(filepath.Join(dir, clientCertFile))
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Equal(got, old) {
		t.Fatal("explicit login did not refresh certificate")
	}
}

// Child processes exercise the OS lock, rather than just an in-process mutex.
func TestConcurrentCredentialBootstrap(t *testing.T) {
	if endpoint := os.Getenv("K8E_TEST_AUTH_ENDPOINT"); endpoint != "" {
		c, err := NewClientWithOptions(endpoint, "valid-key", ConnectOptions{CAFile: os.Getenv("K8E_TEST_AUTH_CA")})
		if err != nil {
			t.Fatal(err)
		}
		c.Close()
		return
	}
	s, endpoint, ca := startAuthGateway(t)
	dir := t.TempDir()
	var wg sync.WaitGroup
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(os.Args[0], "-test.run=^TestConcurrentCredentialBootstrap$")
			cmd.Env = append(os.Environ(), "K8E_TEST_AUTH_ENDPOINT="+endpoint, "K8E_TEST_AUTH_CA="+ca, "K8E_SANDBOX_CERT_DIR="+dir)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Errorf("child: %v: %s", err, out)
			}
		}()
	}
	wg.Wait()
	if s.calls.Load() != 1 {
		t.Fatalf("expected one bootstrap, got %d", s.calls.Load())
	}
	if _, err := loadMTLSMaterial(filepath.Join(dir, caFileName), filepath.Join(dir, clientCertFile), filepath.Join(dir, clientKeyFile)); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialLockDeadline(t *testing.T) {
	dir := t.TempDir()
	unlock, err := lockCredentials(context.Background(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
	defer cancel()
	if release, err := lockCredentials(ctx, dir); err == nil {
		release()
		t.Fatal("contended lock ignored deadline")
	}
}

func TestResetCredentialsReplacesCAAndCorruptKey(t *testing.T) {
	_, endpoint, ca := startAuthGateway(t)
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)
	c, err := NewClientWithOptions(endpoint, "valid-key", ConnectOptions{CAFile: ca})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	if err := os.WriteFile(filepath.Join(dir, clientKeyFile), []byte("broken key"), 0600); err != nil {
		t.Fatal(err)
	}
	next, nextEndpoint, nextCA := startAuthGateway(t)
	c, err = NewClientWithOptions(nextEndpoint, "valid-key", ConnectOptions{CAFile: nextCA, ResetCerts: true})
	if err != nil {
		t.Fatal(err)
	}
	c.Close()
	mat, err := loadMTLSMaterial(filepath.Join(dir, caFileName), filepath.Join(dir, clientCertFile), filepath.Join(dir, clientKeyFile))
	if err != nil {
		t.Fatal(err)
	}
	if len(mat.clientCert.Certificate) == 0 {
		t.Fatal("missing client certificate")
	}
	got, err := os.ReadFile(filepath.Join(dir, caFileName))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != next.caPEM {
		t.Fatal("reset did not replace CA")
	}
	if err := checkEndpointStamp(dir, nextEndpoint); err != nil {
		t.Fatal(err)
	}
}

func TestCredentialPublicationRollsBack(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprint(existing), func(t *testing.T) {
			dir := t.TempDir()
			files := newCredentialFiles()
			seedCredentials(t, dir, files, existing)
			if err := publishCredentials(dir, files, renameAndFailOnThird()); err == nil {
				t.Fatal("expected publication failure")
			}
			assertCredentialsRolledBack(t, dir, files, existing)
		})
	}
}

// newCredentialFiles is a full credential set whose publication can be made to
// fail partway through, exercising the rollback path.
func newCredentialFiles() []credentialFile {
	return []credentialFile{
		{clientKeyFile, []byte("new-key"), 0600},
		{caFileName, []byte("new-ca"), 0644},
		{clientCertFile, []byte("new-cert"), 0644},
		{endpointStampFile, []byte("new-endpoint"), 0644},
	}
}

// seedCredentials pre-creates every credential file when existing is true, so
// the rollback has something to restore; otherwise the directory stays empty.
func seedCredentials(t *testing.T, dir string, files []credentialFile, existing bool) {
	t.Helper()
	if !existing {
		return
	}
	for _, f := range files {
		if err := os.WriteFile(filepath.Join(dir, f.name), []byte("old-"+f.name), f.mode); err != nil {
			t.Fatal(err)
		}
	}
}

// renameAndFailOnThird renames credential files until the third one, which
// fails with an injected disk error.
func renameAndFailOnThird() func(from, to string) error {
	calls := 0
	return func(from, to string) error {
		calls++
		if calls == 3 {
			return errors.New("injected disk failure")
		}
		return os.Rename(from, to)
	}
}

// assertCredentialsRolledBack verifies the directory still holds the previous
// credentials, or nothing at all when the bootstrap had not published before.
func assertCredentialsRolledBack(t *testing.T, dir string, files []credentialFile, existing bool) {
	t.Helper()
	for _, f := range files {
		data, err := os.ReadFile(filepath.Join(dir, f.name))
		if !existing {
			if !os.IsNotExist(err) {
				t.Fatalf("partial bootstrap file %s remained: %v", f.name, err)
			}
			continue
		}
		if err != nil || string(data) != "old-"+f.name {
			t.Fatalf("%s not restored: %q %v", f.name, data, err)
		}
	}
}
