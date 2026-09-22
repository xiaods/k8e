package rqlitecompat

import (
	"context"
	"strings"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestM1ThreeNodeContracts runs the same semantic scenario the single-member
// differential test runs against a three-member rqlite cluster and compares it
// observation-by-observation with the repository's embedded etcd. One semantic
// implementation must serve both cluster shapes (Issue #616).
func TestM1ThreeNodeContracts(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 3)
	srv := startCompat(t, cluster, nil)

	three := runContractScenario(t, dialClientV3(t, srv.addr))
	etcd := runContractScenario(t, startEmbeddedEtcdClient(t))
	compareContractRuns(t, three, etcd)
}

// TestM1ThreeNodeFailoverKeepsServing kills the rqlite leader while the layer
// is serving and checks the user-visible recovery: acknowledged keys are still
// readable, the next write is acknowledged with a higher revision, and a watch
// created after the failover delivers the new events in revision order.
func TestM1ThreeNodeFailoverKeepsServing(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 3)
	srv := startCompat(t, cluster, nil)
	cli := dialClientV3(t, srv.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var err error
	for _, key := range []string{contractPrefix + "survivor", contractPrefix + "survivor2"} {
		if _, err = cli.Put(ctx, key, "before-failover"); err != nil {
			t.Fatalf("put %s: %v", key, err)
		}
	}
	before, err := cli.Get(ctx, contractPrefix+"survivor")
	requireNoError(t, "read before failover", err)
	requireEqual(t, "the acknowledged key is readable", getCount(before), int64(1))

	leader, err := cluster.Leader(ctx)
	requireNoError(t, "find the rqlite leader", err)
	oldRelease := currentRelease(t, cli)
	cluster.Kill(leader)
	newLeader, err := cluster.WaitForNewLeader(ctx, leader.ID)
	requireNoError(t, "elect a new leader", err)
	requireTrue(t, "a different member leads after the kill", newLeader.ID != leader.ID)
	cluster.Restart(leader)

	// The layer reconnects to the surviving members: the write is retried
	// through the outage the way a real client retries it.
	var put *clientv3.PutResponse
	waitForCondition(t, "a write acknowledged after failover", 90*time.Second, func() error {
		put, err = cli.Put(ctx, contractPrefix+"after-failover", "after")
		return err
	})
	requireTrue(t, "the revision advanced after failover",
		put.Header.Revision > before.Header.Revision)

	get, err := cli.Get(ctx, contractPrefix+"survivor")
	requireNoError(t, "read the acknowledged key after failover", err)
	requireEqual(t, "a committed key survives the leader change", getCount(get), int64(1))
	requireBytes(t, "the committed value is unchanged", getKvs(get)[0].Value, []byte("before-failover"))

	// A watch created after the failover delivers the events that follow it,
	// in revision order, like a client that re-establishes a broken stream.
	wch := cli.Watch(ctx, contractPrefix+"after-", clientv3.WithPrefix())
	sleepForWatch()
	if _, err := cli.Put(ctx, contractPrefix+"after-watch-1", "w1"); err != nil {
		t.Fatalf("put after-failover watch 1: %v", err)
	}
	if _, err := cli.Put(ctx, contractPrefix+"after-watch-2", "w2"); err != nil {
		t.Fatalf("put after-failover watch 2: %v", err)
	}
	requireEqual(t, "events are delivered in revision order after failover",
		strings.Join(watchKeys(t, wch, 2), " "),
		contractPrefix+"after-watch-1=w1 "+contractPrefix+"after-watch-2=w2")

	// The store revision keeps coming from rqlite, not from a stale in-memory
	// copy on the node that lost the leader.
	requireTrue(t, "the store revision is still served after the failover",
		currentRelease(t, cli) > oldRelease)
}

// TestM1LayerRestartContinuity stops the compatibility server and starts a new
// one against the same rqlite data. No MVCC state may live in the layer: the
// revision, the history, the lease and the event log must all continue.
func TestM1LayerRestartContinuity(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 1)

	first := startCompat(t, cluster, nil)
	cli := dialClientV3(t, first.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	if _, err := cli.Put(ctx, contractPrefix+"restart", "1"); err != nil {
		t.Fatalf("put 1: %v", err)
	}
	head, err := cli.Get(ctx, contractPrefix+"restart")
	requireNoError(t, "read after the first put", err)
	histRev := getRev(head)
	if _, err := cli.Put(ctx, contractPrefix+"restart", "2"); err != nil {
		t.Fatalf("put 2: %v", err)
	}
	grant, err := cli.Grant(ctx, 300)
	requireNoError(t, "grant lease", err)
	if _, err := cli.Put(ctx, contractPrefix+"leased", "v", clientv3.WithLease(grantID(grant))); err != nil {
		t.Fatalf("put with lease: %v", err)
	}
	head, err = cli.Get(ctx, contractPrefix+"restart")
	requireNoError(t, "read the head", err)
	before := getRev(head)

	first.stop(t)

	second := startCompat(t, cluster, nil)
	cli2 := dialClientV3(t, second.addr)

	head, err = cli2.Get(ctx, contractPrefix+"restart")
	requireNoError(t, "read the head from the new server", err)
	requireEqual(t, "the revision continues across a layer restart", getRev(head), before)
	requireBytes(t, "the value is intact", getKvs(head)[0].Value, []byte("2"))

	hist, err := cli2.Get(ctx, contractPrefix+"restart", clientv3.WithRev(histRev))
	requireNoError(t, "read the historical revision from the new server", err)
	requireEqual(t, "the MVCC history is intact", getCount(hist), int64(1))
	requireBytes(t, "the historical value is the older one", getKvs(hist)[0].Value, []byte("1"))

	leases, err := cli2.Leases(ctx)
	requireNoError(t, "list leases from the new server", err)
	requireTrue(t, "the lease survives the layer restart", containsLease(leaseStatuses(leases), grantID(grant)))
	ttl, err := cli2.TimeToLive(ctx, grantID(grant), clientv3.WithAttachedKeys())
	requireNoError(t, "lease TTL from the new server", err)
	requireEqual(t, "the lease keeps its attached key", len(ttlKeys(ttl)), 1)

	// The event log is durable, so a watcher created by the new server
	// receives the events committed after it started, in order.
	wch := cli2.Watch(ctx, contractPrefix+"replay", clientv3.WithPrefix())
	sleepForWatch()
	if _, err := cli2.Put(ctx, contractPrefix+"replay-1", "r1"); err != nil {
		t.Fatalf("put replay 1: %v", err)
	}
	if _, err := cli2.Put(ctx, contractPrefix+"replay-2", "r2"); err != nil {
		t.Fatalf("put replay 2: %v", err)
	}
	requireEqual(t, "the new server delivers the events in order",
		strings.Join(watchKeys(t, wch, 2), " "),
		contractPrefix+"replay-1=r1 "+contractPrefix+"replay-2=r2")

	head, err = cli2.Get(ctx, contractPrefix+"restart")
	requireNoError(t, "read the head before the last write", err)
	put, err := cli2.Put(ctx, contractPrefix+"restart", "3")
	requireNoError(t, "write after the layer restart", err)
	requireEqual(t, "the next write gets the next revision", put.Header.Revision, getRev(head)+1)
}

// watchKeys reads count events and returns them as "key=value", in the order
// the client receives them.
func watchKeys(t *testing.T, ch clientv3.WatchChan, count int) []string {
	t.Helper()
	var got []string
	deadline := time.After(20 * time.Second)
	for len(got) < count {
		select {
		case resp, ok := <-ch:
			if !ok {
				t.Fatalf("watch stream closed after %d of %d events", len(got), count)
			}
			if err := resp.Err(); err != nil {
				t.Fatalf("watch error after %d of %d events: %v", len(got), count, err)
			}
			for _, ev := range resp.Events {
				got = append(got, string(ev.Kv.Key)+"="+string(ev.Kv.Value))
			}
		case <-deadline:
			t.Fatalf("watch timed out after %d of %d events: %v", len(got), count, got)
		}
	}
	return got
}

// sleepForWatch lets the server register a watcher before the writes it must
// observe. The layer polls the event log, so a watcher created after a write
// legitimately does not see it.
func sleepForWatch() { time.Sleep(300 * time.Millisecond) }

// currentRelease reads the store revision through the client API: a
// write-free way to see what the layer currently serves.
func currentRelease(t *testing.T, cli *clientv3.Client) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := cli.Get(ctx, contractPrefix+"revision-probe")
	requireNoError(t, "read the current revision", err)
	return getRev(resp)
}
