package sandboxcli

import (
	"errors"
	"testing"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

func TestIsSessionExpired_grpcNotFound(t *testing.T) {
	err := status.Error(codes.NotFound, "session sess-abc not found")
	if !isSessionExpired(err) {
		t.Error("expected true for gRPC NotFound")
	}
}

func TestIsSessionExpired_grpcFailedPrecondition(t *testing.T) {
	err := status.Error(codes.FailedPrecondition, "session sess-abc has no pod IP after 60s")
	if !isSessionExpired(err) {
		t.Error("expected true for gRPC FailedPrecondition")
	}
}

func TestIsSessionExpired_grpcUnavailableNotNoPodIP(t *testing.T) {
	err := status.Error(codes.Unavailable, "sandboxd exec: connection refused")
	if isSessionExpired(err) {
		t.Error("expected false for gRPC Unavailable (not no pod IP)")
	}
}

func TestIsSessionExpired_sentinelError(t *testing.T) {
	err := ErrSessionGone
	if !isSessionExpired(err) {
		t.Error("expected true for ErrSessionGone sentinel")
	}
}

func TestIsSessionExpired_wrappedSentinel(t *testing.T) {
	err := errors.New("some context")
	if isSessionExpired(err) {
		t.Error("expected false for plain error")
	}
}

func TestIsSessionExpired_nil(t *testing.T) {
	if isSessionExpired(nil) {
		t.Error("expected false for nil error")
	}
}
