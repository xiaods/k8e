//go:build tandem_differential

package differential

import (
	"context"
	"testing"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
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
