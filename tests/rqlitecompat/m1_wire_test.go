package rqlitecompat

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/embedw"
	compat "github.com/xiaods/k8e/pkg/rqlitecompat"
	etcdserverpb "go.etcd.io/etcd/api/v3/etcdserverpb"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
)

// ---------------------------------------------------------------------------
// TLS
// ---------------------------------------------------------------------------

// TestM1TLSAndMTLSServeTheSameContract runs the plaintext contract over TLS and
// over mTLS: a client that trusts the CA (and, for mTLS, presents a certificate
// the CA signed) drives the layer normally, and a client that does not is
// rejected before a single RPC is applied.
func TestM1TLSAndMTLSServeTheSameContract(t *testing.T) {
	requireRQLite(t)
	certs := newTestCerts(t)

	t.Run("server TLS", func(t *testing.T) {
		cluster := StartCluster(t, 1)
		srv := startCompat(t, cluster, func(cfg *compat.Config) { cfg.TLSConfig = certs.server })
		cli := clientv3OverTLS(t, srv.addr, certs.clientTLS())

		if _, err := cli.Put(context.Background(), contractPrefix+"tls", "over-tls"); err != nil {
			t.Fatalf("put over TLS: %v", err)
		}
		get, err := cli.Get(context.Background(), contractPrefix+"tls")
		requireNoError(t, "get over TLS", err)
		requireEqual(t, "the value round-trips over TLS", string(getKvs(get)[0].Value), "over-tls")
		leases, err := cli.Leases(context.Background())
		requireNoError(t, "leases over TLS", err)
		requireEqual(t, "the lease surface is served over TLS", len(leaseStatuses(leases)), 0)

		// A handshake with an unknown CA fails before any etcd RPC runs. This
		// is the error an operator sees when the client is missing the CA.
		conn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", srv.addr, certs.untrustedTLS())
		requireTrue(t, "the handshake fails when the CA is unknown", err != nil)
		requireTrue(t, "the failure names the certificate",
			strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "x509"))
		if conn != nil {
			_ = conn.Close()
		}

		// The gRPC client behind that handshake cannot apply anything either,
		// and a client that speaks plaintext to a TLS endpoint never gets a
		// connection.
		bounded, cancelBounded := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelBounded()
		untrusted := clientv3OverTLS(t, srv.addr, certs.untrustedTLS())
		if _, err := untrusted.Put(bounded, contractPrefix+"untrusted", "x"); err == nil {
			t.Fatal("a client that does not trust the CA wrote a key")
		}
		plain := dialClientV3(t, srv.addr)
		if _, err := plain.Put(bounded, contractPrefix+"plaintext", "x"); err == nil {
			t.Fatal("a plaintext client wrote a key to a TLS endpoint")
		}

		// The rejected clients wrote nothing.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		get, err = cli.Get(ctx, contractPrefix+"untrusted")
		requireNoError(t, "read after the rejected writes", err)
		requireEqual(t, "a rejected client cannot write", getCount(get), int64(0))
		get, err = cli.Get(ctx, contractPrefix+"plaintext")
		requireNoError(t, "read after the rejected plaintext write", err)
		requireEqual(t, "a plaintext client cannot write", getCount(get), int64(0))
	})

	t.Run("mutual TLS", func(t *testing.T) {
		cluster := StartCluster(t, 1)
		mtls := certs.server.Clone()
		mtls.ClientAuth = tls.RequireAndVerifyClientCert
		srv := startCompat(t, cluster, func(cfg *compat.Config) { cfg.TLSConfig = mtls })

		cli := clientv3OverTLS(t, srv.addr, certs.mutualTLS())
		if _, err := cli.Put(context.Background(), contractPrefix+"mtls", "over-mtls"); err != nil {
			t.Fatalf("put over mTLS: %v", err)
		}
		get, err := cli.Get(context.Background(), contractPrefix+"mtls")
		requireNoError(t, "get over mTLS", err)
		requireEqual(t, "the value round-trips over mTLS", string(getKvs(get)[0].Value), "over-mtls")

		// Without the client certificate the server refuses the connection: a
		// TLS 1.2 handshake fails at once, a TLS 1.3 handshake on the first
		// application write.
		tlsConn, err := tls.DialWithDialer(&net.Dialer{Timeout: 5 * time.Second}, "tcp", srv.addr, certs.clientTLS())
		if err != nil {
			// TLS 1.2 fails the handshake itself.
			requireTrue(t, "the refusal names the certificate",
				strings.Contains(err.Error(), "certificate") || strings.Contains(err.Error(), "tls"))
		} else {
			defer tlsConn.Close()
			requireNoError(t, "set the connection deadline", tlsConn.SetDeadline(time.Now().Add(5*time.Second)))
			if _, err = tlsConn.Write([]byte("x")); err == nil {
				// TLS 1.3 completes the handshake and reports the rejected
				// client certificate on the first read instead.
				_, err = tlsConn.Read(make([]byte, 1))
			}
			requireTrue(t, "mTLS without a client certificate is refused", err != nil)
		}

		// The gRPC client without the certificate cannot write either.
		noCert := clientv3OverTLS(t, srv.addr, certs.clientTLS())
		bounded, cancelBounded := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancelBounded()
		if _, err := noCert.Put(bounded, contractPrefix+"no-cert", "x"); err == nil {
			t.Fatal("mTLS without a client certificate wrote a key")
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		get, err = cli.Get(ctx, contractPrefix+"no-cert")
		requireNoError(t, "read after the rejected mTLS write", err)
		requireEqual(t, "a certificate-less client cannot write", getCount(get), int64(0))
	})
}

// clientv3OverTLS builds the clientv3 client an application would use against a
// TLS compatibility endpoint.
func clientv3OverTLS(t *testing.T, addr string, cfg *tls.Config) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{addr},
		DialTimeout: 10 * time.Second,
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(credentials.NewTLS(cfg)),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)),
		},
	})
	requireNoError(t, "clientv3 dial TLS", err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// ---------------------------------------------------------------------------
// RangeStream
// ---------------------------------------------------------------------------

const streamPrefix = "m1stream/"

// TestM1RangeStreamDifferential streams the same ranges from the layer and from
// the embedded etcd and compares chunk by chunk: which keys each chunk carries,
// which chunk carries the header, and the count/more of the last one.
func TestM1RangeStreamDifferential(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 1)
	srv := startCompat(t, cluster, nil)
	rqliteKV := etcdserverpb.NewKVClient(srv.conn)
	rqlite := dialClientV3(t, srv.addr)

	etcd := startEmbeddedEtcdClient(t)
	etcdKV := etcdserverpb.NewKVClient(etcd.ActiveConnection())

	const keys = 25
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	for i := 0; i < keys; i++ {
		key := fmt.Sprintf("%s%02d", streamPrefix, i)
		value := fmt.Sprintf("v%02d", i)
		if _, err := rqlite.Put(ctx, key, value); err != nil {
			t.Fatalf("put %s into the layer: %v", key, err)
		}
		if _, err := etcd.Put(ctx, key, value); err != nil {
			t.Fatalf("put %s into etcd: %v", key, err)
		}
	}
	rqliteBase := baselineRevision(t, rqlite)
	etcdBase := baselineRevision(t, etcd)
	rangeEnd := []byte(clientv3.GetPrefixRangeEnd(streamPrefix))

	cases := []struct {
		name string
		req  *etcdserverpb.RangeRequest
	}{
		{"no-limit", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd}},
		{"limit-12", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, Limit: 12}},
		{"limit-25", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, Limit: keys}},
		{"limit-5", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, Limit: 5}},
		{"limit-100", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, Limit: 100}},
		{"count-only", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, CountOnly: true}},
		{"keys-only", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, KeysOnly: true}},
		{"from-key", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix + "10"), RangeEnd: []byte{0}}},
		{"single-key", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix + "07")}},
		{"empty-key", &etcdserverpb.RangeRequest{Key: []byte{}}},
		{"bad-order", &etcdserverpb.RangeRequest{
			Key: []byte(streamPrefix), RangeEnd: rangeEnd,
			SortTarget: etcdserverpb.RangeRequest_KEY, SortOrder: etcdserverpb.RangeRequest_DESCEND,
		}},
		{"revision-filter", &etcdserverpb.RangeRequest{Key: []byte(streamPrefix), RangeEnd: rangeEnd, MinModRevision: 1}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := streamText(t, rqliteKV, tc.req, rqliteBase)
			want := streamText(t, etcdKV, tc.req, etcdBase)
			requireEqual(t, "RangeStream chunking", got, want)
		})
	}

	// The last chunk's count must equal the unary Range count, so a streaming
	// client that only reads the tail still sees etcd's Count/More contract.
	t.Run("tail-matches-unary-range", func(t *testing.T) {
		unary, err := rqliteKV.Range(ctx, &etcdserverpb.RangeRequest{
			Key: []byte(streamPrefix), RangeEnd: rangeEnd, Limit: 12,
		})
		requireNoError(t, "unary range with a limit", err)
		text := streamText(t, rqliteKV, &etcdserverpb.RangeRequest{
			Key: []byte(streamPrefix), RangeEnd: rangeEnd, Limit: 12,
		}, rqliteBase)
		parts := strings.Split(text, " | ")
		last := parts[len(parts)-2]
		requireTrue(t, "the last chunk carries the unary Count and More",
			strings.Contains(last, "count="+strconv.FormatInt(unary.Count, 10)) &&
				strings.Contains(last, "more="+strconv.FormatBool(unary.More)))
	})
}

// baselineRevision is the store revision a fresh client observes, used to
// compare revisions between two backends whose counters start differently.
func baselineRevision(t *testing.T, cli *clientv3.Client) int64 {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	resp, err := cli.Get(ctx, contractPrefix+"baseline-probe")
	requireNoError(t, "read the baseline revision", err)
	return getRev(resp)
}

// streamText drains a RangeStream and renders every chunk, so two backends can
// be compared chunk by chunk. Revisions are rendered relative to the backend's
// baseline.
func streamText(t *testing.T, kv etcdserverpb.KVClient, req *etcdserverpb.RangeRequest, base int64) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	stream, err := kv.RangeStream(ctx, req)
	if err != nil {
		return "open error: " + errText(err)
	}
	var parts []string
	for {
		resp, err := stream.Recv()
		switch {
		case errors.Is(err, io.EOF):
			parts = append(parts, "EOF")
			return strings.Join(parts, " | ")
		case err != nil:
			parts = append(parts, "error "+errText(err))
			return strings.Join(parts, " | ")
		}
		parts = append(parts, rangeChunkText(resp.RangeResponse, base))
	}
}

// rangeChunkText renders one streamed chunk: its keys, whether it carries the
// header, and its count/more.
func rangeChunkText(resp *etcdserverpb.RangeResponse, base int64) string {
	kvs := make([]string, 0, len(resp.Kvs))
	for _, kv := range resp.Kvs {
		kvs = append(kvs, string(kv.Key)+"="+string(kv.Value))
	}
	text := "kvs[" + strings.Join(kvs, " ") + "]"
	if resp.Header != nil {
		text += " hdr=" + strconv.FormatInt(resp.Header.Revision-base, 10)
	}
	return text + " count=" + strconv.FormatInt(resp.Count, 10) + " more=" + strconv.FormatBool(resp.More)
}

// ---------------------------------------------------------------------------
// Request size and timeout limits
// ---------------------------------------------------------------------------

// TestM1RequestSizeLimitsMatchEtcd compares the size limits with the embedded
// etcd configured identically: a request above max-request-bytes is refused
// with etcd's own error, a request beyond the transport allowance is refused by
// gRPC, and a request below both is applied.
func TestM1RequestSizeLimitsMatchEtcd(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 1)
	const maxRequestBytes = 32 * 1024
	srv := startCompat(t, cluster, func(cfg *compat.Config) { cfg.MaxRecvMsgSize = maxRequestBytes })
	etcd := startEmbeddedEtcdClientWith(t, func(cfg *embedw.Config) { cfg.MaxRequestBytes = maxRequestBytes })

	// The raw gRPC surface is used on both sides, so the status codes are the
	// server's, not a client library's rendering of them.
	backends := []struct {
		name string
		cli  *clientv3.Client
		kv   etcdserverpb.KVClient
	}{
		{"layer", dialClientV3(t, srv.addr), srv.kv()},
		{"etcd", etcd, etcdserverpb.NewKVClient(etcd.ActiveConnection())},
	}

	// Inside max-request-bytes: both accept the write and return the value.
	small := bytes.Repeat([]byte("a"), 28*1024)
	// Above max-request-bytes, still inside the transport allowance: etcd's own
	// error, not a transport failure.
	over := bytes.Repeat([]byte("b"), maxRequestBytes+512)
	// Above the transport allowance too: gRPC refuses the message.
	beyond := bytes.Repeat([]byte("c"), maxRequestBytes+512*1024+1024)

	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()

	for _, b := range backends {
		t.Run(b.name+"/below-limit", func(t *testing.T) {
			_, err := b.kv.Put(ctx, &etcdserverpb.PutRequest{
				Key: []byte(streamPrefix + "size-ok"), Value: small,
			})
			if err != nil {
				t.Fatalf("put below max-request-bytes: %v", err)
			}
			get, err := b.cli.Get(ctx, streamPrefix+"size-ok")
			requireNoError(t, "read below max-request-bytes", err)
			requireEqual(t, "the value round-trips", len(getKvs(get)[0].Value), len(small))
		})
	}

	oversized := []struct {
		name string
		call func(etcdserverpb.KVClient) error
	}{
		{"put", func(kv etcdserverpb.KVClient) error {
			_, err := kv.Put(ctx, &etcdserverpb.PutRequest{
				Key: []byte(streamPrefix + "size-over"), Value: over,
			})
			return err
		}},
		{"txn", func(kv etcdserverpb.KVClient) error {
			_, err := kv.Txn(ctx, &etcdserverpb.TxnRequest{Success: []*etcdserverpb.RequestOp{{
				Request: &etcdserverpb.RequestOp_RequestPut{RequestPut: &etcdserverpb.PutRequest{
					Key: []byte(streamPrefix + "size-txn-over"), Value: over,
				}},
			}}})
			return err
		}},
	}
	for _, tc := range oversized {
		t.Run("over-limit/"+tc.name, func(t *testing.T) {
			var first string
			for i, b := range backends {
				err := tc.call(b.kv)
				requireCode(t, "a request above max-request-bytes", err, codes.InvalidArgument)
				requireEqual(t, "the error message matches etcd",
					status.Convert(err).Message(), "etcdserver: request is too large")
				if i == 0 {
					first = errText(err)
					continue
				}
				requireEqual(t, "both backends agree on the oversized error", first, errText(err))
			}
			// The refused requests were not applied.
			for _, b := range backends {
				get, err := b.cli.Get(ctx, streamPrefix+"size-over")
				requireNoError(t, "read after the refused put", err)
				requireEqual(t, "a refused write leaves nothing behind", getCount(get), int64(0))
				get, err = b.cli.Get(ctx, streamPrefix+"size-txn-over")
				requireNoError(t, "read after the refused txn", err)
				requireEqual(t, "a refused txn leaves nothing behind", getCount(get), int64(0))
			}
		})
	}

	t.Run("beyond-transport-allowance", func(t *testing.T) {
		var seen []codes.Code
		for _, b := range backends {
			_, err := b.kv.Put(ctx, &etcdserverpb.PutRequest{
				Key: []byte(streamPrefix + "size-beyond"), Value: beyond,
			})
			requireTrue(t, "the transport refuses the message", err != nil)
			seen = append(seen, status.Code(err))
		}
		requireEqual(t, "both backends refuse it the same way", seen[1], seen[0])
		requireEqual(t, "gRPC reports the transport refusal as ResourceExhausted",
			seen[0], codes.ResourceExhausted)
	})

	// A refused request never advanced the revision on either backend.
	for _, b := range backends {
		before := baselineRevision(t, b.cli)
		_, err := b.kv.Put(ctx, &etcdserverpb.PutRequest{
			Key: []byte(streamPrefix + "size-over"), Value: over,
		})
		if err == nil {
			t.Fatal("the oversized put was accepted")
		}
		after := baselineRevision(t, b.cli)
		if after != before {
			t.Fatalf("%s: a refused request advanced the revision: %d -> %d", b.name, before, after)
		}
	}
}

// TestM1TimeoutLimits checks the deadline paths: an expired context fails fast
// with gRPC's own code instead of hanging, and a keep-alive stream ends with
// the client deadline while the lease it renewed stays alive.
func TestM1TimeoutLimits(t *testing.T) {
	requireRQLite(t)
	cluster := StartCluster(t, 1)
	srv := startCompat(t, cluster, nil)
	cli := dialClientV3(t, srv.addr)

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if _, err := cli.Put(ctx, streamPrefix+"timeout", "v"); err != nil {
		t.Fatalf("put: %v", err)
	}

	canceled, cancelNow := context.WithCancel(ctx)
	cancelNow()
	_, err := srv.kv().Range(canceled, &etcdserverpb.RangeRequest{Key: []byte(streamPrefix + "timeout")})
	requireCode(t, "a canceled context", err, codes.Canceled)

	expired, cancelExpired := context.WithTimeout(ctx, time.Nanosecond)
	defer cancelExpired()
	time.Sleep(time.Millisecond)
	_, err = srv.kv().Range(expired, &etcdserverpb.RangeRequest{Key: []byte(streamPrefix + "timeout")})
	requireCode(t, "an expired deadline", err, codes.DeadlineExceeded)

	grant, err := cli.Grant(ctx, 300)
	requireNoError(t, "grant lease", err)
	kctx, kcancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer kcancel()
	ka, err := cli.KeepAlive(kctx, grantID(grant))
	requireNoError(t, "open the keep-alive stream", err)

	responses := 0
	deadline := time.After(30 * time.Second)
loop:
	for {
		select {
		case resp, ok := <-ka:
			if !ok {
				break loop
			}
			if resp != nil {
				responses++
			}
		case <-deadline:
			t.Fatal("the keep-alive stream did not end with the client deadline")
		}
	}
	requireTrue(t, "the keep-alive stream was served before the deadline", responses > 0)

	// The client going away does not revoke the lease.
	ttl, err := cli.TimeToLive(ctx, grantID(grant))
	requireNoError(t, "lease TTL after the keep-alive stream ended", err)
	requireEqual(t, "the lease is still the one that was granted", ttlGranted(ttl), int64(300))
}
