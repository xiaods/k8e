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

	// A second bootstrap must not reset the revision counter or the schema row.
	if err := Bootstrap(ctx, client); err != nil {
		t.Fatalf("second bootstrap: %v", err)
	}
	if got, err := store.SchemaVersion(ctx); err != nil || got != SchemaVersion {
		t.Fatalf("schema version = %d (%v), want %d", got, err, SchemaVersion)
	}
	if got, err := store.MetaRevision(ctx); err != nil || got != 0 {
		t.Fatalf("revision after bootstrap = %d (%v), want 0", got, err)
	}

	// Keys and values must round-trip byte for byte, including 0x00, 0xff and
	// 0x80 — the reason blobs are sent as byte arrays and read with blob_array.
	key := []byte{0x2f, 0x00, 0xff, 0x80, 0x01}
	value := []byte{0x00, 0x00, 0xff, 0x80}
	res, err := store.TxnCAS(ctx, CASRequest{RequestID: "binary-1", Key: key, Value: value})
	if err != nil {
		t.Fatalf("create binary key: %v", err)
	}
	if !res.Succeeded || res.Revision != 1 {
		t.Fatalf("create binary key: succeeded=%v revision=%d, want true/1", res.Succeeded, res.Revision)
	}
	if !bytes.Equal(res.KV.Key, key) || !bytes.Equal(res.KV.Value, value) {
		t.Fatalf("txn response key/value = %x/%x, want %x/%x", res.KV.Key, res.KV.Value, key, value)
	}

	kv, revision, err := store.Range(ctx, key)
	if err != nil {
		t.Fatalf("range binary key: %v", err)
	}
	if !kv.Exists || !bytes.Equal(kv.Key, key) || !bytes.Equal(kv.Value, value) {
		t.Fatalf("range key/value = %v %x/%x, want %x/%x", kv.Exists, kv.Key, kv.Value, key, value)
	}
	if revision != 1 {
		t.Fatalf("range header revision = %d, want 1", revision)
	}

	missing, revision, err := store.Range(ctx, []byte{0x2f, 0x00, 0xfe})
	if err != nil {
		t.Fatalf("range missing key: %v", err)
	}
	if missing.Exists {
		t.Fatalf("range of a missing key reported an entry: %+v", missing)
	}
	if revision != 1 {
		t.Fatalf("range header revision for missing key = %d, want 1", revision)
	}

	if err := store.assertOrderedPrefix(ctx, []byte{0x2f}, [][]byte{key}); err != nil {
		t.Fatal(err)
	}
}

func TestTxnCompareBranchesAndRevision(t *testing.T) {
	_, _, store := bootstrap(t, 1)
	ctx := context.Background()
	key := []byte("/registry/sandbox-matrix/token/abc")

	// Create: the compare is `mod_revision == 0` and the key is absent.
	create, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-1", Key: key, Value: []byte("v1")})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if !create.Succeeded || create.Revision != 1 {
		t.Fatalf("create: succeeded=%v revision=%d, want true/1", create.Succeeded, create.Revision)
	}
	if create.KV.CreateRevision != 1 || create.KV.ModRevision != 1 || create.KV.Version != 1 {
		t.Fatalf("create kv = %+v, want create=mod=version=1", create.KV)
	}

	// Update with a matching compare commits at the next revision and bumps
	// only the version; create_revision is preserved.
	update, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-2", Key: key, ExpectModRevision: 1, Value: []byte("v2")})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if !update.Succeeded || update.Revision != 2 {
		t.Fatalf("update: succeeded=%v revision=%d, want true/2", update.Succeeded, update.Revision)
	}
	if update.KV.CreateRevision != 1 || update.KV.ModRevision != 2 || update.KV.Version != 2 {
		t.Fatalf("update kv = %+v, want create=1 mod=2 version=2", update.KV)
	}

	// A stale compare fails, changes nothing and — like etcd's
	// storeTxnWrite.End — does not advance the revision.
	stale, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-3", Key: key, ExpectModRevision: 1, Value: []byte("v3")})
	if err != nil {
		t.Fatalf("stale compare: %v", err)
	}
	if stale.Succeeded {
		t.Fatal("stale compare reported success")
	}
	if stale.Revision != 2 {
		t.Fatalf("stale compare reported revision %d, want the unchanged revision 2", stale.Revision)
	}
	if stale.KV.ModRevision != 2 || string(stale.KV.Value) != "v2" {
		t.Fatalf("stale compare response kv = %+v, want the current v2@2", stale.KV)
	}
	if got, err := store.MetaRevision(ctx); err != nil || got != 2 {
		t.Fatalf("revision after failed compare = %d (%v), want 2", got, err)
	}
	if err := store.assertEqualHistory(ctx, key, []HistoryEntry{
		{ModRevision: 1, Version: 1, Value: []byte("v1")},
		{ModRevision: 2, Version: 2, Value: []byte("v2")},
	}); err != nil {
		t.Fatal(err)
	}
	if outcome, revision, exists, err := store.RequestRecord(ctx, "tx-3"); err != nil || !exists || outcome != "cas_failed" || revision != 2 {
		t.Fatalf("failed compare record = %q rev=%d exists=%v (%v), want cas_failed/2/true", outcome, revision, exists, err)
	}

	// A create against an existing key fails the same way.
	dup, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-4", Key: key, Value: []byte("v3")})
	if err != nil {
		t.Fatalf("duplicate create: %v", err)
	}
	if dup.Succeeded || dup.Revision != 2 {
		t.Fatalf("duplicate create: succeeded=%v revision=%d, want false/2", dup.Succeeded, dup.Revision)
	}

	// No revision was consumed by the two failed compares: the next write
	// takes revision 3 and the history stays gap-free.
	next, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-5", Key: key, ExpectModRevision: 2, Value: []byte("v4")})
	if err != nil {
		t.Fatalf("update after failed compares: %v", err)
	}
	if !next.Succeeded || next.Revision != 3 {
		t.Fatalf("update after failed compares: succeeded=%v revision=%d, want true/3", next.Succeeded, next.Revision)
	}

	// Delete writes a tombstone at the next revision and removes the key.
	del, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-6", Key: key, ExpectModRevision: 3, Delete: true})
	if err != nil {
		t.Fatalf("delete: %v", err)
	}
	if !del.Succeeded || del.Revision != 4 {
		t.Fatalf("delete: succeeded=%v revision=%d, want true/4", del.Succeeded, del.Revision)
	}
	if del.KV.Exists {
		t.Fatalf("delete response still reports the key: %+v", del.KV)
	}
	if err := store.assertEqualHistory(ctx, key, []HistoryEntry{
		{ModRevision: 1, Version: 1, Value: []byte("v1")},
		{ModRevision: 2, Version: 2, Value: []byte("v2")},
		{ModRevision: 3, Version: 3, Value: []byte("v4")},
		{ModRevision: 4, Version: 4, Deleted: true},
	}); err != nil {
		t.Fatal(err)
	}
	after, revision, err := store.Range(ctx, key)
	if err != nil {
		t.Fatalf("range after delete: %v", err)
	}
	if after.Exists || revision != 4 {
		t.Fatalf("range after delete = exists=%v revision=%d, want false/4", after.Exists, revision)
	}

	// Deleting a key that is already gone fails the compare and, again, does
	// not advance the revision.
	gone, err := store.TxnCAS(ctx, CASRequest{RequestID: "tx-7", Key: key, ExpectModRevision: 3, Delete: true})
	if err != nil {
		t.Fatalf("delete of a missing key: %v", err)
	}
	if gone.Succeeded || gone.Revision != 4 {
		t.Fatalf("delete of a missing key: succeeded=%v revision=%d, want false/4", gone.Succeeded, gone.Revision)
	}
	if got, err := store.MetaRevision(ctx); err != nil || got != 4 {
		t.Fatalf("revision after failed delete = %d (%v), want 4", got, err)
	}
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
		requestID := fmt.Sprintf("atomic-%d", i)
		value := []byte(fmt.Sprintf("value-%d", i))

		before := store.Client().Requests()
		kv, revision, err := store.Range(ctx, key)
		if err != nil {
			t.Fatalf("iteration %d: read current revision: %v", i, err)
		}
		res, err := store.TxnCAS(ctx, CASRequest{
			RequestID:         requestID,
			Key:               key,
			ExpectModRevision: kv.ModRevision,
			Value:             value,
		})
		if err != nil {
			t.Fatalf("iteration %d: txn: %v", i, err)
		}
		if requests := store.Client().Requests() - before; requests != 2 {
			t.Fatalf("iteration %d: the read+txn used %d HTTP requests, want 2 (one per operation)", i, requests)
		}
		if !res.Succeeded {
			// The racer won the compare; that is fine, the atomicity
			// assertions below only apply to a committing transaction.
			continue
		}
		if res.KV.ModRevision != res.Revision {
			t.Fatalf("iteration %d: response carries key@%d but the transaction committed at revision %d",
				i, res.KV.ModRevision, res.Revision)
		}
		if !bytes.Equal(res.KV.Value, value) {
			t.Fatalf("iteration %d: response value %q, want %q", i, res.KV.Value, value)
		}
		if res.Revision <= revision {
			t.Fatalf("iteration %d: revision %d did not advance past %d", i, res.Revision, revision)
		}
		entries, err := store.History(ctx, key)
		if err != nil {
			t.Fatalf("iteration %d: history: %v", i, err)
		}
		// The racer may have written newer revisions by now, so look for the
		// revision this transaction committed rather than the tail.
		found := false
		for _, entry := range entries {
			if entry.ModRevision == res.Revision {
				found = true
				if !bytes.Equal(entry.Value, value) {
					t.Fatalf("iteration %d: history for revision %d holds %q, want %q", i, res.Revision, entry.Value, value)
				}
			}
		}
		if !found {
			t.Fatalf("iteration %d: no history row for the committed revision %d", i, res.Revision)
		}
	}

	racing.Store(false)
	if err := <-racerErr; err != nil {
		t.Fatalf("racer: %v", err)
	}
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
	if err != nil {
		t.Fatalf("txn with a lost response: %v", err)
	}
	if !res.Succeeded || res.Revision != 1 {
		t.Fatalf("txn with a lost response: succeeded=%v revision=%d, want true/1", res.Succeeded, res.Revision)
	}
	if !res.Deduped {
		t.Fatal("the retry was not recognised as a replay of the recorded request id")
	}
	if lost := recorder.lostResponses(); lost != 1 {
		t.Fatalf("dropped %d responses, want exactly 1", lost)
	}

	// Exactly one write happened: one revision, one history row, one request
	// record, and the value is the one we sent.
	if got, err := store.MetaRevision(ctx); err != nil || got != 1 {
		t.Fatalf("revision = %d (%v), want 1", got, err)
	}
	if entries, err := store.History(ctx, key); err != nil || len(entries) != 1 {
		t.Fatalf("history = %+v (%v), want exactly one entry", entries, err)
	} else if string(entries[0].Value) != "v1" || entries[0].ModRevision != 1 {
		t.Fatalf("history entry = %+v, want rev 1 value v1", entries[0])
	}
	if count, err := store.CountHistory(ctx, key); err != nil || count != 1 {
		t.Fatalf("history count = %d (%v), want 1", count, err)
	}
	if count, err := store.CountRequests(ctx); err != nil || count != 1 {
		t.Fatalf("request count = %d (%v), want 1", count, err)
	}
	if outcome, revision, exists, err := store.RequestRecord(ctx, "lost-1"); err != nil || !exists || outcome != "committed" || revision != 1 {
		t.Fatalf("request record = %q rev=%d exists=%v (%v), want committed/1/true", outcome, revision, exists, err)
	}

	// A replayed request id is idempotent even after other writes: it must not
	// consume a revision or add history.
	again, err := store.TxnCAS(ctx, CASRequest{RequestID: "lost-1", Key: key, Value: []byte("v1")})
	if err != nil {
		t.Fatalf("explicit replay: %v", err)
	}
	if !again.Deduped || again.Revision != 1 || len(again.KV.Value) == 0 {
		t.Fatalf("explicit replay = %+v, want deduped at revision 1", again)
	}
	if got, err := store.MetaRevision(ctx); err != nil || got != 1 {
		t.Fatalf("revision after replay = %d (%v), want 1", got, err)
	}
	if count, err := store.CountHistory(ctx, key); err != nil || count != 1 {
		t.Fatalf("history count after replay = %d (%v), want 1", count, err)
	}

	// The linearizable read sees the committed value.
	kv, revision, err := store.Range(ctx, key)
	if err != nil {
		t.Fatalf("read after lost response: %v", err)
	}
	if !kv.Exists || string(kv.Value) != "v1" || revision != 1 {
		t.Fatalf("read after lost response = exists=%v value=%q revision=%d, want true/v1/1", kv.Exists, kv.Value, revision)
	}
}

// TestLinearizableReadFromEveryNode writes once, then reads through each node
// of a three-node cluster with an explicit linearizable level and checks that
// every node reports the committed revision, not a stale one.
func TestLinearizableReadFromEveryNode(t *testing.T) {
	cluster, client, store := bootstrap(t, 3)
	ctx := context.Background()
	key := []byte("/registry/k8e/linearizable")

	writer, err := store.TxnCAS(ctx, CASRequest{RequestID: "lin-1", Key: key, Value: []byte("value")})
	if err != nil {
		t.Fatalf("write: %v", err)
	}
	if !writer.Succeeded || writer.Revision != 1 {
		t.Fatalf("write: succeeded=%v revision=%d, want true/1", writer.Succeeded, writer.Revision)
	}

	if _, withoutLevel := client.Reads(); withoutLevel != 0 {
		t.Fatalf("%d read requests were sent without an explicit consistency level; rqlite's default is weak", withoutLevel)
	}

	recorder := newRecordingTransport()
	for _, n := range cluster.Nodes() {
		nodeClient := NewClient(n.Endpoint())
		nodeClient.SetTransport(recorder)
		kv, revision, err := NewStore(nodeClient).Range(ctx, key)
		if err != nil {
			t.Fatalf("read via %s: %v", n.ID, err)
		}
		if !kv.Exists || string(kv.Value) != "value" || revision != 1 {
			t.Fatalf("read via %s = exists=%v value=%q revision=%d, want true/value/1", n.ID, kv.Exists, kv.Value, revision)
		}
		if total, withoutLevel := nodeClient.Reads(); total != 1 || withoutLevel != 0 {
			t.Fatalf("read via %s used %d read requests (%d without a level), want 1 (0)", n.ID, total, withoutLevel)
		}
	}
	for _, url := range recorder.readURLs() {
		if !bytes.Contains([]byte(url), []byte("level=linearizable")) {
			t.Fatalf("read request %s did not ask for a linearizable read", url)
		}
	}
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
	if err != nil {
		t.Fatalf("find leader: %v", err)
	}

	const workers = 4
	const perWorker = 20

	type ack struct {
		worker    int
		index     int
		requestID string
		key       []byte
		revision  int64
		deduped   bool
	}
	var (
		mu    sync.Mutex
		acks  = map[string]ack{}
		acked atomic.Int64
		errs  = make(chan error, workers)
		wg    sync.WaitGroup
	)

	for w := 0; w < workers; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			workerStore := NewStore(NewClient(cluster.Endpoints()...))
			for i := 0; i < perWorker; i++ {
				key := []byte(fmt.Sprintf("/registry/k8e/switch/w%d/%03d", w, i))
				requestID := fmt.Sprintf("switch-%d-%d", w, i)
				res, err := workerStore.TxnCAS(ctx, CASRequest{RequestID: requestID, Key: key, Value: []byte(requestID)})
				if err != nil {
					errs <- fmt.Errorf("worker %d write %d: %w", w, i, err)
					return
				}
				if !res.Succeeded {
					errs <- fmt.Errorf("worker %d write %d did not commit", w, i)
					return
				}
				mu.Lock()
				acks[requestID] = ack{worker: w, index: i, requestID: requestID, key: key, revision: res.Revision, deduped: res.Deduped}
				mu.Unlock()
				acked.Add(1)
			}
		}(w)
	}

	// Kill the leader while the workers are mid-flight; wait until at least a
	// few writes have been acknowledged so the cluster is demonstrably busy.
	deadline := time.Now().Add(15 * time.Second)
	for acked.Load() < 5 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if acked.Load() < 5 {
		t.Fatalf("only %d writes were acknowledged before the leader switch", acked.Load())
	}
	cluster.Kill(leader)
	t.Logf("killed leader %s", leader.ID)

	newLeader, err := cluster.WaitForNewLeader(ctx, leader.ID)
	if err != nil {
		t.Fatalf("no new leader after killing %s: %v", leader.ID, err)
	}
	t.Logf("new leader is %s", newLeader.ID)

	wg.Wait()
	select {
	case err := <-errs:
		t.Fatalf("write after leader switch: %v", err)
	default:
	}

	mu.Lock()
	collected := make(map[string]ack, len(acks))
	for id, a := range acks {
		collected[id] = a
	}
	mu.Unlock()

	want := workers * perWorker
	if len(collected) != want {
		t.Fatalf("acknowledged %d writes, want %d", len(collected), want)
	}

	// Every acknowledged write owns a distinct revision and, because all keys
	// are created exactly once, the acknowledged revisions are exactly
	// 1..want: no revision was lost, skipped or handed out twice.
	revisions := map[int64]string{}
	for id, a := range collected {
		if other, dup := revisions[a.revision]; dup {
			t.Fatalf("revision %d was acknowledged for both %s and %s", a.revision, other, id)
		}
		if a.revision < 1 || a.revision > int64(want) {
			t.Fatalf("%s was acknowledged at revision %d, outside 1..%d", id, a.revision, want)
		}
		revisions[a.revision] = id
	}
	for rev := int64(1); rev <= int64(want); rev++ {
		if _, ok := revisions[rev]; !ok {
			t.Fatalf("revision %d was never acknowledged", rev)
		}
	}

	// Every acknowledged write is still readable, at the revision it was
	// acknowledged with, through a survivor.
	for id, a := range collected {
		kv, revision, err := store.Range(ctx, a.key)
		if err != nil {
			t.Fatalf("read back %s: %v", id, err)
		}
		if !kv.Exists || string(kv.Value) != a.requestID || kv.ModRevision != a.revision {
			t.Fatalf("read back %s = exists=%v value=%q mod=%d, want %q@%d",
				id, kv.Exists, kv.Value, kv.ModRevision, a.requestID, a.revision)
		}
		if revision < a.revision {
			t.Fatalf("read back %s reported revision %d, below the acknowledged %d", id, revision, a.revision)
		}
	}

	// Restart the killed node and let it catch up from the new leader.
	cluster.Restart(leader)
	status, err := client.Status(ctx, newLeader.Endpoint())
	if err != nil {
		t.Fatalf("status of new leader: %v", err)
	}
	if err := cluster.waitForNodeCatchUp(ctx, leader, status.Store.DBAppliedIndex); err != nil {
		t.Fatalf("restarted node: %v", err)
	}

	// A read from the restarted node returns the acknowledged data.
	restartedStore := NewStore(NewClient(leader.Endpoint()))
	kv, revision, err := restartedStore.Range(ctx, collected["switch-0-0"].key)
	if err != nil {
		t.Fatalf("read from restarted node: %v", err)
	}
	if !kv.Exists || kv.ModRevision != collected["switch-0-0"].revision || revision < kv.ModRevision {
		t.Fatalf("read from restarted node = exists=%v mod=%d revision=%d, want %d",
			kv.Exists, kv.ModRevision, revision, collected["switch-0-0"].revision)
	}
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

	committed, err := store.TxnCAS(ctx, CASRequest{RequestID: "quorum-1", Key: []byte("/registry/k8e/quorum/existing"), Value: []byte("before")})
	if err != nil || !committed.Succeeded || committed.Revision != 1 {
		t.Fatalf("initial write: %+v (%v)", committed, err)
	}

	leader, err := cluster.Leader(ctx)
	if err != nil {
		t.Fatalf("find leader: %v", err)
	}

	// Leave exactly one node running: it can neither elect a leader nor reach
	// one, so every write must fail without committing.
	var surviving *Node
	for _, n := range cluster.Nodes() {
		if n.ID == leader.ID {
			continue
		}
		if surviving == nil {
			surviving = n
			continue
		}
		cluster.Kill(n)
	}
	cluster.Kill(leader)
	t.Logf("killed every node but %s", surviving.ID)

	// A quorum-less write fails one of two ways, both bounded and both
	// observed against v10.3.5: rqlite answers 503 "leader not found" once it
	// knows there is no leader, or the follower still forwards to the dead
	// leader and the request ends in the adapter's own context deadline.
	// Either way the adapter must bound its retries and report, not hang.
	shortCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	failedKey := []byte("/registry/k8e/quorum/failed")
	started := time.Now()
	_, err = NewStore(NewClient(surviving.Endpoint())).TxnCAS(shortCtx, CASRequest{
		RequestID: "quorum-2",
		Key:       failedKey,
		Value:     []byte("never"),
	})
	elapsed := time.Since(started)
	if err == nil {
		t.Fatal("a write without quorum reported success")
	}
	if elapsed > 12*time.Second {
		t.Fatalf("the write without quorum returned after %s, want a bounded failure", elapsed)
	}
	msg := err.Error()
	if !strings.Contains(msg, "leader") && !strings.Contains(msg, "deadline exceeded") {
		t.Fatalf("the write failed with %v, want a no-leader or bounded-deadline failure", err)
	}
	t.Logf("write without quorum failed after %s as expected: %v", elapsed.Round(time.Millisecond), err)

	// Restore quorum and confirm nothing from the failed write exists.
	for _, n := range cluster.Nodes() {
		if n.cmd == nil {
			cluster.Restart(n)
		}
	}
	readyCtx, cancelReady := context.WithTimeout(ctx, 60*time.Second)
	defer cancelReady()
	if err := cluster.WaitForReady(readyCtx); err != nil {
		t.Fatalf("cluster did not recover: %v", err)
	}

	if got, err := store.MetaRevision(readyCtx); err != nil || got != 1 {
		t.Fatalf("revision after a failed write = %d (%v), want the unchanged 1", got, err)
	}
	kv, revision, err := store.Range(readyCtx, failedKey)
	if err != nil {
		t.Fatalf("read the failed key: %v", err)
	}
	if kv.Exists || revision != 1 {
		t.Fatalf("failed write left data behind: exists=%v revision=%d", kv.Exists, revision)
	}
	if _, _, exists, err := store.RequestRecord(readyCtx, "quorum-2"); err != nil || exists {
		t.Fatalf("failed write left a request record behind: exists=%v (%v)", exists, err)
	}

	// The cluster is usable again and continues from the same revision.
	after, err := store.TxnCAS(readyCtx, CASRequest{RequestID: "quorum-3", Key: failedKey, Value: []byte("after")})
	if err != nil || !after.Succeeded || after.Revision != 2 {
		t.Fatalf("write after recovery: %+v (%v), want revision 2", after, err)
	}
}

// TestClusterCrashRestartPreservesCommittedData kills every node with SIGKILL
// and restarts them from disk: every acknowledged write must still be there,
// at the same revision, and the revision counter must continue rather than
// restart.
func TestClusterCrashRestartPreservesCommittedData(t *testing.T) {
	cluster, _, store := bootstrap(t, 3)
	ctx := context.Background()

	keys := map[string][]byte{}
	for i, name := range []string{"a", "b", "c"} {
		key := []byte("/registry/k8e/crash/" + name)
		keys[name] = key
		res, err := store.TxnCAS(ctx, CASRequest{
			RequestID: fmt.Sprintf("crash-%d", i),
			Key:       key,
			Value:     []byte(name),
		})
		if err != nil || !res.Succeeded || res.Revision != int64(i+1) {
			t.Fatalf("write %s: %+v (%v)", name, res, err)
		}
	}
	// Update one key and delete another so the history has all three shapes.
	if _, err := store.TxnCAS(ctx, CASRequest{RequestID: "crash-update", Key: keys["a"], ExpectModRevision: 1, Value: []byte("a2")}); err != nil {
		t.Fatalf("update: %v", err)
	}
	if _, err := store.TxnCAS(ctx, CASRequest{RequestID: "crash-delete", Key: keys["c"], ExpectModRevision: 3, Delete: true}); err != nil {
		t.Fatalf("delete: %v", err)
	}

	for _, n := range cluster.Nodes() {
		cluster.Kill(n)
	}

	restartCtx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	for _, n := range cluster.Nodes() {
		cluster.Restart(n)
	}
	if err := cluster.WaitForReady(restartCtx); err != nil {
		t.Fatalf("cluster did not recover from a crash: %v", err)
	}

	if got, err := store.MetaRevision(restartCtx); err != nil || got != 5 {
		t.Fatalf("revision after crash restart = %d (%v), want 5", got, err)
	}
	store.assertKV(t, restartCtx, keys["a"], []byte("a2"), 1, 4, 2)
	store.assertKV(t, restartCtx, keys["b"], []byte("b"), 2, 2, 1)
	if kv, _, err := store.Range(restartCtx, keys["c"]); err != nil || kv.Exists {
		t.Fatalf("deleted key %q came back: %+v (%v)", keys["c"], kv, err)
	}
	if err := store.assertEqualHistory(restartCtx, keys["a"], []HistoryEntry{
		{ModRevision: 1, Version: 1, Value: []byte("a")},
		{ModRevision: 4, Version: 2, Value: []byte("a2")},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := store.TxnCAS(restartCtx, CASRequest{RequestID: "crash-after", Key: []byte("/registry/k8e/crash/d"), Value: []byte("d")})
	if err != nil || !res.Succeeded || res.Revision != 6 {
		t.Fatalf("write after crash restart: %+v (%v), want revision 6", res, err)
	}

	// A replayed request id from before the crash is still de-duplicated.
	replay, err := store.TxnCAS(restartCtx, CASRequest{RequestID: "crash-0", Key: keys["a"], Value: []byte("a")})
	if err != nil || !replay.Deduped {
		t.Fatalf("replay after crash restart: %+v (%v), want a de-duplicated result", replay, err)
	}
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

	for i, key := range keys {
		res, err := store.TxnCAS(ctx, CASRequest{
			RequestID: fmt.Sprintf("order-%d", i),
			Key:       key,
			Value:     all,
		})
		if err != nil || !res.Succeeded {
			t.Fatalf("write key %x: %+v (%v)", key, res, err)
		}
	}

	kv, _, err := store.Range(ctx, keys[2])
	if err != nil {
		t.Fatalf("range %x: %v", keys[2], err)
	}
	if !bytes.Equal(kv.Key, keys[2]) || !bytes.Equal(kv.Value, all) {
		t.Fatalf("round trip of %x gave key %x and a %d-byte value", keys[2], kv.Key, len(kv.Value))
	}

	// An empty prefix means "every key": unbounded end, ordered by key bytes.
	entries, _, err := store.RangePrefix(ctx, []byte{}, 10)
	if err != nil {
		t.Fatalf("range all: %v", err)
	}
	if len(entries) != len(keys) {
		t.Fatalf("range all returned %d entries, want %d", len(entries), len(keys))
	}
	for i, entry := range entries {
		if !bytes.Equal(entry.Key, wantOrder[i]) {
			t.Fatalf("entry %d is %x, want %x (bytewise order)", i, entry.Key, wantOrder[i])
		}
	}

	// A prefix of only 0xff cannot be bounded by an end key; the query must
	// still return exactly the keys with that prefix.
	entries, _, err = store.RangePrefix(ctx, []byte{0xff}, 10)
	if err != nil {
		t.Fatalf("range 0xff prefix: %v", err)
	}
	if len(entries) != 1 || !bytes.Equal(entries[0].Key, keys[2]) {
		t.Fatalf("0xff prefix returned %d entries (%x), want just %x", len(entries), keyList(entries), keys[2])
	}

	entries, _, err = store.RangePrefix(ctx, []byte{0x00}, 10)
	if err != nil {
		t.Fatalf("range 0x00 prefix: %v", err)
	}
	if len(entries) != 1 || !bytes.Equal(entries[0].Key, keys[0]) {
		t.Fatalf("0x00 prefix returned %d entries (%x), want just %x", len(entries), keyList(entries), keys[0])
	}
}

func keyList(entries []KV) [][]byte {
	out := make([][]byte, 0, len(entries))
	for _, e := range entries {
		out = append(out, e.Key)
	}
	return out
}
