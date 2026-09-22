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

	passedExpr, compareParams := compareExpr(req.Compares)
	stmts := []Statement{
		{SQL: `DELETE FROM txn_ctx`},
		{SQL: `INSERT INTO txn_ctx(id, passed, rev) SELECT 1, CASE WHEN ` + passedExpr + ` THEN 1 ELSE 0 END, ` + revisionExpr + ` + 1`,
			Params: compareParams},
	}

	type ref struct {
		kind    OpKind
		success bool
		op      Op
		result  int
	}
	var refs []ref

	appendOps := func(ops []Op, success bool) error {
		guard := `(SELECT passed FROM txn_ctx WHERE id = 1) = 0`
		if success {
			guard = `(SELECT passed FROM txn_ctx WHERE id = 1) = 1`
		}
		for _, op := range ops {
			switch op.Kind {
			case OpRange:
				rangeStmts, err := rangeStatements(*op.Range, current, current, guard)
				if err != nil {
					return err
				}
				stmts = append(stmts, rangeStmts...)
				refs = append(refs, ref{kind: OpRange, success: success, op: op, result: len(stmts) - 1})
			case OpPut:
				core, prevIdx := putCore(*op.Put, txnRevExpr, guard)
				stmts = append(stmts, core...)
				refs = append(refs, ref{kind: OpPut, success: success, op: op, result: len(stmts) - len(core) + prevIdx})
			case OpDelete:
				core, prevIdx := deleteCore(*op.Delete, txnRevExpr, guard)
				stmts = append(stmts, core...)
				refs = append(refs, ref{kind: OpDelete, success: success, op: op, result: len(stmts) - len(core) + prevIdx})
			}
		}
		return nil
	}
	if err := appendOps(req.Success, true); err != nil {
		return nil, err
	}
	if err := appendOps(req.Failure, false); err != nil {
		return nil, err
	}

	stmts = append(stmts, Statement{SQL: bumpTxnSQL})
	stateIdx := len(stmts)
	stmts = append(stmts, Statement{SQL: `SELECT passed, rev FROM txn_ctx WHERE id = 1`})
	revIdx := len(stmts)
	stmts = append(stmts, Statement{SQL: readRevisionSQL})

	resp, err := s.client.Write(ctx, stmts...)
	if err != nil {
		return nil, err
	}
	if len(resp.Results) != len(stmts) {
		return nil, &Err{Message: fmt.Sprintf("txn response has %d results, want %d", len(resp.Results), len(stmts))}
	}

	state, err := resp.Results[stateIdx].row(0)
	if err != nil {
		return nil, err
	}
	rev, err := readRevisionResult(resp.Results[revIdx])
	if err != nil {
		return nil, err
	}
	result := &TxnResult{Succeeded: state.int("passed") == 1, Revision: rev}

	for _, r := range refs {
		if r.success != result.Succeeded {
			continue
		}
		switch r.kind {
		case OpRange:
			rr, err := decodeRange(resp.Results[r.result], *r.op.Range)
			if err != nil {
				return nil, err
			}
			rr.Revision = rev
			result.Responses = append(result.Responses, OpResponse{Kind: OpRange, Range: rr})
		case OpPut:
			prev, err := firstKV(resp.Results[r.result])
			if err != nil {
				return nil, err
			}
			if (r.op.Put.IgnoreValue || r.op.Put.IgnoreLease) && prev == nil {
				return nil, rpctypes.ErrGRPCKeyNotFound
			}
			// The previous KV is only part of the response when the op asked
			// for it, exactly like etcd's txn put (etcdserver/txn/put.go).
			if !r.op.Put.PrevKv {
				prev = nil
			}
			result.Responses = append(result.Responses, OpResponse{Kind: OpPut, Put: &PutResult{PrevKV: prev, Revision: rev}})
		case OpDelete:
			prev, err := kvsFromResult(resp.Results[r.result])
			if err != nil {
				return nil, err
			}
			if !r.op.Delete.PrevKv {
				prev = nil
			}
			result.Responses = append(result.Responses, OpResponse{Kind: OpDelete, Delete: &DeleteResult{Deleted: int64(len(prev)), PrevKVs: prev, Revision: rev}})
		}
	}
	return result, nil
}

// validateTxn rejects requests exactly the way etcd's v3rpc layer does before
// a transaction is applied (api/v3rpc/key.go checkTxnRequest/checkIntervals),
// and rejects the shapes this layer deliberately does not implement with an
// explicit error instead of a silently wrong result.
func validateTxn(req TxnRequest) error {
	// etcd measures the transaction size as the largest of the three lists.
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

	for _, c := range req.Compares {
		if len(c.Key) == 0 {
			return rpctypes.ErrGRPCEmptyKey
		}
		if len(c.RangeEnd) > 0 {
			return status.Error(codes.Unimplemented, "etcdserver: compare over a range is not supported by the rqlite compat layer")
		}
	}
	checkOp := func(op Op) error {
		switch op.Kind {
		case OpRange:
			if op.Range == nil {
				return status.Error(codes.InvalidArgument, "etcdserver: nil range op")
			}
			if len(op.Range.Key) == 0 {
				return rpctypes.ErrGRPCEmptyKey
			}
			if op.Range.Revision != 0 {
				return status.Error(codes.Unimplemented, "etcdserver: a range op inside a txn can not specify a revision in the rqlite compat layer")
			}
		case OpPut:
			if op.Put == nil {
				return status.Error(codes.InvalidArgument, "etcdserver: nil put op")
			}
			if err := checkPutRequest(*op.Put); err != nil {
				return err
			}
		case OpDelete:
			if op.Delete == nil {
				return status.Error(codes.InvalidArgument, "etcdserver: nil delete op")
			}
			if len(op.Delete.Key) == 0 {
				return rpctypes.ErrGRPCEmptyKey
			}
		}
		return nil
	}
	for _, ops := range [][]Op{req.Success, req.Failure} {
		for _, op := range ops {
			if err := checkOp(op); err != nil {
				return err
			}
		}
		// Overlapping writes in the same branch are rejected; the then/else
		// branches are mutually exclusive, so a key may appear in both.
		if err := checkIntervals(ops); err != nil {
			return err
		}
	}
	return nil
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
