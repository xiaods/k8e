//go:build tandem_integration

package tandem

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	pb "go.etcd.io/etcd/api/v3/etcdserverpb"
	mvccpb "go.etcd.io/etcd/api/v3/mvccpb"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// Requires a real Linux Tandem binary; no protocol mocks or external PKI tools.
func TestTandemWatchTLS(t *testing.T) {
	binary := os.Getenv("TANDEM_TEST_BINARY")
	if binary == "" {
		t.Fatal("set TANDEM_TEST_BINARY to a Linux Tandem executable")
	}
	for _, mutual := range []bool{false, true} {
		t.Run(fmt.Sprintf("mtls=%t", mutual), func(t *testing.T) {
			endpoint, cfg := startTestTandem(t, binary, mutual)
			ctx, cli, cancel := newTestClient(t, endpoint, cfg, 20*time.Second)
			defer cancel()
			waitForReady(t, ctx, cli, 200*time.Millisecond, 5*time.Second, "gRPC readiness")
			testWatchLifecycle(t, ctx, cli)
			testTLSRejection(t, endpoint, cfg, mutual)
		})
	}
}

// newTestClient returns a client plus the cancel func for its context. The
// caller must defer the cancel inside its own subtest so teardown runs after
// the stream is done and before the Tandem process cleanup registered by
// startTestTandem; cancelling from a t.Cleanup hook would race the stream.
func newTestClient(t *testing.T, endpoint string, cfg *tls.Config, timeout time.Duration) (context.Context, *clientv3.Client, context.CancelFunc) {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, TLS: cfg, DialTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cli.Close() })
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	return ctx, cli, cancel
}

// Wait for the actual gRPC listener rather than assuming process start == readiness.
func waitForReady(t *testing.T, ctx context.Context, cli *clientv3.Client, probeTimeout, overall time.Duration, what string) {
	t.Helper()
	var err error
	for deadline := time.Now().Add(overall); time.Now().Before(deadline); {
		probe, stop := context.WithTimeout(ctx, probeTimeout)
		_, err = cli.Get(probe, "ready")
		stop()
		if err == nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("%s: %v", what, err)
}

func testWatchLifecycle(t *testing.T, ctx context.Context, cli *clientv3.Client) {
	t.Helper()
	stream, err := pb.NewWatchClient(cli.ActiveConnection()).Watch(ctx)
	if err != nil {
		t.Fatal(err)
	}
	w := &watchFixture{t: t, stream: stream}
	first, compactRev := w.watchUpdates(ctx, cli)
	w.watchReplay(first)
	w.watchFilters(ctx, cli, first, compactRev)
}

// watchFixture drives a single watch stream through the nested protobuf requests.
type watchFixture struct {
	t      *testing.T
	stream pb.Watch_WatchClient
}

func (w *watchFixture) create(r *pb.WatchCreateRequest) int64 {
	w.t.Helper()
	if err := w.stream.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CreateRequest{CreateRequest: r}}); err != nil {
		w.t.Fatal(err)
	}
	ack, err := w.stream.Recv()
	if err != nil || ack == nil || !ack.Created {
		w.t.Fatalf("create: %+v %v", ack, err)
	}
	return ack.WatchId
}

func (w *watchFixture) recv() *pb.WatchResponse {
	w.t.Helper()
	response, err := w.stream.Recv()
	if err != nil {
		w.t.Fatal(err)
	}
	return response
}

func (w *watchFixture) expectEvents(watchID int64, n int, what string) *pb.WatchResponse {
	w.t.Helper()
	got := w.recv()
	if got.WatchId != watchID || len(got.Events) != n {
		w.t.Fatalf("%s: %+v", what, got)
	}
	return got
}

// cancel sends a cancel request and drains its acknowledgement. The server
// replies with a canceled response on the same stream, so the caller must not
// leave it queued where the next create's Recv would read it as the create ack.
func (w *watchFixture) cancel(watchID int64) {
	w.t.Helper()
	if err := w.stream.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_CancelRequest{CancelRequest: &pb.WatchCancelRequest{WatchId: watchID}}}); err != nil {
		w.t.Fatal(err)
	}
	got := w.recv()
	if !got.Canceled || got.WatchId != watchID {
		w.t.Fatalf("cancel: %+v", got)
	}
}

func (w *watchFixture) progress() {
	w.t.Helper()
	if err := w.stream.Send(&pb.WatchRequest{RequestUnion: &pb.WatchRequest_ProgressRequest{ProgressRequest: &pb.WatchProgressRequest{}}}); err != nil {
		w.t.Fatal(err)
	}
}

// watchUpdates covers the live watch path: create, split transaction, failure
// branch, then cancel. It returns the first response and the transaction
// revision later phases compact against.
func (w *watchFixture) watchUpdates(ctx context.Context, cli *clientv3.Client) (*clientv3.PutResponse, int64) {
	w.t.Helper()
	watchID := w.create(&pb.WatchCreateRequest{Key: []byte("p/"), RangeEnd: []byte("p0"), PrevKv: true})
	first, err := cli.Put(ctx, "p/a", "old")
	if err != nil {
		w.t.Fatal(err)
	}
	initial := w.expectEvents(watchID, 1, "initial put")
	if initial.Events[0].PrevKv != nil {
		w.t.Fatalf("initial put published prevKV: %+v", initial)
	}
	txn, err := cli.Txn(ctx).Then(clientv3.OpPut("p/a", "new"), clientv3.OpPut("p/b", "b")).Commit()
	if err != nil {
		w.t.Fatal(err)
	}
	split := w.expectEvents(watchID, 2, "transaction split/lost")
	for _, event := range split.Events {
		if event.Kv.ModRevision != txn.Header.Revision {
			w.t.Fatalf("transaction revision: %+v", split)
		}
	}
	if split.Events[0].PrevKv == nil || string(split.Events[0].PrevKv.Value) != "old" {
		w.t.Fatalf("previous KV: %+v", split)
	}
	// Failed compares must only publish the selected failure branch.
	_, err = cli.Txn(ctx).If(clientv3.Compare(clientv3.Value("p/a"), "=", "wrong")).
		Then(clientv3.OpPut("p/never", "bad")).Else(clientv3.OpDelete("p/b")).Commit()
	if err != nil {
		w.t.Fatal(err)
	}
	failure := w.expectEvents(watchID, 1, "failure branch")
	w.expectDeleteEvent(failure, "p/b", "b")
	// Cancel is a nested protobuf request; the same stream stays usable.
	w.cancel(watchID)
	return first, txn.Header.Revision
}

// expectDeleteEvent asserts a single DELETE event carrying the previous value.
func (w *watchFixture) expectDeleteEvent(got *pb.WatchResponse, key, prevValue string) {
	w.t.Helper()
	if got.Events[0].Type != mvccpb.DELETE || string(got.Events[0].Kv.Key) != key {
		w.t.Fatalf("failure branch event: %+v", got)
	}
	if got.Events[0].PrevKv == nil || string(got.Events[0].PrevKv.Value) != prevValue {
		w.t.Fatalf("delete prevKV: %+v", got)
	}
}

// watchReplay replays history from an earlier revision and cancels again.
func (w *watchFixture) watchReplay(first *clientv3.PutResponse) {
	w.t.Helper()
	replayID := w.create(&pb.WatchCreateRequest{Key: []byte("p/"), RangeEnd: []byte("p0"), StartRevision: first.Header.Revision, PrevKv: true})
	for _, n := range []int{1, 2, 1} {
		w.expectEvents(replayID, n, "replay grouping")
	}
	w.cancel(replayID)
}

// watchFilters covers NOPUT suppression, prevKv on delete, and compaction.
func (w *watchFixture) watchFilters(ctx context.Context, cli *clientv3.Client, first *clientv3.PutResponse, compactRev int64) {
	w.t.Helper()
	// NOPUT suppresses writes; an explicit progress response proves the queue drained.
	filteredID := w.create(&pb.WatchCreateRequest{Key: []byte("p/"), RangeEnd: []byte("p0"), Filters: []pb.WatchCreateRequest_FilterType{pb.WatchCreateRequest_NOPUT}})
	if _, err := cli.Put(ctx, "p/filter", "value"); err != nil {
		w.t.Fatal(err)
	}
	w.progress()
	if got := w.recv(); len(got.Events) != 0 || got.WatchId != -1 {
		w.t.Fatalf("NOPUT filter: %+v", got)
	}
	if _, err := cli.Delete(ctx, "p/filter"); err != nil {
		w.t.Fatal(err)
	}
	deleted := w.expectEvents(filteredID, 1, "prev_kv=false")
	if deleted.Events[0].PrevKv != nil {
		w.t.Fatalf("prev_kv=false published prevKV: %+v", deleted)
	}
	if _, err := cli.Compact(ctx, compactRev); err != nil {
		w.t.Fatal(err)
	}
	compactID := w.create(&pb.WatchCreateRequest{Key: []byte("p/"), RangeEnd: []byte("p0"), StartRevision: first.Header.Revision})
	if got := w.recv(); !got.Canceled || got.WatchId != compactID || got.CompactRevision != compactRev {
		w.t.Fatalf("compaction: %+v", got)
	}
}

func testTLSRejection(t *testing.T, endpoint string, cfg *tls.Config, mutual bool) {
	t.Helper()
	if mutual {
		missing := cfg.Clone()
		missing.Certificates = nil
		expectTLSRejected(t, endpoint, missing)
		// A certificate signed by an unrelated CA must also be rejected.
		_, _, alien := testPKI(t)
		untrusted := cfg.Clone()
		untrusted.Certificates = alien.Certificates
		expectTLSRejected(t, endpoint, untrusted)
	}
	unknownServer := cfg.Clone()
	unknownServer.RootCAs = x509.NewCertPool()
	expectTLSRejected(t, endpoint, unknownServer)
}

func expectTLSRejected(t *testing.T, endpoint string, cfg *tls.Config) {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{Endpoints: []string{endpoint}, TLS: cfg, DialTimeout: time.Second})
	if err != nil {
		return
	}
	defer cli.Close()
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := cli.Get(ctx, "ready"); err == nil {
		t.Fatal("untrusted TLS connection accepted")
	}
}

func startTestTandem(t *testing.T, binary string, mutual bool) (string, *tls.Config) {
	t.Helper()
	rqliteURL, _ := startTestRqlite(t, t.TempDir(), "", "")
	cert, key, cfg := testPKI(t)
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	listener.Close()
	log, err := os.Create(filepath.Join(t.TempDir(), "tandem.log"))
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(binary)
	cmd.Env = append(os.Environ(), "TANDEM_RQLITE_ADDR="+rqliteURL, "TANDEM_DATA_DIR="+t.TempDir(), "TANDEM_LISTEN_ADDR="+address, "TANDEM_TLS_CERT_FILE="+cert, "TANDEM_TLS_KEY_FILE="+key,
		"TANDEM_TLS_CA_FILE="+filepath.Join(filepath.Dir(cert), "ca.pem"), "TANDEM_TLS_REQUIRE_CLIENT_CERT="+strconv.FormatBool(mutual))
	cmd.Stdout = log
	cmd.Stderr = log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Kill()
		cmd.Wait()
		log.Close()
		if t.Failed() {
			data, _ := os.ReadFile(log.Name())
			t.Log(string(data))
		}
	})
	return "https://" + address, cfg
}

func testPKI(t *testing.T) (string, string, *tls.Config) {
	t.Helper()
	dir := t.TempDir()
	caKey, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	ca := &x509.Certificate{SerialNumber: big.NewInt(1), Subject: pkix.Name{CommonName: "Tandem test CA"}, NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true, KeyUsage: x509.KeyUsageCertSign}
	caDER, err := x509.CreateCertificate(rand.Reader, ca, ca, &caKey.PublicKey, caKey)
	if err != nil {
		t.Fatal(err)
	}
	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: caDER})
	if err := os.WriteFile(filepath.Join(dir, "ca.pem"), caPEM, 0600); err != nil {
		t.Fatal(err)
	}
	issue := func(name string, usage x509.ExtKeyUsage) (string, string, tls.Certificate) {
		key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
		if err != nil {
			t.Fatal(err)
		}
		template := &x509.Certificate{SerialNumber: big.NewInt(int64(usage) + 2), Subject: pkix.Name{CommonName: name}, NotBefore: ca.NotBefore, NotAfter: ca.NotAfter, KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{usage}, IPAddresses: []net.IP{net.ParseIP("127.0.0.1")}}
		der, err := x509.CreateCertificate(rand.Reader, template, ca, &key.PublicKey, caKey)
		if err != nil {
			t.Fatal(err)
		}
		keyDER, err := x509.MarshalPKCS8PrivateKey(key)
		if err != nil {
			t.Fatal(err)
		}
		cp := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
		kp := pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})
		certFile, keyFile := filepath.Join(dir, name+".pem"), filepath.Join(dir, name+".key")
		if err := os.WriteFile(certFile, cp, 0600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(keyFile, kp, 0600); err != nil {
			t.Fatal(err)
		}
		pair, err := tls.X509KeyPair(cp, kp)
		if err != nil {
			t.Fatal(err)
		}
		return certFile, keyFile, pair
	}
	cert, key, _ := issue("server", x509.ExtKeyUsageServerAuth)
	_, _, client := issue("client", x509.ExtKeyUsageClientAuth)
	roots := x509.NewCertPool()
	roots.AppendCertsFromPEM(caPEM)
	return cert, key, &tls.Config{RootCAs: roots, Certificates: []tls.Certificate{client}, MinVersion: tls.VersionTLS12}
}
