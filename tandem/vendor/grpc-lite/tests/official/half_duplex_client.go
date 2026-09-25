package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
)

var (
	serverHost = flag.String("server_host", "127.0.0.1", "server host")
	serverPort = flag.Int("server_port", 10000, "server port")
)

type receiveResult struct {
	response *testgrpc.StreamingOutputCallResponse
	err      error
}

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	target := net.JoinHostPort(*serverHost, strconv.Itoa(*serverPort))
	conn, err := grpc.NewClient(target, grpc.WithTransportCredentials(insecure.NewCredentials()))
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream, err := testgrpc.NewTestServiceClient(conn).HalfDuplexCall(ctx)
	if err != nil {
		return fmt.Errorf("open HalfDuplexCall: %w", err)
	}

	requestSizes := [...]int{1, 2, 3}
	responseSizes := [...]int{3 * 1024 * 1024, 3 * 1024 * 1024, 3 * 1024 * 1024}
	results := make(chan receiveResult, len(responseSizes)+1)
	go func() {
		defer close(results)
		for {
			response, recvErr := stream.Recv()
			results <- receiveResult{response: response, err: recvErr}
			if recvErr != nil {
				return
			}
		}
	}()

	for index := range requestSizes {
		request := &testgrpc.StreamingOutputCallRequest{
			ResponseType: testgrpc.PayloadType_COMPRESSABLE,
			ResponseParameters: []*testgrpc.ResponseParameters{
				{Size: int32(responseSizes[index])},
			},
			Payload: &testgrpc.Payload{
				Type: testgrpc.PayloadType_COMPRESSABLE,
				Body: make([]byte, requestSizes[index]),
			},
		}
		if err := stream.Send(request); err != nil {
			return fmt.Errorf("send request %d: %w", index, err)
		}
	}

	select {
	case result := <-results:
		return fmt.Errorf("received before client half-close: %v", result.err)
	case <-time.After(100 * time.Millisecond):
	}

	if err := stream.CloseSend(); err != nil {
		return fmt.Errorf("close send: %w", err)
	}
	for index, expectedSize := range responseSizes {
		select {
		case result := <-results:
			if result.err != nil {
				return fmt.Errorf("receive response %d: %w", index, result.err)
			}
			payload := result.response.GetPayload()
			if payload == nil || payload.GetType() != testgrpc.PayloadType_COMPRESSABLE {
				return fmt.Errorf("response %d has invalid payload", index)
			}
			if len(payload.GetBody()) != expectedSize {
				return fmt.Errorf("response %d size = %d, want %d", index, len(payload.GetBody()), expectedSize)
			}
		case <-ctx.Done():
			return fmt.Errorf("receive response %d: %w", index, ctx.Err())
		}
	}

	select {
	case result := <-results:
		if !errors.Is(result.err, io.EOF) {
			return fmt.Errorf("terminal receive: %v", result.err)
		}
	case <-ctx.Done():
		return fmt.Errorf("wait for terminal status: %w", ctx.Err())
	}
	return nil
}
