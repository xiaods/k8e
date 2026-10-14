//go:build tandem_differential

package differential

import (
	"context"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// leaseID returns a fixed id rather than letting the server choose one. etcd
// derives lease ids from a hash seeded by a random value at startup and Tandem
// allocates from a persisted sequence, so an auto-assigned id is never
// comparable — pinning it on both sides is what makes the lease axis mean
// anything. Each case takes its own id because etcd refuses a second grant of
// an id that is still live, and the cases share one harness.
//
// clientv3.Grant takes no option for this, so these cases go through the
// generated Lease client, which issues the same RPC the official client does.
func leaseID(n int64) int64 { return 4242 + n }

func leaseClient(t *testing.T, client *clientv3.Client) pb.LeaseClient {
	t.Helper()
	return pb.NewLeaseClient(client.ActiveConnection())
}

// TestDifferentialLease covers the lease surface the apiserver drives. Leases
// were the most heavily repaired subsystem in the M1 audit, and the first
// differential run found a further defect here, so this is a regression net
// as much as a search.
func TestDifferentialLease(t *testing.T) {
	h := NewHarness(t)

	grant := step{
		name: "LeaseGrant returns the id it was asked for",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			response, err := leaseClient(t, client).LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 60, ID: leaseID(1)})
			return Observation{Op: "LeaseGrant", Code: codeOf(err), LeaseID: response.GetID()}
		},
	}
	runCase(t, h, grant)

	// TimeToLive on a live lease reports the TTL it was granted. An unknown
	// or already-expired one reports -1, which is how the apiserver's lease
	// manager tells "gone" apart from "the transport broke" — the first
	// differential run caught Tandem answering NotFound here.
	ttl := step{
		name: "TimeToLive reports the granted TTL for a live lease",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			lease := leaseClient(t, client)
			if _, err := lease.LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 60, ID: leaseID(2)}); err != nil {
				t.Fatalf("grant: %v", err)
			}
			response, err := lease.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: leaseID(2)})
			observation := Observation{Op: "LeaseTimeToLive", Code: codeOf(err), LeaseID: response.GetID()}
			if err == nil {
				observation.Count = response.GetGrantedTTL()
			}
			return observation
		},
	}
	runCase(t, h, ttl)

	unknown := step{
		name: "an unknown lease reports TTL -1 rather than an error",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			response, err := leaseClient(t, client).LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: 0xDEADBEEF})
			observation := Observation{Op: "UnknownLeaseTTL", Code: codeOf(err)}
			if err == nil {
				// -1 is the whole point: a successful response carrying a
				// sentinel, not a status code the caller must interpret.
				observation.Count = response.GetTTL()
			}
			return observation
		},
	}
	runCase(t, h, unknown)

	// Revoking a lease must remove the keys attached to it. The attached-key
	// read is included so attachment is compared too, not only the removal.
	revoke := step{
		name: "revoking a lease removes the keys attached to it",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			lease := leaseClient(t, client)
			if _, err := lease.LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 60, ID: leaseID(3)}); err != nil {
				t.Fatalf("grant: %v", err)
			}
			for _, key := range []string{"diff/lease/a", "diff/lease/b"} {
				if _, err := client.Put(ctx, key, "1", clientv3.WithLease(clientv3.LeaseID(leaseID(3)))); err != nil {
					t.Fatalf("attach %s: %v", key, err)
				}
			}
			attached, err := lease.LeaseTimeToLive(ctx, &pb.LeaseTimeToLiveRequest{ID: leaseID(3), Keys: true})
			if err != nil {
				t.Fatalf("read attached keys: %v", err)
			}
			if _, err := lease.LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: leaseID(3)}); err != nil {
				return Observation{Op: "LeaseAttachedKeys", Code: codeOf(err), Count: int64(len(attached.GetKeys()))}
			}
			response, err := client.Get(ctx, "diff/lease/", clientv3.WithPrefix())
			observation := Observation{
				Op:       "LeaseRevoke",
				Code:     codeOf(err),
				Count:    int64(len(attached.GetKeys())),
				KVs:      kvs(response.Kvs, base),
				Revision: relative(response.Header.GetRevision(), base),
			}
			return observation
		},
	}
	runCase(t, h, revoke)

	// LeaseLeases read the in-memory manager directly, which the persistent
	// path never populates, so it reported an empty list while grants were
	// live in rqlite.
	list := step{
		name: "LeaseLeases lists the granted leases",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			lease := leaseClient(t, client)
			for i := int64(0); i < 2; i++ {
				if _, err := lease.LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: 60, ID: leaseID(10 + i)}); err != nil {
					t.Fatalf("grant: %v", err)
				}
			}
			response, err := lease.LeaseLeases(ctx, &pb.LeaseLeasesRequest{})
			observation := Observation{Op: "LeaseLeases", Code: codeOf(err), Count: int64(len(response.GetLeases()))}
			return observation
		},
	}
	runCase(t, h, list)

	revokeUnknown := step{
		name: "revoking an unknown lease is NotFound on both",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			_, err := leaseClient(t, client).LeaseRevoke(ctx, &pb.LeaseRevokeRequest{ID: 0xDEADBEEF})
			return Observation{Op: "RevokeUnknown", Code: codeOf(err)}
		},
	}
	runCase(t, h, revokeUnknown)
}

// TestDifferentialLeaseExpiry covers the TTL expiry axis the apiserver relies on
// most: a leased key must vanish once its lease dies, and the delete must
// arrive as a normal revision + Watch event rather than silently rewinding the
// store.
func TestDifferentialLeaseExpiry(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h := NewHarness(t)

	grantID := leaseID(200)
	ttl := int64(2)
	keys := []string{"diff/expiry/a", "diff/expiry/b"}

	for _, be := range []*Backend{h.Etcd, h.Tandem} {
		lease := pb.NewLeaseClient(be.Client.ActiveConnection())
		if _, err := lease.LeaseGrant(ctx, &pb.LeaseGrantRequest{TTL: ttl, ID: grantID}); err != nil {
			t.Fatalf("grant on %s: %v", be.Name, err)
		}
		for _, key := range keys {
			if _, err := be.Client.Put(ctx, key, "v", clientv3.WithLease(clientv3.LeaseID(grantID))); err != nil {
				t.Fatalf("attach %s on %s: %v", key, be.Name, err)
			}
		}
		resp, err := be.Client.Get(ctx, "diff/expiry/", clientv3.WithPrefix())
		if err != nil || resp.Count != int64(len(keys)) {
			t.Fatalf("keys missing right after attach on %s: count=%d err=%v", be.Name, resp.Count, err)
		}
	}

	etcdStart := revOf(t, ctx, h.Etcd.Client)
	tandemStart := revOf(t, ctx, h.Tandem.Client)

	deadline := time.Now().Add(30 * time.Second)
	var etcdGone, tandemGone time.Time
	for time.Now().Before(deadline) {
		if etcdGone.IsZero() {
			if resp, err := h.Etcd.Client.Get(ctx, "diff/expiry/", clientv3.WithPrefix()); err == nil && resp.Count == 0 {
				etcdGone = time.Now()
			}
		}
		if tandemGone.IsZero() {
			if resp, err := h.Tandem.Client.Get(ctx, "diff/expiry/", clientv3.WithPrefix()); err == nil && resp.Count == 0 {
				tandemGone = time.Now()
			}
		}
		if !etcdGone.IsZero() && !tandemGone.IsZero() {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}

	if etcdGone.IsZero() {
		t.Fatal("etcd did not auto-delete leased keys within 30s")
	}
	if tandemGone.IsZero() {
		t.Fatal("tandem did not auto-delete leased keys within 30s")
	}

	etcdEnd := revOf(t, ctx, h.Etcd.Client)
	tandemEnd := revOf(t, ctx, h.Tandem.Client)
	if etcdEnd <= etcdStart {
		t.Errorf("etcd revision did not advance after expiry: start=%d end=%d", etcdStart, etcdEnd)
	}
	if tandemEnd <= tandemStart {
		t.Errorf("tandem revision did not advance after expiry: start=%d end=%d", tandemStart, tandemEnd)
	}

	watchCtx, watchCancel := context.WithTimeout(ctx, 30*time.Second)
	defer watchCancel()
	etcdCh := h.Etcd.Client.Watch(watchCtx, "diff/expiry/", clientv3.WithPrefix(), clientv3.WithRev(etcdStart))
	tandemCh := h.Tandem.Client.Watch(watchCtx, "diff/expiry/", clientv3.WithPrefix(), clientv3.WithRev(tandemStart))
	time.Sleep(500 * time.Millisecond)
	<-time.After(time.Until(deadline.Add(time.Second)))

	etcdDeletes := collectDeleteEvents(t, etcdCh, len(keys))
	tandemDeletes := collectDeleteEvents(t, tandemCh, len(keys))
	if etcdDeletes != len(keys) {
		t.Errorf("etcd watcher saw %d deletes, want %d", etcdDeletes, len(keys))
	}
	if tandemDeletes != len(keys) {
		t.Errorf("tandem watcher saw %d deletes, want %d", tandemDeletes, len(keys))
	}

	t.Logf("LeaseExpiry: etcd gone=%s tandem gone=%s etcdRev %d->%d tandemRev %d->%d etcdDel=%d tandemDel=%d",
		etcdGone.Format("15:04:05"), tandemGone.Format("15:04:05"), etcdStart, etcdEnd, tandemStart, tandemEnd, etcdDeletes, tandemDeletes)
}

func revOf(t *testing.T, ctx context.Context, client *clientv3.Client) int64 {
	t.Helper()
	resp, err := client.Get(ctx, "diff/expiry/")
	if err != nil {
		t.Fatalf("read revision: %v", err)
	}
	return resp.Header.Revision
}

func collectDeleteEvents(t *testing.T, ch clientv3.WatchChan, want int) int {
	t.Helper()
	count := 0
	deadline := time.After(20 * time.Second)
	for count < want {
		select {
		case resp, ok := <-ch:
			if !ok {
				return count
			}
			if resp.Err() != nil {
				t.Logf("watch err: %v", resp.Err())
				return count
			}
			for _, ev := range resp.Events {
				if ev.Type == mvccpb.DELETE {
					count++
				}
			}
		case <-deadline:
			return count
		}
	}
	return count
}
