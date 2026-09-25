package differential

import (
	"errors"
	"fmt"
	"strings"

	"go.etcd.io/etcd/api/v3/mvccpb"
	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// Observation is one normalized answer from a backend. A case runs the same
// operation against both and compares the two Observations, so anything a
// normalizer has to blank out is blanked before the comparison rather than
// reported as a difference.
type Observation struct {
	// Op names the step, so a failure points at the operation rather than at
	// an index.
	Op string
	// Code is the gRPC status code. Messages are never compared: Tandem sends
	// the Zig error name and etcd sends prose, so the text is always noise.
	Code codes.Code
	// Revision is the store revision relative to the baseline taken when the
	// case started. Absolute revisions are not comparable because the two
	// stores do not share an identity, and a difference is a difference in
	// how many revisions an operation consumed.
	Revision int64
	// Count, KVs and More carry the Range-shaped results.
	Count int64
	KVs   []KeyValue
	More  bool
	// LeaseID is only set by lease cases.
	LeaseID int64
}

// KeyValue is a key/value pair reduced to the fields both backends can agree
// on. Version, CreateRevision and ModRevision are relative to the case
// baseline for the same reason Revision is.
type KeyValue struct {
	Key            string
	Value          string
	CreateRevision int64
	ModRevision    int64
	Version        int64
	Lease          int64
}

func (kv KeyValue) String() string {
	return fmt.Sprintf("{%q=%q create=%d mod=%d ver=%d lease=%d}",
		kv.Key, kv.Value, kv.CreateRevision, kv.ModRevision, kv.Version, kv.Lease)
}

func (o Observation) String() string {
	parts := []string{fmt.Sprintf("%s: code=%s rev=%d", o.Op, o.Code, o.Revision)}
	if o.Count != 0 || o.More || len(o.KVs) > 0 {
		parts = append(parts, fmt.Sprintf("count=%d more=%t", o.Count, o.More))
		for _, kv := range o.KVs {
			parts = append(parts, kv.String())
		}
	}
	if o.LeaseID != 0 {
		parts = append(parts, fmt.Sprintf("lease=%d", o.LeaseID))
	}
	return strings.Join(parts, " ")
}

// Compare reports the first meaningful difference between two observations, or
// an empty string when they agree. It is written as a sequence of readable
// checks so a failure says what differed rather than dumping two structs.
func Compare(want, got Observation) string {
	if want.Op != got.Op {
		return fmt.Sprintf("step mismatch: %s vs %s", want.Op, got.Op)
	}
	if want.Code != got.Code {
		return fmt.Sprintf("%s: status code %s (etcd) vs %s (tandem)", want.Op, want.Code, got.Code)
	}
	if want.Code != codes.OK {
		// A non-OK status has no payload worth comparing; etcd's own message
		// text and Tandem's error name are expected to differ.
		return ""
	}
	if want.Revision != got.Revision {
		return fmt.Sprintf("%s: revision advanced by %d (etcd) vs %d (tandem)", want.Op, want.Revision, got.Revision)
	}
	if want.Count != got.Count {
		return fmt.Sprintf("%s: count %d (etcd) vs %d (tandem)", want.Op, want.Count, got.Count)
	}
	if want.More != got.More {
		return fmt.Sprintf("%s: more %t (etcd) vs %t (tandem)", want.Op, want.More, got.More)
	}
	if len(want.KVs) != len(got.KVs) {
		return fmt.Sprintf("%s: %d keys (etcd) vs %d (tandem)\n  etcd:   %s\n  tandem: %s",
			want.Op, len(want.KVs), len(got.KVs), formatKVs(want.KVs), formatKVs(got.KVs))
	}
	for i := range want.KVs {
		if want.KVs[i] != got.KVs[i] {
			return fmt.Sprintf("%s: key %d is %s (etcd) vs %s (tandem)", want.Op, i, want.KVs[i], got.KVs[i])
		}
	}
	if want.LeaseID != got.LeaseID {
		return fmt.Sprintf("%s: lease id %d (etcd) vs %d (tandem)", want.Op, want.LeaseID, got.LeaseID)
	}
	return ""
}

func formatKVs(kvs []KeyValue) string {
	if len(kvs) == 0 {
		return "(none)"
	}
	parts := make([]string, 0, len(kvs))
	for _, kv := range kvs {
		parts = append(parts, kv.String())
	}
	return strings.Join(parts, ", ")
}

// codeOf extracts the status code, treating a nil error as OK. The message is
// deliberately dropped.
//
// etcd's client does not surface these as gRPC statuses. The interceptors
// return rpctypes.EtcdError, which carries a Code() but does not implement
// GRPCStatus(), so status.FromError cannot read it and would report Unknown
// for every failure — making a real mismatch look like an unknown one on both
// sides and hiding the comparison entirely.
func codeOf(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	var etcdErr rpctypes.EtcdError
	if errors.As(err, &etcdErr) {
		return etcdErr.Code()
	}
	if s, ok := status.FromError(err); ok {
		return s.Code()
	}
	return codes.Unknown
}

// kv normalizes one mvccpb pair against a baseline revision.
func kv(in *mvccpb.KeyValue, baseline int64) KeyValue {
	out := KeyValue{
		Key:   string(in.Key),
		Value: string(in.Value),
		Lease: in.Lease,
	}
	// A revision at or below the baseline was created before the case began;
	// subtracting keeps a pre-existing key comparable without asserting where
	// either store started counting.
	if in.CreateRevision > baseline {
		out.CreateRevision = in.CreateRevision - baseline
	}
	if in.ModRevision > baseline {
		out.ModRevision = in.ModRevision - baseline
	}
	out.Version = in.Version
	return out
}

func kvs(in []*mvccpb.KeyValue, baseline int64) []KeyValue {
	if len(in) == 0 {
		return nil
	}
	out := make([]KeyValue, 0, len(in))
	for _, item := range in {
		if item == nil {
			continue
		}
		out = append(out, kv(item, baseline))
	}
	return out
}

// relative reduces a header revision to a delta from the case baseline. etcd
// and Tandem do not share a revision space, so only the movement is comparable.
func relative(revision, baseline int64) int64 {
	if revision <= baseline {
		return 0
	}
	return revision - baseline
}
