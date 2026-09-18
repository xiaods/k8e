package netsy

import (
	"context"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
	"go.etcd.io/etcd/client/v3/kubernetes"
)

// TestIntegrationRealNetsyDatastore starts a real netsy process backed by a fake
// S3 server and drives it with go.etcd.io/etcd/client/v3/kubernetes — the exact
// client kube-apiserver's etcd3 store uses (store.Create/GuaranteedUpdate call
// Client.Kubernetes.OptimisticPut) — over the config and mTLS PKI k8e generates.
// It is the end-to-end proof that the rendered config, the PKI and the endpoint
// k8e hands to kube-apiserver actually work.
//
// It is opt-in because it needs the netsy and dev-s3 binaries:
//
//	NETSY_BINARY=/path/to/netsy NETSY_DEV_S3=/path/to/dev-s3 \
//	  go test ./pkg/netsy/ -run TestIntegration -count=1 -v
func TestIntegrationRealNetsyDatastore(t *testing.T) {
	netsyBinary := os.Getenv("NETSY_BINARY")
	devS3Binary := os.Getenv("NETSY_DEV_S3")
	if netsyBinary == "" || devS3Binary == "" {
		t.Skip("set NETSY_BINARY and NETSY_DEV_S3 to run the real Netsy datastore test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	// Fake S3 backed by a temp dir; netsy persists everything here.
	const bucket = "netsy-integration"
	t.Setenv("AWS_ENDPOINT_URL", "http://"+startDevS3(t, ctx, devS3Binary, bucket))
	setS3Credentials(t)

	process, err := Start(ctx, integrationConfig(t, netsyBinary, bucket))
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(process.Stop)
	t.Logf("netsy client API ready at %s", process.Endpoint())

	client := newIntegrationClient(t, process)
	defer client.Close()

	const key = "/registry/namespaces/default"

	// store.Create: OptimisticPut guarded on ModRevision == 0.
	mustPut(t, ctx, client, process, "create", key, "payload", 0)

	// Read it back, then watch for the update — the watch cache path.
	watch := client.Watch(ctx, key)
	rev := mustGetValue(t, ctx, client, process, key, "payload")

	// store.GuaranteedUpdate: OptimisticPut guarded on the observed revision,
	// with GetOnFailure so a conflict returns the winning value.
	mustPut(t, ctx, client, process, "update", key, "updated", rev)
	assertWatchEvent(t, watch, process)

	// A stale write must fail and report the winner, which is how the api server
	// turns an OptimisticPut failure into a 409 Conflict.
	currentRev := assertStalePutConflicts(t, ctx, client, process, key, rev)

	// List and Count over a prefix, as the listers and the watch cache do.
	assertListAndCount(t, ctx, client, process)

	// store.Delete: OptimisticDelete guarded on the observed revision.
	mustDelete(t, ctx, client, process, key, currentRev)
	assertKeyGone(t, ctx, client, process, key)
}

// startDevS3 starts the fake S3 server netsy persists to and returns its address.
func startDevS3(t *testing.T, ctx context.Context, binary, bucket string) string {
	t.Helper()
	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(freePort(t)))
	s3 := exec.CommandContext(ctx, binary, "-addr", addr, "-bucket", bucket, "-dir", t.TempDir())
	if err := s3.Start(); err != nil {
		t.Fatalf("failed to start dev-s3: %v", err)
	}
	t.Cleanup(func() { _ = s3.Process.Kill(); _, _ = s3.Process.Wait() })
	waitForTCP(t, addr, 30*time.Second)
	return addr
}

// setS3Credentials points the AWS SDK netsy uses at the fake S3 server.
func setS3Credentials(t *testing.T) {
	t.Helper()
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_S3_USE_PATH_STYLE", "true")
}

func integrationConfig(t *testing.T, netsyBinary, bucket string) Config {
	t.Helper()
	dataDir := t.TempDir()
	cfg := Config{
		Binary:       netsyBinary,
		ClusterID:    "k8e-integration",
		NodeID:       "k8e-integration",
		DataDir:      dataDir,
		CertDir:      filepath.Join(dataDir, "tls"),
		ClientPort:   freePort(t),
		PeerPort:     freePort(t),
		ElectionPort: freePort(t),
		HealthPort:   freePort(t),
		Storage:      Storage{Provider: "s3", Bucket: bucket, Encryption: "provider-managed"},
		ReadyTimeout: 2 * time.Minute,
	}
	cfg.HealthPollInterval = 200 * time.Millisecond
	return cfg
}

// newIntegrationClient returns the client kube-apiserver's etcd3 store uses,
// authenticated with the PKI k8e generated for the datastore.
func newIntegrationClient(t *testing.T, process *Process) *kubernetes.Client {
	t.Helper()
	tlsConfig, err := clientTLS(process.CertPaths())
	if err != nil {
		t.Fatalf("clientTLS() error = %v", err)
	}
	client, err := kubernetes.New(clientv3.Config{
		Endpoints:   []string{process.Endpoint()},
		DialTimeout: 15 * time.Second,
		TLS:         tlsConfig,
	})
	if err != nil {
		t.Fatalf("kubernetes.New() error = %v\n%s", err, process.Logs())
	}
	return client
}

// mustPut performs a revision-guarded OptimisticPut and reports the short step
// name on failure, so a violated contract names the store operation it broke.
func mustPut(t *testing.T, ctx context.Context, client *kubernetes.Client, process *Process, step, key, value string, revision int64) {
	t.Helper()
	resp, err := client.Kubernetes.OptimisticPut(ctx, key, []byte(value), revision, kubernetes.PutOptions{GetOnFailure: true})
	if err != nil {
		t.Fatalf("%s (OptimisticPut) error = %v\n%s", step, err, process.Logs())
	}
	if !resp.Succeeded {
		t.Fatalf("%s: OptimisticPut guarded on revision %d did not succeed", step, revision)
	}
}

// mustGetValue asserts the value stored at key and returns its ModRevision.
func mustGetValue(t *testing.T, ctx context.Context, client *kubernetes.Client, process *Process, key, want string) int64 {
	t.Helper()
	got, err := client.Kubernetes.Get(ctx, key, kubernetes.GetOptions{})
	if err != nil {
		t.Fatalf("Get(%s) error = %v\n%s", key, err, process.Logs())
	}
	if got.KV == nil || string(got.KV.Value) != want {
		t.Fatalf("Get(%s) = %+v, want the value %q", key, got.KV, want)
	}
	return got.KV.ModRevision
}

func assertWatchEvent(t *testing.T, watch clientv3.WatchChan, process *Process) {
	t.Helper()
	select {
	case resp := <-watch:
		if err := resp.Err(); err != nil {
			t.Fatalf("watch error = %v", err)
		}
		if len(resp.Events) != 1 || string(resp.Events[0].Kv.Value) != "updated" {
			t.Fatalf("watch events = %+v, want one PUT of \"updated\"", resp.Events)
		}
	case <-time.After(30 * time.Second):
		t.Fatalf("timed out waiting for the watch event\n%s", process.Logs())
	}
}

// assertStalePutConflicts overwrites a stale revision and returns the revision of
// the winning value the conflict reported.
func assertStalePutConflicts(t *testing.T, ctx context.Context, client *kubernetes.Client, process *Process, key string, revision int64) int64 {
	t.Helper()
	stale, err := client.Kubernetes.OptimisticPut(ctx, key, []byte("stale"), revision, kubernetes.PutOptions{GetOnFailure: true})
	if err != nil {
		t.Fatalf("stale OptimisticPut error = %v\n%s", err, process.Logs())
	}
	if stale.Succeeded {
		t.Fatal("stale OptimisticPut succeeded, want a conflict")
	}
	if stale.KV == nil || string(stale.KV.Value) != "updated" {
		t.Fatalf("conflict KV = %+v, want the winning value", stale.KV)
	}
	return stale.KV.ModRevision
}

func assertListAndCount(t *testing.T, ctx context.Context, client *kubernetes.Client, process *Process) {
	t.Helper()
	listed, err := client.Kubernetes.List(ctx, "/registry/namespaces/", kubernetes.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v\n%s", err, process.Logs())
	}
	if len(listed.Kvs) != 1 || string(listed.Kvs[0].Value) != "updated" {
		t.Fatalf("List() = %+v, want one \"updated\" entry", listed.Kvs)
	}
	count, err := client.Kubernetes.Count(ctx, "/registry/namespaces/", kubernetes.CountOptions{})
	if err != nil {
		t.Fatalf("Count() error = %v\n%s", err, process.Logs())
	}
	if count != 1 {
		t.Fatalf("Count() = %d, want 1", count)
	}
}

func mustDelete(t *testing.T, ctx context.Context, client *kubernetes.Client, process *Process, key string, revision int64) {
	t.Helper()
	deleted, err := client.Kubernetes.OptimisticDelete(ctx, key, revision, kubernetes.DeleteOptions{GetOnFailure: true})
	if err != nil {
		t.Fatalf("delete (OptimisticDelete) error = %v\n%s", err, process.Logs())
	}
	if !deleted.Succeeded {
		t.Fatal("OptimisticDelete did not succeed")
	}
}

func assertKeyGone(t *testing.T, ctx context.Context, client *kubernetes.Client, process *Process, key string) {
	t.Helper()
	gone, err := client.Kubernetes.Get(ctx, key, kubernetes.GetOptions{})
	if err != nil {
		t.Fatalf("Get() after delete error = %v\n%s", err, process.Logs())
	}
	if gone.KV != nil {
		t.Fatalf("Get() after delete = %+v, want no key", gone.KV)
	}
}

func waitForTCP(t *testing.T, addr string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 500*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s to accept connections", addr)
}
