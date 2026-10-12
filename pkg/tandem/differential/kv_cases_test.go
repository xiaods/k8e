//go:build tandem_differential

package differential

import (
	"context"
	"testing"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// rangeOf pulls the Range-shaped op out of a committed transaction. A
// transaction's answers are addressed by position rather than by type, so the
// index is explicit at each call site: a bug that shifted a response into the
// wrong slot then shows up as a nil here rather than as a silent mismatch.
func rangeOf(t *testing.T, response *clientv3.TxnResponse, index int) *clientv3.GetResponse {
	t.Helper()
	if index >= len(response.Responses) {
		t.Fatalf("transaction returned %d responses, want at least %d", len(response.Responses), index+1)
	}
	// A nil here means the op at that position was not a Range. Without this
	// check the field reads below would silently produce zeroes and the case
	// would pass for the wrong reason.
	inner := response.Responses[index].GetResponseRange()
	if inner == nil {
		t.Fatalf("response %d is not a Range; transaction returned the wrong op shape", index)
	}
	return (*clientv3.GetResponse)(inner)
}

// TestDifferentialKV walks the KV surface the apiserver actually uses. Each
// step runs against a real embedded etcd and against Tandem, and the answers
// are compared field by field.
func TestDifferentialKV(t *testing.T) {
	h := NewHarness(t)
	// Binary key and value: the apiserver stores these routinely, and a
	// backend that round-trips them as text corrupts Secrets.
	put := step{
		name: "Put",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			key := "diff/\x00binary\xff"
			response, err := client.Put(ctx, key, "value\x00\xff;--drop")
			return Observation{Op: "Put", Code: codeOf(err), Revision: relative(response.Header.GetRevision(), base)}
		},
	}
	readBack := step{
		name: "Range round-trips binary",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			response, err := client.Get(ctx, "diff/\x00binary\xff")
			observation := Observation{Op: "Range", Code: codeOf(err), Count: response.Count}
			if err == nil {
				observation.KVs = kvs(response.Kvs, base)
			}
			return observation
		},
	}
	prefix := step{
		name: "Range prefix ordering and count",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			for _, key := range []string{"diff/prefix/c", "diff/prefix/a", "diff/prefix/b"} {
				if _, err := client.Put(ctx, key, "v"); err != nil {
					t.Fatalf("seed %s: %v", key, err)
				}
			}
			response, err := client.Get(ctx, "diff/prefix/", clientv3.WithPrefix())
			observation := Observation{Op: "RangePrefix", Code: codeOf(err), Count: response.Count, More: response.More}
			if err == nil {
				observation.KVs = kvs(response.Kvs, base)
			}
			return observation
		},
	}
	runCase(t, h, put, readBack, prefix)

	// revision semantics: a failed compare must not consume a revision, which
	// is what a CAS loop depends on to make progress.
	failedCompare := step{
		name: "failed compare does not advance revision",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			if _, err := client.Put(ctx, "diff/cas/present", "v1"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			// A compare that cannot hold: the key exists, so create-if-absent fails.
			response, err := client.Txn(ctx).
				If(clientv3.Compare(clientv3.Version("diff/cas/present"), "=", 0)).
				Then(clientv3.OpPut("diff/cas/present", "bad")).
				Else(clientv3.OpGet("diff/cas/present")).
				Commit()
			observation := Observation{Op: "FailedCompare", Code: codeOf(err)}
			if err == nil {
				observation.Revision = relative(response.Header.Revision, base)
				// The Else branch holds the only op, so its answer is first.
				observation.Count = rangeOf(t, response, 0).Count
				observation.KVs = kvs(rangeOf(t, response, 0).Kvs, base)
			}
			return observation
		},
	}
	runCase(t, h, failedCompare)

	// One revision for many writes: the apiserver's object writes and the
	// bootstrap transactions both rely on this.
	sharedRevision := step{
		name: "one revision for multiple writes in one txn",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			response, err := client.Txn(ctx).
				Then(
					clientv3.OpPut("diff/shared/a", "1"),
					clientv3.OpPut("diff/shared/b", "2"),
					clientv3.OpGet("diff/shared/", clientv3.WithPrefix()),
				).
				Commit()
			observation := Observation{Op: "SharedRevision", Code: codeOf(err)}
			if err == nil {
				observation.Revision = relative(response.Header.Revision, base)
				// Two puts precede the Range, so it answers at index 2.
				observation.Count = rangeOf(t, response, 2).Count
				observation.KVs = kvs(rangeOf(t, response, 2).Kvs, base)
			}
			return observation
		},
	}
	runCase(t, h, sharedRevision)

	// KIP-29 lists tombstone version parity as verified-in-M1 or not at all.
	tombstone := step{
		name: "delete writes a tombstone etcd can read back at the same revision",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			if _, err := client.Put(ctx, "diff/tomb", "v1"); err != nil {
				t.Fatalf("seed: %v", err)
			}
			if _, err := client.Put(ctx, "diff/tomb", "v2"); err != nil {
				t.Fatalf("second put: %v", err)
			}
			deleted, err := client.Delete(ctx, "diff/tomb")
			observation := Observation{Op: "Delete", Code: codeOf(err), Count: deleted.Deleted}
			if err != nil {
				return observation
			}
			observation.Revision = relative(deleted.Header.Revision, base)
			// Reading the deleted key at its own delete revision must return
			// the tombstone: present, with the version the delete left behind.
			history, readErr := client.Get(ctx, "diff/tomb", clientv3.WithRev(deleted.Header.Revision))
			if readErr != nil {
				t.Fatalf("read tombstone: %v", readErr)
			}
			observation.Count = history.Count
			observation.KVs = kvs(history.Kvs, base)
			return observation
		},
	}
	runCase(t, h, tombstone)
}

// TestDifferentialErrors compares status codes for the failure paths the
// apiserver branches on. Messages are never compared — Tandem sends the Zig
// error name and etcd sends prose — so this asserts the code only.
func TestDifferentialErrors(t *testing.T) {
	h := NewHarness(t)

	compacted := step{
		name: "read at a compacted revision is OutOfRange",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			for i := range 4 {
				if _, err := client.Put(ctx, "diff/compact/key", string(rune('a'+i))); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			current, err := client.Get(ctx, "diff/compact/key")
			if err != nil {
				t.Fatalf("read current: %v", err)
			}
			watermark := current.Header.Revision - 1
			if _, err := client.Compact(ctx, watermark); err != nil {
				t.Fatalf("compact: %v", err)
			}
			_, err = client.Get(ctx, "diff/compact/key", clientv3.WithRev(watermark-1))
			return Observation{Op: "Compacted", Code: codeOf(err)}
		},
	}
	runCase(t, h, compacted)

	// A read at exactly the compaction revision. etcd's guard is
	// `rev < compactMainRev`, so the boundary revision itself is still
	// readable — it is the one that is kept. A `<=` guard rejects a read that
	// etcd serves, which a controller resuming from its last seen revision
	// would hit as a spurious OutOfRange.
	atWatermark := step{
		name: "a read at the compaction revision itself still succeeds",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			for i := range 3 {
				if _, err := client.Put(ctx, "diff/watermark/key", string(rune('a'+i))); err != nil {
					t.Fatalf("seed: %v", err)
				}
			}
			current, err := client.Get(ctx, "diff/watermark/key")
			if err != nil {
				t.Fatalf("read current: %v", err)
			}
			watermark := current.Header.Revision
			if _, err := client.Compact(ctx, watermark); err != nil {
				t.Fatalf("compact to %d: %v", watermark, err)
			}
			response, err := client.Get(ctx, "diff/watermark/key", clientv3.WithRev(watermark))
			observation := Observation{Op: "ReadAtWatermark", Code: codeOf(err)}
			if err == nil {
				observation.Count = response.Count
				observation.KVs = kvs(response.Kvs, base)
			}
			return observation
		},
	}
	runCase(t, h, atWatermark)

	leaseNotFound := step{
		name: "unknown lease is NotFound",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			_, err := client.TimeToLive(ctx, 0xDEADBEEF)
			return Observation{Op: "LeaseNotFound", Code: codeOf(err)}
		},
	}
	runCase(t, h, leaseNotFound)

	// A Range inside a Txn that names a revision must answer from history.
	// etcd checks the revision against the store and either answers from the
	// historical snapshot or refuses; answering from current state instead
	// returns data the caller did not ask for and cannot detect.
	txnHistoricalRange := step{
		name: "a Range in a Txn at an older revision answers from history",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			if _, err := client.Put(ctx, "diff/txnhist", "v1"); err != nil {
				t.Fatalf("first put: %v", err)
			}
			past, err := client.Get(ctx, "diff/txnhist")
			if err != nil {
				t.Fatalf("read for baseline: %v", err)
			}
			// Move the key on, so a current-state answer is distinguishable
			// from the historical one.
			if _, err := client.Put(ctx, "diff/txnhist", "v2"); err != nil {
				t.Fatalf("second put: %v", err)
			}
			response, err := client.Txn(ctx).
				Then(clientv3.OpGet("diff/txnhist", clientv3.WithRev(past.Header.Revision))).
				Commit()
			observation := Observation{Op: "TxnHistoricalRange", Code: codeOf(err)}
			if err == nil {
				observation.Revision = relative(response.Header.Revision, base)
				observation.Count = rangeOf(t, response, 0).Count
				observation.KVs = kvs(rangeOf(t, response, 0).Kvs, base)
			}
			return observation
		},
	}
	runCase(t, h, txnHistoricalRange)

	futureRevision := step{
		name: "read at a future revision is OutOfRange",
		run: func(t *testing.T, ctx context.Context, client *clientv3.Client, base int64) Observation {
			_, err := client.Get(ctx, "diff/future", clientv3.WithRev(base+1000))
			return Observation{Op: "FutureRevision", Code: codeOf(err)}
		},
	}
	runCase(t, h, futureRevision)
}
