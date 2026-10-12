//go:build tandem_differential

package differential

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
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/xiaods/k8e/tests/etcdrobustness"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Backend is one end of a differential comparison: an etcd-compatible
// endpoint reached through the same clientv3 interface on both sides.
type Backend struct {
	Name     string
	Endpoint string
	Client   *clientv3.Client
}

// Harness holds one etcd and one Tandem, both serving the same operations.
// Every process it starts is registered with t.Cleanup, so a test never has
// to tear the harness down by hand.
type Harness struct {
	Etcd   *Backend
	Tandem *Backend
}

// startEtcd brings up an in-process embedded etcd, which is the same
// k8s-forked 3.7.1 that K8E ships today and therefore the semantics Tandem has
// to match.
func startEtcd(t *testing.T) *Backend {
	t.Helper()
	clientURL := "http://" + freeAddress(t)
	peerURL := "http://" + freeAddress(t)
	node, err := etcdrobustness.StartNode(etcdrobustness.NodeOptions{
		Name:      "differential",
		Dir:       t.TempDir(),
		ClientURL: clientURL,
		PeerURL:   peerURL,
	})
	if err != nil {
		t.Fatalf("start embedded etcd: %v", err)
	}
	stop := func() { node.Close() }
	t.Cleanup(stop)

	client, err := etcdrobustness.NewClient(clientURL)
	if err != nil {
		t.Fatalf("dial embedded etcd: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	waitReady(t, client, 30*time.Second, "embedded etcd")
	return &Backend{Name: "etcd", Endpoint: clientURL, Client: client}
}

// startTandem brings up rqlite and a Tandem process speaking etcd v3 over
// mTLS, mirroring how the Supervisor runs them in production.
func startTandem(t *testing.T) *Backend {
	t.Helper()
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Fatal("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	rqlited := os.Getenv("TANDEM_TEST_RQLITED")
	if rqlited == "" {
		t.Fatal("set TANDEM_TEST_RQLITED to a rqlited executable")
	}

	rqliteURL := startRqlite(t, rqlited)
	cert, key, cfg := testPKI(t)
	address := freeAddress(t)
	log, err := os.Create(filepath.Join(t.TempDir(), "tandem.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(),
		"TANDEM_RQLITE_ADDR="+rqliteURL,
		"TANDEM_DATA_DIR="+t.TempDir(),
		"TANDEM_LISTEN_ADDR="+address,
		"TANDEM_TLS_CERT_FILE="+cert,
		"TANDEM_TLS_KEY_FILE="+key,
		"TANDEM_TLS_CA_FILE="+filepath.Join(filepath.Dir(cert), "ca.pem"),
		"TANDEM_TLS_REQUIRE_CLIENT_CERT="+strconv.FormatBool(false),
	)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatalf("start tandem: %v", err)
	}
	stopServer := func() {
		cmd.Process.Kill()
		cmd.Wait()
		log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	}
	t.Cleanup(stopServer)

	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"https://" + address},
		TLS:         cfg,
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		t.Fatalf("dial tandem: %v", err)
	}
	t.Cleanup(func() { client.Close() })
	waitReady(t, client, 30*time.Second, "tandem")
	return &Backend{Name: "tandem", Endpoint: "https://" + address, Client: client}
}

// NewHarness starts one etcd and one Tandem for the calling test. Both are
// torn down when the test finishes.
func NewHarness(t *testing.T) *Harness {
	t.Helper()
	etcdBackend := startEtcd(t)
	tandemBackend := startTandem(t)
	return &Harness{Etcd: etcdBackend, Tandem: tandemBackend}
}

func startRqlite(t *testing.T, binary string) string {
	t.Helper()
	address := freeAddress(t)
	raft := freeAddress(t)
	log, err := os.CreateTemp(t.TempDir(), "rqlite-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-node-id", "differential", "-http-addr", address, "-raft-addr", raft, t.TempDir())
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatalf("start rqlite: %v", err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cmd.Process.Kill()
			cmd.Wait()
			log.Close()
			if t.Failed() {
				data, _ := os.ReadFile(log.Name())
				t.Log(string(data))
			}
		})
	}
	t.Cleanup(stop)

	url := "http://" + address
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	for deadline := time.Now().Add(30 * time.Second); time.Now().Before(deadline); {
		if response, err := client.Get(url + "/readyz"); err == nil {
			response.Body.Close()
			if response.StatusCode == http.StatusOK {
				return url
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("rqlite failed readiness")
	return ""
}

// waitReady polls with a real request. A successful Dial only proves the socket
// accepted, not that the schema is loaded and the store can answer.
func waitReady(t *testing.T, client *clientv3.Client, overall time.Duration, what string) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(overall); time.Now().Before(deadline); {
		probe, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
		_, err = client.Get(probe, "ready-probe")
		cancel()
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s never became ready: %v", what, err)
}

func freeAddress(t *testing.T) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	return listener.Addr().String()
}

// testPKI mints a throwaway CA and leaf. Tandem terminates mTLS in
// production, so the differential path has to as well.
func testPKI(t *testing.T) (string, string, *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "differential CA"},
		NotBefore:             time.Now().Add(-time.Minute),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign,
	}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	leafCert, leafKey := issue(t, ca, caKey, "127.0.0.1")
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err := os.WriteFile(certPath, leafCert, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(keyPath, leafKey, 0600); err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AppendCertsFromPEM(caPEM)
	return certPath, keyPath, &tls.Config{RootCAs: pool, ServerName: "127.0.0.1", MinVersion: tls.VersionTLS12}
}

func issue(t *testing.T, ca *x509.Certificate, caKey *ecdsa.PrivateKey, host string) ([]byte, []byte) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    time.Now().Add(-time.Minute),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:  []net.IP{net.ParseIP(host)},
	}
	der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}),
		pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})
}
