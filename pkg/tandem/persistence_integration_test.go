//go:build tandem_integration

package tandem

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
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

func testAddress(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
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
