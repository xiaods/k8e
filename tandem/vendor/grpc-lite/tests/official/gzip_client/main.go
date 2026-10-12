package main

import (
	"context"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials/insecure"
	"google.golang.org/grpc/encoding/gzip"
	testgrpc "google.golang.org/grpc/interop/grpc_testing"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

const (
	requestSize  = 271828
	responseSize = 314159
)

var (
	serverHost = flag.String("server_host", "127.0.0.1", "server host")
	serverPort = flag.Int("server_port", 10000, "server port")
	testCase   = flag.String("test_case", "gzip_request", "gzip interop test case")
)

type rpcStats struct {
	mu                       sync.Mutex
	outboundEncoding         string
	inboundEncoding          string
	outboundLength           int
	outboundCompressedLength int
	inboundLength            int
	inboundCompressedLength  int
}

func (s *rpcStats) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}

func (s *rpcStats) HandleRPC(_ context.Context, event stats.RPCStats) {
	s.mu.Lock()
	defer s.mu.Unlock()
	switch event := event.(type) {
	case *stats.OutHeader:
		s.outboundEncoding = event.Compression
	case *stats.InHeader:
		s.inboundEncoding = event.Compression
	case *stats.OutPayload:
		s.outboundLength = event.Length
		s.outboundCompressedLength = event.CompressedLength
	case *stats.InPayload:
		s.inboundLength = event.Length
		s.inboundCompressedLength = event.CompressedLength
	}
}

func (*rpcStats) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}

func (*rpcStats) HandleConn(context.Context, stats.ConnStats) {}

func (s *rpcStats) encodings() (string, string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outboundEncoding, s.inboundEncoding
}

func (s *rpcStats) payloadLengths() (int, int, int, int) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outboundLength, s.outboundCompressedLength, s.inboundLength, s.inboundCompressedLength
}

type namedCompressor struct {
	grpc.Compressor
	name string
}

func (c namedCompressor) Type() string { return c.name }

func main() {
	flag.Parse()
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run() error {
	tracker := &rpcStats{}
	dialOptions := []grpc.DialOption{
		grpc.WithTransportCredentials(insecure.NewCredentials()),
		grpc.WithStatsHandler(tracker),
	}

	callOptions := []grpc.CallOption{}
	expectRequestCompression := "identity"
	expectResponseCompression := "identity"
	expectCode := codes.OK
	request := newRequest()

	switch *testCase {
	case "identity_unary":
		request.ExpectCompressed = &testgrpc.BoolValue{Value: false}
		request.ResponseCompressed = &testgrpc.BoolValue{Value: false}
	case "gzip_request":
		callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
		expectRequestCompression = gzip.Name
		request.ExpectCompressed = &testgrpc.BoolValue{Value: true}
		request.ResponseCompressed = &testgrpc.BoolValue{Value: false}
	case "gzip_response":
		expectResponseCompression = gzip.Name
		request.ExpectCompressed = &testgrpc.BoolValue{Value: false}
		request.ResponseCompressed = &testgrpc.BoolValue{Value: true}
	case "identity_expectation_error":
		request.ExpectCompressed = &testgrpc.BoolValue{Value: true}
		expectCode = codes.InvalidArgument
	case "gzip_expectation_error":
		callOptions = append(callOptions, grpc.UseCompressor(gzip.Name))
		expectRequestCompression = gzip.Name
		request.ExpectCompressed = &testgrpc.BoolValue{Value: false}
		expectCode = codes.InvalidArgument
	case "unsupported_request_encoding":
		dialOptions = append(dialOptions, grpc.WithCompressor(namedCompressor{
			Compressor: grpc.NewGZIPCompressor(),
			name:       "unsupported",
		}))
		expectRequestCompression = "unsupported"
		expectCode = codes.Unimplemented
	default:
		return fmt.Errorf("unsupported test case %q", *testCase)
	}

	target := net.JoinHostPort(*serverHost, strconv.Itoa(*serverPort))
	conn, err := grpc.NewClient(target, dialOptions...)
	if err != nil {
		return fmt.Errorf("create client: %w", err)
	}
	defer conn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	response, callErr := testgrpc.NewTestServiceClient(conn).UnaryCall(ctx, request, callOptions...)
	if got := status.Code(callErr); got != expectCode {
		return fmt.Errorf("UnaryCall status = %s (%v), want %s", got, callErr, expectCode)
	}

	outboundEncoding, inboundEncoding := tracker.encodings()
	if !encodingMatches(outboundEncoding, expectRequestCompression) {
		return fmt.Errorf("request compression = %q, want %q", outboundEncoding, expectRequestCompression)
	}
	if expectCode != codes.OK {
		return nil
	}
	if !encodingMatches(inboundEncoding, expectResponseCompression) {
		return fmt.Errorf("response compression = %q, want %q", inboundEncoding, expectResponseCompression)
	}
	payload := response.GetPayload()
	if payload == nil || payload.GetType() != testgrpc.PayloadType_COMPRESSABLE {
		return fmt.Errorf("response has invalid payload")
	}
	if len(payload.GetBody()) != responseSize {
		return fmt.Errorf("response payload size = %d, want %d", len(payload.GetBody()), responseSize)
	}
	outLength, outCompressedLength, inLength, inCompressedLength := tracker.payloadLengths()
	if err := checkPayloadCompression("request", expectRequestCompression, outLength, outCompressedLength); err != nil {
		return err
	}
	return checkPayloadCompression("response", expectResponseCompression, inLength, inCompressedLength)
}

func newRequest() *testgrpc.SimpleRequest {
	return &testgrpc.SimpleRequest{
		ResponseType: testgrpc.PayloadType_COMPRESSABLE,
		ResponseSize: responseSize,
		Payload: &testgrpc.Payload{
			Type: testgrpc.PayloadType_COMPRESSABLE,
			Body: make([]byte, requestSize),
		},
	}
}

func encodingMatches(got, want string) bool {
	if want == "identity" {
		return got == "" || got == "identity"
	}
	return got == want
}

func checkPayloadCompression(direction, encoding string, length, compressedLength int) error {
	if length == 0 {
		return fmt.Errorf("%s payload stats were not recorded", direction)
	}
	if encoding == "gzip" && compressedLength >= length {
		return fmt.Errorf("%s gzip payload length = %d, uncompressed length = %d", direction, compressedLength, length)
	}
	if encoding == "identity" && compressedLength != length {
		return fmt.Errorf("%s identity payload length = %d, uncompressed length = %d", direction, compressedLength, length)
	}
	return nil
}
