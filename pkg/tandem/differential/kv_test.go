//go:build tandem_differential

package differential

import (
	"context"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

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
