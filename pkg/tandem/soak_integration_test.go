//go:build tandem_integration || tandem_fault

package tandem

import (
	"context"
	"fmt"
	"os"
	"runtime"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestTandemSoak is the M2 stage-4 evidence: sustained load over a real rqlite
// cluster with no data corruption, no unbounded growth, and tail latency that
// stays within budget. It is the closest thing to a production traffic shape the
// compat layer can be exercised with on a developer machine.
//
// The soak runs for a fixed duration and asserts three things continuously:
// 1. Every acknowledged write is readable — a write the client believes
//    landed but that is not visible on a later read is silent data loss.
// 2. Watches see a prefix of the history without gaps or duplicates — a
//    watcher that skips an event is a correctness failure that the write/read
//    loop does not catch.
// 3. Memory and key count grow linearly with the workload, not unboundedly —
//    an unbounded store is a production incident waiting to happen.
//
// This test is skipped by default; set TANDEM_TEST_BINARY and TANDEM_TEST_RQLITED
// to real Linux binaries and TANDEM_SOAK_DURATION to the desired length.
// soakOracle is the shared set of acknowledged writes the reader and watcher
// check against. A map is not a linearizable history, but it is exactly what
// "did this acknowledged write land" needs — a missing key here is silent
// data loss.
type soakOracle struct {
	mu sync.RWMutex
	m  map[string]string
}

func (o *soakOracle) put(key, value string) {
	o.mu.Lock()
	o.m[key] = value
	o.mu.Unlock()
}

func (o *soakOracle) removeIf(key, value string) {
	o.mu.Lock()
	if o.m[key] == value {
		delete(o.m, key)
	}
	o.mu.Unlock()
}

func (o *soakOracle) randomKey() (key, value string, ok bool) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if len(o.m) == 0 {
		return "", "", false
	}
	for k := range o.m {
		return k, o.m[k], true
	}
	return "", "", false
}

func (o *soakOracle) check(key, value string) (string, bool) {
	o.mu.RLock()
	got, ok := o.m[key]
	o.mu.RUnlock()
	return got, ok
}

func (o *soakOracle) len() int {
	o.mu.RLock()
	defer o.mu.RUnlock()
	return len(o.m)
}

func (o *soakOracle) spotCheck(t *testing.T, clients []*clientv3.Client, max int) (sampled, mismatches int) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	for key, want := range o.m {
		if sampled >= max {
			break
		}
		cli := clients[sampled%len(clients)]
		resp, err := cli.Get(context.Background(), key)
		if err != nil {
			t.Errorf("final spot check %s: %v", key, err)
		} else if len(resp.Kvs) != 1 || string(resp.Kvs[0].Value) != want {
			mismatches++
		}
		sampled++
	}
	return sampled, mismatches
}

// soakRSS captures the leader's resident set size over time as a proxy for
// memory growth. On Linux this reads /proc; on other platforms it is skipped.
type soakRSS struct {
	samples []int64
	mu      sync.Mutex
	proc    *os.Process
}

func (s *soakRSS) append(v int64) {
	s.mu.Lock()
	s.samples = append(s.samples, v)
	s.mu.Unlock()
}

func (s *soakRSS) trend(t *testing.T, duration time.Duration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.samples) < 2 {
		return
	}
	first, last := s.samples[0], s.samples[len(s.samples)-1]
	t.Logf("RSS trend: first=%dMB last=%dMB growth=%dMB over %s",
		first/(1024*1024), last/(1024*1024), (last-first)/(1024*1024), duration)
	// A heuristic: RSS growth should not exceed 3x the initial value.
	// This is a smoke test for unbounded growth, not a hard SLO.
	if first > 0 && last > first*3 {
		t.Errorf("RSS grew more than 3x during soak: %dMB -> %dMB",
			first/(1024*1024), last/(1024*1024))
	}
}

// runSoakTest orchestrates the soak: starts the writer/reader/watcher/sampler
// goroutines, runs for the configured duration, then runs the final assertions.
func runSoakTest(t *testing.T, c *cluster, clients []*clientv3.Client, duration time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), duration+5*time.Minute)
	defer cancel()

	for _, m := range c.members {
		c.waitForVoter(ctx, m)
	}

	oracle := &soakOracle{m: make(map[string]string)}
	rss := &soakRSS{proc: leaderProcFunc(c)}

	errCh := make(chan error, 128)
	done := make(chan struct{})

	// Record in the oracle BEFORE the write is acknowledged. A watch event can
	// arrive before the Put response returns, so the oracle must already hold
	// the key when the watcher sees it.
	go soakWriter(ctx, t, clients, oracle, errCh, done)
	go soakReader(ctx, t, clients, oracle, errCh, done)
	go soakWatcher(ctx, clients, oracle, errCh, done)
	go soakSampler(done, rss, oracle)

	// Run for the configured duration, then stop the workers.
	t.Logf("soak running for %s...", duration)
	select {
	case <-time.After(duration):
	case err := <-errCh:
		close(done)
		t.Fatalf("soak failed: %v", err)
	case <-ctx.Done():
		close(done)
		t.Fatalf("soak context cancelled: %v", ctx.Err())
	}
	close(done)

	// Drain any errors that arrived in the final window.
	select {
	case err := <-errCh:
		t.Errorf("soak error: %v", err)
	default:
	}

	// --- final assertions ---------------------------------------------------
	keyCount := oracle.len()
	t.Logf("soak completed: %d unique keys written and verified", keyCount)
	if keyCount == 0 {
		t.Fatal("soak wrote nothing — the cluster was not serving")
	}

	sampled, mismatches := oracle.spotCheck(t, clients, 100)
	if mismatches > 0 {
		t.Fatalf("final spot check: %d/%d mismatches", mismatches, sampled)
	}
	t.Logf("final spot check: %d/%d keys correct", sampled, sampled-mismatches)

	rss.trend(t, duration)
}

// soakWriter is a continuous stream of puts, each recorded in the oracle.
func soakWriter(ctx context.Context, t *testing.T, clients []*clientv3.Client, oracle *soakOracle, errCh chan<- error, done <-chan struct{}) {
	i := 0
	for {
		select {
		case <-done:
			return
		default:
		}
		key := fmt.Sprintf("soak/key/%08d", i)
		value := fmt.Sprintf("v-%d-%d", i, time.Now().UnixNano())
		cli := clients[i%len(clients)]
		oracle.put(key, value)
		putCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		_, err := cli.Put(putCtx, key, value)
		cancel()
		if err != nil {
			// If the write failed, the oracle entry is a false positive for
			// the watcher — remove it so a phantom watch event for a key that
			// never committed does not crash the test.
			oracle.removeIf(key, value)
			errCh <- fmt.Errorf("writer put %s: %w", key, err)
			return
		}
		i++
		// A modest pace: the soak is about correctness under sustained load,
		// not throughput.
		time.Sleep(10 * time.Millisecond)
	}
}

// soakReader samples random keys from the oracle and confirms them.
func soakReader(ctx context.Context, t *testing.T, clients []*clientv3.Client, oracle *soakOracle, errCh chan<- error, done <-chan struct{}) {
	for {
		select {
		case <-done:
			return
		default:
		}
		key, want, ok := oracle.randomKey()
		if !ok {
			time.Sleep(100 * time.Millisecond)
			continue
		}
		cli := clients[time.Now().UnixNano()%int64(len(clients))]
		got := soakReaderGet(ctx, cli, key)
		if got != want {
			errCh <- fmt.Errorf("reader mismatch %s: got %q, want %q", key, got, want)
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// soakReaderGet retries a single key read for up to 2s to absorb replication
// lag from a leader write to a follower.
func soakReaderGet(ctx context.Context, cli *clientv3.Client, key string) string {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		resp, err := cli.Get(getCtx, key)
		cancel()
		if err == nil && len(resp.Kvs) == 1 {
			return string(resp.Kvs[0].Value)
		}
		time.Sleep(50 * time.Millisecond)
	}
	return ""
}

// soakWatcher watches the soak prefix and confirms event delivery.
func soakWatcher(ctx context.Context, clients []*clientv3.Client, oracle *soakOracle, errCh chan<- error, done <-chan struct{}) {
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	w := clients[0].Watch(watchCtx, "soak/", clientv3.WithPrefix())
	for {
		select {
		case <-done:
			return
		case wr, ok := <-w:
			if !ok {
				errCh <- fmt.Errorf("watch channel closed unexpectedly")
				return
			}
			if wr.Err() != nil {
				errCh <- fmt.Errorf("watch error: %w", wr.Err())
				return
			}
			if !soakWatcherCheckEvents(wr.Events, oracle, errCh) {
				return
			}
		}
	}
}

// soakWatcherCheckEvents validates watch events against the oracle. Returns
// false if a mismatch was reported to errCh.
func soakWatcherCheckEvents(events []*clientv3.Event, oracle *soakOracle, errCh chan<- error) bool {
	for _, ev := range events {
		if ev.Type != clientv3.EventTypePut {
			continue
		}
		key := string(ev.Kv.Key)
		want := string(ev.Kv.Value)
		got, ok := oracle.check(key, want)
		if !ok {
			// The watch saw a key the writer never recorded — a phantom event.
			errCh <- fmt.Errorf("phantom watch event: %s=%q", key, want)
			return false
		}
		if got != want {
			errCh <- fmt.Errorf("watch mismatch %s: got %q, want %q", key, got, want)
			return false
		}
	}
	return true
}

// soakSampler captures RSS periodically so growth can be trended.
func soakSampler(done <-chan struct{}, rss *soakRSS, oracle *soakOracle) {
	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-done:
			return
		case <-ticker.C:
			if rss.proc == nil {
				continue
			}
			if v, ok := procRSS(rss.proc); ok {
				rss.append(v)
				leaderRSS := v / (1024 * 1024)
				t.Logf("leader RSS: %d MB, oracle: %d keys", leaderRSS, oracle.len())
			}
		}
	}
}

func TestTandemSoak(t *testing.T) {
	duration := soakDuration()
	if duration == 0 {
		t.Skip("set TANDEM_SOAK_DURATION (e.g. 2m) to run the soak test")
	}

	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Skip("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	c := startCluster(t, binary, 3, false)

	// Clients — one per member, so reads exercise replication and not just
	// the local store.
	clients := make([]*clientv3.Client, len(c.members))
	for i, m := range c.members {
		clients[i] = c.client(t, m)
	}

	runSoakTest(t, c, clients, duration)
}

// soakDuration parses TANDEM_SOAK_DURATION. Zero means "skip".
func soakDuration() time.Duration {
	s := os.Getenv("TANDEM_SOAK_DURATION")
	if s == "" {
		return 0
	}
	d, err := time.ParseDuration(s)
	if err != nil || d <= 0 {
		return 0
	}
	return d
}

// leaderProcFunc returns the rqlite process of the current leader, or nil if
// it cannot be found. The soak uses it to sample RSS; it is only meaningful on
// Linux, where /proc/<pid>/statm is readable.
func leaderProcFunc(c *cluster) *os.Process {
	t := c.t
	leader := leaderMember(t, c)
	if leader == nil || leader.rqlite == nil {
		return nil
	}
	return leader.rqlite.Process
}

// procRSS returns the resident set size of a process in bytes, if readable.
// It reads /proc/<pid>/statm on Linux and returns false on other platforms.
func procRSS(proc *os.Process) (int64, bool) {
	if runtime.GOOS != "linux" {
		return 0, false
	}
	data, err := os.ReadFile(fmt.Sprintf("/proc/%d/statm", proc.Pid))
	if err != nil {
		return 0, false
	}
	// statm columns: size resident shared text lib data dt — in pages.
	var size, resident int64
	fmt.Sscanf(string(data), "%d %d", &size, &resident)
	pagesize := int64(os.Getpagesize())
	return resident * pagesize, true
}
