//go:build tandem_differential

package differential

import (
	"context"
	"testing"
	"time"

	"go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// These helpers normalize a backend's answer against the revision the case
// started at. They live beside the cases rather than in normalize.go because
// only the tagged case files call them, and a non-tagged build would otherwise
// carry them as unreferenced production code.

// kv normalizes one mvccpb pair against a baseline revision.
func kv(in *mvccpb.KeyValue, baseline int64) KeyValue {
	out := KeyValue{
		Key:   string(in.Key),
		Value: string(in.Value),
		Lease: in.Lease,
	}
	// A revision at or below the baseline was created before the case began;
	// subtracting keeps a pre-existing key comparable without asserting where
	// either store started counting.
	if in.CreateRevision > baseline {
		out.CreateRevision = in.CreateRevision - baseline
	}
	if in.ModRevision > baseline {
		out.ModRevision = in.ModRevision - baseline
	}
	out.Version = in.Version
	return out
}

func kvs(in []*mvccpb.KeyValue, baseline int64) []KeyValue {
	if len(in) == 0 {
		return nil
	}
	out := make([]KeyValue, 0, len(in))
	for _, item := range in {
		if item == nil {
			continue
		}
		out = append(out, kv(item, baseline))
	}
	return out
}

// relative reduces a header revision to a delta from the case baseline. etcd
// and Tandem do not share a revision space, so only the movement is comparable.
func relative(revision, baseline int64) int64 {
	if revision <= baseline {
		return 0
	}
	return revision - baseline
}

// step is one operation in a case. It receives a client and the revision that
// store sat at before the step, so the observation it returns can express
// results as deltas from that baseline.
//
// The same step function runs against both backends. Everything that could
// differ between them — endpoint, transport, revision space — arrives as an
// argument rather than being captured in a closure, so the two runs really do
// execute the identical operation.
type step struct {
	name string
	run  func(t *testing.T, ctx context.Context, client *clientv3.Client, baseline int64) Observation
}

// runCase executes the steps in order against both backends and compares each
// pair. Both sides see the identical sequence against their own fresh store,
// which is what makes the per-step revision deltas comparable.
func runCase(t *testing.T, h *Harness, steps ...step) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			etcdBaseline := baseline(t, ctx, h.Etcd.Client)
			tandemBaseline := baseline(t, ctx, h.Tandem.Client)
			want := s.run(t, ctx, h.Etcd.Client, etcdBaseline)
			got := s.run(t, ctx, h.Tandem.Client, tandemBaseline)
			if difference := Compare(want, got); difference != "" {
				t.Errorf("%s\n  %s", s.name, difference)
			}
		})
	}
}

// baseline is the revision a store sits at before the step under test. Taking
// it per step keeps an earlier step's writes from shifting the numbers.
func baseline(t *testing.T, ctx context.Context, client *clientv3.Client) int64 {
	t.Helper()
	response, err := client.Get(ctx, "diff-probe-baseline")
	if err != nil {
		t.Fatalf("read baseline: %v", err)
	}
	return response.Header.Revision
}
