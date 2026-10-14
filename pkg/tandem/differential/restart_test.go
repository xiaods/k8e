//go:build tandem_differential

package differential

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/xiaods/k8e/tests/etcdrobustness"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Crash recovery is the differential axis the existing suite did not cover:
// after the identical write sequence, a hard kill of the store must leave a
// restarted backend with the same keys, values and versions, and revision
// numbering must neither rewind nor skip.
//
// The etcd member runs in a re-executed child of this test binary so the
// parent can SIGKILL it the way a supervisor kills a crashed server.
// Tandem is SIGKILLed together with its rqlite: the whole data stack dies, so
// recovery has to come from the Raft log on disk.

const (
	diffChildDir    = "TANDEM_DIFF_ETCD_CHILD_DIR"
	diffChildClient = "TANDEM_DIFF_ETCD_CHILD_CLIENT_URL"
	diffChildPeer   = "TANDEM_DIFF_ETCD_CHILD_PEER_URL"
)

func TestDifferentialEtcdChild(t *testing.T) {
	dir := os.Getenv(diffChildDir)
	clientURL := os.Getenv(diffChildClient)
	peerURL := os.Getenv(diffChildPeer)
	if dir == "" || clientURL == "" || peerURL == "" {
		t.Skip("etcd child env not set; this entry point only serves the parent test")
	}
	node, err := etcdrobustness.StartNode(etcdrobustness.NodeOptions{
		Name:      "differential",
		Dir:       dir,
		ClientURL: clientURL,
		PeerURL:   peerURL,
	})
	if err != nil {
		t.Fatalf("child: start embedded etcd: %v", err)
	}
	defer node.Close()
	select {} // serve until the parent SIGKILLs this process
}

type etcdChild struct {
	cmd       *exec.Cmd
	dir       string
	clientURL string
	peerURL   string
}

func startEtcdChild(t *testing.T) *etcdChild {
	t.Helper()
	child := &etcdChild{
		dir:       t.TempDir(),
		clientURL: "http://" + freeAddress(t),
		peerURL:   "http://" + freeAddress(t),
	}
	log, err := os.Create(filepath.Join(t.TempDir(), "etcd-child.log"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = log.Close() })
	cmd := exec.Command(os.Args[0], "-test.run=^TestDifferentialEtcdChild$", "-test.v", "-test.timeout=0")
	cmd.Env = append(os.Environ(),
		diffChildDir+"="+child.dir,
		diffChildClient+"="+child.clientURL,
		diffChildPeer+"="+child.peerURL,
	)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatalf("start etcd child: %v", err)
	}
	child.cmd = cmd
	t.Cleanup(func() {
		if child.cmd != nil && child.cmd.ProcessState == nil {
			_ = child.cmd.Process.Kill()
			_ = child.cmd.Wait()
		}
	})
	return child
}

func (c *etcdChild) kill(t *testing.T) {
	t.Helper()
	if err := c.cmd.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL etcd child: %v", err)
	}
	if err := c.cmd.Wait(); err != nil {
		if _, ok := err.(*exec.ExitError); !ok {
			t.Fatalf("etcd child failed before the kill: %v", err)
		}
	}
	c.cmd = nil
}

type tandemMember struct {
	binary       string
	rqliteBinary string
	address      string
	rqliteURL    string
	rqliteAddr   string
	rqliteRaft   string
	rqliteDir    string
	dataDir      string
	certPath     string
	keyPath      string
	caPath       string
	clientTLS    *tls.Config
	rqliteCmd    *exec.Cmd
	tandemCmd    *exec.Cmd
}

func newTandemMember(t *testing.T) *tandemMember {
	t.Helper()
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Fatal("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	rqlited := os.Getenv("TANDEM_TEST_RQLITED")
	if rqlited == "" {
		t.Fatal("set TANDEM_TEST_RQLITED to a rqlited executable")
	}
	certPath, keyPath, tlsCfg := testPKI(t)
	return &tandemMember{
		binary:       binary,
		rqliteBinary: rqlited,
		address:      freeAddress(t),
		rqliteAddr:   freeAddress(t),
		rqliteRaft:   freeAddress(t),
		rqliteDir:    t.TempDir(),
		dataDir:      t.TempDir(),
		certPath:     certPath,
		keyPath:      keyPath,
		caPath:       filepath.Join(filepath.Dir(certPath), "ca.pem"),
		clientTLS:    tlsCfg,
	}
}

func (m *tandemMember) startRqlite(t *testing.T) {
	t.Helper()
	log, err := os.Create(filepath.Join(m.rqliteDir, "rqlite.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(m.rqliteBinary, "-node-id", "differential",
		"-http-addr", m.rqliteAddr, "-raft-addr", m.rqliteRaft, m.rqliteDir)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatalf("start rqlite: %v", err)
	}
	m.rqliteCmd = cmd
	m.rqliteURL = "http://" + m.rqliteAddr
	t.Cleanup(func() {
		if m.rqliteCmd != nil {
			_ = m.rqliteCmd.Process.Kill()
			_ = m.rqliteCmd.Wait()
		}
		_ = log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	})
	if !waitForURL(t, m.rqliteURL+"/readyz", 30*time.Second) {
		t.Fatal("rqlite failed readiness")
	}
}

func (m *tandemMember) startTandem(t *testing.T) *clientv3.Client {
	t.Helper()
	log, err := os.Create(filepath.Join(m.dataDir, "tandem.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(m.binary)
	cmd.Env = append(os.Environ(),
		"TANDEM_RQLITE_ADDR="+m.rqliteURL,
		"TANDEM_DATA_DIR="+m.dataDir,
		"TANDEM_LISTEN_ADDR="+m.address,
		"TANDEM_TLS_CERT_FILE="+m.certPath,
		"TANDEM_TLS_KEY_FILE="+m.keyPath,
		"TANDEM_TLS_CA_FILE="+m.caPath,
		"TANDEM_TLS_REQUIRE_CLIENT_CERT=false",
	)
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatalf("start tandem: %v", err)
	}
	m.tandemCmd = cmd
	t.Cleanup(func() {
		if m.tandemCmd != nil {
			_ = m.tandemCmd.Process.Kill()
			_ = m.tandemCmd.Wait()
		}
		_ = log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	})
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{"https://" + m.address},
		DialTimeout: 5 * time.Second,
		TLS:         m.clientTLS,
	})
	if err != nil {
		t.Fatalf("dial tandem: %v", err)
	}
	waitReady(t, client, 30*time.Second, "tandem")
	return client
}

func (m *tandemMember) killAll(t *testing.T) {
	t.Helper()
	for _, cmd := range []*exec.Cmd{m.tandemCmd, m.rqliteCmd} {
		if cmd == nil {
			continue
		}
		if err := cmd.Process.Kill(); err != nil {
			t.Fatalf("SIGKILL member process: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			if _, ok := err.(*exec.ExitError); !ok {
				t.Fatalf("member process failed before the kill: %v", err)
			}
		}
	}
	m.tandemCmd = nil
	m.rqliteCmd = nil
}

func writeSeed(t *testing.T, ctx context.Context, client *clientv3.Client) {
	t.Helper()
	binaryValue := string([]byte{0x00, 0x01, 0xff, 0xfe})
	if _, err := client.Put(ctx, "restart/a", "one"); err != nil {
		t.Fatalf("seed put a: %v", err)
	}
	if _, err := client.Put(ctx, "restart/b", binaryValue); err != nil {
		t.Fatalf("seed put b: %v", err)
	}
	if _, err := client.Put(ctx, "restart/a", "two"); err != nil {
		t.Fatalf("seed update a: %v", err)
	}
	if _, err := client.Delete(ctx, "restart/b"); err != nil {
		t.Fatalf("seed delete b: %v", err)
	}
	if _, err := client.Put(ctx, "restart/\x00\xff", "edge"); err != nil {
		t.Fatalf("seed put binary key: %v", err)
	}
}

type storedKey struct {
	Key     string
	Value   string
	Version int64
}

func readKeys(t *testing.T, ctx context.Context, client *clientv3.Client, prefix string) []storedKey {
	t.Helper()
	resp, err := client.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithSort(clientv3.SortByKey, clientv3.SortAscend))
	if err != nil {
		t.Fatalf("range %s: %v", prefix, err)
	}
	keys := make([]storedKey, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		keys = append(keys, storedKey{Key: string(kv.Key), Value: string(kv.Value), Version: kv.Version})
	}
	return keys
}

func currentRevision(t *testing.T, ctx context.Context, client *clientv3.Client, prefix string) int64 {
	t.Helper()
	resp, err := client.Get(ctx, prefix, clientv3.WithPrefix())
	if err != nil {
		t.Fatalf("range %s for revision: %v", prefix, err)
	}
	return resp.Header.Revision
}

func compareRecoveredKeys(t *testing.T, etcdKeys, tandemKeys []storedKey) {
	t.Helper()
	if len(etcdKeys) != len(tandemKeys) {
		t.Fatalf("recovered key count differs: etcd=%d tandem=%d\n  etcd:   %v\n  tandem: %v",
			len(etcdKeys), len(tandemKeys), etcdKeys, tandemKeys)
	}
	for i := range etcdKeys {
		if etcdKeys[i] != tandemKeys[i] {
			t.Fatalf("recovered key %d differs:\n  etcd:   %+v\n  tandem: %+v", i, etcdKeys[i], tandemKeys[i])
		}
	}
}

func TestDifferentialCrashRecovery(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 180*time.Second)
	defer cancel()

	child := startEtcdChild(t)
	etcdCl := newEtcdClient(t, child.clientURL)
	writeSeed(t, ctx, etcdCl)
	etcdBefore := currentRevision(t, ctx, etcdCl, "restart/")
	_ = etcdCl.Close()
	child.kill(t)

	node, err := etcdrobustness.StartNode(etcdrobustness.NodeOptions{
		Name:      "differential",
		Dir:       child.dir,
		ClientURL: child.clientURL,
		PeerURL:   child.peerURL,
	})
	if err != nil {
		t.Fatalf("restart the SIGKILLed etcd member: %v", err)
	}
	defer node.Close()
	etcdRec := newEtcdClient(t, child.clientURL)

	member := newTandemMember(t)
	member.startRqlite(t)
	tandemCl := member.startTandem(t)
	writeSeed(t, ctx, tandemCl)
	tandemBefore := currentRevision(t, ctx, tandemCl, "restart/")
	_ = tandemCl.Close()
	member.killAll(t)
	member.startRqlite(t)
	tandemRec := member.startTandem(t)

	compareRecoveredKeys(t,
		readKeys(t, ctx, etcdRec, "restart/"),
		readKeys(t, ctx, tandemRec, "restart/"))

	etcdAfter := currentRevision(t, ctx, etcdRec, "restart/")
	tandemAfter := currentRevision(t, ctx, tandemRec, "restart/")
	if etcdAfter != etcdBefore {
		t.Errorf("etcd revision after crash recovery: %d, want %d", etcdAfter, etcdBefore)
	}
	if tandemAfter != tandemBefore {
		t.Errorf("tandem revision after crash recovery: %d, want %d", tandemAfter, tandemBefore)
	}
	if _, err := etcdRec.Put(ctx, "restart/after", "one"); err != nil {
		t.Fatalf("etcd write after recovery: %v", err)
	}
	if _, err := tandemRec.Put(ctx, "restart/after", "one"); err != nil {
		t.Fatalf("tandem write after recovery: %v", err)
	}
	if got := currentRevision(t, ctx, etcdRec, "restart/"); got != etcdAfter+1 {
		t.Errorf("etcd revision after post-recovery write: %d, want %d", got, etcdAfter+1)
	}
	if got := currentRevision(t, ctx, tandemRec, "restart/"); got != tandemAfter+1 {
		t.Errorf("tandem revision after post-recovery write: %d, want %d", got, tandemAfter+1)
	}

	etcdEvents := collectRecoveryWatch(t, ctx, etcdRec, func(ctx context.Context) error {
		if _, err := etcdRec.Put(ctx, "restart/watched", "v"); err != nil {
			return err
		}
		_, err := etcdRec.Delete(ctx, "restart/watched")
		return err
	})
	tandemEvents := collectRecoveryWatch(t, ctx, tandemRec, func(ctx context.Context) error {
		if _, err := tandemRec.Put(ctx, "restart/watched", "v"); err != nil {
			return err
		}
		_, err := tandemRec.Delete(ctx, "restart/watched")
		return err
	})
	if diff := compareWatchEvents(etcdEvents, tandemEvents); diff != "" {
		t.Errorf("post-recovery watch differs\n  %s", diff)
	}
}

func collectRecoveryWatch(t *testing.T, ctx context.Context, client *clientv3.Client, apply func(context.Context) error) []watchEvent {
	t.Helper()
	watchCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	ch := client.Watch(watchCtx, "restart/", clientv3.WithPrefix(), clientv3.WithPrevKV())
	time.Sleep(500 * time.Millisecond)
	if err := apply(ctx); err != nil {
		t.Fatalf("post-recovery writes: %v", err)
	}
	var events []watchEvent
	deadline := time.After(10 * time.Second)
	for {
		select {
		case resp := <-ch:
			if resp.Err() != nil {
				t.Fatalf("post-recovery watch: %v", resp.Err())
			}
			for _, ev := range resp.Events {
				events = append(events, normalizeEvent(ev, resp.Header.Revision))
			}
			if len(events) >= 2 {
				return sortEvents(events)
			}
		case <-deadline:
			t.Fatalf("post-recovery watch delivered %d events, want 2: %v", len(events), events)
		}
	}
}

func compareWatchEvents(etcdEvents, tandemEvents []watchEvent) string {
	if len(etcdEvents) != len(tandemEvents) || len(etcdEvents) < 2 {
		return fmt.Sprintf("event count %d (etcd) vs %d (tandem)", len(etcdEvents), len(tandemEvents))
	}
	for i := range etcdEvents {
		if etcdEvents[i].Type != tandemEvents[i].Type || etcdEvents[i].Key != tandemEvents[i].Key ||
			etcdEvents[i].Value != tandemEvents[i].Value || etcdEvents[i].Version != tandemEvents[i].Version {
			return fmt.Sprintf("event %d etcd=%+v tandem=%+v", i, etcdEvents[i], tandemEvents[i])
		}
	}
	return ""
}

func newEtcdClient(t *testing.T, endpoint string) *clientv3.Client {
	t.Helper()
	cl, err := etcdrobustness.NewClient(endpoint)
	if err != nil {
		t.Fatalf("dial %s: %v", endpoint, err)
	}
	waitReady(t, cl, 30*time.Second, endpoint)
	return cl
}

func waitForURL(t *testing.T, url string, timeout time.Duration) bool {
	t.Helper()
	cl := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer cl.CloseIdleConnections()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if resp, err := cl.Get(url); err == nil {
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return true
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
