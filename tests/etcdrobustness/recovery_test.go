package etcdrobustness

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// Environment variables of the child process a strong-kill round re-executes.
// The child hosts the member under test only; the workload and the operation
// history run in the parent, which the kill never reaches.
const (
	envChildDir    = "ETCD_ROBUSTNESS_CHILD_DIR"
	envChildClient = "ETCD_ROBUSTNESS_CHILD_CLIENT_URL"
	envChildPeer   = "ETCD_ROBUSTNESS_CHILD_PEER_URL"

	// envWorkDir points the per-test data directories at a larger filesystem.
	envWorkDir = "ETCD_ROBUSTNESS_WORKDIR"

	readyFileName       = "ready"
	fingerprintFileName = "fingerprint.json"
	childLogFileName    = "child.log"

	defaultSigkillRounds = 10
	defaultSigkillSeed   = int64(612)

	// The kill lands between killDwellFloor and killDwellJitter after the first
	// acknowledged write.
	killDwellFloor  = 200 * time.Millisecond
	killDwellJitter = 500 * time.Millisecond
)

// fingerprint is the cluster identity a node observed before the fault. A
// recovery that recreated an empty cluster would change it.
type fingerprint struct {
	MemberID  uint64 `json:"member_id"`
	ClusterID uint64 `json:"cluster_id"`
	Revision  int64  `json:"revision"`
}

// workDir returns a scratch directory per test. The directory is kept when the
// test failed (or ETCD_ROBUSTNESS_KEEP=1 is set) so the data directory and the
// node log survive for diagnosis, as the design requires.
//
// One member preallocates a 64MB WAL, and the disk-fault cases copy it, so the
// default temporary filesystem can be too small: set ETCD_ROBUSTNESS_WORKDIR to
// a directory on a larger filesystem.
func workDir(t *testing.T) string {
	t.Helper()
	base := os.Getenv(envWorkDir)
	if base == "" {
		base = os.TempDir()
	}
	dir, err := os.MkdirTemp(base, "etcd-robustness-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if t.Failed() || os.Getenv("ETCD_ROBUSTNESS_KEEP") == "1" {
			t.Logf("retaining robustness work dir %s", dir)
			return
		}
		if err := os.RemoveAll(dir); err != nil {
			t.Logf("could not remove %s: %v", dir, err)
		}
	})
	return dir
}

// reserveURLs picks two free loopback ports and returns the client and peer URL.
// The ports are reserved once per round and reused across restarts so the
// recovered node is reached at the same endpoint.
func reserveURLs(t *testing.T) (string, string) {
	t.Helper()
	ports := make([]int, 2)
	for i := range ports {
		listener, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatal(err)
		}
		ports[i] = listener.Addr().(*net.TCPAddr).Port
		if err := listener.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return fmt.Sprintf("http://127.0.0.1:%d", ports[0]), fmt.Sprintf("http://127.0.0.1:%d", ports[1])
}

func startNode(t *testing.T, opts NodeOptions) *Node {
	t.Helper()
	node, err := StartNode(opts)
	if err != nil {
		t.Fatalf("start embedded etcd: %v", err)
	}
	t.Cleanup(node.Close)
	return node
}

func mustClient(t *testing.T, endpoint string) *clientv3.Client {
	t.Helper()
	client, err := NewClient(endpoint)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Close() })
	return client
}

// TestEmbeddedEtcdGracefulRestart restarts a single member against the same
// data directory and checks the phase-1 oracle: cluster identity, revision and
// data survive, and CRUD/CAS/Watch keep working.
func TestEmbeddedEtcdGracefulRestart(t *testing.T) {
	dir := workDir(t)
	clientURL, peerURL := reserveURLs(t)
	opts := NodeOptions{Name: "robustness", Dir: dir, ClientURL: clientURL, PeerURL: peerURL}

	node := startNode(t, opts)
	client := mustClient(t, clientURL)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	historyPath := filepath.Join(dir, "history.jsonl")
	recorder := mustRecorder(t, historyPath, time.Now)
	defer recorder.Close()

	// Immutable, checksummed records: the oracle can tell a survivor from a
	// value nobody ever wrote.
	writeRecords(ctx, t, client, recorder, clientURL, 20)
	// Concurrent CAS: exactly one writer may win the same expected revision.
	writeCASRace(ctx, t, client, recorder, clientURL)

	// Watch must deliver the mutation and the delete, from a revision that
	// cannot miss either.
	watchMutation(ctx, t, client, "watch/", 2, func() {
		if _, err := client.Put(ctx, "watch/entry", "value"); err != nil {
			t.Fatal(err)
		}
		if _, err := client.Delete(ctx, "watch/entry"); err != nil {
			t.Fatal(err)
		}
	})

	before, err := client.Status(ctx, clientURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := recorder.Close(); err != nil {
		t.Fatal(err)
	}
	if err := client.Close(); err != nil {
		t.Fatal(err)
	}
	node.Close()

	// Restart against the same data directory: StartNode sees member/snap and
	// asks for cluster state "existing" instead of bootstrapping a new cluster.
	startNode(t, opts)
	recovered := mustClient(t, clientURL)
	after, err := recovered.Status(ctx, clientURL)
	if err != nil {
		t.Fatal(err)
	}

	history := mustLoadHistory(t, historyPath)
	report, err := Verify(ctx, history, ClientReader(recovered))
	if err != nil {
		t.Fatal(err)
	}
	violations := identityViolations(before.Header.MemberId, before.Header.ClusterId, after, "restart")
	violations = append(violations, report.Violations...)
	violations = append(violations, VerifyRevisionContinuity(after.Header.Revision, MaxAcknowledgedRevision(history))...)
	if len(violations) > 0 {
		t.Fatalf("graceful restart oracle violations:\n%s", strings.Join(violations, "\n"))
	}

	// The recovered cluster must still serve CRUD and Watch.
	assertServingAfterRecovery(ctx, t, recovered)
}

// assertServingAfterRecovery proves the recovered member is a working store and
// not only a readable one.
func assertServingAfterRecovery(ctx context.Context, t *testing.T, client *clientv3.Client) {
	t.Helper()
	if _, err := client.Put(ctx, "after/restart", "ok"); err != nil {
		t.Fatalf("write after restart: %v", err)
	}
	got, err := client.Get(ctx, "after/restart")
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Kvs) != 1 || string(got.Kvs[0].Value) != "ok" {
		t.Fatalf("read after restart = %+v", got.Kvs)
	}

	// The Issue asks for CRUD/CAS/Watch on the recovered member, so exercise
	// the rest of it: a stale CAS must lose, the current one must win, and a
	// delete must remove the key.
	opCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	revision := got.Kvs[0].ModRevision
	stale, err := client.Txn(opCtx).
		If(clientv3.Compare(clientv3.ModRevision("after/restart"), "=", revision-1)).
		Then(clientv3.OpPut("after/restart", "stale")).
		Commit()
	if err != nil {
		t.Fatalf("stale CAS after restart: %v", err)
	}
	if stale.Succeeded {
		t.Fatal("a CAS with a stale expected revision won on the recovered member")
	}
	won, err := client.Txn(opCtx).
		If(clientv3.Compare(clientv3.ModRevision("after/restart"), "=", revision)).
		Then(clientv3.OpPut("after/restart", "cas-ok")).
		Commit()
	if err != nil {
		t.Fatalf("CAS after restart: %v", err)
	}
	if !won.Succeeded {
		t.Fatal("a CAS with the current expected revision lost on the recovered member")
	}
	if _, err := client.Delete(opCtx, "after/restart"); err != nil {
		t.Fatalf("delete after restart: %v", err)
	}

	watchMutation(ctx, t, client, "watch/", 1, func() {
		if _, err := client.Put(ctx, "watch/after-restart", "ok"); err != nil {
			t.Fatal(err)
		}
	})
}

// writeRecords issues count acknowledged puts through the recorder.
func writeRecords(ctx context.Context, t *testing.T, client *clientv3.Client, recorder *Recorder, endpoint string, count int) {
	t.Helper()
	for i := 0; i < count; i++ {
		key := fmt.Sprintf("records/%03d", i)
		value := fmt.Sprintf("record-%03d-%s", i, strings.Repeat("x", 32))
		attempt := mustBegin(t, recorder, Operation{Kind: KindPut, Key: key, Value: value, Endpoint: endpoint})
		response, err := client.Put(ctx, key, value)
		if err != nil {
			_ = attempt.Unknown(err)
			t.Fatal(err)
		}
		must(t, attempt.Acknowledge(response.Header.Revision))
	}
}

// writeCASRace races concurrent compare-and-swaps on one key and requires
// exactly one winner: a second winner for the same expected revision would mean
// the CAS was not really applied.
func writeCASRace(ctx context.Context, t *testing.T, client *clientv3.Client, recorder *Recorder, endpoint string) {
	t.Helper()
	const (
		raceKey = "cas/race"
		racers  = 8
	)
	if _, err := client.Put(ctx, raceKey, "seed"); err != nil {
		t.Fatal(err)
	}
	seed, err := client.Get(ctx, raceKey)
	if err != nil {
		t.Fatal(err)
	}
	seedRevision := seed.Kvs[0].ModRevision

	start := make(chan struct{})
	results := make(chan error, racers)
	var winners int32
	var wg sync.WaitGroup
	for i := 0; i < racers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			value := fmt.Sprintf("winner-%d", i)
			attempt, err := recorder.Begin(Operation{
				Kind:           KindCAS,
				Key:            raceKey,
				Value:          value,
				ExpectValue:    "seed",
				ExpectRevision: seedRevision,
				Endpoint:       endpoint,
			})
			if err != nil {
				results <- err
				return
			}
			<-start
			txn, err := client.Txn(ctx).
				If(clientv3.Compare(clientv3.ModRevision(raceKey), "=", seedRevision)).
				Then(clientv3.OpPut(raceKey, value)).
				Commit()
			if err != nil {
				results <- attempt.Unknown(err)
				return
			}
			if txn.Succeeded {
				atomic.AddInt32(&winners, 1)
				results <- attempt.Acknowledge(txn.Header.Revision)
				return
			}
			results <- attempt.Reject(errors.New("compare failed"))
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	for err := range results {
		if err != nil {
			t.Fatal(err)
		}
	}
	if winners != 1 {
		t.Fatalf("concurrent CAS winners = %d, want exactly 1", winners)
	}
}

// watchMutation watches prefix from the revision before the mutation and fails
// unless mutate delivered want events.
func watchMutation(ctx context.Context, t *testing.T, client *clientv3.Client, prefix string, want int, mutate func()) {
	t.Helper()
	count, err := client.Get(ctx, prefix, clientv3.WithPrefix(), clientv3.WithCountOnly())
	if err != nil {
		t.Fatal(err)
	}
	watchCtx, watchCancel := context.WithCancel(ctx)
	defer watchCancel()
	watch := client.Watch(watchCtx, prefix, clientv3.WithPrefix(), clientv3.WithRev(count.Header.Revision+1))
	mutate()
	if err := collectEvents(watch, want); err != nil {
		t.Fatal(err)
	}
}

// identityViolations reports a changed member or cluster id, which would mean
// the member was not recovered but recreated.
func identityViolations(memberID, clusterID uint64, after *clientv3.StatusResponse, phase string) []string {
	var violations []string
	if after.Header.MemberId != memberID {
		violations = append(violations, fmt.Sprintf("member id changed across %s: %x -> %x", phase, memberID, after.Header.MemberId))
	}
	if after.Header.ClusterId != clusterID {
		violations = append(violations, fmt.Sprintf("cluster id changed across %s: %x -> %x", phase, clusterID, after.Header.ClusterId))
	}
	return violations
}

// collectEvents waits until the watch delivered want events.
func collectEvents(watch clientv3.WatchChan, want int) error {
	deadline := time.After(15 * time.Second)
	seen := 0
	for seen < want {
		select {
		case response, ok := <-watch:
			if !ok {
				return fmt.Errorf("watch channel closed after %d of %d events", seen, want)
			}
			seen += len(response.Events)
		case <-deadline:
			return fmt.Errorf("watch delivered %d of %d events", seen, want)
		}
	}
	return nil
}

// TestEmbeddedEtcdSigkillDuringWrites runs the headline phase-1 scenario:
// continuously acknowledged writes are cut off by SIGKILL at a seeded moment,
// then the same data directory is recovered and checked.
//
// It is a fixed-seed, repeated (default 10 rounds) deterministic run: the
// rounds differ only in the seeded dwell time before the kill.
func TestEmbeddedEtcdSigkillDuringWrites(t *testing.T) {
	if os.Getenv("ETCD_ROBUSTNESS_SKIP_SIGKILL") == "1" {
		t.Skip("ETCD_ROBUSTNESS_SKIP_SIGKILL=1")
	}
	rounds := sigkillRounds(t)
	for round := 0; round < rounds; round++ {
		round := round
		t.Run(fmt.Sprintf("round-%02d", round), func(t *testing.T) {
			runSigkillRound(t, round, sigkillDwell(defaultSigkillSeed, round))
		})
	}
}

// sigkillRounds reads the configured round count, default 10.
func sigkillRounds(t *testing.T) int {
	t.Helper()
	raw := os.Getenv("ETCD_ROBUSTNESS_ROUNDS")
	if raw == "" {
		return defaultSigkillRounds
	}
	parsed, err := strconv.Atoi(raw)
	if err != nil || parsed < 1 {
		t.Fatalf("ETCD_ROBUSTNESS_ROUNDS=%q is not a positive integer", raw)
	}
	return parsed
}

// sigkillDwell is the seeded dwell between the first acknowledged write and the
// kill. It is derived from the run seed and the round with a hash rather than a
// PRNG, so the sequence is reproducible and every round kills at a different
// moment.
func sigkillDwell(seed int64, round int) time.Duration {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%d:%d", seed, round)))
	jitter := binary.BigEndian.Uint64(digest[:8]) % uint64(killDwellJitter)
	return killDwellFloor + time.Duration(jitter)
}

// runSigkillRound starts a child member, drives the acknowledged workload from
// this process — outside the member under test — SIGKILLs the child at the
// seeded moment and checks the recovered data directory.
//
// The workload and the recorder stay in the parent, so the SIGKILL cannot take
// the record of a response that already arrived: an acknowledged write that the
// recovered store lost stays visible instead of hiding as an unknown outcome.
func runSigkillRound(t *testing.T, round int, dwell time.Duration) {
	t.Helper()
	dir := workDir(t)
	clientURL, peerURL := reserveURLs(t)
	target := sigkillRoundTarget{dir: dir, clientURL: clientURL, peerURL: peerURL}
	historyPath := filepath.Join(dir, "history.jsonl")
	logPath := filepath.Join(dir, childLogFileName)

	command := startSigkillChild(t, target, logPath)
	waitForFile(t, filepath.Join(dir, readyFileName), 60*time.Second, logPath)

	client := mustClient(t, clientURL)
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	writeFingerprint(ctx, t, target, client)

	recorder := mustRecorder(t, historyPath, time.Now)
	defer recorder.Close()

	firstAck := make(chan struct{})
	workloadDone := make(chan error, 1)
	go func() {
		workloadDone <- runSigkillWorkload(ctx, client, recorder, clientURL, fmt.Sprintf("r%02d", round), firstAck)
	}()

	// The kill must land while writes are being acknowledged, not while the
	// member is still coming up.
	select {
	case <-firstAck:
	case err := <-workloadDone:
		t.Fatalf("the workload stopped before acknowledging a write: %v", err)
	case <-time.After(60 * time.Second):
		t.Fatal("no write was acknowledged within 60s; the workload never reached the server")
	}
	time.Sleep(dwell)
	killChild(t, command)
	if err := <-workloadDone; err != nil {
		t.Fatalf("the workload failed while the member was killed: %v", err)
	}

	history := mustLoadHistory(t, historyPath)
	acknowledged := countOutcome(history, OutcomeAcknowledged)
	if acknowledged == 0 {
		t.Fatalf("no write was acknowledged in %s before the kill; the workload never reached the server", dwell)
	}
	verifyKilledRound(t, round, dwell, target, history, acknowledged)
}

// writeFingerprint records the member identity before the fault from the
// parent, so the identity check does not depend on the process being killed.
func writeFingerprint(ctx context.Context, t *testing.T, target sigkillRoundTarget, client *clientv3.Client) {
	t.Helper()
	status, err := client.Status(ctx, target.clientURL)
	if err != nil {
		t.Fatalf("status before the kill: %v", err)
	}
	writeJSON(t, filepath.Join(target.dir, fingerprintFileName), fingerprint{
		MemberID:  status.Header.MemberId,
		ClusterID: status.Header.ClusterId,
		Revision:  status.Header.Revision,
	})
}

// runSigkillWorkload issues acknowledged puts — and a delete every five puts —
// until the member under test is killed under a request. firstAck is closed
// after the first acknowledged write so the caller can time the kill.
//
// The terminal request is recorded as unknown: the member died while the client
// was waiting, so the outcome was never learned. A response that did arrive is
// already fsynced as acknowledged before the kill can land, because this
// recorder is not the process that dies.
func runSigkillWorkload(ctx context.Context, client *clientv3.Client, recorder *Recorder, endpoint, runID string, firstAck chan<- struct{}) error {
	keys := make([]string, 0, 1024)
	for seq := 0; ; seq++ {
		key := fmt.Sprintf("workload/%s/%06d", runID, seq)
		value := fmt.Sprintf("%s:%06d:%s", runID, seq, strings.Repeat("v", 32))
		attempt, err := recorder.Begin(Operation{Kind: KindPut, Key: key, Value: value, Endpoint: endpoint})
		if err != nil {
			return err
		}
		response, putErr := timedPut(ctx, client, key, value)
		if putErr != nil {
			return attempt.Unknown(putErr)
		}
		if err := attempt.Acknowledge(response); err != nil {
			return err
		}
		if seq == 0 {
			close(firstAck)
		}
		keys = append(keys, key)

		if seq%5 == 4 {
			deleted := keys[seq-4]
			deleteAttempt, err := recorder.Begin(Operation{Kind: KindDelete, Key: deleted, Endpoint: endpoint})
			if err != nil {
				return err
			}
			revision, deleteErr := timedDelete(ctx, client, deleted)
			if deleteErr != nil {
				return deleteAttempt.Unknown(deleteErr)
			}
			if err := deleteAttempt.Acknowledge(revision); err != nil {
				return err
			}
		}
	}
}

// sigkillRoundTarget is where one strong-kill round runs: the data directory
// and the addresses the killed member is restarted on.
type sigkillRoundTarget struct {
	dir       string
	clientURL string
	peerURL   string
}

// startSigkillChild re-executes this test binary as the member child. The
// parent kills it, so the fault is a real SIGKILL delivered by another process,
// and the child is cleaned up if the round fails before the kill.
func startSigkillChild(t *testing.T, target sigkillRoundTarget, logPath string) *exec.Cmd {
	t.Helper()
	childLog, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = childLog.Close() })

	command := exec.Command(os.Args[0], "-test.run=^TestEmbeddedEtcdRobustnessChild$", "-test.v")
	command.Env = append(os.Environ(),
		envChildDir+"="+target.dir,
		envChildClient+"="+target.clientURL,
		envChildPeer+"="+target.peerURL,
	)
	command.Stdout = childLog
	command.Stderr = childLog
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if command.ProcessState != nil {
			return // the round already waited for the killed child
		}
		_ = command.Process.Kill()
		_, _ = command.Process.Wait()
	})
	return command
}

// killChild SIGKILLs the child and waits for it, so the fault under test is the
// kill and not a leaked process.
func killChild(t *testing.T, command *exec.Cmd) {
	t.Helper()
	if err := command.Process.Kill(); err != nil {
		t.Fatalf("SIGKILL the child: %v", err)
	}
	if err := command.Wait(); err != nil && !strings.Contains(err.Error(), "killed") {
		t.Fatalf("child failed before the kill: %v", err)
	}
}

// verifyKilledRound restarts the killed member on the same data directory and
// checks member identity, the acknowledged history and revision continuity.
func verifyKilledRound(t *testing.T, round int, dwell time.Duration, target sigkillRoundTarget, history []Record, acknowledged int) {
	t.Helper()
	before := readFingerprint(t, filepath.Join(target.dir, fingerprintFileName))

	startNode(t, NodeOptions{Name: "robustness", Dir: target.dir, ClientURL: target.clientURL, PeerURL: target.peerURL})
	client := mustClient(t, target.clientURL)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	after, err := client.Status(ctx, target.clientURL)
	if err != nil {
		t.Fatal(err)
	}
	violations := identityViolations(before.MemberID, before.ClusterID, after, "the kill")
	report, err := Verify(ctx, history, ClientReader(client))
	if err != nil {
		t.Fatal(err)
	}
	violations = append(violations, report.Violations...)
	violations = append(violations, VerifyRevisionContinuity(after.Header.Revision, MaxAcknowledgedRevision(history))...)
	if len(violations) > 0 {
		t.Fatalf("round %02d after %s: %d acknowledged, %d unknown, violations:\n%s",
			round, dwell, acknowledged, len(report.UnknownIntents), strings.Join(violations, "\n"))
	}
	t.Logf("round %02d: killed after %s, %d acknowledged, %d unknown, revision %d recovered",
		round, dwell, acknowledged, len(report.UnknownIntents), after.Header.Revision)
}

// mustLoadHistory loads the recorded operation history or fails the test.
func mustLoadHistory(t *testing.T, path string) []Record {
	t.Helper()
	history, err := LoadHistory(path)
	if err != nil {
		t.Fatal(err)
	}
	return history
}

// countOutcome counts the records with the given terminal outcome.
func countOutcome(history []Record, outcome Outcome) int {
	count := 0
	for _, record := range history {
		if record.Outcome == outcome {
			count++
		}
	}
	return count
}

// TestEmbeddedEtcdRobustnessChild is the child process of the strong-kill
// rounds: it hosts the member under test and nothing else. The workload and the
// operation history stay in the parent process, so the SIGKILL cannot take the
// record of an acknowledged response with it.
func TestEmbeddedEtcdRobustnessChild(t *testing.T) {
	dir := os.Getenv(envChildDir)
	if dir == "" {
		t.Skip("not a robustness child process")
	}
	node, err := StartNode(NodeOptions{
		Name:      "robustness",
		Dir:       dir,
		ClientURL: os.Getenv(envChildClient),
		PeerURL:   os.Getenv(envChildPeer),
	})
	if err != nil {
		t.Fatalf("child: start node: %v", err)
	}
	defer node.Close()
	if err := os.WriteFile(filepath.Join(dir, readyFileName), []byte("ready\n"), 0600); err != nil {
		t.Fatalf("child: ready marker: %v", err)
	}
	// The parent SIGKILLs this process when the round ends; block until then.
	select {}
}

// timedPut issues a put with its own deadline so a stalled request becomes an
// unknown outcome rather than blocking the kill window.
func timedPut(ctx context.Context, client *clientv3.Client, key, value string) (int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := client.Put(opCtx, key, value)
	if err != nil {
		return 0, err
	}
	return response.Header.Revision, nil
}

func timedDelete(ctx context.Context, client *clientv3.Client, key string) (int64, error) {
	opCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	response, err := client.Delete(opCtx, key)
	if err != nil {
		return 0, err
	}
	return response.Header.Revision, nil
}

func waitForFile(t *testing.T, path string, timeout time.Duration, logPath string) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	body, _ := os.ReadFile(logPath)
	t.Fatalf("child did not become ready within %s; %s tail:\n%s", timeout, logPath, tailLines(string(body), 40))
}

func tailLines(body string, lines int) string {
	parts := strings.Split(strings.TrimRight(body, "\n"), "\n")
	if len(parts) > lines {
		parts = parts[len(parts)-lines:]
	}
	return strings.Join(parts, "\n")
}

func writeJSON(t *testing.T, path string, value any) {
	t.Helper()
	body, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, body, 0600); err != nil {
		t.Fatal(err)
	}
}

func readFingerprint(t *testing.T, path string) fingerprint {
	t.Helper()
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read fingerprint: %v", err)
	}
	var value fingerprint
	if err := json.Unmarshal(body, &value); err != nil {
		t.Fatalf("parse fingerprint: %v", err)
	}
	return value
}
