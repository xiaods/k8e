package sandboxcli

import (
	"context"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/client"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type probeClient struct {
	pb.SandboxServiceClient
	get func(context.Context) error
}

func (p probeClient) GetSession(ctx context.Context, _ *pb.GetSessionRequest, _ ...grpc.CallOption) (*pb.GetSessionResponse, error) {
	return nil, p.get(ctx)
}

func TestVerifyGatewayReadOnlyAndDeadline(t *testing.T) {
	for _, code := range []codes.Code{codes.OK, codes.NotFound, codes.PermissionDenied, codes.Unauthenticated, codes.Unavailable, codes.Unknown} {
		err := verifyGateway(&client.Client{SandboxServiceClient: probeClient{get: func(ctx context.Context) error {
			deadline, ok := ctx.Deadline()
			if !ok || time.Until(deadline) > gatewayVerifyTimeout {
				t.Fatal("verification needs bounded deadline")
			}
			return status.Error(code, "not found")
		}}})
		if (err == nil) != (code == codes.OK || code == codes.NotFound) {
			t.Fatalf("code=%v err=%v", code, err)
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
	defer cancel()
	err := probeGateway(ctx, probeClient{get: func(ctx context.Context) error { <-ctx.Done(); return ctx.Err() }})
	if err != context.DeadlineExceeded {
		t.Fatalf("deadline not propagated: %v", err)
	}
}
