package rqlitecompat

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"
)

// KV is a key-value entry as stored by the adapter. Exists is false for a
// missing key (or a tombstone), matching an empty etcd Range response.
type KV struct {
	Key            []byte
	Value          []byte
	CreateRevision int64
	ModRevision    int64
	Version        int64
	Exists         bool
}

// CASRequest is one compare-and-swap transaction. ExpectModRevision is the
// mod_revision the key must currently have; 0 means "the key must not exist"
// (etcd's `Compare(ModRevision(key), "=", 0)`), exactly as
// pkg/etcdstorage creates bootstrap tokens.
type CASRequest struct {
	RequestID         string
	Key               []byte
	ExpectModRevision int64
	Value             []byte
	Delete            bool
}

// CASResult is the transaction outcome. Revision is the etcd response-header
// revision: the revision the transaction committed at, or the unchanged
// current revision when the compare failed (etcd only advances the revision
// when the transaction actually changed state — see
// server/storage/mvcc/kvstore_txn.go, storeTxnWrite.End). Deduped reports
// that this request id had already been applied and the result was recovered
// from the recorded outcome instead of being re-executed.
type CASResult struct {
	Revision  int64
	Succeeded bool
	Deduped   bool
	KV        KV
}

// Store is the prototype adapter: the etcd-shaped operations on top of rqlite.
type Store struct {
	client     *Client
	retryDelay time.Duration
	timeout    time.Duration
}

// NewStore wraps a client. The retry budget bounds every operation so a
// cluster without a leader cannot turn into an unbounded retry loop.
func NewStore(c *Client) *Store {
	return &Store{
		client:     c,
		retryDelay: 100 * time.Millisecond,
		timeout:    30 * time.Second,
	}
}

// Client exposes the underlying rqlite client (request counters, status).
func (s *Store) Client() *Client { return s.client }

// TxnCAS runs the compare, the mutation, the revision allocation, the history
// write and the response read as one rqlite transaction — a single
// /db/request?transaction call carrying one Raft log entry.
func (s *Store) TxnCAS(ctx context.Context, req CASRequest) (CASResult, error) {
	if req.RequestID == "" {
		return CASResult{}, errors.New("rqlitecompat: CASRequest.RequestID is required")
	}
	if len(req.Key) == 0 {
		return CASResult{}, errors.New("rqlitecompat: CASRequest.Key is required")
	}

	kind := "cas_put"
	if req.Delete {
		kind = "cas_delete"
	}
	params := map[string]any{
		"rid": req.RequestID,
		"key": Blob(req.Key),
	}

	stmts := []Statement{
		{SQL: `INSERT INTO applied_requests(request_id, kind, outcome, revision)
		       VALUES(:rid, :kind, 'pending', 0)
		       ON CONFLICT(request_id) DO NOTHING`, Params: withKind(params, kind)},
	}
	stmts = append(stmts, mutationStatements(req, params)...)
	stmts = append(stmts,
		Statement{SQL: historyStatement, Params: params},
		Statement{SQL: recordOutcomeStatement, Params: params},
		Statement{SQL: bumpRevisionStatement, Params: params},
		Statement{SQL: readOutcomeStatement, Params: params},
	)

	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.write(ctx, stmts)
	if err != nil {
		return CASResult{}, err
	}

	// results[0] is the request-identity insert: rows_affected == 0 means the
	// request id was already recorded, so this attempt was a replay.
	deduped := resp.Results[0].RowsAffected == 0
	outcome, err := resp.Results[len(resp.Results)-1].row(0)
	if err != nil {
		return CASResult{}, fmt.Errorf("read txn outcome: %w", err)
	}

	res := CASResult{
		Deduped:   deduped,
		Succeeded: outcome.str("outcome") == "committed",
		Revision:  outcome.int("revision"),
	}
	if res.Revision <= 0 {
		return CASResult{}, fmt.Errorf("rqlitecompat: transaction recorded revision %d", res.Revision)
	}
	res.KV = KV{
		Key:            outcome.bytes("key"),
		Value:          outcome.bytes("value"),
		CreateRevision: outcome.int("create_revision"),
		ModRevision:    outcome.int("mod_revision"),
		Version:        outcome.int("version"),
		Exists:         outcome.bytes("key") != nil,
	}
	return res, nil
}

// Range reads one key at an explicit linearizable level and returns the entry
// together with the store revision from a single SQL statement, so the data
// and the header revision come from one consistent view.
func (s *Store) Range(ctx context.Context, key []byte) (KV, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.client.Read(ctx, Statement{SQL: rangeStatement, Params: map[string]any{"key": Blob(key)}})
	if err != nil {
		return KV{}, 0, err
	}
	row, err := resp.Results[0].row(0)
	if err != nil {
		return KV{}, 0, fmt.Errorf("read key: %w", err)
	}
	kv := KV{
		Key:            row.bytes("key"),
		Value:          row.bytes("value"),
		CreateRevision: row.int("create_revision"),
		ModRevision:    row.int("mod_revision"),
		Version:        row.int("version"),
	}
	kv.Exists = kv.Key != nil
	return kv, row.int("revision"), nil
}

// RangePrefix returns up to limit entries whose key begins with prefix, in
// byte order, together with the store revision. SQLite orders BLOBs with
// memcmp, so the result order is the order etcd's range scans rely on.
func (s *Store) RangePrefix(ctx context.Context, prefix []byte, limit int64) ([]KV, int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	params := map[string]any{"start": Blob(prefix), "limit": limit, "end": nil}
	if end := prefixEnd(prefix); end != nil {
		params["end"] = Blob(end)
	}
	resp, err := s.client.Read(ctx, Statement{
		SQL: `SELECT (SELECT value FROM k8e_compat_meta WHERE name = 'revision') AS revision,
		             key, create_revision, mod_revision, version, value
		      FROM kv
		      WHERE key >= :start AND (:end IS NULL OR key < :end)
		      ORDER BY key
		      LIMIT :limit`,
		Params: params,
	})
	if err != nil {
		return nil, 0, err
	}

	result := resp.Results[0]
	entries := make([]KV, 0, len(result.Values))
	var revision int64
	for i := range result.Values {
		row, err := result.row(i)
		if err != nil {
			return nil, 0, err
		}
		revision = row.int("revision")
		entries = append(entries, KV{
			Key:            row.bytes("key"),
			Value:          row.bytes("value"),
			CreateRevision: row.int("create_revision"),
			ModRevision:    row.int("mod_revision"),
			Version:        row.int("version"),
			Exists:         true,
		})
	}
	if len(entries) == 0 {
		// Still report the current revision for an empty range.
		rev, err := s.MetaRevision(ctx)
		if err != nil {
			return nil, 0, err
		}
		revision = rev
	}
	return entries, revision, nil
}

// prefixEnd returns the smallest key greater than every key with the prefix,
// or nil when the prefix cannot be bounded (all 0xff).
func prefixEnd(prefix []byte) []byte {
	end := make([]byte, len(prefix))
	copy(end, prefix)
	for i := len(end) - 1; i >= 0; i-- {
		if end[i] < 0xff {
			end[i]++
			return end[:i+1]
		}
	}
	return nil
}

// MetaRevision reads the current global revision.
func (s *Store) MetaRevision(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.client.Read(ctx, Statement{SQL: `SELECT value FROM k8e_compat_meta WHERE name = 'revision'`})
	if err != nil {
		return 0, err
	}
	row, err := resp.Results[0].row(0)
	if err != nil {
		return 0, fmt.Errorf("read revision: %w", err)
	}
	return row.int("value"), nil
}

// SchemaVersion reads the recorded schema version.
func (s *Store) SchemaVersion(ctx context.Context) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.client.Read(ctx, Statement{SQL: `SELECT value FROM k8e_compat_meta WHERE name = 'schema_version'`})
	if err != nil {
		return 0, err
	}
	row, err := resp.Results[0].row(0)
	if err != nil {
		return 0, fmt.Errorf("read schema version: %w", err)
	}
	return row.int("value"), nil
}

// History returns every retained revision of a key in revision order.
func (s *Store) History(ctx context.Context, key []byte) ([]HistoryEntry, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.client.Read(ctx, Statement{
		SQL:    `SELECT mod_revision, version, deleted, value FROM kv_history WHERE key = :key ORDER BY mod_revision`,
		Params: map[string]any{"key": Blob(key)},
	})
	if err != nil {
		return nil, err
	}
	entries := make([]HistoryEntry, 0, len(resp.Results[0].Values))
	for i := range resp.Results[0].Values {
		row, err := resp.Results[0].row(i)
		if err != nil {
			return nil, err
		}
		entries = append(entries, HistoryEntry{
			ModRevision: row.int("mod_revision"),
			Version:     row.int("version"),
			Deleted:     row.int("deleted") == 1,
			Value:       row.bytes("value"),
		})
	}
	return entries, nil
}

// HistoryEntry is one retained revision, including tombstones.
type HistoryEntry struct {
	ModRevision int64
	Version     int64
	Deleted     bool
	Value       []byte
}

// RequestRecord returns the recorded outcome of a request id.
func (s *Store) RequestRecord(ctx context.Context, requestID string) (outcome string, revision int64, exists bool, err error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.client.Read(ctx, Statement{
		SQL:    `SELECT outcome, revision FROM applied_requests WHERE request_id = :rid`,
		Params: map[string]any{"rid": requestID},
	})
	if err != nil {
		return "", 0, false, err
	}
	result := resp.Results[0]
	if len(result.Values) == 0 {
		return "", 0, false, nil
	}
	row, err := result.row(0)
	if err != nil {
		return "", 0, false, err
	}
	return row.str("outcome"), row.int("revision"), true, nil
}

// CountHistory returns how many revisions are retained for a key.
func (s *Store) CountHistory(ctx context.Context, key []byte) (int64, error) {
	resp, err := s.readCount(ctx, `SELECT count(*) FROM kv_history WHERE key = :key`, map[string]any{"key": Blob(key)})
	if err != nil {
		return 0, err
	}
	return resp, nil
}

// CountRequests returns how many request ids are recorded.
func (s *Store) CountRequests(ctx context.Context) (int64, error) {
	return s.readCount(ctx, `SELECT count(*) FROM applied_requests`, nil)
}

// CountAllHistory returns how many revisions are retained for all keys.
func (s *Store) CountAllHistory(ctx context.Context) (int64, error) {
	return s.readCount(ctx, `SELECT count(*) FROM kv_history`, nil)
}

func (s *Store) readCount(ctx context.Context, sql string, params map[string]any) (int64, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()

	resp, err := s.client.Read(ctx, Statement{SQL: sql, Params: params})
	if err != nil {
		return 0, err
	}
	row, err := resp.Results[0].row(0)
	if err != nil {
		return 0, err
	}
	for _, v := range row {
		if n, ok := v.(int64); ok {
			return n, nil
		}
	}
	return 0, errors.New("rqlitecompat: count query returned no integer")
}

// write sends the transaction, retrying only transient failures. The statement
// list is identical on every attempt — including the request id — so a retry
// after a lost response is de-duplicated by the applied_requests row instead
// of being applied twice.
func (s *Store) write(ctx context.Context, stmts []Statement) (Response, error) {
	attempts := 0
	var lastErr error
	for {
		attempts++
		resp, err := s.client.Write(ctx, stmts...)
		if err == nil {
			return resp, nil
		}
		var rerr *Err
		if !errors.As(err, &rerr) || !rerr.Transient() {
			return Response{}, err
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return Response{}, fmt.Errorf("rqlitecompat: write failed after %d attempts: %w", attempts, lastErr)
		case <-time.After(s.retryDelay):
		}
	}
}

func withKind(params map[string]any, kind string) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out["kind"] = kind
	return out
}

// pendingRequest is the de-duplication guard: it is true only while this
// request id has not been recorded as applied yet, which is the first attempt.
const pendingRequest = `(SELECT outcome FROM applied_requests WHERE request_id = :rid) = 'pending'`

func mutationStatements(req CASRequest, params map[string]any) []Statement {
	pending := pendingRequest
	if req.Delete {
		return []Statement{
			{SQL: `INSERT INTO kv_history(key, mod_revision, version, deleted, value)
			       SELECT key,
			              (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1,
			              version + 1, 1, NULL
			       FROM kv
			       WHERE key = :key AND mod_revision = :expect AND ` + pending, Params: withExpect(params, req.ExpectModRevision)},
			{SQL: `DELETE FROM kv
			       WHERE key = :key AND mod_revision = :expect AND ` + pending, Params: withExpect(params, req.ExpectModRevision)},
		}
	}
	if req.ExpectModRevision == 0 {
		return []Statement{
			{SQL: `INSERT INTO kv(key, create_revision, mod_revision, version, lease, value)
			       SELECT :key,
			              (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1,
			              (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1,
			              1, 0, :val
			       WHERE ` + pending + ` AND NOT EXISTS (SELECT 1 FROM kv WHERE key = :key)`,
				Params: withValue(params, req.Value)},
		}
	}
	return []Statement{
		{SQL: `UPDATE kv
		       SET value = :val,
		           mod_revision = (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1,
		           version = version + 1
		       WHERE key = :key AND mod_revision = :expect AND ` + pending,
			Params: withValue(withExpect(params, req.ExpectModRevision), req.Value)},
	}
}

// historyStatement retains the revision the transaction just produced. The
// WHERE clause is keyed on the freshly allocated revision and on the request
// still being new, so it fires only when this attempt actually mutated the
// key; a replay writes nothing.
const historyStatement = `INSERT INTO kv_history(key, mod_revision, version, deleted, value)
	SELECT key, mod_revision, version, 0, value FROM kv
	WHERE key = :key
	  AND mod_revision = (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1
	  AND ` + pendingRequest

// mutationApplied is true exactly when this attempt created the history row
// for the revision it is about to hand out — that is, when the mutation
// landed. Every statement that uses it runs before the revision advances, so
// a history row at meta+1 can only be one created by this transaction; a
// failed compare leaves no such row.
const mutationApplied = `EXISTS (
		SELECT 1 FROM kv_history
		WHERE key = :key
		  AND mod_revision = (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1
	)`

// bumpRevisionStatement advances the global revision. It runs after the
// outcome has been recorded, and it is safe against a replay because a
// replayed transaction never writes a history row at the next revision.
const bumpRevisionStatement = `UPDATE k8e_compat_meta SET value = value + 1
	WHERE name = 'revision' AND ` + mutationApplied

// recordOutcomeStatement stores the outcome and the revision this transaction
// committed at. A failed compare keeps the revision untouched, matching etcd's
// storeTxnWrite.End.
const recordOutcomeStatement = `UPDATE applied_requests
	SET revision = CASE WHEN ` + mutationApplied + `
	        THEN (SELECT value FROM k8e_compat_meta WHERE name = 'revision') + 1
	        ELSE (SELECT value FROM k8e_compat_meta WHERE name = 'revision') END,
	    outcome = CASE WHEN ` + mutationApplied + `
	        THEN 'committed' ELSE 'cas_failed' END
	WHERE request_id = :rid AND outcome = 'pending'`

const readOutcomeStatement = `SELECT ap.outcome AS outcome,
	       ap.revision AS revision,
	       ki.key AS key,
	       ki.create_revision AS create_revision,
	       ki.mod_revision AS mod_revision,
	       ki.version AS version,
	       ki.value AS value
	FROM applied_requests AS ap
	LEFT JOIN kv AS ki ON ki.key = :key
	WHERE ap.request_id = :rid`

const rangeStatement = `SELECT (SELECT value FROM k8e_compat_meta WHERE name = 'revision') AS revision,
	       ki.key AS key,
	       ki.create_revision AS create_revision,
	       ki.mod_revision AS mod_revision,
	       ki.version AS version,
	       ki.value AS value
	FROM (SELECT 1 AS one) AS single
	LEFT JOIN kv AS ki ON ki.key = :key`

func withExpect(params map[string]any, expect int64) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out["expect"] = expect
	return out
}

func withValue(params map[string]any, value []byte) map[string]any {
	out := make(map[string]any, len(params)+1)
	for k, v := range params {
		out[k] = v
	}
	out["val"] = Blob(value)
	return out
}

// row decodes row i of a statement result into named values: int64 for
// integers, []byte for BLOBs (rqlite returns them as byte arrays because of
// blob_array) and string for text. Absent values (LEFT JOIN misses) are nil.
func (r StatementResult) row(i int) (rowValues, error) {
	if i >= len(r.Values) {
		return nil, fmt.Errorf("statement returned %d rows, want row %d", len(r.Values), i)
	}
	values := r.Values[i]
	if len(values) != len(r.Columns) {
		return nil, fmt.Errorf("row %d has %d values for %d columns", i, len(values), len(r.Columns))
	}
	out := make(rowValues, len(r.Columns))
	for j, name := range r.Columns {
		kind := ""
		if j < len(r.Types) {
			kind = r.Types[j]
		}
		v, err := decodeValue(kind, values[j])
		if err != nil {
			return nil, fmt.Errorf("column %q: %w", name, err)
		}
		out[name] = v
	}
	return out, nil
}

type rowValues map[string]any

func (r rowValues) int(column string) int64 {
	switch v := r[column].(type) {
	case int64:
		return v
	case nil:
		return 0
	default:
		return 0
	}
}

func (r rowValues) str(column string) string {
	s, _ := r[column].(string)
	return s
}

func (r rowValues) bytes(column string) []byte {
	switch v := r[column].(type) {
	case []byte:
		return v
	case string:
		return []byte(v)
	default:
		return nil
	}
}

func decodeValue(kind string, value any) (any, error) {
	if value == nil {
		return nil, nil
	}
	switch kind {
	case "integer", "real":
		n, ok := value.(json.Number)
		if !ok {
			return nil, fmt.Errorf("expected number, got %T", value)
		}
		return n.Int64()
	case "text":
		s, ok := value.(string)
		if !ok {
			return nil, fmt.Errorf("expected text, got %T", value)
		}
		return s, nil
	case "blob":
		items, ok := value.([]any)
		if !ok {
			return nil, fmt.Errorf("expected byte array for BLOB, got %T", value)
		}
		out := make([]byte, 0, len(items))
		for _, item := range items {
			n, ok := item.(json.Number)
			if !ok {
				return nil, fmt.Errorf("expected byte value, got %T", item)
			}
			b, err := n.Int64()
			if err != nil || b < 0 || b > 255 {
				return nil, fmt.Errorf("byte value out of range: %v", item)
			}
			out = append(out, byte(b))
		}
		return out, nil
	case "":
		// NULL or an expression whose type SQLite did not report.
		return value, nil
	default:
		return value, nil
	}
}
