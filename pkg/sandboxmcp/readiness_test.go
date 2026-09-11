package sandboxmcp

import (
	"context"
	"errors"
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// failingStore makes the durable state lookup fail, as a Kubernetes outage would.
type failingStore struct{ RecordStore }

func (failingStore) Get(context.Context, string) (Record, error) {
	return Record{}, errors.New("storage down")
}

// stubSandboxClient returns a fixed gateway error so the readiness check can be
// exercised without opening a listener, which would need insecure credentials.
type stubSandboxClient struct {
	pb.SandboxServiceClient
	err error
}

func (client stubSandboxClient) GetSession(context.Context, *pb.GetSessionRequest, ...grpc.CallOption) (*pb.GetSessionResponse, error) {
	return nil, client.err
}

func TestReadinessChecksStateAndGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	store := &testStore{records: map[string]Record{}}
	answered := status.Error(codes.InvalidArgument, "session_id required")

	if err := readiness(failingStore{}, stubSandboxClient{err: answered})(ctx); err == nil {
		t.Fatal("state store failure reported ready")
	}
	// Any application-level error proves the gateway answered.
	if err := readiness(store, stubSandboxClient{err: answered})(ctx); err != nil {
		t.Fatalf("reachable gateway reported unready: %v", err)
	}
	if err := readiness(store, stubSandboxClient{})(ctx); err != nil {
		t.Fatalf("healthy gateway reported unready: %v", err)
	}
	// Transport and credential failures mean the adapter cannot serve tool calls.
	for _, code := range []codes.Code{codes.Unavailable, codes.DeadlineExceeded, codes.Canceled, codes.Unauthenticated} {
		if err := readiness(store, stubSandboxClient{err: status.Error(code, "gateway down")})(ctx); err == nil {
			t.Fatalf("%v gateway reported ready", code)
		}
	}
}
