//go:build tandem_integration || tandem_fault

package tandem

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestTandemUncertainWriteIsNotRetried(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		// Simulate a committed mutation whose HTTP response is lost.
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			t.Error(err)
			return
		}
		conn.Close()
	}))
	defer server.Close()
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+server.URL, "TANDEM_PROBE_PHASE=uncertain-write")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("probe: %v: %s", err, out)
	}
	if got := requests.Load(); got != 1 {
		t.Fatalf("write was sent %d times, want exactly one attempt", got)
	}
}

func TestTandemPersistentRangePagination(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=range")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("pagination: %v: %s", err, out)
	}
}

func TestTandemPersistentTxnRangeIsAtomic(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=txn-range")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("txn range: %v: %s", err, out)
	}
	if !bytes.Contains(out, []byte("txn-range ok")) {
		t.Fatalf("probe did not report a successful transaction range: %s", out)
	}
}

func TestTandemPersistentTxnCompareTargets(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=txn-compares")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("txn compares: %v: %s", err, out)
	}
	if !bytes.Contains(out, []byte("txn-compares ok 10")) {
		t.Fatalf("compare targets did not all pass: %s", out)
	}
}

func TestTandemPersistentLeaseReleaseFreesOwner(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=lease-release")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lease release: %v: %s", err, out)
	}
}

func TestTandemPersistentLeaseOwnerIsExclusive(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=lease-owner")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lease owner: %v: %s", err, out)
	}
	// The second instance must find nothing to reap: without an exclusive
	// claim both would delete the key and both would advance the revision.
	if !bytes.Contains(out, []byte("lease-owner a=")) || !bytes.Contains(out, []byte("b=0")) {
		t.Fatalf("second instance was not excluded from expiry: %s", out)
	}
}

func TestTandemPersistentLeaseTakeoverAfterCrash(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=lease-takeover")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("lease takeover: %v: %s", err, out)
	}
}

func TestTandemPersistentLeaseIDsAreShared(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=lease-ids")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("shared lease IDs: %v: %s", err, out)
	}
}

func TestTandemPersistentStandalonePreviousValues(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=standalone-prev")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("standalone previous values: %v: %s", err, out)
	}
}

func TestTandemPersistentLeaseExpiry(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	cmd := exec.CommandContext(t.Context(), probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=lease-expiry")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("lease expiry: %v: %s", err, out)
	}
	// The probe fails on a surviving key, so reaching here means the leased key
	// was deleted and the expiry produced a DELETE event.
	if !bytes.Contains(out, []byte("remaining=0")) {
		t.Fatalf("probe did not report the key as removed: %s", out)
	}
}

func TestTandemPersistenceConcurrentTxn(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the persistence-probe executable")
	}
	url, _ := startTestRqlite(t, t.TempDir(), "", "")
	const contenders = 8
	winners := make(chan bool, contenders)
	var wg sync.WaitGroup
	for range contenders {
		wg.Add(1)
		go func() {
			defer wg.Done()
			cmd := exec.Command(probe)
			cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE=txn-cas")
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Errorf("independent CAS: %v: %s", err, out)
				return
			}
			winners <- bytes.Contains(out, []byte("CAS winner=true"))
		}()
	}
	wg.Wait()
	close(winners)
	count := 0
	for won := range winners {
		if won {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("independent Pipeline CAS winners=%d, want 1", count)
	}
}

var (
	testPortMu    sync.Mutex
	testPortsUsed = map[int]bool{}
)

// testAddress returns a localhost host:port this process has not handed out
// before.
//
// Picking a free port is done by listening on port 0 and closing, which frees
// it immediately. Two such calls in a row can therefore return the SAME port:
// the allocator recycles what was just released. rqlite, told to serve its HTTP
// API and its Raft listener on one address, refuses to start —
// "fatal: HTTP and Raft addresses must differ" — and the caller only sees a
// readiness timeout. Same story for the addresses of different cluster members.
//
// So the port is recorded and a collision retries. Registry entries are never
// released: a test's addresses stay distinct for the whole process run, which
// is what keeps the pair, and the members of a multi-node fixture, apart.
func testAddress(t *testing.T) string {
	t.Helper()
	for attempt := 0; attempt < 1000; attempt++ {
		l, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		addr := l.Addr().String()
		l.Close()

		_, portText, err := net.SplitHostPort(addr)
		if err != nil {
			t.Fatal(err)
		}
		port, err := strconv.Atoi(portText)
		if err != nil {
			t.Fatal(err)
		}

		testPortMu.Lock()
		duplicate := testPortsUsed[port]
		if !duplicate {
			testPortsUsed[port] = true
		}
		testPortMu.Unlock()
		if !duplicate {
			return addr
		}
	}
	t.Fatal("could not allocate a unique loopback address after 1000 attempts")
	return ""
}

// TestTestAddressNeverRepeatsAPort guards the allocator the fixtures depend on.
//
// The old implementation could return the same port twice, because each call
// released its port before returning it; that is what made rqlite abort with
// "HTTP and Raft addresses must differ" and the test report a readiness timeout
// instead.
//
// This is a structural guard rather than a reproduction: whether the kernel
// recycles a just-freed port is an allocation detail. I could not make the old
// implementation fail here on this host (10 consecutive runs, 200 calls each,
// no duplicate), so the old implementation was not inherently broken; the
// registry removes the possibility of the collision, and this test stands
// behind the guarantee that it does.
//
// The two cases that matter are the ones this test distinguishes:
//
//   - Two consecutive calls, as in `address, raft := testAddress(t),
//     testAddress(t)` (the rqlite fixtures)
//   - Members of a multi-node cluster fixture, each taking its own pair
//
// So the assertions run the pair repeatedly and the cluster-sized sequence, and
// fail loudly on any repeat. Removing the registry in favour of the
// old two-line body does not necessarily fail this test on this host, but it
// reintroduces the state that made the production test flaky, which is the
// part worth pinning.
func TestTestAddressNeverRepeatsAPort(t *testing.T) {
	// A single allocator serving two callers back to back is the rqlite shape.
	for i := 0; i < 50; i++ {
		address := testAddress(t)
		raft := testAddress(t)
		if address == raft {
			t.Fatalf("iteration %d: HTTP and Raft addresses are both %s; rqlite will refuse to start", i, address)
		}
	}

	// A sequence for every member of the largest fixture in the suite.
	seen := map[string]struct{}{}
	for i := 0; i < 3; i++ {
		for j := 0; j < 2; j++ {
			addr := testAddress(t)
			if _, dup := seen[addr]; dup {
				t.Fatalf("member %d probe %d: address %s already handed out", i, j, addr)
			}
			seen[addr] = struct{}{}
		}
	}
}

// Every test owns its rqlite process and data; no pre-existing daemon is used.
func startTestRqlite(t *testing.T, data, address, raft string) (string, func()) {
	t.Helper()
	binary := os.Getenv("TANDEM_TEST_RQLITED")
	if binary == "" {
		t.Fatal("set TANDEM_TEST_RQLITED to a rqlited executable")
	}
	if address == "" {
		address = testAddress(t)
	}
	if raft == "" {
		raft = testAddress(t)
	}
	log, err := os.CreateTemp(t.TempDir(), "rqlite-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "-node-id", "persistence-test", "-http-addr", address, "-raft-addr", raft, data)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		log.Close()
		t.Fatal(err)
	}
	var once sync.Once
	stop := func() {
		once.Do(func() {
			// Deliberately abrupt: restart must recover committed data without a graceful snapshot.
			cmd.Process.Kill()
			cmd.Wait()
			log.Close()
			if t.Failed() {
				b, _ := os.ReadFile(log.Name())
				t.Log(string(b))
			}
		})
	}
	t.Cleanup(stop)
	client := &http.Client{Timeout: time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	url := "http://" + address
	for deadline := time.Now().Add(15 * time.Second); time.Now().Before(deadline); {
		r, err := client.Get(url + "/readyz")
		if err == nil {
			r.Body.Close()
			if r.StatusCode == http.StatusOK {
				return url, stop
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("rqlite failed readiness")
	return "", stop
}

func TestTandemPersistenceRestart(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the zig build persistence-probe executable")
	}
	data := filepath.Join(t.TempDir(), "rqlite")
	address, raft := testAddress(t), testAddress(t)
	url, stop := startTestRqlite(t, data, address, raft)
	run := func(phase string) {
		t.Helper()
		cmd := exec.Command(probe)
		cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+url, "TANDEM_PROBE_PHASE="+phase)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("%s: %v\n%s", phase, err, output)
		}
	}
	run("write")
	run("rollback")
	run("read") // independent Pipeline process
	stop()
	startTestRqlite(t, data, address, raft)
	run("read") // after rqlite process failure/recovery
}

func TestTandemPersistenceFailsClosed(t *testing.T) {
	probe := os.Getenv("TANDEM_TEST_PROBE")
	if probe == "" {
		t.Fatal("set TANDEM_TEST_PROBE to the zig build persistence-probe executable")
	}
	// Select an unused local port, then close it so connection refusal is immediate.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	cmd := exec.Command(probe)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR=http://"+address, "TANDEM_PROBE_PHASE=write")
	if output, err := cmd.CombinedOutput(); err == nil {
		t.Fatalf("unavailable rqlite silently accepted a write: %s", output)
	}
}
