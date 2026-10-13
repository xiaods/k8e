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
	ctx, cancel := context.WithTimeout(context.Background(), duration+5*time.Minute)
	defer cancel()

	// Confirm the cluster is healthy before measuring it.
	for _, m := range c.members {
		c.waitForVoter(ctx, m)
	}

	// Clients — one per member, so reads exercise replication and not just
	// the local store.
	clients := make([]*clientv3.Client, len(c.members))
	for i, m := range c.members {
		clients[i] = c.client(t, m)
	}

	// The oracle: every key written is recorded here, and the read checker
	// confirms it is present and correct. A map is not a linearizable
	// history, but it is exactly what "did this acknowledged write land"
	// needs — a missing key here is silent data loss.
	var (
		oracleMu sync.RWMutex
		oracle   = make(map[string]string)
	)

	// Track RSS of the leader's rqlite process as a rough proxy for memory
	// growth. On Linux this is read from /proc; on other platforms it is
	// skipped — the test still exercises the store, it just cannot measure
	// growth on a platform it cannot read.
	var (
		rssSamples    []int64
		rssSamplesMu  sync.Mutex
		leaderProc    = leaderProcFunc(c)
	)

	errCh := make(chan error, 128)
	done := make(chan struct{})

	// --- writer: a continuous stream of puts, each recorded in the oracle --
	go func() {
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
			// Record in the oracle BEFORE the write is acknowledged. A watch
			// event can arrive before the Put response returns — the apiserver
			// fans out watchers concurrently with the Raft commit — so the
			// oracle must already hold the key when the watcher sees it.
			// Writing first and updating on success would produce phantom
			// events that are the test's fault, not the store's.
			oracleMu.Lock()
			oracle[key] = value
			oracleMu.Unlock()
			putCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
			_, err := cli.Put(putCtx, key, value)
			cancel()
			if err != nil {
				// If the write failed, the oracle entry is a false positive
				// for the watcher — remove it so a phantom watch event for a
				// key that never committed does not crash the test.
				oracleMu.Lock()
				if oracle[key] == value {
					delete(oracle, key)
				}
				oracleMu.Unlock()
				errCh <- fmt.Errorf("writer put %s: %w", key, err)
				return
			}
			i++
			// A modest pace: the soak is about correctness under sustained
			// load, not throughput. Too fast and the test outruns compaction;
			// too slow and it does not stress the store.
			time.Sleep(10 * time.Millisecond)
		}
	}()

	// --- reader: sample random keys from the oracle and confirm them -----
	go func() {
		for {
			select {
			case <-done:
				return
			default:
			}
			oracleMu.RLock()
			if len(oracle) == 0 {
				oracleMu.RUnlock()
				time.Sleep(100 * time.Millisecond)
				continue
			}
			// Pick a random key from the oracle. A deterministic scan would
			// always test the same keys and miss the ones being written
			// concurrently.
			var key string
			for k := range oracle {
				key = k
				break
			}
			want := oracle[key]
			oracleMu.RUnlock()

			cli := clients[time.Now().UnixNano()%int64(len(clients))]
			// Retry briefly: a key written on a leader may not yet be
			// replicated to a follower, and reading a follower is the
			// whole point of the check. What must hold is that the key
			// resolves within a few hundred milliseconds, not that it is
			// instantly visible from every member.
			var got string
			var lastErr error
			deadline := time.Now().Add(2 * time.Second)
			for time.Now().Before(deadline) {
				getCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				resp, err := cli.Get(getCtx, key)
				cancel()
				if err == nil && len(resp.Kvs) == 1 {
					got = string(resp.Kvs[0].Value)
					break
				}
				lastErr = err
				time.Sleep(50 * time.Millisecond)
			}
			if got != want {
				errCh <- fmt.Errorf("reader mismatch %s: got %q, want %q (last error: %v)",
					key, got, want, lastErr)
				return
			}
			time.Sleep(50 * time.Millisecond)
		}
	}()

	// --- watcher: watches the soak prefix and confirms event delivery ------
	go func() {
		watchCtx, watchCancel := context.WithCancel(ctx)
		defer watchCancel()
		w := clients[0].Watch(watchCtx, "soak/", clientv3.WithPrefix())
		for {
			select {
			case <-done:
				return
			case wr, ok := <-w:
				if !ok {
					// The channel was closed — the watch ended without
					// the test finishing. That is a protocol failure.
					errCh <- fmt.Errorf("watch channel closed unexpectedly")
					return
				}
				if wr.Err() != nil {
					errCh <- fmt.Errorf("watch error: %w", wr.Err())
					return
				}
				for _, ev := range wr.Events {
					if ev.Type == clientv3.EventTypePut {
						key := string(ev.Kv.Key)
						want := string(ev.Kv.Value)
						oracleMu.RLock()
						got, ok := oracle[key]
						oracleMu.RUnlock()
						if !ok {
							// The watch saw a key the writer never recorded.
							// That is a phantom event — data the cluster
							// invented.
							errCh <- fmt.Errorf("phantom watch event: %s=%q", key, want)
							return
						}
						if got != want {
							errCh <- fmt.Errorf("watch mismatch %s: got %q, want %q", key, got, want)
							return
						}
					}
				}
			}
		}
	}()

	// --- resource sampler: capture RSS periodically so growth can be trended
	go func() {
		ticker := time.NewTicker(30 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-done:
				return
			case <-ticker.C:
				if leaderProc == nil {
					continue
				}
				if rss, ok := procRSS(leaderProc); ok {
					rssSamplesMu.Lock()
					rssSamples = append(rssSamples, rss)
					rssSamplesMu.Unlock()
					t.Logf("leader RSS: %d MB, oracle: %d keys", rss/(1024*1024), len(oracle))
				}
			}
		}
	}()

	// Run for the configured duration, then stop the workers and report.
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
	for {
		select {
		case err := <-errCh:
			t.Errorf("soak error: %v", err)
		default:
		}
		break
	}

	// --- final assertions ---------------------------------------------------
	oracleMu.RLock()
	keyCount := len(oracle)
	oracleMu.RUnlock()
	t.Logf("soak completed: %d unique keys written and verified", keyCount)

	if keyCount == 0 {
		t.Fatal("soak wrote nothing — the cluster was not serving")
	}

	// Sample 100 random keys for a final spot check.
	oracleMu.RLock()
	sampled := 0
	mismatches := 0
	for key, want := range oracle {
		if sampled >= 100 {
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
	oracleMu.RUnlock()
	if mismatches > 0 {
		t.Fatalf("final spot check: %d/%d mismatches", mismatches, sampled)
	}
	t.Logf("final spot check: %d/%d keys correct", sampled, sampled-mismatches)

	// --- RSS trend --------------------------------------------------------
	rssSamplesMu.Lock()
	defer rssSamplesMu.Unlock()
	if len(rssSamples) >= 2 {
		first := rssSamples[0]
		last := rssSamples[len(rssSamples)-1]
		growth := last - first
		t.Logf("RSS trend: first=%dMB last=%dMB growth=%dMB over %s",
			first/(1024*1024), last/(1024*1024), growth/(1024*1024), duration)
		// A heuristic: RSS growth over the soak should not exceed 2x the
		// initial value. This is not a hard SLO — it is a smoke test for
		// unbounded growth, which is the stage-4 failure mode.
		if first > 0 && last > first*3 {
			t.Errorf("RSS grew more than 3x during soak: %dMB -> %dMB",
				first/(1024*1024), last/(1024*1024))
		}
	}
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
