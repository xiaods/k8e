package rqlitecompat

import (
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/embedw"
	clientv3 "go.etcd.io/etcd/client/v3"
	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

// dialClientV3 returns the clientv3 client an application would use against
// the compatibility layer: plaintext gRPC on the server's address.
func dialClientV3(t *testing.T, addr string) *clientv3.Client {
	t.Helper()
	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{addr},
		DialTimeout: 5 * time.Second,
		DialOptions: []grpc.DialOption{
			grpc.WithTransportCredentials(insecure.NewCredentials()),
			grpc.WithDefaultCallOptions(grpc.MaxCallRecvMsgSize(64<<20), grpc.MaxCallSendMsgSize(64<<20)),
		},
	})
	requireNoError(t, "clientv3 dial compat server", err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// startEmbeddedEtcdClient starts the embedded etcd this repository ships today
// on loopback ports and returns a client for it. It is the reference backend of
// the differential suite, so it runs with etcd's own defaults (including the
// 1.5 MiB max-request-bytes limit).
func startEmbeddedEtcdClient(t *testing.T) *clientv3.Client {
	t.Helper()
	return startEmbeddedEtcdClientWith(t, nil)
}

// startEmbeddedEtcdClientWith starts embedded etcd like the repository ships it
// and lets a test override the config (for example MaxRequestBytes, so the size
// limits can be compared with the layer's).
func startEmbeddedEtcdClientWith(t *testing.T, mutate func(*embedw.Config)) *clientv3.Client {
	t.Helper()
	dir := t.TempDir()
	clientURL := freeLoopbackURL(t)
	peerURL := freeLoopbackURL(t)
	metricsURL := freeLoopbackURL(t)

	cfg := embedw.Config{
		Name:                 "m1-etcd",
		DataDir:              dir,
		AdvertisePeerURLs:    []url.URL{peerURL},
		AdvertiseClientURLs:  []url.URL{clientURL},
		ListenPeerURLs:       []url.URL{peerURL},
		ListenClientURLs:     []url.URL{clientURL},
		ListenMetricsURLs:    []url.URL{metricsURL},
		InitialCluster:       "m1-etcd=" + peerURL.String(),
		ClusterState:         "new",
		StrictReconfigCheck:  true,
		SnapshotCount:        10000,
		TickMs:               100,
		ElectionTimeout:      1000,
		MaxRequestBytes:      1572864,
		MaxConcurrentStreams: 1000,
		QuotaSize:            2 * 1024 * 1024 * 1024,
		Logger:               "zap",
		LogOutputs:           []string{"stderr"},
		ExtraLines:           map[string]interface{}{"log-level": "error"},
	}
	if mutate != nil {
		mutate(&cfg)
	}
	e, err := embedw.Start(cfg)
	requireNoError(t, "start embedded etcd", err)
	t.Cleanup(e.Close)

	cli, err := clientv3.New(clientv3.Config{
		Endpoints:   []string{clientURL.String()},
		DialTimeout: 10 * time.Second,
	})
	requireNoError(t, "clientv3 embedded etcd", err)
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// freeLoopbackURL reserves a loopback port and returns it as an http URL.
func freeLoopbackURL(t *testing.T) url.URL {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	requireNoError(t, "reserve loopback port", err)
	addr := ln.Addr().String()
	requireNoError(t, "close reserved port", ln.Close())
	u, err := url.Parse("http://" + addr)
	requireNoError(t, "parse loopback url", err)
	return *u
}
