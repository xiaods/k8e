//go:build darwin
// +build darwin

package cri

import (
	"context"
	"fmt"

	"google.golang.org/grpc"
)

func Connection(ctx context.Context, address string) (*grpc.ClientConn, error) {
	return nil, fmt.Errorf("CRI is not supported on darwin")
}
