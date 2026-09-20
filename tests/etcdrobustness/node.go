package etcdrobustness

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"time"

	"github.com/xiaods/k8e/pkg/embedw"
	clientv3 "go.etcd.io/etcd/client/v3"
)

// NodeOptions describes one single-member embedded etcd node.
//
// The data directory is derived from Dir as Dir/data, so restarting a node
// with the same Dir reuses the WAL and snapshots exactly like a K8E server
// restarted against the same --data-dir.
type NodeOptions struct {
	Name         string
	Dir          string
	ClientURL    string
	PeerURL      string
	QuotaBytes   int64
	CorruptCheck bool
}

// Node is a started embedded etcd member.
type Node struct {
	opts NodeOptions
	etcd *embedw.EmbeddedEtcd
}

// DataDir is the etcd data directory inside Dir.
func (o NodeOptions) DataDir() string { return filepath.Join(o.Dir, "data") }

// LogPath is where the node writes its own log, outside the data directory.
func (o NodeOptions) LogPath() string { return filepath.Join(o.Dir, "etcd.log") }

// StartNode starts an embedded etcd member through K8E's pkg/embedw entry point.
//
// It reuses the data directory when one is already initialized and asks for
// cluster state "existing"; otherwise it bootstraps a new single-member
// cluster. The node's log goes to NodeOptions.LogPath so a fault can be
// diagnosed without the data directory being mounted.
func StartNode(opts NodeOptions) (*Node, error) {
	if opts.Name == "" {
		opts.Name = "robustness"
	}
	if opts.ClientURL == "" || opts.PeerURL == "" {
		return nil, fmt.Errorf("StartNode: ClientURL and PeerURL are required")
	}
	clientURL, err := url.Parse(opts.ClientURL)
	if err != nil {
		return nil, fmt.Errorf("parse client URL %q: %w", opts.ClientURL, err)
	}
	peerURL, err := url.Parse(opts.PeerURL)
	if err != nil {
		return nil, fmt.Errorf("parse peer URL %q: %w", opts.PeerURL, err)
	}

	clusterState := "new"
	if _, err := os.Stat(filepath.Join(opts.DataDir(), "member", "snap")); err == nil {
		clusterState = "existing"
	}

	embedded, err := embedw.Start(embedw.Config{
		Name:                 opts.Name,
		DataDir:              opts.DataDir(),
		AdvertisePeerURLs:    []url.URL{*peerURL},
		AdvertiseClientURLs:  []url.URL{*clientURL},
		ListenPeerURLs:       []url.URL{*peerURL},
		ListenClientURLs:     []url.URL{*clientURL},
		InitialCluster:       fmt.Sprintf("%s=%s", opts.Name, opts.PeerURL),
		ClusterState:         clusterState,
		StrictReconfigCheck:  true,
		SnapshotCount:        10000,
		TickMs:               100,
		ElectionTimeout:      1000,
		MaxRequestBytes:      10 * 1024 * 1024,
		MaxConcurrentStreams: 1000,
		QuotaSize:            opts.QuotaBytes,
		Logger:               "zap",
		LogOutputs:           []string{opts.LogPath()},
		InitialCorruptCheck:  opts.CorruptCheck,
	})
	if err != nil {
		return nil, fmt.Errorf("start embedded etcd on %s: %w", opts.DataDir(), err)
	}
	return &Node{opts: opts, etcd: embedded}, nil
}

// Close stops the node gracefully. It is safe to call more than once.
func (n *Node) Close() {
	if n == nil || n.etcd == nil {
		return
	}
	n.etcd.Close()
	n.etcd = nil
}

// NewClient dials a node with a bounded dial timeout.
func NewClient(endpoint string) (*clientv3.Client, error) {
	client, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{endpoint},
		DialTimeout: 5 * time.Second,
	})
	if err != nil {
		return nil, fmt.Errorf("dial etcd %s: %w", endpoint, err)
	}
	return client, nil
}

// ClientReader adapts a client to the oracle's Reader.
func ClientReader(client *clientv3.Client) Reader {
	return func(ctx context.Context, key string) (State, error) {
		response, err := client.Get(ctx, key)
		if err != nil {
			return State{}, err
		}
		if response.Count == 0 {
			return State{}, nil
		}
		kv := response.Kvs[0]
		return State{
			Exists:      true,
			ValueSHA256: HashValue(string(kv.Value)),
			Revision:    kv.ModRevision,
		}, nil
	}
}
