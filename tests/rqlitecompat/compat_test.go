package rqlitecompat

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// bootstrap starts a cluster and returns a client, a store and the cluster.
func bootstrap(t *testing.T, nodes int) (*Cluster, *Client, *Store) {
	t.Helper()
	cluster := StartCluster(t, nodes)
	client := cluster.Client()
	if err := Bootstrap(context.Background(), client); err != nil {
		t.Fatalf("bootstrap schema: %v", err)
	}
	return cluster, client, NewStore(client)
}

func TestSchemaBootstrapIsIdempotentAndBlobsRoundTrip(t *testing.T) {
	_, client, store := bootstrap(t, 1)
	ctx := context.Background()

	assertBootstrapIsIdempotent(ctx, t, client, store)
	assertBinaryBlobsRoundTrip(ctx, t, store)
}

// assertBootstrapIsIdempotent re-runs the schema bootstrap and checks that a
// second run resets neither the revision counter nor the schema row.
func assertBootstrapIsIdempotent(ctx context.Context, t *testing.T, client *Client, store *Store) {
	t.Helper()
	requireNoError(t, "second bootstrap", Bootstrap(ctx, client))

	version, err := store.SchemaVersion(ctx)
	requireNoError(t, "read the schema version", err)
	requireEqual(t, "schema version", version, int64(SchemaVersion))

	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision", err)
	requireEqual(t, "revision after bootstrap", revision, int64(0))
}

// assertBinaryBlobsRoundTrip writes keys and values containing 0x00, 0xff and
// 0x80 — the reason blobs are sent as byte arrays and read with blob_array —
// and reads them back byte for byte.
func assertBinaryBlobsRoundTrip(ctx context.Context, t *testing.T, store *Store) {
	t.Helper()
	key := []byte{0x2f, 0x00, 0xff, 0x80, 0x01}
	value := []byte{0x00, 0x00, 0xff, 0x80}

	res, err := store.TxnCAS(ctx, CASRequest{RequestID: "binary-1", Key: key, Value: value})
	requireNoError(t, "create binary key", err)
	requireTrue(t, fmt.Sprintf("create binary key = %+v, want it to commit at revision 1", res), res.Succeeded && res.Revision == 1)
	requireBytes(t, "txn response key", res.KV.Key, key)
	requireBytes(t, "txn response value", res.KV.Value, value)

	kv, revision, err := store.Range(ctx, key)
	requireNoError(t, "range binary key", err)
	requireTrue(t, fmt.Sprintf("range binary key = %+v, want the entry that was written", kv), kv.Exists)
	requireBytes(t, "ranged key", kv.Key, key)
	requireBytes(t, "ranged value", kv.Value, value)
	requireEqual(t, "range header revision", revision, int64(1))

	missing, revision, err := store.Range(ctx, []byte{0x2f, 0x00, 0xfe})
	requireNoError(t, "range missing key", err)
	requireTrue(t, fmt.Sprintf("range of a missing key reported an entry: %+v", missing), !missing.Exists)
	requireEqual(t, "range header revision for a missing key", revision, int64(1))

	requireNoError(t, "prefix scan", store.assertOrderedPrefix(ctx, []byte{0x2f}, [][]byte{key}))
}

func TestTxnCompareBranchesAndRevision(t *testing.T) {
	_, _, store := bootstrap(t, 1)
	ctx := context.Background()
	key := []byte("/registry/sandbox-matrix/token/abc")

	casCreateThenUpdate(ctx, t, store, key)
	casFailedComparesKeepRevision(ctx, t, store, key)
	casDeleteWritesTombstone(ctx, t, store, key)
	casFailedDeleteKeepsRevision(ctx, t, store, key)
}

// casCreateThenUpdate checks the two committing compares: the create compares
// `mod_revision == 0` against an absent key, an update with a matching compare
// commits at the next revision and bumps only the version.
func casCreateThenUpdate(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	create, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-1", Key: key, Value: []byte("v1")})
	requireNoError(t, "create", err)
	requireTrue(t, fmt.Sprintf("create = %+v, want it to commit at revision 1", create), create.Succeeded && create.Revision == 1)
	requireEqual(t, "create create_revision", create.KV.CreateRevision, int64(1))
	requireEqual(t, "create mod_revision", create.KV.ModRevision, int64(1))
	requireEqual(t, "create version", create.KV.Version, int64(1))

	update, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-2", Key: key, ExpectModRevision: 1, Value: []byte("v2")})
	requireNoError(t, "update", err)
	requireTrue(t, fmt.Sprintf("update = %+v, want it to commit at revision 2", update), update.Succeeded && update.Revision == 2)
	requireEqual(t, "update create_revision", update.KV.CreateRevision, int64(1))
	requireEqual(t, "update mod_revision", update.KV.ModRevision, int64(2))
	requireEqual(t, "update version", update.KV.Version, int64(2))
}

// casFailedComparesKeepRevision checks that a stale compare and a create
// against an existing key both fail, change nothing and — like etcd's
// storeTxnWrite.End — do not advance the revision, so the next write takes the
// revision the failed compares did not consume.
func casFailedComparesKeepRevision(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	stale, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-3", Key: key, ExpectModRevision: 1, Value: []byte("v3")})
	requireNoError(t, "stale compare", err)
	requireTrue(t, fmt.Sprintf("stale compare = %+v, want a failed compare", stale), !stale.Succeeded)
	requireEqual(t, "stale compare reported revision", stale.Revision, int64(2))
	requireEqual(t, "stale compare response mod_revision", stale.KV.ModRevision, int64(2))
	requireBytes(t, "stale compare response value", stale.KV.Value, []byte("v2"))

	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the failed compare", err)
	requireEqual(t, "revision after the failed compare", revision, int64(2))
	requireNoError(t, "history after the failed compare", store.assertEqualHistory(ctx, key, []HistoryEntry{
		{ModRevision: 1, Version: 1, Value: []byte("v1")},
		{ModRevision: 2, Version: 2, Value: []byte("v2")},
	}))

	outcome, recordRevision, exists, err := store.RequestRecord(ctx, "tx-3")
	requireNoError(t, "read the failed compare record", err)
	requireTrue(t, "the failed compare is recorded", exists)
	requireEqual(t, "failed compare outcome", outcome, "cas_failed")
	requireEqual(t, "failed compare recorded revision", recordRevision, int64(2))

	dup, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-4", Key: key, Value: []byte("v3")})
	requireNoError(t, "duplicate create", err)
	requireTrue(t, fmt.Sprintf("duplicate create = %+v, want a failed compare", dup), !dup.Succeeded)
	requireEqual(t, "duplicate create reported revision", dup.Revision, int64(2))

	next, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-5", Key: key, ExpectModRevision: 2, Value: []byte("v4")})
	requireNoError(t, "update after the failed compares", err)
	requireTrue(t, fmt.Sprintf("update after the failed compares = %+v, want it to commit at revision 3", next), next.Succeeded && next.Revision == 3)
}

// casDeleteWritesTombstone checks that a delete writes a tombstone at the next
// revision, removes the key from a range and keeps the tombstone in history.
func casDeleteWritesTombstone(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	del, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-6", Key: key, ExpectModRevision: 3, Delete: true})
	requireNoError(t, "delete", err)
	requireTrue(t, fmt.Sprintf("delete = %+v, want it to commit at revision 4", del), del.Succeeded && del.Revision == 4)
	requireTrue(t, fmt.Sprintf("delete response still reports the key: %+v", del.KV), !del.KV.Exists)
	requireNoError(t, "history after the delete", store.assertEqualHistory(ctx, key, []HistoryEntry{
		{ModRevision: 1, Version: 1, Value: []byte("v1")},
		{ModRevision: 2, Version: 2, Value: []byte("v2")},
		{ModRevision: 3, Version: 3, Value: []byte("v4")},
		{ModRevision: 4, Version: 4, Deleted: true},
	}))

	after, revision, err := store.Range(ctx, key)
	requireNoError(t, "range after the delete", err)
	requireTrue(t, fmt.Sprintf("range after the delete = exists=%v revision=%d, want false/4", after.Exists, revision), !after.Exists)
	requireEqual(t, "range revision after the delete", revision, int64(4))
}

// casFailedDeleteKeepsRevision checks that deleting a key that is already gone
// fails the compare and, again, does not advance the revision.
func casFailedDeleteKeepsRevision(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	gone, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-7", Key: key, ExpectModRevision: 3, Delete: true})
	requireNoError(t, "delete of a missing key", err)
	requireTrue(t, fmt.Sprintf("delete of a missing key = %+v, want a failed compare", gone), !gone.Succeeded)
	requireEqual(t, "failed delete reported revision", gone.Revision, int64(4))

	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the failed delete", err)
	requireEqual(t, "revision after the failed delete", revision, int64(4))
}

// TestTxnResponseComesFromTheCommittingTransaction proves the response is
// produced by the same transaction that mutated the key: a second writer
// hammers the same key, and every response must still report the revision it
// committed at with the value it wrote. An adapter that read the key back
// after the transaction would report the racer's newer revision.
func TestTxnResponseComesFromTheCommittingTransaction(t *testing.T) {
	cluster, _, store := bootstrap(t, 1)
	key := []byte("/registry/k8e/atomic")

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	racer := NewStore(NewClient(cluster.Endpoints()...))
	var racing atomic.Bool
	racing.Store(true)
	racerErr := make(chan error, 1)
	go func() {
		racerErr <- raceLoop(ctx, racer, key, &racing)
	}()

	const iterations = 50
	for i := 0; i < iterations; i++ {
		assertCommittedResponse(ctx, t, store, key, i)
	}

	racing.Store(false)
	requireNoError(t, "racer", <-racerErr)
}

// assertCommittedResponse runs one read+compare-and-swap iteration of the
// atomicity test and checks that a committing transaction reports the revision
// it committed at, with the value it wrote, in the transaction response
// itself: the read+txn pair costs two HTTP requests, so nothing is read back
// afterwards.
func assertCommittedResponse(ctx context.Context, t *testing.T, store *Store, key []byte, i int) {
	t.Helper()
	requestID := fmt.Sprintf("atomic-%d", i)
	value := []byte(fmt.Sprintf("value-%d", i))

	before := store.Client().Requests()
	kv, revision, err := store.Range(ctx, key)
	requireNoError(t, fmt.Sprintf("iteration %d: read the current revision", i), err)

	res, err := store.TxnCAS(ctx, CASRequest{
		RequestID:         requestID,
		Key:               key,
		ExpectModRevision: kv.ModRevision,
		Value:             value,
	})
	requireNoError(t, fmt.Sprintf("iteration %d: txn", i), err)
	requireEqual(t, fmt.Sprintf("iteration %d: HTTP requests used by the read+txn", i),
		store.Client().Requests()-before, int64(2))

	if !res.Succeeded {
		// The racer won the compare; that is fine, the atomicity assertions
		// below only apply to a committing transaction.
		return
	}
	requireEqual(t, fmt.Sprintf("iteration %d: mod_revision of the response", i), res.KV.ModRevision, res.Revision)
	requireBytes(t, fmt.Sprintf("iteration %d: response value", i), res.KV.Value, value)
	requireTrue(t, fmt.Sprintf("iteration %d: revision %d did not advance past %d", i, res.Revision, revision), res.Revision > revision)
	requireNoError(t, fmt.Sprintf("iteration %d: history of the committed revision", i),
		store.assertHistoryValue(ctx, key, res.Revision, value))
}

// raceLoop keeps bumping the key so that a post-commit read would be visible.
func raceLoop(ctx context.Context, store *Store, key []byte, running *atomic.Bool) error {
	for running.Load() {
		if err := ctx.Err(); err != nil {
			return nil
		}
		kv, _, err := store.Range(ctx, key)
		if err != nil {
			return fmt.Errorf("racer read: %w", err)
		}
		_, err = store.TxnCAS(ctx, CASRequest{
			RequestID:         fmt.Sprintf("racer-%d", time.Now().UnixNano()),
			Key:               key,
			ExpectModRevision: kv.ModRevision,
			Value:             []byte("racer"),
		})
		if err != nil {
			return fmt.Errorf("racer txn: %w", err)
		}
	}
	return nil
}

// TestLostResponseReplayIsExactlyOnce commits a transaction, drops its HTTP
// response after rqlite produced it, and lets the adapter retry with the same
// request id: the retry must recover the original outcome instead of applying
// the write twice.
func TestLostResponseReplayIsExactlyOnce(t *testing.T) {
	_, client, store := bootstrap(t, 1)
	ctx := context.Background()
	key := []byte("/registry/k8e/lost")

	recorder := newRecordingTransport()
	recorder.loseNext(1)
	client.SetTransport(recorder)

	res, err := store.TxnCAS(ctx, CASRequest{RequestID: "lost-1", Key: key, Value: []byte("v1")})
	requireNoError(t, "txn with a lost response", err)
	requireTrue(t, fmt.Sprintf("txn with a lost response = %+v, want it to commit at revision 1", res), res.Succeeded && res.Revision == 1)
	requireTrue(t, "the retry was not recognised as a replay of the recorded request id", res.Deduped)
	requireEqual(t, "dropped responses", recorder.lostResponses(), 1)

	assertExactlyOneWrite(ctx, t, store, key)
	assertReplayIsIdempotent(ctx, t, store, key)
	assertLostWriteIsReadable(ctx, t, store, key)
}

// assertExactlyOneWrite checks the write that lost its response happened once:
// one revision, one history row, one request record, holding what was sent.
func assertExactlyOneWrite(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision", err)
	requireEqual(t, "revision after the lost response", revision, int64(1))

	entries, err := store.History(ctx, key)
	requireNoError(t, "read the history", err)
	requireEqual(t, fmt.Sprintf("history after the lost response, want exactly one entry, got %+v", entries), len(entries), 1)
	requireEqual(t, "history entry revision", entries[0].ModRevision, int64(1))
	requireBytes(t, "history entry value", entries[0].Value, []byte("v1"))

	count, err := store.CountHistory(ctx, key)
	requireNoError(t, "count the history", err)
	requireEqual(t, "history count after the lost response", count, int64(1))

	requests, err := store.CountRequests(ctx)
	requireNoError(t, "count the request records", err)
	requireEqual(t, "request count after the lost response", requests, int64(1))

	outcome, recordRevision, exists, err := store.RequestRecord(ctx, "lost-1")
	requireNoError(t, "read the request record", err)
	requireTrue(t, "the replayed request id is recorded", exists)
	requireEqual(t, "recorded outcome", outcome, "committed")
	requireEqual(t, "recorded revision", recordRevision, int64(1))
}

// assertReplayIsIdempotent checks that a replayed request id does not consume
// a revision and does not add history.
func assertReplayIsIdempotent(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	again, err := store.TxnCAS(ctx, CASRequest{RequestID: "lost-1", Key: key, Value: []byte("v1")})
	requireNoError(t, "explicit replay", err)
	requireTrue(t, fmt.Sprintf("explicit replay = %+v, want a de-duplicated result at revision 1", again), again.Deduped && again.Revision == 1)
	requireTrue(t, "the explicit replay does not report the recorded value", len(again.KV.Value) > 0)

	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the replay", err)
	requireEqual(t, "revision after the replay", revision, int64(1))

	count, err := store.CountHistory(ctx, key)
	requireNoError(t, "count the history after the replay", err)
	requireEqual(t, "history count after the replay", count, int64(1))
}

// assertLostWriteIsReadable checks the linearizable read sees the value whose
// response was lost.
func assertLostWriteIsReadable(ctx context.Context, t *testing.T, store *Store, key []byte) {
	t.Helper()
	kv, revision, err := store.Range(ctx, key)
	requireNoError(t, "read after the lost response", err)
	requireTrue(t, fmt.Sprintf("read after the lost response = %+v, want the entry that was written", kv), kv.Exists)
	requireBytes(t, "value after the lost response", kv.Value, []byte("v1"))
	requireEqual(t, "revision after the lost response", revision, int64(1))
}

// TestLinearizableReadFromEveryNode writes once, then reads through each node
// of a three-node cluster with an explicit linearizable level and checks that
// every node reports the committed revision, not a stale one.
func TestLinearizableReadFromEveryNode(t *testing.T) {
	cluster, _, store := bootstrap(t, 3)
	ctx := context.Background()
	key := []byte("/registry/k8e/linearizable")

	writer, err := store.TxnCAS(ctx, CASRequest{RequestID: "lin-1", Key: key, Value: []byte("value")})
	requireNoError(t, "write", err)
	requireTrue(t, fmt.Sprintf("write = %+v, want it to commit at revision 1", writer), writer.Succeeded && writer.Revision == 1)

	recorder := newRecordingTransport()
	for _, n := range cluster.Nodes() {
		nodeClient := NewClient(n.Endpoint())
		nodeClient.SetTransport(recorder)
		kv, revision, err := NewStore(nodeClient).Range(ctx, key)
		requireNoError(t, fmt.Sprintf("read via %s", n.ID), err)
		requireTrue(t, fmt.Sprintf("read via %s = %+v, want the value written at revision 1", n.ID, kv), kv.Exists && revision == 1)
		requireBytes(t, fmt.Sprintf("value read via %s", n.ID), kv.Value, []byte("value"))
		requireEqual(t, fmt.Sprintf("read requests used by the read via %s", n.ID), nodeClient.Reads(), int64(1))
	}
	for _, url := range recorder.readURLs() {
		requireTrue(t, fmt.Sprintf("read request %s did not ask for a linearizable read", url),
			bytes.Contains([]byte(url), []byte("level=linearizable")))
	}
}

// ack is the acknowledgement of one worker write: the request id, the key it
// created and the revision it committed at.
type ack struct {
	requestID string
	key       []byte
	revision  int64
}

// ackLog is the shared record of the write acknowledgements, safe for the
// parallel workers that produce them.
type ackLog struct {
	mu    sync.Mutex
	acks  map[string]ack
	acked atomic.Int64
}

func newAckLog() *ackLog { return &ackLog{acks: map[string]ack{}} }

func (l *ackLog) record(a ack) {
	l.mu.Lock()
	l.acks[a.requestID] = a
	l.mu.Unlock()
	l.acked.Add(1)
}

func (l *ackLog) snapshot() map[string]ack {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := make(map[string]ack, len(l.acks))
	for id, a := range l.acks {
		out[id] = a
	}
	return out
}

// TestLeaderSwitchKeepsAcknowledgedWrites kills the Raft leader while several
// independent adapter clients write, then proves that every acknowledged
// write survived, that revisions were allocated exactly once, and that the
// killed node catches up after it restarts.
func TestLeaderSwitchKeepsAcknowledgedWrites(t *testing.T) {
	cluster, client, store := bootstrap(t, 3)
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	leader, err := cluster.Leader(ctx)
	requireNoError(t, "find the leader", err)

	const workers = 4
	const perWorker = 20
	const want = workers * perWorker

	acks := newAckLog()
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			if err := writeWorker(ctx, cluster.Endpoints(), w, perWorker, acks); err != nil {
				errs <- err
			}
		}(w)
	}

	// Kill the leader while the workers are mid-flight; wait until a few writes
	// have been acknowledged so the cluster is demonstrably busy.
	requireTrue(t, "only a few writes were acknowledged before the leader switch",
		waitForAcks(acks, 5, 15*time.Second))
	cluster.Kill(leader)
	t.Logf("killed leader %s", leader.ID)

	newLeader, err := cluster.WaitForNewLeader(ctx, leader.ID)
	requireNoError(t, fmt.Sprintf("no new leader after killing %s", leader.ID), err)
	t.Logf("new leader is %s", newLeader.ID)

	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("write after leader switch: %v", err)
	default:
	}

	collected := acks.snapshot()
	requireEqual(t, "acknowledged writes", len(collected), want)
	assertAcknowledgedRevisions(t, collected, want)
	assertAcknowledgedWritesReadable(ctx, t, store, collected)
	assertRestartedNodeCaughtUp(ctx, t, cluster, client, leader, newLeader, collected)
}

// writeWorker writes perWorker distinct keys through its own adapter client and
// records every acknowledgement.
func writeWorker(ctx context.Context, endpoints []string, worker, perWorker int, acks *ackLog) error {
	workerStore := NewStore(NewClient(endpoints...))
	for i := 0; i < perWorker; i++ {
		requestID := fmt.Sprintf("switch-%d-%d", worker, i)
		key := []byte(fmt.Sprintf("/registry/k8e/switch/w%d/%03d", worker, i))
		res, err := workerStore.TxnCAS(ctx, CASRequest{RequestID: requestID, Key: key, Value: []byte(requestID)})
		if err != nil {
			return fmt.Errorf("worker %d write %d: %w", worker, i, err)
		}
		if !res.Succeeded {
			return fmt.Errorf("worker %d write %d did not commit", worker, i)
		}
		acks.record(ack{requestID: requestID, key: key, revision: res.Revision})
	}
	return nil
}

// waitForAcks waits until at least want writes have been acknowledged.
func waitForAcks(acks *ackLog, want int64, timeout time.Duration) bool {
	deadline := time.Now().Add(timeout)
	for acks.acked.Load() < want && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	return acks.acked.Load() >= want
}

// assertAcknowledgedRevisions checks that every acknowledged write owns a
// distinct revision and that, because all keys are created exactly once, the
// acknowledged revisions are exactly 1..want: no revision was lost, skipped or
// handed out twice.
func assertAcknowledgedRevisions(t *testing.T, collected map[string]ack, want int) {
	t.Helper()
	revisions := map[int64]string{}
	for id, a := range collected {
		other, dup := revisions[a.revision]
		requireTrue(t, fmt.Sprintf("revision %d was acknowledged for both %s and %s", a.revision, other, id), !dup)
		requireTrue(t, fmt.Sprintf("%s was acknowledged at revision %d, outside 1..%d", id, a.revision, want),
			a.revision >= 1 && a.revision <= int64(want))
		revisions[a.revision] = id
	}
	for rev := int64(1); rev <= int64(want); rev++ {
		_, ok := revisions[rev]
		requireTrue(t, fmt.Sprintf("revision %d was never acknowledged", rev), ok)
	}
}

// assertAcknowledgedWritesReadable checks that every acknowledged write is
// still readable through a survivor, at the revision it was acknowledged with.
func assertAcknowledgedWritesReadable(ctx context.Context, t *testing.T, store *Store, collected map[string]ack) {
	t.Helper()
	for id, a := range collected {
		kv, revision, err := store.Range(ctx, a.key)
		requireNoError(t, fmt.Sprintf("read back %s", id), err)
		requireTrue(t, fmt.Sprintf("read back %s = exists=%v value=%q mod=%d, want %q@%d",
			id, kv.Exists, kv.Value, kv.ModRevision, a.requestID, a.revision),
			kv.Exists && bytes.Equal(kv.Value, []byte(a.requestID)) && kv.ModRevision == a.revision)
		requireTrue(t, fmt.Sprintf("read back %s reported revision %d, below the acknowledged %d", id, revision, a.revision),
			revision >= a.revision)
	}
}

// assertRestartedNodeCaughtUp restarts the killed node, waits for it to apply
// the new leader's index and reads the first acknowledged write from it.
func assertRestartedNodeCaughtUp(ctx context.Context, t *testing.T, cluster *Cluster, client *Client, killed, newLeader *Node, collected map[string]ack) {
	t.Helper()
	cluster.Restart(killed)
	status, err := client.Status(ctx, newLeader.Endpoint())
	requireNoError(t, "read the status of the new leader", err)
	requireNoError(t, "the restarted node did not catch up", cluster.waitForNodeCatchUp(ctx, killed, status.Store.DBAppliedIndex))

	first := collected["switch-0-0"]
	kv, revision, err := NewStore(NewClient(killed.Endpoint())).Range(ctx, first.key)
	requireNoError(t, "read from the restarted node", err)
	requireTrue(t, fmt.Sprintf("read from the restarted node = exists=%v mod=%d revision=%d, want the acknowledged %q@%d",
		kv.Exists, kv.ModRevision, revision, first.requestID, first.revision),
		kv.Exists && kv.ModRevision == first.revision && revision >= kv.ModRevision)
}

// recordingTransport counts normal traffic and can drop a received response,
// which is how an uncertain write is simulated: rqlite answered, the client
// never saw it.
type recordingTransport struct {
	base http.RoundTripper

	mu       sync.Mutex
	lose     int
	lost     int
	reads    []string
	maxReads int
	count    int64
}

func newRecordingTransport() *recordingTransport {
	return &recordingTransport{base: http.DefaultTransport, maxReads: 64}
}

func (rt *recordingTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.count++
	shouldLose := rt.lose > 0
	if shouldLose {
		rt.lose--
	}
	isRead := req.Method == http.MethodPost && req.URL.Path == "/db/query"
	if isRead && len(rt.reads) < rt.maxReads {
		rt.reads = append(rt.reads, req.URL.String())
	}
	rt.mu.Unlock()

	resp, err := rt.base.RoundTrip(req)
	if err != nil {
		return nil, err
	}
	if shouldLose {
		_ = resp.Body.Close()
		rt.mu.Lock()
		rt.lost++
		rt.mu.Unlock()
		return nil, fmt.Errorf("simulated connection loss after the server responded")
	}
	return resp, nil
}

func (rt *recordingTransport) loseNext(n int) {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	rt.lose = n
}

func (rt *recordingTransport) lostResponses() int {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return rt.lost
}

func (rt *recordingTransport) readURLs() []string {
	rt.mu.Lock()
	defer rt.mu.Unlock()
	return append([]string(nil), rt.reads...)
}

// TestNoQuorumFailsFastWithoutPartialWrite is the failure path of the M0 user
// journey: with quorum lost, an adapter write must fail with a bounded,
// explicit error and must not leave a partial write or consume a revision.
func TestNoQuorumFailsFastWithoutPartialWrite(t *testing.T) {
	cluster, _, store := bootstrap(t, 3)
	ctx := context.Background()
	failedKey := []byte("/registry/k8e/quorum/failed")

	committed, err := store.TxnCAS(ctx, CASRequest{RequestID: "quorum-1", Key: []byte("/registry/k8e/quorum/existing"), Value: []byte("before")})
	requireNoError(t, "initial write", err)
	requireTrue(t, fmt.Sprintf("initial write = %+v, want it to commit at revision 1", committed), committed.Succeeded && committed.Revision == 1)

	leader, err := cluster.Leader(ctx)
	requireNoError(t, "find the leader", err)

	surviving := killAllButOne(cluster, leader)
	t.Logf("killed every node but %s", surviving.ID)

	assertQuorumlessWriteFails(ctx, t, surviving, failedKey)

	restoreCluster(cluster)
	readyCtx, cancelReady := context.WithTimeout(ctx, 60*time.Second)
	defer cancelReady()
	requireNoError(t, "the cluster did not recover", cluster.WaitForReady(readyCtx))

	assertFailedWriteLeftNoTrace(readyCtx, t, store, failedKey)

	after, err := store.TxnCAS(readyCtx, CASRequest{RequestID: "quorum-3", Key: failedKey, Value: []byte("after")})
	requireNoError(t, "write after recovery", err)
	requireTrue(t, fmt.Sprintf("write after recovery = %+v, want revision 2", after), after.Succeeded && after.Revision == 2)
}

// killAllButOne kills every node of the cluster but one and returns the
// survivor: it can neither elect a leader nor reach one.
func killAllButOne(cluster *Cluster, leader *Node) *Node {
	var surviving *Node
	for _, n := range cluster.Nodes() {
		if n.ID == leader.ID {
			continue
		}
		if surviving != nil {
			cluster.Kill(n)
			continue
		}
		surviving = n
	}
	cluster.Kill(leader)
	return surviving
}

// assertQuorumlessWriteFails checks that a write without quorum fails with a
// bounded, explicit error instead of hanging. It fails one of two ways, both
// observed against v10.3.5: rqlite answers 503 "leader not found" once it knows
// there is no leader, or the follower still forwards to the dead leader and the
// request ends in the adapter's own context deadline. Either way the adapter
// must bound its retries and report.
func assertQuorumlessWriteFails(ctx context.Context, t *testing.T, surviving *Node, failedKey []byte) {
	t.Helper()
	shortCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	started := time.Now()
	_, err := NewStore(NewClient(surviving.Endpoint())).TxnCAS(shortCtx, CASRequest{
		RequestID: "quorum-2",
		Key:       failedKey,
		Value:     []byte("never"),
	})
	elapsed := time.Since(started)
	requireTrue(t, "a write without quorum reported success", err != nil)
	requireTrue(t, fmt.Sprintf("the write without quorum returned after %s, want a bounded failure", elapsed),
		elapsed <= 12*time.Second)
	msg := err.Error()
	requireTrue(t, fmt.Sprintf("the write failed with %v, want a no-leader or bounded-deadline failure", err),
		strings.Contains(msg, "leader") || strings.Contains(msg, "deadline exceeded"))
	t.Logf("write without quorum failed after %s as expected: %v", elapsed.Round(time.Millisecond), err)
}

// restoreCluster restarts every node that is not running.
func restoreCluster(cluster *Cluster) {
	for _, n := range cluster.Nodes() {
		if n.cmd == nil {
			cluster.Restart(n)
		}
	}
}

// assertFailedWriteLeftNoTrace checks that the failed write consumed no
// revision, left no data and recorded no request id.
func assertFailedWriteLeftNoTrace(ctx context.Context, t *testing.T, store *Store, failedKey []byte) {
	t.Helper()
	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the failed write", err)
	requireEqual(t, "revision after a failed write", revision, int64(1))

	kv, rangeRevision, err := store.Range(ctx, failedKey)
	requireNoError(t, "read the failed key", err)
	requireTrue(t, fmt.Sprintf("failed write left data behind: exists=%v revision=%d", kv.Exists, rangeRevision),
		!kv.Exists && rangeRevision == 1)

	_, _, exists, err := store.RequestRecord(ctx, "quorum-2")
	requireNoError(t, "read the failed write record", err)
	requireTrue(t, "failed write left a request record behind", !exists)
}

// TestClusterCrashRestartPreservesCommittedData kills every node with SIGKILL
// and restarts them from disk: every acknowledged write must still be there,
// at the same revision, and the revision counter must continue rather than
// restart.
func TestClusterCrashRestartPreservesCommittedData(t *testing.T) {
	cluster, _, store := bootstrap(t, 3)
	ctx := context.Background()

	keys := writeCrashFixture(ctx, t, store)
	killAndRestartCluster(t, cluster)

	restartCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	requireNoError(t, "the cluster did not recover from a crash", cluster.WaitForReady(restartCtx))

	assertPostCrashState(restartCtx, t, store, keys)
}

// writeCrashFixture writes three keys, updates one and deletes another so the
// history has all three shapes before the cluster is killed.
func writeCrashFixture(ctx context.Context, t *testing.T, store *Store) map[string][]byte {
	t.Helper()
	keys := map[string][]byte{}
	for i, name := range []string{"a", "b", "c"} {
		key := []byte("/registry/k8e/crash/" + name)
		keys[name] = key
		res, err := store.TxnCAS(ctx, CASRequest{RequestID: fmt.Sprintf("crash-%d", i), Key: key, Value: []byte(name)})
		requireNoError(t, fmt.Sprintf("write %s", name), err)
		requireTrue(t, fmt.Sprintf("write %s = %+v, want it to commit at revision %d", name, res, i+1),
			res.Succeeded && res.Revision == int64(i+1))
	}
	_, err := store.TxnCAS(ctx, CASRequest{RequestID: "crash-update", Key: keys["a"], ExpectModRevision: 1, Value: []byte("a2")})
	requireNoError(t, "update", err)
	_, err = store.TxnCAS(ctx, CASRequest{RequestID: "crash-delete", Key: keys["c"], ExpectModRevision: 3, Delete: true})
	requireNoError(t, "delete", err)
	return keys
}

// killAndRestartCluster kills every node and starts them again from disk.
func killAndRestartCluster(t *testing.T, cluster *Cluster) {
	t.Helper()
	for _, n := range cluster.Nodes() {
		cluster.Kill(n)
	}
	for _, n := range cluster.Nodes() {
		cluster.Restart(n)
	}
}

// assertPostCrashState checks that everything committed before the crash is
// still readable at the same revision and that the revision counter continues
// instead of restarting.
func assertPostCrashState(ctx context.Context, t *testing.T, store *Store, keys map[string][]byte) {
	t.Helper()
	revision, err := store.MetaRevision(ctx)
	requireNoError(t, "read the revision after the crash restart", err)
	requireEqual(t, "revision after the crash restart", revision, int64(5))

	store.assertKV(ctx, t, keys["a"], []byte("a2"), 1, 4, 2)
	store.assertKV(ctx, t, keys["b"], []byte("b"), 2, 2, 1)

	kv, _, err := store.Range(ctx, keys["c"])
	requireNoError(t, "read the deleted key", err)
	requireTrue(t, fmt.Sprintf("deleted key %q came back: %+v", keys["c"], kv), !kv.Exists)
	requireNoError(t, "history after the crash restart", store.assertEqualHistory(ctx, keys["a"], []HistoryEntry{
		{ModRevision: 1, Version: 1, Value: []byte("a")},
		{ModRevision: 4, Version: 2, Value: []byte("a2")},
	}))

	res, err := store.TxnCAS(ctx, CASRequest{RequestID: "crash-after", Key: []byte("/registry/k8e/crash/d"), Value: []byte("d")})
	requireNoError(t, "write after the crash restart", err)
	requireTrue(t, fmt.Sprintf("write after the crash restart = %+v, want revision 6", res), res.Succeeded && res.Revision == 6)

	replay, err := store.TxnCAS(ctx, CASRequest{RequestID: "crash-0", Key: keys["a"], Value: []byte("a")})
	requireNoError(t, "replay after the crash restart", err)
	requireTrue(t, fmt.Sprintf("replay after the crash restart = %+v, want a de-duplicated result", replay), replay.Deduped)
}

// TestBinaryKeysRangeInByteOrder checks the two properties etcd range scans
// need from the storage layer: arbitrary non-UTF-8 keys and values survive
// byte for byte, and SQLite's BLOB ordering is the bytewise order etcd
// promises. A TEXT encoding of a key would fail both.
func TestBinaryKeysRangeInByteOrder(t *testing.T) {
	_, _, store := bootstrap(t, 1)
	ctx := context.Background()

	all := make([]byte, 256)
	for i := range all {
		all[i] = byte(i)
	}
	keys := [][]byte{
		{0x00, 0xff, 0x80},
		[]byte("a\xc3\x28"),
		{0xff, 0x00},
	}
	// Bytewise: 0x00… < 0x61 ('a') < 0xff…, and never in the order written.
	wantOrder := [][]byte{keys[0], keys[1], keys[2]}

	writeOrderedKeys(ctx, t, store, keys, all)

	kv, _, err := store.Range(ctx, keys[2])
	requireNoError(t, fmt.Sprintf("range %x", keys[2]), err)
	requireBytes(t, fmt.Sprintf("round trip of %x returned the key", keys[2]), kv.Key, keys[2])
	requireBytes(t, fmt.Sprintf("round trip of %x returned the value", keys[2]), kv.Value, all)

	assertBytewiseOrder(ctx, t, store, wantOrder)

	// A prefix of only 0xff cannot be bounded by an end key; the query must
	// still return exactly the keys with that prefix.
	assertPrefixReturnsOnly(ctx, t, store, []byte{0xff}, keys[2])
	assertPrefixReturnsOnly(ctx, t, store, []byte{0x00}, keys[0])
}

// writeOrderedKeys writes every key with the same value.
func writeOrderedKeys(ctx context.Context, t *testing.T, store *Store, keys [][]byte, value []byte) {
	t.Helper()
	for i, key := range keys {
		res, err := store.TxnCAS(ctx, CASRequest{RequestID: fmt.Sprintf("order-%d", i), Key: key, Value: value})
		requireNoError(t, fmt.Sprintf("write key %x", key), err)
		requireTrue(t, fmt.Sprintf("write key %x = %+v, want it to commit", key, res), res.Succeeded)
	}
}

// assertBytewiseOrder checks that an empty prefix ("every key") returns exactly
// the expected keys in byte order.
func assertBytewiseOrder(ctx context.Context, t *testing.T, store *Store, wantOrder [][]byte) {
	t.Helper()
	entries, _, err := store.RangePrefix(ctx, []byte{}, 10)
	requireNoError(t, "range all", err)
	requireEqual(t, "entries returned by the range over every key", len(entries), len(wantOrder))
	for i, entry := range entries {
		requireBytes(t, fmt.Sprintf("entry %d (bytewise order)", i), entry.Key, wantOrder[i])
	}
}

// assertPrefixReturnsOnly checks that a prefix scan returns exactly one entry,
// the expected key.
func assertPrefixReturnsOnly(ctx context.Context, t *testing.T, store *Store, prefix, want []byte) {
	t.Helper()
	entries, _, err := store.RangePrefix(ctx, prefix, 10)
	requireNoError(t, fmt.Sprintf("range %x prefix", prefix), err)
	requireEqual(t, fmt.Sprintf("entries returned by the %x prefix", prefix), len(entries), 1)
	requireBytes(t, fmt.Sprintf("the entry returned by the %x prefix", prefix), entries[0].Key, want)
}
