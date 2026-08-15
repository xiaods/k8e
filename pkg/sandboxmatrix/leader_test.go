package sandboxmatrix

import (
	"context"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/daemons/config"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	kubefake "k8s.io/client-go/kubernetes/fake"
)

func TestLeaderElectionIdentity(t *testing.T) {
	// Explicit identity wins.
	cfg := config.SandboxConfig{LeaderElectionIdentity: "node-a"}
	if got := leaderElectionIdentity(cfg); got != "node-a" {
		t.Fatalf("explicit identity = %q, want node-a", got)
	}
	// Empty falls back to hostname (always non-empty on a real host).
	cfg = config.SandboxConfig{}
	if got := leaderElectionIdentity(cfg); got == "" {
		t.Fatal("empty identity should fall back to hostname, got empty")
	}
}

// runCounter tracks how many times a leader-gated callback has been started.
type runCounter struct {
	mu    sync.Mutex
	calls int
}

func (c *runCounter) inc() {
	c.mu.Lock()
	c.calls++
	c.mu.Unlock()
}

func (c *runCounter) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.calls
}

// fastLeaderConfig shortens the election timers so tests run in well under a
// second instead of the production 15s lease.
func fastLeaderConfig() config.SandboxConfig {
	return config.SandboxConfig{
		Namespace:              "sandbox-matrix",
		LeaderElectionIdentity: "test-node",
	}
}

func TestRunLeaderGated_ElectsExactlyOneLeader(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s := kubefake.NewSimpleClientset()

	var leaders atomic.Int32
	var wg sync.WaitGroup

	// Two candidates contend for the same Lease; exactly one must start.
	for _, id := range []string{"cand-1", "cand-2"} {
		cfg := fastLeaderConfig()
		cfg.LeaderElectionIdentity = id
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runLeaderGated(ctx, k8s, cfg, func(leaderCtx context.Context) {
				leaders.Add(1)
				<-leaderCtx.Done()
			})
		}()
	}

	// Give the electors time to acquire and observe leadership.
	deadline := time.Now().Add(5 * time.Second)
	for leaders.Load() != 1 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if leaders.Load() != 1 {
		t.Fatalf("expected exactly 1 leader, got %d", leaders.Load())
	}

	cancel()
	wg.Wait()

	// After the parent context is cancelled, leadership is released and no
	// second candidate should have started.
	if leaders.Load() != 1 {
		t.Fatalf("leadership should not transfer after shutdown, leaders=%d", leaders.Load())
	}
}

func TestRunLeaderGated_HandoverOnLeaderLoss(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s := kubefake.NewSimpleClientset()

	var wg sync.WaitGroup
	started := make(chan string, 8) // identity of each leader that starts
	var count runCounter

	runCb := func(id string) func(leaderCtx context.Context) {
		return func(lc context.Context) {
			count.inc()
			started <- id
			<-lc.Done()
		}
	}

	// startCandidate runs a leader-gated elector on baseCtx with the given
	// identity. Identities are unique per candidate so lease takeover is
	// unambiguous.
	startCandidate := func(baseCtx context.Context, id string) {
		cfg := fastLeaderConfig()
		cfg.LeaderElectionIdentity = id
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = runLeaderGated(baseCtx, k8s, cfg, runCb(id))
		}()
	}

	// A becomes leader; B starts as a follower and keeps retrying.
	ctxA, cancelA := context.WithCancel(ctx)
	startCandidate(ctxA, "cand-A")
	select {
	case id := <-started:
		if id != "cand-A" {
			t.Fatalf("first leader = %s, want cand-A", id)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("cand-A never became leader")
	}
	startCandidate(ctx, "cand-B")

	// Give B time to observe A's leadership (standby, not leader).
	time.Sleep(200 * time.Millisecond)
	if got := count.count(); got != 1 {
		t.Fatalf("B must be a follower while A holds the lease; run called %d times (want 1)", got)
	}

	// A loses leadership (its elector's ctx is cancelled; ReleaseOnCancel
	// releases the lease). B must take over and start its reconcilers.
	cancelA()
	select {
	case id := <-started:
		if id != "cand-B" {
			t.Fatalf("takeover leader = %s, want cand-B", id)
		}
	case <-time.After(8 * time.Second):
		t.Fatal("cand-B never took over after leader release")
	}

	cancel()
	wg.Wait()
}

// TestRunLeaderGated_StandbyDoesNotRunReconcilers verifies that a follower's
// callback is not invoked until it wins the lease: two candidates, the loser
// must never call run.
func TestRunLeaderGated_StandbyDoesNotRunReconcilers(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s := kubefake.NewSimpleClientset()

	var count runCounter
	var wg sync.WaitGroup
	runCb := func(leaderCtx context.Context) {
		count.inc()
		<-leaderCtx.Done()
	}

	// First candidate holds the lease the whole time.
	startedA := make(chan struct{})
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runLeaderGated(ctx, k8s, fastLeaderConfig(), func(lc context.Context) {
			count.inc()
			close(startedA)
			<-lc.Done()
		})
	}()
	select {
	case <-startedA:
	case <-time.After(5 * time.Second):
		t.Fatal("candidate A never became leader")
	}

	// Second candidate starts; with A holding a live lease (renewing), B must
	// stay a follower and never invoke run. Give it enough time to attempt
	// acquisition multiple times.
	cfgB := fastLeaderConfig()
	cfgB.LeaderElectionIdentity = "cand-B"
	wg.Add(1)
	go func() {
		defer wg.Done()
		_ = runLeaderGated(ctx, k8s, cfgB, runCb)
	}()
	time.Sleep(1 * time.Second)

	if got := count.count(); got != 1 {
		t.Fatalf("follower must not run reconcilers; run called %d times (want 1)", got)
	}

	cancel()
	wg.Wait()
}

// Sanity: the lease is created in the sandbox-matrix namespace with the
// expected name after a successful election.
func TestRunLeaderGated_CreatesLeaseInNamespace(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	k8s := kubefake.NewSimpleClientset()
	done := make(chan struct{})
	go func() {
		_ = runLeaderGated(ctx, k8s, fastLeaderConfig(), func(lc context.Context) {
			close(done)
			<-lc.Done()
		})
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("never became leader")
	}

	lease, err := k8s.CoordinationV1().Leases("sandbox-matrix").Get(ctx, leaderElectionLeaseName, metav1.GetOptions{})
	if err != nil {
		t.Fatalf("lease not created: %v", err)
	}
	if lease.Spec.HolderIdentity == nil || *lease.Spec.HolderIdentity == "" {
		t.Fatal("lease holder identity empty")
	}
}
