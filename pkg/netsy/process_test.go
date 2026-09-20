package netsy

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials"
)

func writeScript(t *testing.T, body string) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "fake-netsy")
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"+body), 0755); err != nil {
		t.Fatal(err)
	}
	return path
}

// fakeMaintenance answers the etcd Maintenance/Status call the readiness probe
// makes, reporting the given leader (Primary) member id.
type fakeMaintenance struct {
	etcdserverpb.UnimplementedMaintenanceServer
	leader uint64
}

func (f *fakeMaintenance) Status(context.Context, *etcdserverpb.StatusRequest) (*etcdserverpb.StatusResponse, error) {
	return &etcdserverpb.StatusResponse{Leader: f.leader}, nil
}

// fakeEtcd serves the Maintenance API on cfg.ClientPort with the certificate
// and CA k8e generates, so the mTLS readiness probe finds a datastore that
// reports the given Primary.
func fakeEtcd(t *testing.T, cfg Config, leader uint64) {
	t.Helper()
	certs, err := EnsurePKI(cfg.CertDir, cfg.ClusterID, cfg.NodeID, DefaultClientName, cfg.ServerHosts())
	if err != nil {
		t.Fatal(err)
	}
	pair, err := tls.LoadX509KeyPair(certs.ServerCert, certs.ServerKey)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("tcp", cfg.BindClient())
	if err != nil {
		t.Fatal(err)
	}
	server := grpc.NewServer(grpc.Creds(credentials.NewTLS(&tls.Config{
		Certificates: []tls.Certificate{pair},
		MinVersion:   tls.VersionTLS13,
	})))
	etcdserverpb.RegisterMaintenanceServer(server, &fakeMaintenance{leader: leader})
	go func() { _ = server.Serve(ln) }()
	t.Cleanup(server.Stop)
}

func testConfig(t *testing.T, binary string) Config {
	t.Helper()
	dir := t.TempDir()
	return Config{
		Binary:             binary,
		ClusterID:          "k8e",
		NodeID:             "k8e",
		DataDir:            filepath.Join(dir, "data"),
		CertDir:            filepath.Join(dir, "tls"),
		HealthPort:         freePort(t),
		ClientPort:         freePort(t),
		PeerPort:           freePort(t),
		ElectionPort:       freePort(t),
		Storage:            Storage{Provider: "s3", Bucket: "k8e-test"},
		ReadyTimeout:       5 * time.Second,
		HealthPollInterval: 25 * time.Millisecond,
	}
}

func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	return ln.Addr().(*net.TCPAddr).Port
}

func TestStartValidatesConfig(t *testing.T) {
	cfg := validConfig()
	cfg.Storage.Bucket = ""
	if _, err := Start(context.Background(), cfg); err == nil {
		t.Fatal("Start() error = nil, want validation error")
	}
}

func TestStartMissingBinary(t *testing.T) {
	cfg := testConfig(t, filepath.Join(t.TempDir(), "does-not-exist"))
	_, err := Start(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "failed to start netsy") {
		t.Fatalf("Start() error = %v, want failure to start netsy", err)
	}
}

func TestStartExitsBeforeReady(t *testing.T) {
	binary := writeScript(t, `echo "boom: bad bucket" >&2; exit 1`)
	cfg := testConfig(t, binary)
	cfg.ReadyTimeout = 10 * time.Second

	_, err := Start(context.Background(), cfg)
	if err == nil {
		t.Fatal("Start() error = nil, want early exit error")
	}
	if !strings.Contains(err.Error(), "exited before becoming ready") {
		t.Fatalf("Start() error = %v, want early exit", err)
	}
	if !strings.Contains(err.Error(), "boom: bad bucket") {
		t.Fatalf("Start() error = %v, want captured netsy output", err)
	}
}

func TestStartTimesOutWithoutWritableDatastore(t *testing.T) {
	binary := writeScript(t, "exec sleep 60")
	cfg := testConfig(t, binary)
	cfg.ReadyTimeout = 300 * time.Millisecond
	// No Primary elected yet: the datastore is reachable but read-only.
	fakeEtcd(t, cfg, 0)

	_, err := Start(context.Background(), cfg)
	if err == nil || !strings.Contains(err.Error(), "timed out after 300ms") {
		t.Fatalf("Start() error = %v, want readiness timeout", err)
	}
}

func TestStartContextCanceled(t *testing.T) {
	binary := writeScript(t, "exec sleep 60")
	cfg := testConfig(t, binary)
	fakeEtcd(t, cfg, 0)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Start(ctx, cfg)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("Start() error = %v, want context.Canceled", err)
	}
}

func TestStartReadyAndStop(t *testing.T) {
	binary := writeScript(t, `echo "netsy starting"; exec sleep 60`)
	cfg := testConfig(t, binary)
	fakeEtcd(t, cfg, 1)

	p, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	if got, want := p.Endpoint(), cfg.Endpoint(); got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
	if _, err := os.Stat(p.CertPaths().CA); err != nil {
		t.Errorf("CA not present: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cfg.DataDir, "netsy.jsonc")); err != nil {
		t.Errorf("rendered config not present: %v", err)
	}

	done := make(chan struct{})
	go func() {
		p.Stop()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(15 * time.Second):
		t.Fatal("Stop() did not terminate the netsy process")
	}
	// The process was terminated, so Wait reports the signal; what matters is
	// that it returned rather than hanging.
	_ = p.Wait()
	if !p.Stopping() {
		t.Error("Stopping() = false after Stop(), want the requested shutdown to be visible")
	}

	// Stopping an already stopped process is a no-op.
	p.Stop()
}

func TestProcessLogsCaptured(t *testing.T) {
	binary := writeScript(t, `echo "netsy starting"; exec sleep 60`)
	cfg := testConfig(t, binary)
	fakeEtcd(t, cfg, 1)

	p, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	defer p.Stop()

	deadline := time.Now().Add(5 * time.Second)
	for p.Logs() == "" && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	if !strings.Contains(p.Logs(), "netsy starting") {
		t.Fatalf("Logs() = %q, want captured process output", p.Logs())
	}
}

func TestStopWithoutProcess(t *testing.T) {
	(&Process{}).Stop() // must not panic
}

func TestMergeEnv(t *testing.T) {
	base := []string{"PATH=/bin", "NETSY_NODE_ID=stale", "HOME=/root"}
	override := []string{"NETSY_NODE_ID=fresh", "NETSY_DEBUG=false"}

	got := mergeEnv(base, override)
	want := []string{"PATH=/bin", "HOME=/root", "NETSY_NODE_ID=fresh", "NETSY_DEBUG=false"}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("mergeEnv() = %v, want %v", got, want)
	}
}

func TestTailBufferKeepsTail(t *testing.T) {
	b := &tailBuffer{max: 8}
	if _, err := b.Write([]byte("12345")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "12345" {
		t.Fatalf("String() = %q, want 12345", got)
	}
	if _, err := b.Write([]byte("6789")); err != nil {
		t.Fatal(err)
	}
	if got := b.String(); got != "23456789" {
		t.Fatalf("String() = %q, want 23456789", got)
	}
}

func TestStartFailsWhenDataDirUnusable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	binary := writeScript(t, "exec sleep 60")
	cfg := testConfig(t, binary)
	parent := filepath.Join(t.TempDir(), "readonly")
	if err := os.Mkdir(parent, 0500); err != nil {
		t.Fatal(err)
	}
	// Restore owner write so t.TempDir cleanup can remove the read-only dir.
	t.Cleanup(func() { _ = os.Chmod(parent, 0600) })
	cfg.DataDir = filepath.Join(parent, "data")

	if _, err := Start(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "data dir") {
		t.Fatalf("Start() error = %v, want data dir error", err)
	}
}

func TestStartFailsWhenConfigUnwritable(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("running as root ignores directory permissions")
	}
	binary := writeScript(t, "exec sleep 60")
	cfg := testConfig(t, binary)
	if err := os.MkdirAll(cfg.DataDir, 0500); err != nil {
		t.Fatal(err)
	}
	// Restore owner write so t.TempDir cleanup can remove the read-only dir.
	t.Cleanup(func() { _ = os.Chmod(cfg.DataDir, 0600) })

	if _, err := Start(context.Background(), cfg); err == nil || !strings.Contains(err.Error(), "failed to write netsy config") {
		t.Fatalf("Start() error = %v, want config write error", err)
	}
}

func TestClientTLSRejectsMissingCertificates(t *testing.T) {
	if _, err := clientTLS(CertPaths{}); err == nil {
		t.Error("clientTLS() error = nil, want a certificate load error")
	}
}

func TestClientTLSRejectsGarbageCA(t *testing.T) {
	dir := t.TempDir()
	certs, err := EnsurePKI(filepath.Join(dir, "tls"), "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	certs.CA = filepath.Join(dir, "garbage-ca.pem")
	if err := os.WriteFile(certs.CA, []byte("not a certificate"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := clientTLS(certs); err == nil || !strings.Contains(err.Error(), "failed to parse netsy CA") {
		t.Errorf("clientTLS() error = %v, want CA parse error", err)
	}
}

func TestClientTLSMissingCAFile(t *testing.T) {
	dir := t.TempDir()
	certs, err := EnsurePKI(filepath.Join(dir, "tls"), "k8e", "k8e", DefaultClientName, []string{"127.0.0.1"})
	if err != nil {
		t.Fatal(err)
	}
	certs.CA = filepath.Join(dir, "absent-ca.pem")
	if _, err := clientTLS(certs); err == nil || !strings.Contains(err.Error(), "failed to read netsy CA") {
		t.Errorf("clientTLS() error = %v, want CA read error", err)
	}
}

func TestWaitReadyRejectsInvalidPKI(t *testing.T) {
	p := &Process{endpoint: "https://127.0.0.1:1", readyAfter: time.Second, poll: time.Millisecond, logs: &tailBuffer{max: 64}, done: make(chan struct{})}
	if err := p.waitReady(context.Background(), CertPaths{}); err == nil ||
		!strings.Contains(err.Error(), "failed to load netsy datastore client certificate") {
		t.Fatalf("waitReady() error = %v, want client certificate error", err)
	}
}

func TestWaitReadyAbortsOnContextCancel(t *testing.T) {
	cfg := testConfig(t, "/bin/true")
	certs, err := EnsurePKI(cfg.CertDir, cfg.ClusterID, cfg.NodeID, DefaultClientName, cfg.ServerHosts())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	p := &Process{
		endpoint:   fmt.Sprintf("https://127.0.0.1:%d", freePort(t)),
		readyAfter: time.Minute,
		poll:       time.Millisecond,
		logs:       &tailBuffer{max: 64},
		done:       make(chan struct{}),
	}
	if err := p.waitReady(ctx, certs); !errors.Is(err, context.Canceled) {
		t.Fatalf("waitReady() error = %v, want context.Canceled", err)
	}
}

func TestWaitReadyTimesOutWhenNoDatastoreListens(t *testing.T) {
	cfg := testConfig(t, "/bin/true")
	p := &Process{
		endpoint:   fmt.Sprintf("https://127.0.0.1:%d", freePort(t)),
		readyAfter: 200 * time.Millisecond,
		poll:       25 * time.Millisecond,
		logs:       &tailBuffer{max: 64},
		done:       make(chan struct{}),
	}
	certs, err := EnsurePKI(cfg.CertDir, cfg.ClusterID, cfg.NodeID, DefaultClientName, cfg.ServerHosts())
	if err != nil {
		t.Fatal(err)
	}
	if err := p.waitReady(context.Background(), certs); err == nil || !strings.Contains(err.Error(), "timed out after 200ms") {
		t.Fatalf("waitReady() error = %v, want timeout", err)
	}
}

// TestProcessCrashIsObservable pins the supervision contract the server relies
// on: a Netsy process that dies after it became ready is reported by Wait and
// is not marked as a requested stop, so the control plane can restart instead
// of serving without a datastore.
func TestProcessCrashIsObservable(t *testing.T) {
	binary := writeScript(t, `echo "netsy starting"; sleep 1; exit 3`)
	cfg := testConfig(t, binary)
	fakeEtcd(t, cfg, 1)

	p, err := Start(context.Background(), cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	exited := make(chan error, 1)
	go func() { exited <- p.Wait() }()
	select {
	case err := <-exited:
		if err == nil {
			t.Error("Wait() = nil, want the exit error of the crashed netsy process")
		}
		if p.Stopping() {
			t.Error("Stopping() = true, want a crash to be distinguishable from a requested stop")
		}
	case <-time.After(15 * time.Second):
		t.Fatal("the netsy exit was never observed")
	}
	if !strings.Contains(p.Logs(), "netsy starting") {
		t.Errorf("Logs() = %q, want the captured output of the crashed process", p.Logs())
	}
}

// TestContextCancelSignalsTermNotKill pins finding 4: canceling the server
// context must terminate Netsy with SIGTERM (giving it the same grace period as
// Stop) instead of the immediate SIGKILL of exec.CommandContext.
func TestContextCancelSignalsTermNotKill(t *testing.T) {
	binary := writeScript(t, `trap 'echo "got term"; exit 42' TERM
echo "netsy starting"
while true; do sleep 1; done`)
	cfg := testConfig(t, binary)
	fakeEtcd(t, cfg, 1)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	p, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(p.Stop)

	cancel()
	exited := make(chan error, 1)
	go func() { exited <- p.Wait() }()
	select {
	case <-exited:
	case <-time.After(15 * time.Second):
		t.Fatal("canceling the context did not terminate the netsy process")
	}
	if !strings.Contains(p.Logs(), "got term") {
		t.Fatalf("Logs() = %q, want the SIGTERM handler to have run; the child was killed instead of signaled", p.Logs())
	}
}
