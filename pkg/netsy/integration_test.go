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
	s3Port := freePort(t)
	s3Addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(s3Port))
	s3Dir := t.TempDir()
	const bucket = "netsy-integration"
	s3 := exec.CommandContext(ctx, devS3Binary, "-addr", s3Addr, "-bucket", bucket, "-dir", s3Dir)
	if err := s3.Start(); err != nil {
		t.Fatalf("failed to start dev-s3: %v", err)
	}
	t.Cleanup(func() { _ = s3.Process.Kill(); _, _ = s3.Process.Wait() })
	waitForTCP(t, s3Addr, 30*time.Second)

	t.Setenv("AWS_ENDPOINT_URL", "http://"+s3Addr)
	t.Setenv("AWS_DEFAULT_REGION", "us-east-1")
	t.Setenv("AWS_ACCESS_KEY_ID", "test")
	t.Setenv("AWS_SECRET_ACCESS_KEY", "test")
	t.Setenv("AWS_S3_USE_PATH_STYLE", "true")

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

	process, err := Start(ctx, cfg)
	if err != nil {
		t.Fatalf("Start() error = %v", err)
	}
	t.Cleanup(process.Stop)
	t.Logf("netsy client API ready at %s", process.Endpoint())

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
	defer client.Close()

	const key = "/registry/namespaces/default"

	// store.Create: OptimisticPut guarded on ModRevision == 0.
	created, err := client.Kubernetes.OptimisticPut(ctx, key, []byte("payload"), 0, kubernetes.PutOptions{})
	if err != nil {
		t.Fatalf("create (OptimisticPut) error = %v\n%s", err, process.Logs())
	}
	if !created.Succeeded {
		t.Fatal("OptimisticPut create did not succeed")
	}

	// Read it back, then watch for the update — the watch cache path.
	watch := client.Watch(ctx, key)
	got, err := client.Kubernetes.Get(ctx, key, kubernetes.GetOptions{})
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if got.KV == nil || string(got.KV.Value) != "payload" {
		t.Fatalf("Get() = %+v, want the value written by OptimisticPut", got.KV)
	}

	// store.GuaranteedUpdate: OptimisticPut guarded on the observed revision,
	// with GetOnFailure so a conflict returns the winning value.
	updated, err := client.Kubernetes.OptimisticPut(ctx, key, []byte("updated"), got.KV.ModRevision, kubernetes.PutOptions{GetOnFailure: true})
	if err != nil {
		t.Fatalf("update (OptimisticPut) error = %v", err)
	}
	if !updated.Succeeded {
		t.Fatalf("OptimisticPut update at ModRevision %d did not succeed", got.KV.ModRevision)
	}

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

	// A stale write must fail and report the winner, which is how the api server
	// turns an OptimisticPut failure into a 409 Conflict.
	stale, err := client.Kubernetes.OptimisticPut(ctx, key, []byte("stale"), got.KV.ModRevision, kubernetes.PutOptions{GetOnFailure: true})
	if err != nil {
		t.Fatalf("stale OptimisticPut error = %v", err)
	}
	if stale.Succeeded {
		t.Fatal("stale OptimisticPut succeeded, want a conflict")
	}
	if stale.KV == nil || string(stale.KV.Value) != "updated" {
		t.Fatalf("conflict KV = %+v, want the winning value", stale.KV)
	}

	// List and Count over a prefix, as the listers and the watch cache do.
	listed, err := client.Kubernetes.List(ctx, "/registry/namespaces/", kubernetes.ListOptions{})
	if err != nil {
		t.Fatalf("List() error = %v", err)
	}
	if len(listed.Kvs) != 1 || string(listed.Kvs[0].Value) != "updated" {
		t.Fatalf("List() = %+v, want one \"updated\" entry", listed.Kvs)
	}
	count, err := client.Kubernetes.Count(ctx, "/registry/namespaces/", kubernetes.CountOptions{})
	if err != nil {
		t.Fatalf("Count() error = %v", err)
	}
	if count != 1 {
		t.Fatalf("Count() = %d, want 1", count)
	}

	// store.Delete: OptimisticDelete guarded on the observed revision.
	deleted, err := client.Kubernetes.OptimisticDelete(ctx, key, stale.KV.ModRevision, kubernetes.DeleteOptions{GetOnFailure: true})
	if err != nil {
		t.Fatalf("delete (OptimisticDelete) error = %v", err)
	}
	if !deleted.Succeeded {
		t.Fatal("OptimisticDelete did not succeed")
	}
	gone, err := client.Kubernetes.Get(ctx, key, kubernetes.GetOptions{})
	if err != nil {
		t.Fatalf("Get() after delete error = %v", err)
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
