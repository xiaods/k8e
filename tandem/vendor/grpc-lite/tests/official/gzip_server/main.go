package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/encoding/gzip"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

var port = flag.Int("port", 10000, "server port")

type rpcStateKey struct{}

type rpcState struct {
	mu                      sync.Mutex
	inboundEncoding         string
	inboundLength           int
	inboundCompressedLength int
}

func (s *rpcState) recordHeader(encoding string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inboundEncoding = encoding
}

func (s *rpcState) recordPayload(length, compressedLength int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.inboundLength = length
	s.inboundCompressedLength = compressedLength
}

func (s *rpcState) requestCompressed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.inboundEncoding == gzip.Name &&
		s.inboundLength > 0 &&
		s.inboundCompressedLength < s.inboundLength
}

type rpcStatsHandler struct{}

func (rpcStatsHandler) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return context.WithValue(ctx, rpcStateKey{}, &rpcState{})
}

func (rpcStatsHandler) HandleRPC(ctx context.Context, event stats.RPCStats) {
	state, ok := ctx.Value(rpcStateKey{}).(*rpcState)
	if !ok {
		return
	}
	switch event := event.(type) {
	case *stats.InHeader:
		state.recordHeader(event.Compression)
	case *stats.InPayload:
		state.recordPayload(event.Length, event.CompressedLength)
	}
}

func (rpcStatsHandler) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (rpcStatsHandler) HandleConn(context.Context, stats.ConnStats) {}

type service struct {
	testgrpc.UnimplementedTestServiceServer
}

func (service) UnaryCall(ctx context.Context, request *testgrpc.SimpleRequest) (*testgrpc.SimpleResponse, error) {
	state, ok := ctx.Value(rpcStateKey{}).(*rpcState)
	if !ok {
		return nil, status.Error(codes.Internal, "request compression stats are unavailable")
	}
	if expected := request.GetExpectCompressed(); expected != nil {
		if expected.GetValue() != state.requestCompressed() {
			return nil, status.Error(codes.InvalidArgument, "request compression did not match expectation")
		}
	}
	responseCompression := "identity"
	if compressed := request.GetResponseCompressed(); compressed != nil && compressed.GetValue() {
		responseCompression = gzip.Name
	}
	if err := grpc.SetSendCompressor(ctx, responseCompression); err != nil {
		return nil, status.Errorf(codes.Internal, "set %s response compression: %v", responseCompression, err)
	}
	if request.GetResponseSize() < 0 {
		return nil, status.Error(codes.InvalidArgument, "negative response size")
	}
	return &testgrpc.SimpleResponse{
		Payload: &testgrpc.Payload{
			Type: request.GetResponseType(),
			Body: make([]byte, request.GetResponseSize()),
		},
	}, nil
}

func main() {
	flag.Parse()
	listener, err := net.Listen("tcp", net.JoinHostPort("", strconv.Itoa(*port)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "listen: %v\n", err)
		os.Exit(1)
	}
	server := grpc.NewServer(grpc.StatsHandler(rpcStatsHandler{}))
	testgrpc.RegisterTestServiceServer(server, service{})
	if err := server.Serve(listener); err != nil {
		fmt.Fprintf(os.Stderr, "serve: %v\n", err)
		os.Exit(1)
	}
}
