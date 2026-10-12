package rqlitecompat

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// The differential contract suite runs one scripted clientv3 scenario against
// two backends — the rqlite compatibility layer and the embedded etcd this
// repository ships today — and compares every observation the user can see:
// results, errors, revisions, event order and the shapes of txn responses.
//
// Revisions are compared relative to a per-run baseline (`base`), because the
// two backends start their revision counter at different values (etcd's first
// write is revision 2, the layer's is revision 1). Everything else is compared
// literally.
const contractPrefix = "m1contract/"

// contractObs is a single named observation.
type contractObs struct {
	Step  string
	Value string
}

// contractRun records observations for one backend.
type contractRun struct {
	t    *testing.T
	ctx  context.Context
	cli  *clientv3.Client
	base int64
	obs  []contractObs
}

func (c *contractRun) record(step, format string, args ...any) {
	c.obs = append(c.obs, contractObs{Step: step, Value: fmt.Sprintf(format, args...)})
}

// rel normalizes an absolute revision against the run baseline. A
// non-positive value is an accessor sentinel (a failed call has no revision)
// and stays as it is.
func (c *contractRun) rel(rev int64) int64 {
	if rev <= 0 {
		return rev
	}
	return rev - c.base
}

// abs converts a baseline-relative revision back to the backend's revision.
func (c *contractRun) abs(rev int64) int64 { return c.base + rev }

// errText renders an error the way a user sees it: the gRPC code plus the
// etcd message.
func errText(err error) string {
	if err == nil {
		return "ok"
	}
	st, ok := status.FromError(err)
	if !ok {
		return "err(" + err.Error() + ")"
	}
	return fmt.Sprintf("%s(%s)", st.Code(), st.Message())
}

// kvsText renders key-values the way a client observes them. MVCC revisions
// are relative to the run's baseline revision because etcd's first write is
// revision 2 and the layer's is revision 1; every other field is literal.
func (r *contractRun) kvsText(kvs []*mvccpb.KeyValue) string {
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		parts = append(parts, fmt.Sprintf("%s=%s@c%d/m%d/v%d/l%d", kv.Key, kv.Value,
			r.rel(kv.CreateRevision), r.rel(kv.ModRevision), kv.Version, kv.Lease))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// runContractScenario drives one backend through the scripted scenario: it fixes
// the run baseline and then walks the phases in the order a client would, so both
// backends record the same sequence of observations.
func runContractScenario(t *testing.T, cli *clientv3.Client) *contractRun {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	t.Cleanup(cancel)
	run := &contractRun{t: t, ctx: ctx, cli: cli}

	base, err := cli.Get(ctx, contractPrefix)
	if err != nil {
		t.Fatalf("baseline get: %v", err)
	}
	run.base = base.Header.Revision

	firstRev := run.kvAndBinaryValues()
	run.ranges()
	run.transactions()
	run.deletes(firstRev)
	run.errorShapes()
	run.leases()
	run.watchStreams()
	run.compaction()
	run.leaseExpiry()
	run.maintenance()

	return run
}

// kvAndBinaryValues walks the KV revision, version and historical-read
// steps plus the binary-safe key/value round-trip, and returns the revision
// of the scenario's first write, which the delete phase compares against.
func (c *contractRun) kvAndBinaryValues() int64 {
	ctx, t, cli := c.ctx, c.t, c.cli
	a, b := contractPrefix+"a", contractPrefix+"b"
	binKey := contractPrefix + "bin\x00\xff"
	binVal := string([]byte{0x00, 0x01, 0xfe, 0xff})
	// --- KV revisions, versions and historical reads -----------------------
	put, err := cli.Put(ctx, a, "1")
	c.record("put-a-1", "rev=%d err=%s", c.rel(putRev(put)), errText(err))
	firstRev := putRev(put)
	get, err := cli.Get(ctx, a)
	c.record("get-a", "count=%d more=%v kvs=%s err=%s", getCount(get), getMore(get), c.kvsText(getKvs(get)), errText(err))

	put, err = cli.Put(ctx, a, "2", clientv3.WithPrevKV())
	c.record("put-a-2", "rev=%d prev=%s err=%s", c.rel(putRev(put)), c.kvsText(putPrev(put)), errText(err))

	get, err = cli.Get(ctx, a, clientv3.WithRev(firstRev))
	c.record("get-a-@hist", "count=%d kvs=%s err=%s", getCount(get), c.kvsText(getKvs(get)), errText(err))

	put, err = cli.Put(ctx, a, "", clientv3.WithIgnoreValue())
	c.record("put-a-ignore-value", "rev=%d err=%s", c.rel(putRev(put)), errText(err))
	get, _ = cli.Get(ctx, a)
	c.record("get-a-after-ignore-value", "count=%d value=%s", getCount(get), c.kvsText(getKvs(get)))

	_, err = cli.Put(ctx, a, "not-allowed", clientv3.WithIgnoreValue())
	c.record("err-ignore-value-provided", "%s", errText(err))

	put, err = cli.Put(ctx, contractPrefix+"ignore-lease-missing", "", clientv3.WithIgnoreLease())
	c.record("err-ignore-lease-missing-key", "rev=%d err=%s", c.rel(putRev(put)), errText(err))

	leaseForIgnore, err := cli.Grant(ctx, 200)
	c.record("lease-grant-for-ignore", "err=%s", errText(err))
	_, err = cli.Put(ctx, contractPrefix+"ignore-lease-provided", "", clientv3.WithIgnoreLease(),
		clientv3.WithLease(grantID(leaseForIgnore)))
	c.record("err-ignore-lease-provided", "%s", errText(err))
	if _, err := cli.Lease.Revoke(ctx, grantID(leaseForIgnore)); err != nil {
		t.Fatalf("revoke ignore lease: %v", err)
	}

	// --- binary keys and values -------------------------------------------
	put, err = cli.Put(ctx, binKey, binVal)
	c.record("put-binary", "rev=%d err=%s", c.rel(putRev(put)), errText(err))
	get, err = cli.Get(ctx, binKey)
	c.record("get-binary", "count=%d value=%q err=%s", getCount(get), valueOf(getKvs(get)), errText(err))

	put, err = cli.Put(ctx, b, "")
	c.record("put-empty-value", "rev=%d err=%s", c.rel(putRev(put)), errText(err))
	get, err = cli.Get(ctx, b)
	c.record("get-empty-value", "count=%d value=%q err=%s", getCount(get), valueOf(getKvs(get)), errText(err))

	return firstRev
}

// ranges walks prefix ranges with limits, sorts, counts and keys_only.
func (c *contractRun) ranges() {
	ctx, t, cli := c.ctx, c.t, c.cli
	var (
		err error
		get *clientv3.GetResponse
	)
	// --- ranges, limits, sort and counts ----------------------------------
	for _, k := range []string{"r1", "r2", "r3"} {
		if _, err := cli.Put(ctx, contractPrefix+k, "v"+k); err != nil {
			t.Fatalf("put %s: %v", k, err)
		}
	}
	get, err = cli.Get(ctx, contractPrefix+"r", clientv3.WithPrefix(), clientv3.WithLimit(2))
	c.record("range-limit-2", "count=%d more=%v kvs=%s err=%s", getCount(get), getMore(get), c.kvsText(getKvs(get)), errText(err))

	get, err = cli.Get(ctx, contractPrefix+"r", clientv3.WithPrefix(),
		clientv3.WithSort(clientv3.SortByKey, clientv3.SortDescend))
	c.record("range-sort-desc", "count=%d kvs=%s err=%s", getCount(get), c.kvsText(getKvs(get)), errText(err))

	get, err = cli.Get(ctx, contractPrefix+"r", clientv3.WithPrefix(), clientv3.WithCountOnly())
	c.record("range-count-only", "count=%d kvs=%s err=%s", getCount(get), c.kvsText(getKvs(get)), errText(err))

	get, err = cli.Get(ctx, contractPrefix+"r", clientv3.WithPrefix(), clientv3.WithKeysOnly())
	c.record("range-keys-only", "count=%d kvs=%s err=%s", getCount(get), c.kvsText(getKvs(get)), errText(err))

	get, err = cli.Get(ctx, contractPrefix+"r", clientv3.WithPrefix(), clientv3.WithLimit(1), clientv3.WithCountOnly())
	c.record("range-limit-1-count-only", "count=%d more=%v err=%s", getCount(get), getMore(get), errText(err))
}

// transactions walks txn success, failure, prev_kv and delete responses.
func (c *contractRun) transactions() {
	ctx, cli := c.ctx, c.cli
	b := contractPrefix + "b"
	// --- transactions ------------------------------------------------------
	snap, _ := cli.Get(ctx, contractPrefix)
	txnRev := c.rel(getRev(snap))
	txn, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(b), "=", 1)).
		Then(clientv3.OpPut(b, "txn-then"), clientv3.OpGet(b)).
		Else(clientv3.OpPut(b, "txn-else")).
		Commit()
	c.record("txn-success", "succeeded=%v rev=%d responses=%s err=%s",
		txnSucceeded(txn), c.rel(txnRev2(txn)), c.txnResponsesText(txn), errText(err))
	c.record("txn-success-delta", "delta=%d", c.rel(txnRev2(txn))-txnRev)

	txn, err = cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(b), "=", 99)).
		Then(clientv3.OpPut(b, "no")).
		Else(clientv3.OpPut(b, "txn-else")).
		Commit()
	c.record("txn-failure", "succeeded=%v responses=%s err=%s", txnSucceeded(txn), c.txnResponsesText(txn), errText(err))

	txn, err = cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Value(b), "=", "txn-else")).
		Then(clientv3.OpPut(b, "txn-chain", clientv3.WithPrevKV()), clientv3.OpDelete(b, clientv3.WithPrevKV())).
		Commit()
	c.record("txn-prev-and-delete", "succeeded=%v rev=%d responses=%s err=%s",
		txnSucceeded(txn), c.rel(txnRev2(txn)), c.txnResponsesText(txn), errText(err))
}

// deletes walks deleting a missing key and a present one, comparing both
// revisions against firstRev.
func (c *contractRun) deletes(firstRev int64) {
	ctx, t, cli := c.ctx, c.t, c.cli
	// --- deletes -----------------------------------------------------------
	del, err := cli.Delete(ctx, contractPrefix+"deleted", clientv3.WithPrevKV())
	c.record("delete-missing", "deleted=%d rev-delta=%d err=%s", delCount(del), c.rel(delRev(del))-c.rel(firstRev), errText(err))

	_, err = cli.Put(ctx, contractPrefix+"deleted", "gone")
	requireNoError(t, "put deleted key", err)
	del, err = cli.Delete(ctx, contractPrefix+"deleted", clientv3.WithPrevKV())
	c.record("delete-present", "deleted=%d prev=%s err=%s", delCount(del), c.kvsText(delPrev(del)), errText(err))
}

// errorShapes records the errors a client can see: an empty key, a future
// revision, too many ops and duplicate keys in one branch.
func (c *contractRun) errorShapes() {
	ctx, cli := c.ctx, c.cli
	var err error
	// --- errors ------------------------------------------------------------
	_, err = cli.Get(ctx, "")
	c.record("err-empty-key", "%s", errText(err))

	_, err = cli.Get(ctx, contractPrefix, clientv3.WithRev(c.abs(1000)))
	c.record("err-future-revision", "%s", errText(err))

	tooMany := make([]clientv3.Op, 0, 130)
	for i := 0; i < 130; i++ {
		tooMany = append(tooMany, clientv3.OpPut(fmt.Sprintf("%sdup-%d", contractPrefix, i), "x"))
	}
	_, err = cli.Txn(ctx).Then(tooMany...).Commit()
	c.record("err-too-many-ops", "%s", errText(err))

	_, err = cli.Txn(ctx).
		Then(clientv3.OpPut(contractPrefix+"twice", "1"), clientv3.OpPut(contractPrefix+"twice", "2")).
		Commit()
	c.record("err-duplicate-key", "%s", errText(err))
}

// leases walks grant, ttl, keepalive, list, revoke and the attached keys.
func (c *contractRun) leases() {
	ctx, cli := c.ctx, c.cli
	var (
		err error
		get *clientv3.GetResponse
	)
	// --- leases ------------------------------------------------------------
	grant, err := cli.Grant(ctx, 100)
	c.record("lease-grant", "err=%s ttl=%d", errText(err), grantTTL(grant))
	leaseID := grantID(grant)

	_, err = cli.Lease.Revoke(ctx, 99999999)
	c.record("lease-revoke-missing", "%s", errText(err))

	_, err = cli.Put(ctx, contractPrefix+"leased", "v", clientv3.WithLease(leaseID))
	c.record("put-with-lease", "%s", errText(err))

	ttl, err := cli.TimeToLive(ctx, leaseID, clientv3.WithAttachedKeys())
	c.record("lease-ttl", "granted-ttl=%d ttl-in-range=%v keys=%d err=%s",
		ttlGranted(ttl), ttlInRange(ttl), len(ttlKeys(ttl)), errText(err))

	ka, err := cli.KeepAliveOnce(ctx, leaseID)
	c.record("lease-keepalive", "ttl-in-range=%v err=%s", kaTTLInRange(ka), errText(err))

	leases, err := cli.Leases(ctx)
	c.record("lease-list", "contains=%v err=%s", containsLease(leaseStatuses(leases), leaseID), errText(err))

	_, err = cli.Put(ctx, contractPrefix+"bad-lease", "v", clientv3.WithLease(99999999))
	c.record("err-put-missing-lease", "%s", errText(err))

	_, err = cli.Lease.Revoke(ctx, leaseID)
	c.record("lease-revoke", "err=%s", errText(err))
	get, err = cli.Get(ctx, contractPrefix+"leased")
	c.record("leased-key-gone", "count=%d err=%s", getCount(get), errText(err))
}

// watchStreams records the event order of a key watcher with prev_kv and of
// a prefix watcher that must filter a key outside its range.
func (c *contractRun) watchStreams() {
	ctx, t, cli := c.ctx, c.t, c.cli
	// --- watch -------------------------------------------------------------
	wk := contractPrefix + "watched"
	wch := cli.Watch(ctx, wk, clientv3.WithPrevKV())
	time.Sleep(300 * time.Millisecond)
	if _, err := cli.Put(ctx, wk, "w1"); err != nil {
		t.Fatalf("put watched 1: %v", err)
	}
	if _, err := cli.Put(ctx, wk, "w2"); err != nil {
		t.Fatalf("put watched 2: %v", err)
	}
	if _, err := cli.Delete(ctx, wk); err != nil {
		t.Fatalf("delete watched: %v", err)
	}
	c.record("watch-events", "%s", watchText(t, wch, 3, c))

	wch = cli.Watch(ctx, contractPrefix+"r", clientv3.WithPrefix())
	time.Sleep(300 * time.Millisecond)
	if _, err := cli.Put(ctx, contractPrefix+"r4", "v4"); err != nil {
		t.Fatalf("put r4: %v", err)
	}
	if _, err := cli.Put(ctx, contractPrefix+"other", "noise"); err != nil {
		t.Fatalf("put noise: %v", err)
	}
	c.record("watch-prefix-filter", "%s", watchText(t, wch, 1, c))
}

// compaction walks the watermark, the surviving revision at it and the
// errors below and at the watermark.
func (c *contractRun) compaction() {
	ctx, cli := c.ctx, c.cli
	a := contractPrefix + "a"
	var (
		err error
		get *clientv3.GetResponse
	)
	// --- compaction --------------------------------------------------------
	get, _ = cli.Get(ctx, contractPrefix)
	compactRev := getRev(get)
	_, err = cli.Compact(ctx, compactRev)
	c.record("compact", "%s", errText(err))
	get, err = cli.Get(ctx, a, clientv3.WithRev(compactRev))
	c.record("get-at-compact-rev", "count=%d kvs=%s err=%s", getCount(get), c.kvsText(getKvs(get)), errText(err))
	_, err = cli.Get(ctx, a, clientv3.WithRev(compactRev-1))
	c.record("err-compacted", "%s", errText(err))
	_, err = cli.Compact(ctx, compactRev-1)
	c.record("err-compact-older", "%s", errText(err))
}

// leaseExpiry waits until the reaper removes the key of a one-second lease.
func (c *contractRun) leaseExpiry() {
	ctx, t, cli := c.ctx, c.t, c.cli
	var (
		err error
		get *clientv3.GetResponse
	)
	// --- lease expiry ------------------------------------------------------
	expiring, err := cli.Grant(ctx, 1)
	c.record("lease-grant-expiring", "err=%s", errText(err))
	if _, err := cli.Put(ctx, contractPrefix+"expiring", "v", clientv3.WithLease(grantID(expiring))); err != nil {
		t.Fatalf("put expiring: %v", err)
	}
	expired := false
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		get, err = cli.Get(ctx, contractPrefix+"expiring")
		if err != nil {
			t.Fatalf("get expiring: %v", err)
		}
		if getCount(get) == 0 {
			expired = true
			break
		}
		time.Sleep(100 * time.Millisecond)
	}
	c.record("lease-expiry-removes-key", "expired=%v", expired)
}

// maintenance records the status and member-list surfaces.
func (c *contractRun) maintenance() {
	ctx, cli := c.ctx, c.cli
	// --- maintenance and cluster surfaces ----------------------------------
	st, err := cli.Maintenance.Status(ctx, cli.Endpoints()[0])
	if err != nil {
		c.record("maintenance-status", "%s", errText(err))
	} else {
		c.record("maintenance-status", "version=%s leader-set=%v raftTerm-set=%v",
			st.Version, st.Leader != 0, st.RaftTerm != 0)
	}

	members, err := cli.MemberList(ctx)
	c.record("member-list", "members=%d err=%s", memberCount(members), errText(err))
}

// Nil-safe accessors: a differential step must record the error of a failed
// call, not panic on the nil response.
func putRev(p *clientv3.PutResponse) int64 {
	if p == nil || p.Header == nil {
		return -1
	}
	return p.Header.Revision
}

func putPrev(p *clientv3.PutResponse) []*mvccpb.KeyValue {
	if p == nil || p.PrevKv == nil {
		return nil
	}
	return []*mvccpb.KeyValue{p.PrevKv}
}

func getCount(g *clientv3.GetResponse) int64 {
	if g == nil {
		return -1
	}
	return g.Count
}

func getMore(g *clientv3.GetResponse) bool { return g != nil && g.More }

func getKvs(g *clientv3.GetResponse) []*mvccpb.KeyValue {
	if g == nil {
		return nil
	}
	return g.Kvs
}

func getRev(g *clientv3.GetResponse) int64 {
	if g == nil || g.Header == nil {
		return -1
	}
	return g.Header.Revision
}

func delCount(d *clientv3.DeleteResponse) int64 {
	if d == nil {
		return -1
	}
	return d.Deleted
}

func delPrev(d *clientv3.DeleteResponse) []*mvccpb.KeyValue {
	if d == nil {
		return nil
	}
	return d.PrevKvs
}

func delRev(d *clientv3.DeleteResponse) int64 {
	if d == nil || d.Header == nil {
		return -1
	}
	return d.Header.Revision
}

func txnSucceeded(x *clientv3.TxnResponse) bool { return x != nil && x.Succeeded }

func txnRev2(x *clientv3.TxnResponse) int64 {
	if x == nil || x.Header == nil {
		return -1
	}
	return x.Header.Revision
}

func grantID(g *clientv3.LeaseGrantResponse) clientv3.LeaseID {
	if g == nil {
		return 0
	}
	return g.ID
}

func grantTTL(g *clientv3.LeaseGrantResponse) int64 {
	if g == nil {
		return -1
	}
	return g.TTL
}

func ttlGranted(t *clientv3.LeaseTimeToLiveResponse) int64 {
	if t == nil {
		return -1
	}
	return t.GrantedTTL
}

func ttlInRange(t *clientv3.LeaseTimeToLiveResponse) bool {
	return t != nil && t.TTL > 0 && t.TTL <= 100
}

func ttlKeys(t *clientv3.LeaseTimeToLiveResponse) [][]byte {
	if t == nil {
		return nil
	}
	return t.Keys
}

func kaTTLInRange(k *clientv3.LeaseKeepAliveResponse) bool {
	return k != nil && k.TTL > 0 && k.TTL <= 100
}

func leaseStatuses(l *clientv3.LeaseLeasesResponse) []clientv3.LeaseStatus {
	if l == nil {
		return nil
	}
	return l.Leases
}

func memberCount(m *clientv3.MemberListResponse) int {
	if m == nil {
		return -1
	}
	return len(m.Members)
}

func valueOf(kvs []*mvccpb.KeyValue) string {
	if len(kvs) == 0 {
		return "<none>"
	}
	return string(kvs[0].Value)
}

func containsLease(leases []clientv3.LeaseStatus, id clientv3.LeaseID) bool {
	for _, l := range leases {
		if l.ID == id {
			return true
		}
	}
	return false
}

// txnResponsesText renders a txn response the way a client observes it.
func (r *contractRun) txnResponsesText(txn *clientv3.TxnResponse) string {
	if txn == nil {
		return "<nil>"
	}
	parts := make([]string, 0, len(txn.Responses))
	for _, op := range txn.Responses {
		switch {
		case op.GetResponseRange() != nil:
			rr := op.GetResponseRange()
			parts = append(parts, fmt.Sprintf("range(count=%d,kvs=%s)", rr.Count, r.kvsText(rr.Kvs)))
		case op.GetResponsePut() != nil:
			parts = append(parts, fmt.Sprintf("put(prev=%s)", r.kvsText(putPrev(&clientv3.PutResponse{PrevKv: op.GetResponsePut().PrevKv}))))
		case op.GetResponseDeleteRange() != nil:
			dr := op.GetResponseDeleteRange()
			parts = append(parts, fmt.Sprintf("delete(deleted=%d,prev=%s)", dr.Deleted, r.kvsText(dr.PrevKvs)))
		case op.GetResponseTxn() != nil:
			parts = append(parts, "nested")
		default:
			parts = append(parts, "unknown")
		}
	}
	return "[" + strings.Join(parts, " ") + "]"
}

// watchText reads count watch events with a hard deadline and renders them in
// the order the user receives them.
func watchText(t *testing.T, ch clientv3.WatchChan, count int, run *contractRun) string {
	t.Helper()
	var parts []string
	got := 0
	deadline := time.After(20 * time.Second)
	for got < count {
		select {
		case resp, ok := <-ch:
			if !ok {
				return "closed after " + strings.Join(parts, " ")
			}
			if resp.Err() != nil {
				parts = append(parts, "err="+errText(resp.Err()))
				return strings.Join(parts, " ")
			}
			if resp.Canceled {
				parts = append(parts, "canceled")
				return strings.Join(parts, " ")
			}
			for _, ev := range resp.Events {
				parts = append(parts, fmt.Sprintf("%s:%s=%s@m%d prev=%s",
					ev.Type, ev.Kv.Key, ev.Kv.Value, run.rel(ev.Kv.ModRevision), run.singleKV(ev.PrevKv)))
				got++
			}
		case <-deadline:
			t.Fatalf("watch timed out after %d of %d events: %v", got, count, parts)
		}
	}
	return strings.Join(parts, " ")
}

func (r *contractRun) singleKV(kv *mvccpb.KeyValue) string {
	if kv == nil {
		return "<none>"
	}
	return r.kvsText([]*mvccpb.KeyValue{kv})
}

// TestM1DifferentialContracts runs the same clientv3 scenario against the
// rqlite compatibility layer and against the embedded etcd this repository
// ships, and fails on the first observation that differs.
func TestM1DifferentialContracts(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 1)
	srv := startCompat(t, cluster, nil)

	rqlite := runContractScenario(t, dialClientV3(t, srv.addr))
	etcd := runContractScenario(t, startEmbeddedEtcdClient(t))
	compareContractRuns(t, rqlite, etcd)
}

// minContractObservations guards against a scenario that silently stops
// observing anything and therefore "passes" without comparing behaviour.
const minContractObservations = 40

// compareContractRuns fails on the first observation the two backends do not
// agree on.
func compareContractRuns(t *testing.T, rqlite, etcd *contractRun) {
	t.Helper()
	if len(rqlite.obs) < minContractObservations || len(etcd.obs) < minContractObservations {
		t.Fatalf("scenario observed too little: rqlite=%d etcd=%d, want at least %d",
			len(rqlite.obs), len(etcd.obs), minContractObservations)
	}
	if len(rqlite.obs) != len(etcd.obs) {
		t.Fatalf("observation count differs: rqlite=%d etcd=%d", len(rqlite.obs), len(etcd.obs))
	}
	for i := range rqlite.obs {
		r, e := rqlite.obs[i], etcd.obs[i]
		if r.Step != e.Step {
			t.Fatalf("observation %d step differs: rqlite=%q etcd=%q", i, r.Step, e.Step)
		}
		if r.Value != e.Value {
			t.Errorf("step %q differs:\n  rqlite: %s\n  etcd:   %s", r.Step, r.Value, e.Value)
		}
	}
}

// TestM1DocumentedDeviations pins the shapes the layer deliberately does not
// implement: each one must fail with an explicit codes.Unimplemented error
// instead of returning a silently wrong result (KIP-29).
func TestM1DocumentedDeviations(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 1)
	srv := startCompat(t, cluster, nil)
	cli := dialClientV3(t, srv.addr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	if _, err := cli.Put(ctx, contractPrefix+"dev", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}

	// A compare over a range is rejected instead of being approximated.
	_, err := cli.Txn(ctx).
		If(clientv3.Compare(clientv3.Version(contractPrefix+"dev"), "=", 1).WithPrefix()).
		Then(clientv3.OpPut(contractPrefix+"dev", "2")).
		Commit()
	requireCode(t, "compare over a range", err, codes.Unimplemented)

	// A range op inside a txn with an explicit revision is rejected.
	_, err = cli.Txn(ctx).Then(clientv3.OpGet(contractPrefix+"dev", clientv3.WithRev(1))).Commit()
	requireCode(t, "txn range op with revision", err, codes.Unimplemented)

	// A physical compaction request is accepted (it is the only compaction
	// rqlite offers) but a maintenance RPC the layer cannot serve fails.
	_, err = cli.Maintenance.Defragment(ctx, cli.Endpoints()[0])
	requireCode(t, "defragment", err, codes.Unimplemented)
	_, err = cli.Maintenance.HashKV(ctx, cli.Endpoints()[0], 0)
	requireCode(t, "hash kv", err, codes.Unimplemented)
	_, err = cli.MemberRemove(ctx, 1)
	requireCode(t, "member remove", err, codes.Unimplemented)
	_, err = cli.MemberAdd(ctx, []string{"http://127.0.0.1:1"})
	requireCode(t, "member add", err, codes.Unimplemented)
}
