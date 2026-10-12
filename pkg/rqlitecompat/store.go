package rqlitecompat

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/binary"
	"sort"
	"strings"
	"time"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
)

// KV is the layer's own key-value view. Binary keys and values are []byte, so
// arbitrary (non-UTF-8) bytes round-trip through SQLite BLOBs.
type KV struct {
	Key            []byte
	Value          []byte
	CreateRevision int64
	ModRevision    int64
	Version        int64
	Lease          int64
}

// EventType mirrors mvccpb.Event_EventType.
type EventType int

const (
	// EventPut is emitted for a put (create or update).
	EventPut EventType = iota
	// EventDelete is emitted for a delete or a lease expiry.
	EventDelete
)

// Event is one durable MVCC event. Revision is the store revision the event
// belongs to; for a delete KV carries only the key, as in mvccpb.
type Event struct {
	Type     EventType
	Revision int64
	KV       *KV
	PrevKV   *KV
}

// Store is the MVCC view over the rqlite schema. It has no in-memory state:
// every operation is one rqlite request, so any process attached to the same
// rqlite cluster observes the same history.
type Store struct {
	client *Client
	owner  string
	now    func() time.Time
}

// NewStore returns a store attached to a rqlite client. owner identifies this
// process for lease bookkeeping and diagnostics.
func NewStore(c *Client, owner string) *Store {
	return &Store{client: c, owner: owner, now: time.Now}
}

// Client exposes the underlying rqlite client (metrics, readiness).
func (s *Store) Client() *Client { return s.client }

func (s *Store) nowUnix() int64 { return s.now().Unix() }

func (s *Store) readInt(ctx context.Context, name string) (int64, error) {
	resp, err := s.client.Read(ctx, Statement{
		SQL:    `SELECT value FROM k8e_compat_meta WHERE name = :name`,
		Params: map[string]any{"name": name},
	})
	if err != nil {
		return 0, err
	}
	if len(resp.Results) == 0 || len(resp.Results[0].Values) == 0 {
		return 0, nil
	}
	r, err := resp.Results[0].row(0)
	if err != nil {
		return 0, err
	}
	return r.int("value"), nil
}

// Revision is the current store revision.
func (s *Store) Revision(ctx context.Context) (int64, error) {
	return s.readInt(ctx, "revision")
}

// CompactRevision is the last compacted revision (0 when never compacted).
func (s *Store) CompactRevision(ctx context.Context) (int64, error) {
	return s.readInt(ctx, "compact_revision")
}

// SortOrder is the requested result ordering.
type SortOrder int

// SortTarget is the field the ordering applies to.
type SortTarget int

// SortTarget values; they mirror etcdserverpb without importing it into the
// storage layer.
const (
	SortByKey SortTarget = iota
	SortByVersion
	SortByCreate
	SortByMod
	SortByValue
)

// SortOrder values.
const (
	SortAscend SortOrder = iota
	SortDescend
)

// RangeOptions is a Range request in the layer's own terms.
type RangeOptions struct {
	Key, RangeEnd                        []byte
	Limit                                int64
	Revision                             int64
	SortOrder                            SortOrder
	SortTarget                           SortTarget
	KeysOnly, CountOnly                  bool
	MinModRevision, MaxModRevision       int64
	MinCreateRevision, MaxCreateRevision int64
}

// RangeResult is the layer's Range response.
type RangeResult struct {
	KVs      []KV
	More     bool
	Count    int64
	Revision int64
}

// revisionExpr is the SQL expression for the current store revision. It is
// stable for a whole request because a mutating request bumps the revision in
// its own final statement.
const revisionExpr = `(SELECT value FROM k8e_compat_meta WHERE name = 'revision')`

// nextRevisionExpr is the revision a standalone write produces.
const nextRevisionExpr = revisionExpr + ` + 1`

// readRevisionSQL reads the committed revision for a response header.
const readRevisionSQL = `SELECT value FROM k8e_compat_meta WHERE name = 'revision'`

// bumpSQL advances the store revision exactly when this request wrote history
// at revision+1. A read-only or no-op request therefore leaves the revision
// untouched, matching etcd.
const bumpSQL = `UPDATE k8e_compat_meta SET value = value + 1 WHERE name = 'revision' ` +
	`AND EXISTS(SELECT 1 FROM kv_history WHERE mod_revision = (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1)`

// bumpTxnSQL is bumpSQL for a txn: the revision was pinned in txn_ctx when the
// branch was chosen, so the check and the write must use txn_ctx.rev.
const bumpTxnSQL = `UPDATE k8e_compat_meta SET value = value + 1 WHERE name = 'revision' ` +
	`AND EXISTS(SELECT 1 FROM kv_history WHERE mod_revision = (SELECT rev FROM txn_ctx WHERE id = 1))`

// Range reads a key or key range, optionally at a historical revision. The
// read uses level=linearizable so it can never observe a stale revision.
func (s *Store) Range(ctx context.Context, req RangeOptions) (*RangeResult, error) {
	current, err := s.Revision(ctx)
	if err != nil {
		return nil, err
	}
	compact, err := s.CompactRevision(ctx)
	if err != nil {
		return nil, err
	}

	rev := req.Revision
	switch {
	case rev > current:
		return nil, rpctypes.ErrGRPCFutureRev
	case rev > 0 && rev < compact:
		return nil, rpctypes.ErrGRPCCompacted
	case rev <= 0:
		rev = current
	}

	stmts, err := rangeStatements(req, rev, current, "")
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Read(ctx, stmts...)
	if err != nil {
		return nil, err
	}
	result, err := decodeRange(resp.Results[0], req)
	if err != nil {
		return nil, err
	}
	result.Revision = current
	return result, nil
}

// rangeStatements builds a single SELECT over either the head of the store
// (rev >= current) or the reconstructed MVCC view at rev. extraWhere is
// appended for txn branch guards.
func rangeStatements(req RangeOptions, rev, current int64, extraWhere string) ([]Statement, error) {
	params := map[string]any{}
	rangeCond := rangeCondition("key", "rk", req.Key, req.RangeEnd, "re", params)
	keyCol, modCol, createCol := "key", "mod_revision", "create_revision"

	var sql string
	if rev >= current {
		sql = `SELECT key, value, create_revision, mod_revision, version, lease FROM kv WHERE ` + rangeCond
	} else {
		keyCol, modCol, createCol = "c.key", "c.mod_revision", "c.create_revision"
		params["rev"] = rev
		// A head row and a history row can describe the same revision right
		// after a write, so the head row is used only when history for that
		// revision was compacted away.
		sql = `WITH candidates AS (
			SELECT key, value, create_revision, mod_revision, version, deleted, lease
			FROM kv_history WHERE mod_revision <= :rev
			UNION ALL
			SELECT key, value, create_revision, mod_revision, version, 0 AS deleted, lease
			FROM kv WHERE mod_revision <= :rev
			AND NOT EXISTS (SELECT 1 FROM kv_history h WHERE h.key = kv.key AND h.mod_revision = kv.mod_revision)
		), latest AS (
			SELECT key, MAX(mod_revision) AS m FROM candidates GROUP BY key
		)
		SELECT c.key, c.value, c.create_revision, c.mod_revision, c.version, c.lease
		FROM candidates c JOIN latest l ON l.key = c.key AND l.m = c.mod_revision
		WHERE c.deleted = 0 AND ` + replaceCols(rangeCond, keyCol)
	}

	if req.MinModRevision > 0 {
		sql += " AND " + modCol + " >= :minmod"
		params["minmod"] = req.MinModRevision
	}
	if req.MaxModRevision > 0 {
		sql += " AND " + modCol + " <= :maxmod"
		params["maxmod"] = req.MaxModRevision
	}
	if req.MinCreateRevision > 0 {
		sql += " AND " + createCol + " >= :mincreate"
		params["mincreate"] = req.MinCreateRevision
	}
	if req.MaxCreateRevision > 0 {
		sql += " AND " + createCol + " <= :maxcreate"
		params["maxcreate"] = req.MaxCreateRevision
	}
	if extraWhere != "" {
		sql += " AND " + extraWhere
	}
	sql += " ORDER BY " + keyCol

	return []Statement{{SQL: sql, Params: params}}, nil
}

// replaceCols qualifies the key column in a range condition with the alias
// used by the historical query. "key" never appears in a param name, so the
// rewrite is unambiguous.
func replaceCols(cond, keyCol string) string {
	return strings.ReplaceAll(cond, "key", keyCol)
}

func decodeRange(res StatementResult, req RangeOptions) (*RangeResult, error) {
	rows, err := res.rows()
	if err != nil {
		return nil, err
	}
	kvs := make([]KV, 0, len(rows))
	for _, r := range rows {
		kv := KV{
			Key:            r.blob("key"),
			Value:          r.blob("value"),
			CreateRevision: r.int("create_revision"),
			ModRevision:    r.int("mod_revision"),
			Version:        r.int("version"),
			Lease:          r.int("lease"),
		}
		if req.KeysOnly {
			kv.Value = nil
		}
		kvs = append(kvs, kv)
	}

	result := &RangeResult{}
	if req.CountOnly {
		result.Count = int64(len(kvs))
		return result, nil
	}

	sortKVs(kvs, req.SortTarget, req.SortOrder)
	// etcd counts every matching key in Count and signals the truncation with
	// More: the unary Range and a txn range op both evaluate the underlying
	// range with withTotalCount (etcdserver/txn/range.go asembleRangeResponse).
	result.Count = int64(len(kvs))
	if req.Limit > 0 && int64(len(kvs)) > req.Limit {
		result.More = true
		kvs = kvs[:req.Limit]
	}
	result.KVs = kvs
	return result, nil
}

func sortKVs(kvs []KV, target SortTarget, order SortOrder) {
	less := func(i, j int) bool { return bytes.Compare(kvs[i].Key, kvs[j].Key) < 0 }
	switch target {
	case SortByVersion:
		less = func(i, j int) bool { return kvs[i].Version < kvs[j].Version }
	case SortByCreate:
		less = func(i, j int) bool { return kvs[i].CreateRevision < kvs[j].CreateRevision }
	case SortByMod:
		less = func(i, j int) bool { return kvs[i].ModRevision < kvs[j].ModRevision }
	case SortByValue:
		less = func(i, j int) bool { return bytes.Compare(kvs[i].Value, kvs[j].Value) < 0 }
	}
	if order == SortDescend {
		base := less
		less = func(i, j int) bool { return base(j, i) }
	}
	sort.SliceStable(kvs, less)
}

// PutOptions is a Put request. IgnoreValue/IgnoreLease keep the current value
// or lease and fail when the key does not exist, as in etcd.
type PutOptions struct {
	Key, Value               []byte
	Lease                    int64
	PrevKv                   bool
	IgnoreValue, IgnoreLease bool
}

// PutResult is a Put response.
type PutResult struct {
	PrevKV   *KV
	Revision int64
}

// Put writes a key. The whole operation — previous-value capture, upsert,
// history, event and revision bump — is one rqlite transaction.
func (s *Store) Put(ctx context.Context, req PutOptions) (*PutResult, error) {
	if err := validateKey(req.Key); err != nil {
		return nil, err
	}
	core, prevIdx := putCore(req, nextRevisionExpr, "")
	resp, err := s.client.Write(ctx, append(core, Statement{SQL: bumpSQL}, Statement{SQL: readRevisionSQL})...)
	if err != nil {
		return nil, err
	}
	if err := leaseCheck(resp, req.Lease); err != nil {
		return nil, err
	}
	if (req.IgnoreValue || req.IgnoreLease) && len(resp.Results[prevIdx].Values) == 0 {
		return nil, rpctypes.ErrGRPCKeyNotFound
	}
	rev, err := readRevisionResult(resp.Results[len(resp.Results)-1])
	if err != nil {
		return nil, err
	}
	prev, err := firstKV(resp.Results[prevIdx])
	if err != nil {
		return nil, err
	}
	return &PutResult{PrevKV: prev, Revision: rev}, nil
}

// putCore builds the statements of a Put up to and including the previous-KV
// select, and reports that select's result index. revExpr lets the same
// builder serve a standalone Put (revision+1) and a Put inside a txn (the
// txn's shared revision); guard restricts the write to a taken txn branch.
func putCore(req PutOptions, revExpr, guard string) ([]Statement, int) {
	params := map[string]any{
		"k":  Blob(req.Key),
		"v":  Blob(emptyBytes(req.Value)),
		"l":  req.Lease,
		"iv": boolInt(req.IgnoreValue),
		"il": boolInt(req.IgnoreLease),
	}
	ignoreGuard := `(:iv = 0 OR EXISTS(SELECT 1 FROM kv WHERE key = :k)) AND (:il = 0 OR EXISTS(SELECT 1 FROM kv WHERE key = :k))`
	leaseGuard := `(:l = 0 OR EXISTS(SELECT 1 FROM leases WHERE id = :l))`
	writeGuard := "(" + ignoreGuard + ") AND (" + leaseGuard + ")"
	if guard != "" {
		writeGuard = "(" + guard + ") AND " + writeGuard
	}

	mutation := "" +
		`INSERT INTO kv(key, value, create_revision, mod_revision, version, lease) ` +
		`SELECT :k, :v, ` + revExpr + `, ` + revExpr + `, 1, :l WHERE ` + writeGuard +
		` ON CONFLICT(key) DO UPDATE SET ` +
		`value = CASE WHEN :iv THEN kv.value ELSE excluded.value END, ` +
		`mod_revision = excluded.mod_revision, ` +
		`version = kv.version + 1, ` +
		`lease = CASE WHEN :il THEN kv.lease ELSE excluded.lease END ` +
		`WHERE ` + writeGuard

	history := "" +
		`INSERT INTO kv_history(key, mod_revision, version, deleted, value, lease, create_revision) ` +
		`SELECT k.key, ` + revExpr + `, k.version, 0, k.value, k.lease, k.create_revision ` +
		`FROM kv k WHERE k.key = :k AND ` + writeGuard

	event := "" +
		`INSERT INTO events(revision, seq, kind, key, value, create_revision, mod_revision, version, lease, ` +
		`prev_value, prev_create_revision, prev_mod_revision, prev_version, prev_lease) ` +
		`SELECT ` + revExpr + `, ` + seqExpr(revExpr, "") + `, 'PUT', k.key, k.value, k.create_revision, k.mod_revision, k.version, k.lease, ` +
		`p.value, COALESCE(p.create_revision, 0), COALESCE(p.mod_revision, 0), COALESCE(p.version, 0), COALESCE(p.lease, 0) ` +
		`FROM kv k LEFT JOIN op_prev p ON p.key = k.key ` +
		`WHERE k.key = :k AND EXISTS(SELECT 1 FROM kv_history WHERE key = :k AND mod_revision = ` + revExpr + `)`

	leaseProbe := `SELECT :l AS lease, EXISTS(SELECT 1 FROM leases WHERE id = :l) AS present`
	prevSelect := `SELECT key, value, create_revision, mod_revision, version, lease FROM op_prev ORDER BY key`

	stmts := []Statement{
		{SQL: `DELETE FROM op_prev`},
		{SQL: `INSERT INTO op_prev(key, value, create_revision, mod_revision, version, lease) SELECT key, value, create_revision, mod_revision, version, lease FROM kv WHERE key = :k`, Params: params},
		{SQL: leaseProbe, Params: params},
		{SQL: mutation, Params: params},
		{SQL: history, Params: params},
		{SQL: event, Params: params},
		{SQL: prevSelect, Params: params},
	}
	return stmts, len(stmts) - 1
}

// DeleteOptions is a DeleteRange request.
type DeleteOptions struct {
	Key, RangeEnd []byte
	PrevKv        bool
}

// DeleteResult is a DeleteRange response.
type DeleteResult struct {
	Deleted  int64
	PrevKVs  []KV
	Revision int64
}

// DeleteRange deletes a key or range in one transaction.
func (s *Store) DeleteRange(ctx context.Context, req DeleteOptions) (*DeleteResult, error) {
	if err := validateKey(req.Key); err != nil {
		return nil, err
	}
	core, prevIdx := deleteCore(req, nextRevisionExpr, "")
	resp, err := s.client.Write(ctx, append(core, Statement{SQL: bumpSQL}, Statement{SQL: readRevisionSQL})...)
	if err != nil {
		return nil, err
	}
	rev, err := readRevisionResult(resp.Results[len(resp.Results)-1])
	if err != nil {
		return nil, err
	}
	prev, err := kvsFromResult(resp.Results[prevIdx])
	if err != nil {
		return nil, err
	}
	return &DeleteResult{Deleted: int64(len(prev)), PrevKVs: prev, Revision: rev}, nil
}

// deleteCore builds the statements of a DeleteRange up to and including the
// previous-KV select, and reports that select's result index.
func deleteCore(req DeleteOptions, revExpr, guard string) ([]Statement, int) {
	params := map[string]any{}
	rangeCond := rangeCondition("key", "rk", req.Key, req.RangeEnd, "re", params)
	guardSQL := ""
	if guard != "" {
		guardSQL = " AND (" + guard + ")"
	}

	capture := `INSERT INTO op_prev(key, value, create_revision, mod_revision, version, lease) ` +
		`SELECT key, value, create_revision, mod_revision, version, lease FROM kv WHERE ` + rangeCond + guardSQL

	history := `INSERT INTO kv_history(key, mod_revision, version, deleted, value, lease, create_revision) ` +
		`SELECT key, ` + revExpr + `, version, 1, NULL, lease, create_revision FROM kv WHERE ` + rangeCond + guardSQL

	event := `INSERT INTO events(revision, seq, kind, key, value, create_revision, mod_revision, version, lease, ` +
		`prev_value, prev_create_revision, prev_mod_revision, prev_version, prev_lease) ` +
		`SELECT ` + revExpr + `, ` + seqExpr(revExpr, "ROW_NUMBER() OVER (ORDER BY key) - 1 + ") + `, 'DELETE', key, NULL, 0, 0, 0, 0, ` +
		`value, create_revision, mod_revision, version, lease ` +
		`FROM kv WHERE ` + rangeCond + guardSQL

	deleteKV := `DELETE FROM kv WHERE ` + rangeCond + guardSQL
	prevSelect := `SELECT key, value, create_revision, mod_revision, version, lease FROM op_prev ORDER BY key`

	stmts := []Statement{
		{SQL: `DELETE FROM op_prev`},
		{SQL: capture, Params: params},
		{SQL: history, Params: params},
		{SQL: event, Params: params},
		{SQL: deleteKV, Params: params},
		{SQL: prevSelect},
	}
	return stmts, len(stmts) - 1
}

// Compact discards history and events at or below rev. The live kv rows are
// never removed: a compacted store still serves the current state.
func (s *Store) Compact(ctx context.Context, rev int64, _ bool) error {
	current, err := s.Revision(ctx)
	if err != nil {
		return err
	}
	compact, err := s.CompactRevision(ctx)
	if err != nil {
		return err
	}
	if rev <= compact {
		return rpctypes.ErrGRPCCompacted
	}
	if rev > current {
		return rpctypes.ErrGRPCFutureRev
	}
	guard := `:rev > (SELECT value FROM k8e_compat_meta WHERE name = 'compact_revision') ` +
		`AND :rev <= (SELECT value FROM k8e_compat_meta WHERE name = 'revision')`
	params := map[string]any{"rev": rev}
	// etcd's compaction keeps, for every key, the latest revision at or below
	// the compacted revision (kvindex.doCompact's `available` set): the state at
	// the compacted revision stays readable, everything older is discarded.
	// Events at or below the compacted revision are dropped; a watcher that has
	// not caught up is canceled by the compact revision it sees.
	dropHistory := `DELETE FROM kv_history WHERE mod_revision < :rev AND ` + guard +
		` AND EXISTS(SELECT 1 FROM kv_history h WHERE h.key = kv_history.key ` +
		`AND h.mod_revision <= :rev AND h.mod_revision > kv_history.mod_revision)`
	_, err = s.client.Write(ctx,
		Statement{SQL: dropHistory, Params: params},
		Statement{SQL: `DELETE FROM events WHERE revision <= :rev AND ` + guard, Params: params},
		Statement{SQL: `UPDATE k8e_compat_meta SET value = :rev WHERE name = 'compact_revision' AND ` + guard, Params: params},
	)
	return err
}

// EventsAfter reads durable events with revision > after, in revision/seq
// order, up to limit rows. more reports whether more rows remain.
func (s *Store) EventsAfter(ctx context.Context, after int64, limit int) ([]Event, bool, error) {
	stmts := []Statement{{
		SQL: `SELECT revision, seq, kind, key, value, create_revision, mod_revision, version, lease, ` +
			`prev_value, prev_create_revision, prev_mod_revision, prev_version, prev_lease ` +
			`FROM events WHERE revision > :after ORDER BY revision, seq LIMIT :limit`,
		Params: map[string]any{"after": after, "limit": limit + 1},
	}}
	resp, err := s.client.Read(ctx, stmts...)
	if err != nil {
		return nil, false, err
	}
	rows, err := resp.Results[0].rows()
	if err != nil {
		return nil, false, err
	}
	more := len(rows) > limit
	if more {
		rows = rows[:limit]
	}
	events := make([]Event, 0, len(rows))
	for _, r := range rows {
		events = append(events, eventFromRow(r))
	}
	return events, more, nil
}

func eventFromRow(r row) Event {
	rev := r.int("revision")
	ev := Event{Revision: rev}
	if r.str("kind") == "DELETE" {
		ev.Type = EventDelete
		// etcd's tombstone records only the key; the watchable store patches in
		// the delete revision so watchers do not skip the event (mvcc
		// watchable_store_txn.End / kvsToEvents).
		ev.KV = &KV{Key: r.blob("key"), ModRevision: rev}
	} else {
		ev.Type = EventPut
		ev.KV = &KV{
			Key:            r.blob("key"),
			Value:          r.blob("value"),
			CreateRevision: r.int("create_revision"),
			ModRevision:    r.int("mod_revision"),
			Version:        r.int("version"),
			Lease:          r.int("lease"),
		}
	}
	if r.blob("prev_value") != nil || r.int("prev_mod_revision") != 0 {
		ev.PrevKV = &KV{
			Key:            r.blob("key"),
			Value:          r.blob("prev_value"),
			CreateRevision: r.int("prev_create_revision"),
			ModRevision:    r.int("prev_mod_revision"),
			Version:        r.int("prev_version"),
			Lease:          r.int("prev_lease"),
		}
	}
	return ev
}

// seqExpr returns the next event sequence number for a revision. prefix lets a
// multi-row insert offset each row inside the same statement.
func seqExpr(revExpr, prefix string) string {
	return prefix + `COALESCE((SELECT MAX(seq) FROM events WHERE revision = ` + revExpr + `), -1) + 1`
}

// rangeCondition renders [key, rangeEnd) for column col. A one-byte zero
// rangeEnd means "all keys >= key"; an empty rangeEnd means an exact key.
func rangeCondition(col, keyParam string, key, rangeEnd []byte, endParam string, params map[string]any) string {
	params[keyParam] = Blob(key)
	if len(rangeEnd) == 0 {
		return col + " = :" + keyParam
	}
	if len(rangeEnd) == 1 && rangeEnd[0] == 0 {
		return col + " >= :" + keyParam
	}
	params[endParam] = Blob(rangeEnd)
	return col + " >= :" + keyParam + " AND " + col + " < :" + endParam
}

func validateKey(key []byte) error {
	if len(key) == 0 {
		return rpctypes.ErrGRPCEmptyKey
	}
	return nil
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// emptyBytes normalizes a nil byte slice to a non-nil empty one so a key or
// value is stored as an empty BLOB (x”) and never as SQL NULL.
func emptyBytes(b []byte) []byte {
	if b == nil {
		return []byte{}
	}
	return b
}

// leaseCheck interprets the lease-probe result (statement 2 of putCore) and
// returns ErrLeaseNotFound when a lease was requested but does not exist.
func leaseCheck(resp Response, lease int64) error {
	if lease == 0 {
		return nil
	}
	if len(resp.Results) < 3 {
		return &Err{Message: "lease probe missing from response"}
	}
	r, err := resp.Results[2].row(0)
	if err != nil {
		return err
	}
	if r.int("present") == 0 {
		return rpctypes.ErrGRPCLeaseNotFound
	}
	return nil
}

func readRevisionResult(res StatementResult) (int64, error) {
	if len(res.Values) == 0 {
		return 0, &Err{Message: "revision read returned no rows"}
	}
	r, err := res.row(0)
	if err != nil {
		return 0, err
	}
	return r.int("value"), nil
}

func firstKV(res StatementResult) (*KV, error) {
	kvs, err := kvsFromResult(res)
	if err != nil || len(kvs) == 0 {
		return nil, err
	}
	return &kvs[0], nil
}

func kvsFromResult(res StatementResult) ([]KV, error) {
	rows, err := res.rows()
	if err != nil {
		return nil, err
	}
	kvs := make([]KV, 0, len(rows))
	for _, r := range rows {
		kvs = append(kvs, KV{
			Key:            r.blob("key"),
			Value:          r.blob("value"),
			CreateRevision: r.int("create_revision"),
			ModRevision:    r.int("mod_revision"),
			Version:        r.int("version"),
			Lease:          r.int("lease"),
		})
	}
	return kvs, nil
}

// NewLeaseID returns a random positive int64 lease id, as etcd does.
func NewLeaseID() (int64, error) {
	var b [8]byte
	if _, err := rand.Read(b[:]); err != nil {
		return 0, err
	}
	return int64(binary.BigEndian.Uint64(b[:]) & 0x7fffffffffffffff), nil
}

func leaseExpiry(now time.Time, ttl int64) int64 { return now.Unix() + ttl }
