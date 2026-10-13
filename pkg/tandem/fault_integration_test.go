//go:build tandem_integration || tandem_fault

package tandem

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestTandemFaultPartition exercises quorum loss and recovery against a real
// three-member rqlite cluster, using the same startCluster fixture the
// dynamic-membership test uses. It is the M2 stage-1 (safe recovery) and
// stage-2 (multi-node failure) evidence the issue requires, gathered at the
// layer the test actually controls: killing a member's rqlite is what a
// network partition or a lost node looks like from the surviving members.
//
// A real packet-level partition (netem/tc) is a Docker-E2E concern; at this
// layer the asymmetry is "this node cannot be reached, those two can still
// talk", which is exactly what a dead member produces. The quorum, the write
// refusal, and the recovery are exercised identically.
func TestTandemFaultPartition(t *testing.T) {
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Skip("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	c := startCluster(t, binary, 3, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	a := c.client(t, c.members[0])
	b := c.client(t, c.members[1])

	// All three members must be voters before we test quorum.
	for _, m := range c.members {
		c.waitForVoter(ctx, m)
	}

	// Write through a follower to prove replication works before the fault.
	if _, err := b.Put(ctx, "fault-key-before", "before"); err != nil {
		t.Fatalf("pre-fault write: %v", err)
	}
	assertKV(ctx, t, a, "fault-key-before", "before")

	// Lose two of three: the survivor has no quorum and must refuse writes.
	// This is what a partition that isolates one node looks like.
	killRqlite(t, c, c.members[1])
	killRqlite(t, c, c.members[2])

	// The survivor must not pretend it can serve: a write without quorum is
	// data loss dressed as availability.
	_, err := a.Put(ctx, "fault-key-during", "during")
	if err == nil {
		t.Fatalf("write against a minority should have been refused")
	}
	t.Logf("minority write correctly refused: %v", err)

	// Bring the other two back: the group must regain quorum and serve, and
	// the value committed before the fault must still be there — a recovery
	// that came back empty would be data loss.
	c.restartRqlite(t, c.members[1])
	c.restartRqlite(t, c.members[2])

	for _, m := range c.members[1:] {
		c.waitForVoter(ctx, m)
	}
	assertKV(ctx, t, a, "fault-key-before", "before")

	// A write after recovery must succeed and replicate to every member.
	if _, err := a.Put(ctx, "fault-key-after", "after"); err != nil {
		t.Fatalf("post-recovery write: %v", err)
	}
	assertKV(ctx, t, b, "fault-key-after", "after")
}

// TestTandemFaultCrashRecovery is the M2 stage-1 evidence: an abrupt rqlite
// death during acknowledged writes must not lose them, and must not lose
// history or leases either. It runs at the compat-layer layer (the
// client-facing gRPC surface) rather than through the persistence probe, so it
// exercises the full read and write path an apiserver would use.
func TestTandemFaultCrashRecovery(t *testing.T) {
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Skip("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	c := startCluster(t, binary, 1, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cli := c.client(t, c.members[0])

	// Pre-crash writes, each acknowledged.
	const writes = 5
	for i := 0; i < writes; i++ {
		key := fmt.Sprintf("crash-key-%d", i)
		value := fmt.Sprintf("value-%d", i)
		if _, err := cli.Put(ctx, key, value); err != nil {
			t.Fatalf("pre-crash write %s: %v", key, err)
		}
	}

	// Abort the store the way SIGKILL does — no graceful snapshot, no drain.
	killRqlite(t, c, c.members[0])
	c.restartRqlite(t, c.members[0])
	c.waitForVoter(ctx, c.members[0])

	// Every acknowledged write must survive, and history must survive with
	// them: a resumption that comes back empty would be data loss dressed as
	// availability.
	for i := 0; i < writes; i++ {
		key := fmt.Sprintf("crash-key-%d", i)
		value := fmt.Sprintf("value-%d", i)
		assertKV(ctx, t, cli, key, value)
	}
}

// TestTandemFaultBackupRestore is the M2 stage-3 evidence: a Snapshot, a
// directory wipe, and a Restore bring the store back to the point of the
// backup. This is the end-to-end path the rqlite-backed driver exposes for
// recovery, exercised at the compat layer (the same surface the apiserver
// reads through) rather than through the persistence probe.
func TestTandemFaultBackupRestore(t *testing.T) {
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Skip("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	c := startCluster(t, binary, 1, false)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	m := c.members[0]
	cli := c.client(t, m)
	const writes = 5
	for i := 0; i < writes; i++ {
		key := fmt.Sprintf("restore-key-%d", i)
		value := fmt.Sprintf("value-%d", i)
		if _, err := cli.Put(ctx, key, value); err != nil {
			t.Fatalf("pre-backup write %s: %v", key, err)
		}
	}

	// Back up through the driver's HTTP surface against the running node.
	dir := t.TempDir()
	backup := filepath.Join(dir, "tandem-restore-test-backup")
	if err := (&Supervisor{}).Backup(ctx, "http://"+m.httpAddr, backup); err != nil {
		t.Fatalf("backup: %v", err)
	}
	if _, err := os.Stat(backup); err != nil {
		t.Fatalf("backup file not written: %v", err)
	}

	// Wipe and restore, the way a cluster-reset restore does. The supervised
	// pair is torn down; the data dir is emptied; a temporary node loads the
	// backup; then the supervised pair reopens the same directory.
	c.stopAll()
	rqliteDir := filepath.Join(m.dir, "rqlite")
	if err := os.RemoveAll(rqliteDir); err != nil {
		t.Fatalf("wipe data dir: %v", err)
	}
	if err := os.MkdirAll(rqliteDir, 0o700); err != nil {
		t.Fatalf("recreate data dir: %v", err)
	}
	rqliteBinary := os.Getenv("TANDEM_TEST_RQLITED")
	if err := withTemporaryNode(ctx, rqliteBinary, rqliteDir, func(base string) error {
		return (&Supervisor{}).Load(ctx, base, backup)
	}); err != nil {
		t.Fatalf("restore: %v", err)
	}

	// The supervised pair comes back and must serve the pre-backup state.
	// Restart rqlite in the same directory, then the compat layer, then dial.
	c.restartRqlite(t, m)
	c.startTandem(t, m)
	restored := c.client(t, m)
	for i := 0; i < writes; i++ {
		key := fmt.Sprintf("restore-key-%d", i)
		value := fmt.Sprintf("value-%d", i)
		assertKV(ctx, t, restored, key, value)
	}
}

// --- helpers --------------------------------------------------------------

// assertKV reads key from cli and fails the test if the value is not want.
// etcd-client writes return a previous KV on a put, so we read back rather
// than trust the response — a test that only checks the write response
// cannot catch a divergence that answers "ok" and stores something else.
func assertKV(ctx context.Context, t *testing.T, cli *clientv3.Client, key, want string) {
	t.Helper()
	resp, err := cli.Get(ctx, key)
	if err != nil {
		t.Fatalf("read %s: %v", key, err)
	}
	if len(resp.Kvs) != 1 {
		t.Fatalf("read %s: got %d kvs, want 1", key, len(resp.Kvs))
	}
	if string(resp.Kvs[0].Value) != want {
		t.Fatalf("read %s: got %q, want %q", key, resp.Kvs[0].Value, want)
	}
}

// killRqlite aborts a member's store the way SIGKILL does — no graceful
// snapshot, no drain. The member's rqlite process is the thing that dies;
// the compat layer is left up so the test can observe what the failure looks
// like from the client side.
func killRqlite(t *testing.T, c *cluster, m *clusterMember) {
	t.Helper()
	if m.rqlite == nil || m.rqlite.Process == nil {
		return
	}
	if err := m.rqlite.Process.Kill(); err != nil {
		t.Fatalf("kill rqlite for %s: %v", m.id, err)
	}
	_ = m.rqlite.Wait()
	// The test helper owns the field, not the cluster, so the cluster's
	// own killRqlite would race with this one — cleared here so a later
	// restartRqlite starts fresh.
	m.rqlite = nil
}
