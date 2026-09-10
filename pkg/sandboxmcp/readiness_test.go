package sandboxmcp

import (
	"context"
	"errors"
	"net"
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/status"
)

// failingStore makes the durable state lookup fail, as a Kubernetes outage would.
type failingStore struct{ RecordStore }

func (failingStore) Get(context.Context, string) (Record, error) {
	return Record{}, errors.New("storage down")
}

// readinessBackend answers the probe RPC with an application error, proving the
// gateway is reachable without performing real work.
type readinessBackend struct {
	pb.UnimplementedSandboxServiceServer
}

func (readinessBackend) GetSession(context.Context, *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	return nil, status.Error(codes.InvalidArgument, "session_id required")
}

func TestReadinessChecksStateAndGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := readiness(failingStore{}, nil)(ctx); err == nil {
		t.Fatal("state store failure reported ready")
	}

	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	grpcServer := grpc.NewServer()
	pb.RegisterSandboxServiceServer(grpcServer, readinessBackend{})
	go grpcServer.Serve(listener)
	connection, err := grpc.NewClient(listener.Addr().String(), grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		t.Fatal(err)
	}
	defer connection.Close()

	client := pb.NewSandboxServiceClient(connection)
	store := &testStore{records: map[string]Record{}}
	if err := readiness(store, client)(ctx); err != nil {
		t.Fatalf("reachable dependencies reported unready: %v", err)
	}

	grpcServer.Stop()
	if err := readiness(store, client)(ctx); err == nil {
		t.Fatal("unreachable gateway reported ready")
	}
}
