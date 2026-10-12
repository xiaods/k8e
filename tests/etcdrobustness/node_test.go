package etcdrobustness

import (
	"context"
	"fmt"
	"net"
	"path/filepath"
	"testing"
	"time"
)

func TestStartNodeRejectsBadOptions(t *testing.T) {
	dir := t.TempDir()
	if _, err := StartNode(NodeOptions{Dir: dir}); err == nil {
		t.Fatal("missing URLs must be rejected")
	}
	for _, opts := range []NodeOptions{
		{Dir: dir, ClientURL: "://bad", PeerURL: "http://127.0.0.1:1"},
		{Dir: dir, ClientURL: "http://127.0.0.1:1", PeerURL: "://bad"},
	} {
		opts := opts
		if _, err := StartNode(opts); err == nil {
			t.Fatalf("StartNode(%+v) must reject the malformed URL", opts)
		}
	}
}

// TestStartNodeReportsEndpointInUse covers the startup error path: a client
// port that is already bound must fail the start instead of hanging.
func TestStartNodeReportsEndpointInUse(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer listener.Close()
	_, peerURL := reserveURLs(t)

	node, startErr, panicValue := startCorruptedNode(NodeOptions{
		Name:      "robustness",
		Dir:       filepath.Join(t.TempDir(), "in-use"),
		ClientURL: fmt.Sprintf("http://127.0.0.1:%d", listener.Addr().(*net.TCPAddr).Port),
		PeerURL:   peerURL,
	})
	if node != nil {
		node.Close()
		t.Fatal("a bound client URL must not produce a running node")
	}
	if startErr == nil && panicValue == nil {
		t.Fatal("a bound client URL must be reported as a startup failure")
	}
	t.Logf("endpoint in use: err=%v panic=%v", startErr, panicValue)
}

// TestNewClientDoesNotServeUnreachableEndpoints documents that clientv3 dials
// lazily: construction succeeds, the first call fails, and Close is safe.
func TestNewClientDoesNotServeUnreachableEndpoints(t *testing.T) {
	client, err := NewClient("://bad")
	if err != nil {
		t.Fatalf("clientv3 dials lazily; construction must not fail: %v", err)
	}
	defer client.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if _, err := client.Get(ctx, "key"); err == nil {
		t.Fatal("an unreachable endpoint must not serve a request")
	}
}

func TestClientReaderReadsAndReportsErrors(t *testing.T) {
	dir := workDir(t)
	clientURL, peerURL := reserveURLs(t)
	startNode(t, NodeOptions{Name: "robustness", Dir: dir, ClientURL: clientURL, PeerURL: peerURL})
	client := mustClient(t, clientURL)
	ctx := context.Background()
	read := ClientReader(client)

	if _, err := client.Put(ctx, "reader/key", "value"); err != nil {
		t.Fatal(err)
	}
	state, err := read(ctx, "reader/key")
	if err != nil {
		t.Fatal(err)
	}
	if !state.Exists || state.ValueSHA256 != HashValue("value") || state.Revision == 0 {
		t.Fatalf("reader state = %+v", state)
	}
	absent, err := read(ctx, "reader/absent")
	if err != nil {
		t.Fatal(err)
	}
	if absent.Exists {
		t.Fatalf("absent key read as %+v", absent)
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err := read(cancelled, "reader/key"); err == nil {
		t.Fatal("a cancelled context must surface as a reader error")
	}
}
