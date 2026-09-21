package rqlitecompat

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// TestAdapterHelperProcess is not a test: it is the child process the
// concurrency tests start, so that "several adapter clients" means several
// independent processes with their own HTTP connections rather than
// goroutines in one process.
func TestAdapterHelperProcess(t *testing.T) {
	if os.Getenv("RQLITE_M0_HELPER") != "1" {
		t.Skip("adapter helper process; started by the concurrency tests")
	}

	endpoints := strings.Split(os.Getenv("RQLITE_M0_ENDPOINTS"), ",")
	mode := os.Getenv("RQLITE_M0_MODE")
	worker := os.Getenv("RQLITE_M0_WORKER")
	count, err := strconv.Atoi(os.Getenv("RQLITE_M0_COUNT"))
	if err != nil {
		fmt.Printf("RESULT ERR %s bad count: %v\n", worker, err)
		return
	}
	expect, _ := strconv.ParseInt(os.Getenv("RQLITE_M0_EXPECT"), 10, 64)

	store := NewStore(NewClient(endpoints...))
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	for i := 0; i < count; i++ {
		requestID := fmt.Sprintf("%s-%03d", worker, i)
		request := CASRequest{RequestID: requestID, Value: []byte(requestID)}
		switch mode {
		case "create":
			request.Key = []byte(fmt.Sprintf("/registry/k8e/procs/%s/%03d", worker, i))
		case "cas":
			request.Key = []byte(os.Getenv("RQLITE_M0_KEY"))
			request.ExpectModRevision = expect
		default:
			fmt.Printf("RESULT ERR %s unknown mode %q\n", worker, mode)
			return
		}

		res, err := store.TxnCAS(ctx, request)
		if err != nil {
			fmt.Printf("RESULT ERR %s %v\n", worker, err)
			return
		}
		if res.Succeeded {
			fmt.Printf("RESULT OK %s %d\n", requestID, res.Revision)
		} else {
			fmt.Printf("RESULT FAIL %s %d\n", requestID, res.Revision)
		}
	}
}

type helperResult struct {
	status    string
	requestID string
	revision  int64
}

// runHelperProcesses starts procs independent processes doing count writes
// each against the given endpoints and returns every reported result.
func runHelperProcesses(t *testing.T, endpoints []string, mode string, procs, count int, extra map[string]string) []helperResult {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	var (
		mu      sync.Mutex
		results []helperResult
		wg      sync.WaitGroup
	)

	for p := 0; p < procs; p++ {
		worker := fmt.Sprintf("p%d", p)
		wg.Add(1)
		go func(worker string) {
			defer wg.Done()
			cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestAdapterHelperProcess$")
			cmd.Env = append(os.Environ(),
				"RQLITE_M0_HELPER=1",
				"RQLITE_M0_ENDPOINTS="+strings.Join(endpoints, ","),
				"RQLITE_M0_MODE="+mode,
				"RQLITE_M0_WORKER="+worker,
				"RQLITE_M0_COUNT="+strconv.Itoa(count),
			)
			for k, v := range extra {
				cmd.Env = append(cmd.Env, k+"="+v)
			}
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Errorf("%s: stdout pipe: %v", worker, err)
				return
			}
			cmd.Stderr = os.Stderr
			if err := cmd.Start(); err != nil {
				t.Errorf("%s: start: %v", worker, err)
				return
			}

			scanner := bufio.NewScanner(stdout)
			collected := []helperResult{}
			for scanner.Scan() {
				line := scanner.Text()
				if !strings.HasPrefix(line, "RESULT ") {
					continue
				}
				fields := strings.Fields(line)
				if len(fields) != 4 {
					t.Errorf("%s: bad result line %q", worker, line)
					continue
				}
				revision, _ := strconv.ParseInt(fields[3], 10, 64)
				collected = append(collected, helperResult{status: fields[1], requestID: fields[2], revision: revision})
			}
			if err := cmd.Wait(); err != nil {
				t.Errorf("%s: helper exited with %v", worker, err)
			}
			mu.Lock()
			results = append(results, collected...)
			mu.Unlock()
		}(worker)
	}
	wg.Wait()
	return results
}

// TestConcurrentAdapterProcessesAllocateContiguousRevisions runs four
// independent adapter processes, each creating 25 keys, and checks that the
// revisions they are acknowledged with are exactly 1..100: no revision was
// lost, skipped or handed out twice under concurrent load.
func TestConcurrentAdapterProcessesAllocateContiguousRevisions(t *testing.T) {
	cluster, _, store := bootstrap(t, 3)
	ctx := context.Background()

	const procs = 4
	const each = 25
	results := runHelperProcesses(t, cluster.Endpoints(), "create", procs, each, nil)

	want := procs * each
	if len(results) != want {
		t.Fatalf("helpers reported %d results, want %d", len(results), want)
	}
	byRevision := map[int64]string{}
	for _, r := range results {
		if r.status != "OK" {
			t.Fatalf("%s reported %q, want every create to succeed", r.requestID, r.status)
		}
		if other, dup := byRevision[r.revision]; dup {
			t.Fatalf("revision %d was acknowledged for both %s and %s", r.revision, other, r.requestID)
		}
		byRevision[r.revision] = r.requestID
	}
	for rev := int64(1); rev <= int64(want); rev++ {
		if _, ok := byRevision[rev]; !ok {
			t.Fatalf("revision %d was never acknowledged; got %d distinct revisions from %d writes", rev, len(byRevision), len(results))
		}
	}

	if got, err := store.MetaRevision(ctx); err != nil || got != int64(want) {
		t.Fatalf("revision after %d concurrent creates = %d (%v), want %d", want, got, err, want)
	}
	if count, err := store.CountAllHistory(ctx); err != nil || count != int64(want) {
		t.Fatalf("history rows = %d (%v), want %d (one per create)", count, err, want)
	}
	if count, err := store.CountRequests(ctx); err != nil || count != int64(want) {
		t.Fatalf("request records = %d (%v), want %d", count, err, want)
	}
}

// TestConcurrentAdapterProcessesRaceOnOneKey is the classic CAS race: four
// processes compare-and-swap the same key with the same expected revision and
// exactly one must win, while the losers leave the revision untouched.
func TestConcurrentAdapterProcessesRaceOnOneKey(t *testing.T) {
	cluster, _, store := bootstrap(t, 3)
	ctx := context.Background()
	key := []byte("/registry/k8e/procs/shared")

	create, err := store.TxnCAS(ctx, CASRequest{RequestID: "shared-0", Key: key, Value: []byte("winner-base")})
	if err != nil || !create.Succeeded || create.Revision != 1 {
		t.Fatalf("create: %+v (%v)", create, err)
	}

	const procs = 4
	results := runHelperProcesses(t, cluster.Endpoints(), "cas", procs, 1, map[string]string{
		"RQLITE_M0_KEY":    string(key),
		"RQLITE_M0_EXPECT": "1",
	})
	if len(results) != procs {
		t.Fatalf("helpers reported %d results, want %d", len(results), procs)
	}

	winners, losers := 0, 0
	for _, r := range results {
		switch r.status {
		case "OK":
			winners++
			if r.revision != 2 {
				t.Fatalf("%s won at revision %d, want 2", r.requestID, r.revision)
			}
		case "FAIL":
			losers++
			// A failed compare reports the revision that is current when it
			// executes. Under a race that can already be the winner's
			// revision; what must never happen is consuming a revision of its
			// own (asserted through the revision and history counts below).
			if r.revision < 1 || r.revision > 2 {
				t.Fatalf("%s lost the compare and reported revision %d, want 1 or 2", r.requestID, r.revision)
			}
		default:
			t.Fatalf("%s reported %q", r.requestID, r.status)
		}
	}
	if winners != 1 || losers != procs-1 {
		t.Fatalf("CAS race gave %d winners and %d losers, want 1 and %d", winners, losers, procs-1)
	}

	if got, err := store.MetaRevision(ctx); err != nil || got != 2 {
		t.Fatalf("revision after the CAS race = %d (%v), want 2: failed compares must not consume revisions", got, err)
	}
	history, err := store.History(ctx, key)
	if err != nil {
		t.Fatalf("history: %v", err)
	}
	if len(history) != 2 {
		t.Fatalf("history has %d entries (%+v), want exactly 2 (create + the single winner)", len(history), history)
	}
	if count, err := store.CountRequests(ctx); err != nil || count != procs+1 {
		t.Fatalf("request records = %d (%v), want %d", count, err, procs+1)
	}
}
