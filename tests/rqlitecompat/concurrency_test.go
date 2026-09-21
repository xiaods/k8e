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
			var collected []helperResult
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
	const want = procs * each
	results := runHelperProcesses(t, cluster.Endpoints(), "create", procs, each, nil)

	requireEqual(t, "results reported by the helper processes", len(results), want)
	assertDistinctRevisions(t, results, want)

	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the concurrent creates", err)
	requireEqual(t, "revision after the concurrent creates", revision, int64(want))

	history, err := store.CountAllHistory(ctx)
	requireNoError(t, "count the history rows", err)
	requireEqual(t, "history rows, one per create", history, int64(want))

	requests, err := store.CountRequests(ctx)
	requireNoError(t, "count the request records", err)
	requireEqual(t, "request records", requests, int64(want))
}

// assertDistinctRevisions checks that every helper reported an acknowledged
// write and that the acknowledged revisions are exactly 1..want.
func assertDistinctRevisions(t *testing.T, results []helperResult, want int) {
	t.Helper()
	byRevision := map[int64]string{}
	for _, r := range results {
		requireTrue(t, fmt.Sprintf("%s reported %q, want every create to succeed", r.requestID, r.status), r.status == "OK")
		other, dup := byRevision[r.revision]
		requireTrue(t, fmt.Sprintf("revision %d was acknowledged for both %s and %s", r.revision, other, r.requestID), !dup)
		byRevision[r.revision] = r.requestID
	}
	for rev := int64(1); rev <= int64(want); rev++ {
		_, ok := byRevision[rev]
		requireTrue(t, fmt.Sprintf("revision %d was never acknowledged; got %d distinct revisions from %d writes", rev, len(byRevision), len(results)), ok)
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
	requireNoError(t, "create", err)
	requireTrue(t, fmt.Sprintf("create = %+v, want the key created at revision 1", create), create.Succeeded && create.Revision == 1)

	const procs = 4
	results := runHelperProcesses(t, cluster.Endpoints(), "cas", procs, 1, map[string]string{
		"RQLITE_M0_KEY":    string(key),
		"RQLITE_M0_EXPECT": "1",
	})
	requireEqual(t, "results reported by the racing helper processes", len(results), procs)

	winners, losers := countRacers(t, results)
	requireTrue(t, fmt.Sprintf("the CAS race gave %d winners and %d losers, want 1 and %d", winners, losers, procs-1),
		winners == 1 && losers == procs-1)

	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the CAS race", err)
	requireEqual(t, "revision after the CAS race: failed compares must not consume revisions", revision, int64(2))

	history, err := store.History(ctx, key)
	requireNoError(t, "read the history after the CAS race", err)
	requireEqual(t, fmt.Sprintf("history entries after the CAS race, want exactly 2 (create + the single winner), got %+v", history),
		len(history), 2)

	requests, err := store.CountRequests(ctx)
	requireNoError(t, "count the request records after the CAS race", err)
	requireEqual(t, "request records after the CAS race", requests, int64(procs+1))
}

// countRacers counts the CAS winners and losers of a race on one key. A failed
// compare reports the revision that is current when it executes. Under a race
// that can already be the winner's revision; what must never happen is a loser
// consuming a revision of its own, which the revision and history counts of the
// caller assert.
func countRacers(t *testing.T, results []helperResult) (winners, losers int) {
	t.Helper()
	for _, r := range results {
		switch r.status {
		case "OK":
			winners++
			requireEqual(t, fmt.Sprintf("%s won at revision", r.requestID), r.revision, int64(2))
		case "FAIL":
			losers++
			requireTrue(t, fmt.Sprintf("%s lost the compare and reported revision %d, want 1 or 2", r.requestID, r.revision),
				r.revision >= 1 && r.revision <= 2)
		default:
			t.Fatalf("%s reported %q", r.requestID, r.status)
		}
	}
	return winners, losers
}
