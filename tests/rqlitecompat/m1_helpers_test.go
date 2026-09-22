package rqlitecompat

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	compat "github.com/xiaods/k8e/pkg/rqlitecompat"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// m1Server is one running compatibility server plus the gRPC clients that
// talk to it over the wire.
type m1Server struct {
	server *compat.Server
	addr   string
	conn   *grpc.ClientConn
	// serveDone is closed when the gRPC listener returns, so a test can stop
	// the layer and start a new one against the same data deterministically.
	serveDone chan struct{}
}

// startCompat boots the layer against an existing rqlite cluster on a loopback
// port and returns gRPC clients for it. mutate may adjust the config (watch
// interval, TLS, message-size limits).
func startCompat(t *testing.T, cluster *Cluster, mutate func(*compat.Config)) *m1Server {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	requireNoError(t, "listen for compat server", err)

	cfg := compat.Config{
		Endpoints:           cluster.Endpoints(),
		Owner:               "m1-" + t.Name(),
		MemberName:          "m1-node",
		ClientURLs:          []string{"http://" + lis.Addr().String()},
		WatchPollInterval:   10 * time.Millisecond,
		LeaseExpiryInterval: 200 * time.Millisecond,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	server, err := compat.NewServer(context.Background(), cfg)
	requireNoError(t, "start compat server", err)
	serveErr := make(chan error, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		serveErr <- server.Serve(lis)
	}()

	m := &m1Server{server: server, addr: lis.Addr().String(), serveDone: done}
	t.Cleanup(func() { m.stop(t) })
	if cfg.TLSConfig != nil {
		return m
	}
	m.conn = dialCompat(t, lis.Addr().String())
	return m
}

// stop stops the layer and waits for the listener to return.
func (m *m1Server) stop(t *testing.T) {
	t.Helper()
	m.server.Stop()
	select {
	case <-m.serveDone:
	case <-time.After(5 * time.Second):
		t.Errorf("compat server %s did not stop", m.addr)
	}
}

func (m *m1Server) kv() etcdserverpb.KVClient { return etcdserverpb.NewKVClient(m.conn) }

func (m *m1Server) watch() etcdserverpb.WatchClient { return etcdserverpb.NewWatchClient(m.conn) }

func (m *m1Server) lease() etcdserverpb.LeaseClient { return etcdserverpb.NewLeaseClient(m.conn) }

func (m *m1Server) maintenance() etcdserverpb.MaintenanceClient {
	return etcdserverpb.NewMaintenanceClient(m.conn)
}

func (m *m1Server) cluster() etcdserverpb.ClusterClient {
	return etcdserverpb.NewClusterClient(m.conn)
}

// dialCompat opens a plaintext gRPC connection with generous message limits so
// a test can exercise the server's own limits.
func dialCompat(t *testing.T, addr string) *grpc.ClientConn {
	t.Helper()
	conn, err := grpc.NewClient(addr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)))
	requireNoError(t, "dial compat server", err)
	t.Cleanup(func() { _ = conn.Close() })
	return conn
}

// testCerts holds a throwaway CA plus a server and a client certificate signed
// by it.
type testCerts struct {
	server     *tls.Config
	clientCert tls.Certificate
	caPEM      []byte
	path       string
}

// newTestCerts generates the CA and the 127.0.0.1 server and client
// certificates used by the TLS tests. Nothing is written outside the test's
// temp dir.
func newTestCerts(t *testing.T) *testCerts {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireNoError(t, "generate CA key", err)
	caTpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "k8e-rqlitecompat-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(24 * time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, caTpl, caTpl, &caKey.PublicKey, caKey)
	requireNoError(t, "create CA certificate", err)
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})

	srvCert, err := signLeaf(t, caTpl, caKey, 2, "127.0.0.1", []net.IP{net.ParseIP("127.0.0.1")}, []string{"localhost"})
	requireNoError(t, "create server certificate", err)
	clientCert, err := signLeaf(t, caTpl, caKey, 3, "m1-test-client", nil, nil)
	requireNoError(t, "create client certificate", err)

	pool := x509.NewCertPool()
	requireTrue(t, "CA pool accepts the generated CA", pool.AppendCertsFromPEM(caPEM))
	path := filepath.Join(dir, "ca.crt")
	requireNoError(t, "write CA", os.WriteFile(path, caPEM, 0o600))
	return &testCerts{
		server:     &tls.Config{Certificates: []tls.Certificate{srvCert}, ClientCAs: pool, MinVersion: tls.VersionTLS12},
		clientCert: clientCert,
		caPEM:      caPEM,
		path:       path,
	}
}

// signLeaf signs a leaf certificate with the test CA.
func signLeaf(t *testing.T, caTpl *x509.Certificate, caKey *ecdsa.PrivateKey, serial int64, cn string, ips []net.IP, names []string) (tls.Certificate, error) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	requireNoError(t, "generate "+cn+" key", err)
	tpl := &x509.Certificate{
		SerialNumber: big.NewInt(serial),
		Subject:      pkix.Name{CommonName: cn},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth, x509.ExtKeyUsageClientAuth},
		IPAddresses:  ips,
		DNSNames:     names,
	}
	der, err := x509.CreateCertificate(rand.Reader, tpl, caTpl, &key.PublicKey, caKey)
	requireNoError(t, "create "+cn+" certificate", err)
	keyDER, err := x509.MarshalECPrivateKey(key)
	requireNoError(t, "marshal "+cn+" key", err)
	cert, err := tls.X509KeyPair(
		pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}),
	)
	if err != nil {
		return tls.Certificate{}, err
	}
	return cert, nil
}

// clientTLS trusts the generated CA: the configuration an application uses to
// talk to a TLS compatibility endpoint.
func (c *testCerts) clientTLS() *tls.Config {
	return &tls.Config{RootCAs: c.pool(), MinVersion: tls.VersionTLS12}
}

// mutualTLS also presents the client certificate the CA signed.
func (c *testCerts) mutualTLS() *tls.Config {
	cfg := c.clientTLS()
	cfg.Certificates = []tls.Certificate{c.clientCert}
	return cfg
}

// untrustedTLS trusts nothing, so the server certificate cannot be verified.
func (c *testCerts) untrustedTLS() *tls.Config {
	return &tls.Config{RootCAs: x509.NewCertPool(), MinVersion: tls.VersionTLS12}
}

func (c *testCerts) pool() *x509.CertPool {
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(c.caPEM)
	return pool
}

// requireCode fails unless err is a gRPC status with code want.// requireCode fails unless err is a gRPC status with code want.
func requireCode(t *testing.T, what string, err error, want codes.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s: expected an error with code %v, got nil", what, want)
	}
	if got := status.Code(err); got != want {
		t.Fatalf("%s: code = %v (%v), want %v", what, got, err, want)
	}
}

// waitForCondition polls until probe returns nil, failing the test on timeout.
// It bounds every wait so a broken path fails fast instead of hanging.
func waitForCondition(t *testing.T, what string, timeout time.Duration, probe func() error) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	var last error
	for {
		if last = probe(); last == nil {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s: %v", what, last)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
