package differential

import (
	"errors"
	"testing"

	"go.etcd.io/etcd/api/v3/v3rpc/rpctypes"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// etcd's client returns rpctypes.EtcdError, which carries a code but does not
// implement GRPCStatus(). Reading it through status.FromError yields Unknown,
// which would make every error comparison vacuous — both sides would look
// identical because neither code could be read.
func TestCodeOfReadsEtcdError(t *testing.T) {
	if got := codeOf(rpctypes.ErrCompacted); got != codes.OutOfRange {
		t.Errorf("ErrCompacted: got %s, want %s", got, codes.OutOfRange)
	}
	if got := codeOf(rpctypes.ErrFutureRev); got != codes.OutOfRange {
		t.Errorf("ErrFutureRev: got %s, want %s", got, codes.OutOfRange)
	}
	if got := codeOf(rpctypes.ErrLeaseNotFound); got != codes.NotFound {
		t.Errorf("ErrLeaseNotFound: got %s, want %s", got, codes.NotFound)
	}
	// Wrapped, as the client may hand it back.
	wrapped := errors.Join(errors.New("put"), rpctypes.ErrGRPCCompacted)
	if got := codeOf(wrapped); got != codes.OutOfRange {
		t.Errorf("wrapped ErrCompacted: got %s, want %s", got, codes.OutOfRange)
	}
	// A plain gRPC status still reads normally.
	if got := codeOf(status.Error(codes.InvalidArgument, "bad")); got != codes.InvalidArgument {
		t.Errorf("plain status: got %s, want %s", got, codes.InvalidArgument)
	}
	if got := codeOf(nil); got != codes.OK {
		t.Errorf("nil error: got %s, want %s", got, codes.OK)
	}
}

// The registry is the only thing standing between a real regression and a
// silently accepted difference, so every entry has to carry the reason it
// exists and what would make it wrong.
func TestKnownDifferencesAreJustified(t *testing.T) {
	for _, difference := range KnownDifferences {
		if difference.Method == "" {
			t.Error("entry with no method")
		}
		if difference.Observed == "" {
			t.Errorf("%s: no description of what Tandem does", difference.Method)
		}
		if difference.Reason == "" {
			t.Errorf("%s: no reason", difference.Method)
		}
		if difference.Regression == "" {
			t.Errorf("%s: no regression trigger", difference.Method)
		}
	}
}

func TestLookupFindsRegisteredMethod(t *testing.T) {
	if _, ok := Lookup("Maintenance.Snapshot"); !ok {
		t.Error("Maintenance.Snapshot is not registered")
	}
	if _, ok := Lookup("KV.Put"); ok {
		t.Error("KV.Put must not be registered: Tandem serves it")
	}
}
