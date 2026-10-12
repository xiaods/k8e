package rqlitecompat

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// MaxTxnOps is the etcd default for the number of ops in one transaction.
const MaxTxnOps = 128

// txnRevExpr is the revision a txn writes at: current+1, pinned before the
// branch is chosen so every op shares the same revision.
const txnRevExpr = `(SELECT rev FROM txn_ctx WHERE id = 1)`

// CompareTarget is the field a Compare inspects.
type CompareTarget int

// CompareTarget values.
const (
	CompareVersion CompareTarget = iota
	CompareCreate
	CompareMod
	CompareValue
	CompareLease
)

// CompareOp is the comparison operator.
type CompareOp int

// CompareOp values.
const (
	CompareEqual CompareOp = iota
	CompareGreater
	CompareLess
	CompareNotEqual
)

// Compare is one term of a txn's conjunction.
type Compare struct {
	Target   CompareTarget
	Key      []byte
	Op       CompareOp
	Version  int64
	Value    []byte
	RangeEnd []byte
}

// OpKind is the type of a txn op.
type OpKind int

// OpKind values.
const (
	OpRange OpKind = iota
	OpPut
	OpDelete
)

// Op is one txn operation.
type Op struct {
	Kind   OpKind
	Range  *RangeOptions
	Put    *PutOptions
	Delete *DeleteOptions
}

// TxnRequest is the layer's txn input.
type TxnRequest struct {
	Compares []Compare
	Success  []Op
	Failure  []Op
}

// OpResponse is the response for one executed op.
type OpResponse struct {
	Kind   OpKind
	Range  *RangeResult
	Put    *PutResult
	Delete *DeleteResult
}

// TxnResult is the layer's txn response.
type TxnResult struct {
	Succeeded bool
	Revision  int64
	Responses []OpResponse
}

// txnRef records the op that produced a response row and where that row lands
// in the transaction's statement list, so the response can be rebuilt in the
// order the client sent the ops.
type txnRef struct {
	kind    OpKind
	success bool
	op      Op
	result  int
}

// Txn applies compares plus the taken branch's ops inside one rqlite
// transaction. Compares are evaluated as SQL over the pre-txn state, the
// branch is pinned in txn_ctx, and every mutation of the branch shares one
// revision, so a concurrent writer can never interleave between compare and
// write.
func (s *Store) Txn(ctx context.Context, req TxnRequest) (*TxnResult, error) {
	if err := validateTxn(req); err != nil {
		return nil, err
	}
	current, err := s.Revision(ctx)
	if err != nil {
		return nil, err
	}
	stmts, refs, err := buildTxnStatements(req, current)
	if err != nil {
		return nil, err
	}
	// The branch state and the revision the transaction committed at are the
	// last two statements of the list.
	stmts = append(stmts,
		Statement{SQL: bumpTxnSQL},
		Statement{SQL: `SELECT passed, rev FROM txn_ctx WHERE id = 1`},
		Statement{SQL: readRevisionSQL},
	)
	resp, err := s.client.Write(ctx, stmts...)
	if err != nil {
		return nil, err
	}
	if len(resp.Results) != len(stmts) {
		return nil, &Err{Message: fmt.Sprintf("txn response has %d results, want %d", len(resp.Results), len(stmts))}
	}
	succeeded, err := txnSucceeded(resp.Results[len(stmts)-2])
	if err != nil {
		return nil, err
	}
	rev, err := readRevisionResult(resp.Results[len(stmts)-1])
	if err != nil {
		return nil, err
	}
	responses, err := decodeTxnResponses(resp, refs, succeeded, rev)
	if err != nil {
		return nil, err
	}
	return &TxnResult{Succeeded: succeeded, Revision: rev, Responses: responses}, nil
}

// buildTxnStatements renders the compares and both branches as the statement
// list of one transaction: the branch predicate is a SQL guard, so the ops of
// the branch that did not run are no-ops.
func buildTxnStatements(req TxnRequest, current int64) ([]Statement, []txnRef, error) {
	passedExpr, compareParams := compareExpr(req.Compares)
	stmts := []Statement{
		{SQL: `DELETE FROM txn_ctx`},
		{SQL: `INSERT INTO txn_ctx(id, passed, rev) SELECT 1, CASE WHEN ` + passedExpr + ` THEN 1 ELSE 0 END, ` + revisionExpr + ` + 1`,
			Params: compareParams},
	}
	thenStmts, thenRefs, err := branchStatements(req.Success, true, current, len(stmts))
	if err != nil {
		return nil, nil, err
	}
	stmts = append(stmts, thenStmts...)
	elseStmts, elseRefs, err := branchStatements(req.Failure, false, current, len(stmts))
	if err != nil {
		return nil, nil, err
	}
	return append(stmts, elseStmts...), append(thenRefs, elseRefs...), nil
}

// branchStatements renders one branch's ops behind the branch guard. offset is
// the index the branch's first statement gets in the transaction, which is
// what turns an op's local result index into a response index.
func branchStatements(ops []Op, success bool, current int64, offset int) ([]Statement, []txnRef, error) {
	guard := `(SELECT passed FROM txn_ctx WHERE id = 1) = 0`
	if success {
		guard = `(SELECT passed FROM txn_ctx WHERE id = 1) = 1`
	}
	var stmts []Statement
	var refs []txnRef
	for _, op := range ops {
		core, resultIdx, err := opStatements(op, current, guard)
		if err != nil {
			return nil, nil, err
		}
		stmts = append(stmts, core...)
		refs = append(refs, txnRef{kind: op.Kind, success: success, op: op,
			result: offset + len(stmts) - len(core) + resultIdx})
	}
	return stmts, refs, nil
}

// opStatements renders one op of a branch and reports the index of the
// statement that carries the op's response row inside the returned list.
func opStatements(op Op, current int64, guard string) ([]Statement, int, error) {
	switch op.Kind {
	case OpRange:
		stmts, err := rangeStatements(*op.Range, current, current, guard)
		if err != nil {
			return nil, 0, err
		}
		return stmts, len(stmts) - 1, nil
	case OpPut:
		stmts, prevIdx := putCore(*op.Put, txnRevExpr, guard)
		return stmts, prevIdx, nil
	case OpDelete:
		stmts, prevIdx := deleteCore(*op.Delete, txnRevExpr, guard)
		return stmts, prevIdx, nil
	default:
		return nil, 0, unknownTxnOp(op.Kind)
	}
}

// txnSucceeded reports whether the transaction took the then branch.
func txnSucceeded(res StatementResult) (bool, error) {
	state, err := res.row(0)
	if err != nil {
		return false, err
	}
	return state.int("passed") == 1, nil
}

// decodeTxnResponses rebuilds the response ops of the executed branch in the
// order the client sent them.
func decodeTxnResponses(resp Response, refs []txnRef, succeeded bool, rev int64) ([]OpResponse, error) {
	var out []OpResponse
	for _, r := range refs {
		if r.success != succeeded {
			continue
		}
		op, err := decodeTxnOp(resp.Results[r.result], r, rev)
		if err != nil {
			return nil, err
		}
		out = append(out, op)
	}
	return out, nil
}

func decodeTxnOp(res StatementResult, r txnRef, rev int64) (OpResponse, error) {
	switch r.kind {
	case OpRange:
		return decodeTxnRange(res, *r.op.Range, rev)
	case OpPut:
		return decodeTxnPut(res, *r.op.Put, rev)
	case OpDelete:
		return decodeTxnDelete(res, *r.op.Delete, rev)
	default:
		return OpResponse{}, unknownTxnOp(r.kind)
	}
}

func decodeTxnRange(res StatementResult, opts RangeOptions, rev int64) (OpResponse, error) {
	rr, err := decodeRange(res, opts)
	if err != nil {
		return OpResponse{}, err
	}
	rr.Revision = rev
	return OpResponse{Kind: OpRange, Range: rr}, nil
}

func decodeTxnPut(res StatementResult, opts PutOptions, rev int64) (OpResponse, error) {
	prev, err := firstKV(res)
	if err != nil {
		return OpResponse{}, err
	}
	if (opts.IgnoreValue || opts.IgnoreLease) && prev == nil {
		return OpResponse{}, rpctypes.ErrGRPCKeyNotFound
	}
	// The previous KV is only part of the response when the op asked for it,
	// exactly like etcd's txn put (etcdserver/txn/put.go).
	if !opts.PrevKv {
		prev = nil
	}
	return OpResponse{Kind: OpPut, Put: &PutResult{PrevKV: prev, Revision: rev}}, nil
}

func decodeTxnDelete(res StatementResult, opts DeleteOptions, rev int64) (OpResponse, error) {
	prev, err := kvsFromResult(res)
	if err != nil {
		return OpResponse{}, err
	}
	if !opts.PrevKv {
		prev = nil
	}
	return OpResponse{Kind: OpDelete, Delete: &DeleteResult{Deleted: int64(len(prev)), PrevKVs: prev, Revision: rev}}, nil
}

// validateTxn rejects requests exactly the way etcd's v3rpc layer does before
// a transaction is applied (api/v3rpc/key.go checkTxnRequest/checkIntervals),
// and rejects the shapes this layer deliberately does not implement with an
// explicit error instead of a silently wrong result.
func validateTxn(req TxnRequest) error {
	if err := checkTxnSize(req); err != nil {
		return err
	}
	if err := checkTxnCompares(req.Compares); err != nil {
		return err
	}
	if err := checkTxnBranch(req.Success); err != nil {
		return err
	}
	return checkTxnBranch(req.Failure)
}

// checkTxnSize mirrors etcd's op limit: the transaction size is the largest of
// the three lists.
func checkTxnSize(req TxnRequest) error {
	opc := len(req.Compares)
	if opc < len(req.Success) {
		opc = len(req.Success)
	}
	if opc < len(req.Failure) {
		opc = len(req.Failure)
	}
	if opc > MaxTxnOps {
		return rpctypes.ErrGRPCTooManyOps
	}
	return nil
}

func checkTxnCompares(compares []Compare) error {
	for _, c := range compares {
		if len(c.Key) == 0 {
			return rpctypes.ErrGRPCEmptyKey
		}
		if len(c.RangeEnd) > 0 {
			return status.Error(codes.Unimplemented, "etcdserver: compare over a range is not supported by the rqlite compat layer")
		}
	}
	return nil
}

// checkTxnBranch validates one branch and rejects overlapping writes in it. The
// then/else branches are mutually exclusive, so a key may appear in both.
func checkTxnBranch(ops []Op) error {
	for _, op := range ops {
		if err := checkTxnOp(op); err != nil {
			return err
		}
	}
	return checkIntervals(ops)
}

func checkTxnOp(op Op) error {
	switch op.Kind {
	case OpRange:
		return checkTxnRangeOp(op.Range)
	case OpPut:
		return checkTxnPutOp(op.Put)
	case OpDelete:
		return checkTxnDeleteOp(op.Delete)
	default:
		return unknownTxnOp(op.Kind)
	}
}

func checkTxnRangeOp(r *RangeOptions) error {
	if r == nil {
		return status.Error(codes.InvalidArgument, "etcdserver: nil range op")
	}
	if len(r.Key) == 0 {
		return rpctypes.ErrGRPCEmptyKey
	}
	if r.Revision != 0 {
		return status.Error(codes.Unimplemented, "etcdserver: a range op inside a txn can not specify a revision in the rqlite compat layer")
	}
	return nil
}

func checkTxnPutOp(p *PutOptions) error {
	if p == nil {
		return status.Error(codes.InvalidArgument, "etcdserver: nil put op")
	}
	return checkPutRequest(*p)
}

func checkTxnDeleteOp(d *DeleteOptions) error {
	if d == nil {
		return status.Error(codes.InvalidArgument, "etcdserver: nil delete op")
	}
	if len(d.Key) == 0 {
		return rpctypes.ErrGRPCEmptyKey
	}
	return nil
}

// unknownTxnOp is the error for an op kind this layer never builds; it exists so
// an unknown kind fails loudly instead of silently dropping the op.
func unknownTxnOp(kind OpKind) error {
	return status.Errorf(codes.InvalidArgument, "etcdserver: unknown txn op kind %d", kind)
}

// checkIntervals reports whether one branch puts and deletes overlap, the way
// etcd's checkIntervals does for a single operation list. Ranges use etcd's
// adt string intervals, where the empty string sorts after every other string
// (the "all keys >= key" marker written as range_end "\x00" therefore never
// overlaps a delete range).
func checkIntervals(ops []Op) error {
	type interval struct{ begin, end []byte }
	var dels []interval
	for _, op := range ops {
		if op.Kind != OpDelete || op.Delete == nil {
			continue
		}
		dels = append(dels, interval{begin: op.Delete.Key, end: op.Delete.RangeEnd})
	}
	covered := func(key []byte) bool {
		for _, d := range dels {
			if len(d.end) == 0 {
				if bytes.Equal(key, d.begin) {
					return true
				}
				continue
			}
			if !affineLess(key, d.begin) && affineLess(key, d.end) {
				return true
			}
		}
		return false
	}
	puts := map[string]bool{}
	for _, op := range ops {
		if op.Kind != OpPut || op.Put == nil {
			continue
		}
		key := string(op.Put.Key)
		if puts[key] || covered(op.Put.Key) {
			return rpctypes.ErrGRPCDuplicateKey
		}
		puts[key] = true
	}
	return nil
}

// affineLess orders keys the way adt.StringAffineComparable does: the empty
// string compares greater than every other string.
func affineLess(a, b []byte) bool {
	if len(a) == 0 {
		return false
	}
	if len(b) == 0 {
		return true
	}
	return bytes.Compare(a, b) < 0
}

// checkPutRequest mirrors etcd's v3rpc checkPutRequest: it is applied to every
// Put, standalone or inside a transaction.
func checkPutRequest(p PutOptions) error {
	if len(p.Key) == 0 {
		return rpctypes.ErrGRPCEmptyKey
	}
	if p.IgnoreValue && len(p.Value) != 0 {
		return rpctypes.ErrGRPCValueProvided
	}
	if p.IgnoreLease && p.Lease != 0 {
		return rpctypes.ErrGRPCLeaseProvided
	}
	return nil
}

// compareExpr renders the conjunction of compares as a SQL boolean. A missing
// key reads as the zero KeyValue (version/create/mod/lease 0, empty value),
// exactly like etcd's compare against a non-existent key.
func compareExpr(compares []Compare) (string, map[string]any) {
	if len(compares) == 0 {
		return "1 = 1", map[string]any{}
	}
	params := map[string]any{}
	parts := make([]string, 0, len(compares))
	for i, c := range compares {
		kp := fmt.Sprintf("ck%d", i)
		vp := fmt.Sprintf("cv%d", i)
		params[kp] = Blob(c.Key)

		var lhs string
		switch c.Target {
		case CompareVersion:
			lhs = "COALESCE((SELECT version FROM kv WHERE key = :" + kp + "), 0)"
			params[vp] = c.Version
		case CompareCreate:
			lhs = "COALESCE((SELECT create_revision FROM kv WHERE key = :" + kp + "), 0)"
			params[vp] = c.Version
		case CompareMod:
			lhs = "COALESCE((SELECT mod_revision FROM kv WHERE key = :" + kp + "), 0)"
			params[vp] = c.Version
		case CompareLease:
			lhs = "COALESCE((SELECT lease FROM kv WHERE key = :" + kp + "), 0)"
			params[vp] = c.Version
		case CompareValue:
			lhs = "COALESCE((SELECT value FROM kv WHERE key = :" + kp + "), x'')"
			params[vp] = Blob(emptyBytes(c.Value))
		default:
			lhs = "0"
			params[vp] = 0
		}
		parts = append(parts, "("+lhs+" "+compareOpSQL(c.Op)+" :"+vp+")")
	}
	return strings.Join(parts, " AND "), params
}

func compareOpSQL(op CompareOp) string {
	switch op {
	case CompareGreater:
		return ">"
	case CompareLess:
		return "<"
	case CompareNotEqual:
		return "!="
	default:
		return "="
	}
}
