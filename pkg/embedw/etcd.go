package embedw

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"gopkg.in/yaml.v2"
	"go.etcd.io/etcd/server/v3/embed"
)

// EmbeddedEtcd wraps an etcd embed.Etcd instance.
type EmbeddedEtcd struct {
	*embed.Etcd
}

// Config represents the settings for an embedded etcd instance.
type Config struct {
	Name                 string
	DataDir              string
	WALDir               string

	// URLs (comma-separated strings in YAML, []url.URL in code — aligned with embed.Config)
	AdvertisePeerURLs    []url.URL
	AdvertiseClientURLs  []url.URL
	ListenPeerURLs       []url.URL
	ListenClientURLs     []url.URL
	ListenClientHTTPURLs []url.URL
	ListenMetricsURLs    []url.URL

	// Cluster
	InitialCluster       string
	ClusterState         string
	ForceNewCluster      bool
	StrictReconfigCheck  bool

	// Performance tuning
	SnapshotCount        int     `yaml:"snapshot-count"`
	TickMs               int     `yaml:"heartbeat-interval"`
	ElectionTimeout      int     `yaml:"election-timeout"`
	MaxRequestBytes      uint    `yaml:"max-request-bytes"`
	MaxConcurrentStreams uint32  `yaml:"max-concurrent-streams"`
	QuotaSize            int64   `yaml:"quota-backend-bytes"`
	AutoCompactionMode   string  `yaml:"auto-compaction-mode"`
	AutoCompactionTTL    string  `yaml:"auto-compaction-retention"`

	// TLS — client (API server → etcd)
	ServerCertFile       string `yaml:"cert-file"`
	ServerKeyFile        string `yaml:"key-file"`
	ServerCAFile         string `yaml:"trusted-ca-file"`
	ClientCertAuth       bool   `yaml:"client-cert-auth"`

	// TLS — peer (etcd member ↔ etcd member)
	PeerCertFile         string `yaml:"peer-cert-file"`
	PeerKeyFile          string `yaml:"peer-key-file"`
	PeerCAFile           string `yaml:"peer-trusted-ca-file"`
	PeerClientCertAuth   bool   `yaml:"peer-client-cert-auth"`

	// Logging
	Logger               string   `yaml:"logger"`
	LogOutputs           []string `yaml:"log-outputs"`
	EnablePprof          bool     `yaml:"enable-pprof"`

	// Experimental
	InitialCorruptCheck         bool          `yaml:"experimental-initial-corrupt-check"`
	WatchProgressNotifyInterval time.Duration `yaml:"experimental-watch-progress-notify-interval"`

	// Extra YAML lines appended to config (for undocumented flags)
	ExtraLines map[string]interface{}
}

// Start starts an embedded etcd server with direct embed.StartEtcd() call.
func Start(cfg Config) (*EmbeddedEtcd, error) {
	ec, err := buildEmbedConfig(cfg)
	if err != nil {
		return nil, err
	}

	e, err := embed.StartEtcd(ec)
	if err != nil {
		return nil, fmt.Errorf("failed to start embedded etcd: %w", err)
	}

	select {
	case <-e.Server.ReadyNotify():
	case <-time.After(60 * time.Second):
		e.Server.Stop()
		return nil, context.DeadlineExceeded
	}

	return &EmbeddedEtcd{Etcd: e}, nil
}

// StartAsync starts an embedded etcd server without waiting for readiness.
func StartAsync(cfg Config) (*EmbeddedEtcd, error) {
	ec, err := buildEmbedConfig(cfg)
	if err != nil {
		return nil, err
	}

	e, err := embed.StartEtcd(ec)
	if err != nil {
		return nil, fmt.Errorf("failed to start embedded etcd: %w", err)
	}

	return &EmbeddedEtcd{Etcd: e}, nil
}

// Close gracefully shuts down the embedded etcd server.
func (e *EmbeddedEtcd) Close() {
	if e.Etcd != nil {
		e.Etcd.Close()
	}
}

func urlsJoin(urls []url.URL) string {
	ss := make([]string, len(urls))
	for i, u := range urls {
		ss[i] = u.String()
	}
	return strings.Join(ss, ",")
}

func buildEmbedConfig(cfg Config) (*embed.Config, error) {
	// Write a YAML config file, then load it via embed.ConfigFromFile.
	// This is the same approach used by the existing executor code,
	// but packaged more cleanly for direct embedding.
	if err := os.MkdirAll(cfg.DataDir, 0700); err != nil {
		return nil, fmt.Errorf("failed to create data dir: %w", err)
	}

	configFile := filepath.Join(cfg.DataDir, "config.yaml")
	if err := writeConfigFile(configFile, cfg); err != nil {
		return nil, err
	}

	ec, err := embed.ConfigFromFile(configFile)
	if err != nil {
		return nil, fmt.Errorf("failed to load etcd config from %s: %w", configFile, err)
	}

	return ec, nil
}

// etcdYAMLConfig mirrors the format expected by embed.ConfigFromFile.
type etcdYAMLConfig struct {
	Name                 string            `yaml:"name"`
	DataDir              string            `yaml:"data-dir"`
	WalDir               string            `yaml:"wal-dir"`
	SnapshotCount        int               `yaml:"snapshot-count"`
	SnapshotCatchUpEntries uint64          `yaml:"snapshot-catchup-entries"`
	MaxSnapFiles         uint              `yaml:"max-snapshots"`
	MaxWALFiles          uint              `yaml:"max-wals"`
	TickMs               int               `yaml:"heartbeat-interval"`
	ElectionMs           int               `yaml:"election-timeout"`
	InitialElectionTickAdvance bool         `yaml:"initial-election-tick-advance"`
	MaxRequestBytes      uint              `yaml:"max-request-bytes"`
	MaxConcurrentStreams uint32            `yaml:"max-concurrent-streams"`
	MaxTxnOps            uint              `yaml:"max-txn-ops"`
	ListenPeerUrls       string            `yaml:"listen-peer-urls"`
	ListenClientUrls     string            `yaml:"listen-client-urls"`
	ListenClientHTTPUrls string            `yaml:"listen-client-http-urls"`
	ListenMetricsUrls    string            `yaml:"listen-metrics-urls"`
	InitialAdvertisePeerURLs string         `yaml:"initial-advertise-peer-urls"`
	AdvertiseClientURLs  string            `yaml:"advertise-client-urls"`
	InitialCluster       string            `yaml:"initial-cluster"`
	InitialClusterToken  string            `yaml:"initial-cluster-token"`
	ClusterState         string            `yaml:"initial-cluster-state"`
	ForceNewCluster      bool              `yaml:"force-new-cluster"`
	StrictReconfigCheck  bool              `yaml:"strict-reconfig-check"`
	AutoCompactionMode   string            `yaml:"auto-compaction-mode"`
	AutoCompactionRetention string           `yaml:"auto-compaction-retention"`
	QuotaBackendBytes    int64             `yaml:"quota-backend-bytes"`
	BackendFreelistType  string            `yaml:"backend-bbolt-freelist-type"`
	BackendBatchLimit    int               `yaml:"backend-batch-limit"`
	BackendBatchInterval time.Duration     `yaml:"backend-batch-interval,omitempty"`
	MaxLearners          int               `yaml:"max-learners"`
	// TLS — client
	CertFile             string            `yaml:"cert-file"`
	KeyFile              string            `yaml:"key-file"`
	TrustedCAFile        string            `yaml:"trusted-ca-file"`
	ClientCertAuth       bool              `yaml:"client-cert-auth"`
	// TLS — peer
	PeerCertFile         string            `yaml:"peer-cert-file"`
	PeerKeyFile          string            `yaml:"peer-key-file"`
	PeerTrustedCAFile    string            `yaml:"peer-trusted-ca-file"`
	PeerClientCertAuth   bool              `yaml:"peer-client-cert-auth"`
	// TLS general
	TlsMinVersion        string            `yaml:"tls-min-version"`
	TlsMaxVersion        string            `yaml:"tls-max-version"`
	CipherSuites         string            `yaml:"cipher-suites"`
	PeerSkipSANVerify    bool              `yaml:"peer-skip-client-san-verification"`
	// Auth
	AuthToken            string            `yaml:"auth-token"`
	BcryptCost           uint              `yaml:"bcrypt-cost"`
	AuthTokenTTL         uint              `yaml:"auth-token-ttl"`
	// CORS / host
	CORS                 string            `yaml:"cors"`
	HostWhitelist        string            `yaml:"host-whitelist"`
	// Logging
	Logger               string            `yaml:"logger"`
	LogOutputs           []string          `yaml:"log-outputs"`
	LogLevel             string            `yaml:"log-level"`
	LogRotationConfig    string            `yaml:"log-rotation-config-json"`
	EnablePprof          bool              `yaml:"enable-pprof"`
	EnableLogRotation    bool              `yaml:"enable-log-rotation"`
	// GRPC
	GRPCKeepAliveMinTime time.Duration     `yaml:"grpc-keepalive-min-time"`
	GRPCKeepAliveInterval time.Duration    `yaml:"grpc-keepalive-interval"`
	GRPCKeepAliveTimeout time.Duration     `yaml:"grpc-keepalive-timeout"`
	// Feature gates
	InitialCorruptCheck bool                `yaml:"experimental-initial-corrupt-check"`
	WatchProgressNotifyInterval time.Duration `yaml:"experimental-watch-progress-notify-interval,omitempty"`

	Extra map[string]interface{} `yaml:",inline"`
}

func writeConfigFile(path string, cfg Config) error {
	y := etcdYAMLConfig{
		Name:                 cfg.Name,
		DataDir:              cfg.DataDir,
		WalDir:               cfg.WALDir,
		SnapshotCount:        cfg.SnapshotCount,
		TickMs:               cfg.TickMs,
		ElectionMs:           cfg.ElectionTimeout,
		MaxRequestBytes:      cfg.MaxRequestBytes,
		MaxConcurrentStreams: cfg.MaxConcurrentStreams,
		QuotaBackendBytes:    cfg.QuotaSize,
		AutoCompactionMode:   cfg.AutoCompactionMode,
		AutoCompactionRetention: cfg.AutoCompactionTTL,
		ListenPeerUrls:       urlsJoin(cfg.ListenPeerURLs),
		ListenClientUrls:     urlsJoin(cfg.ListenClientURLs),
		ListenClientHTTPUrls: urlsJoin(cfg.ListenClientHTTPURLs),
		ListenMetricsUrls:    urlsJoin(cfg.ListenMetricsURLs),
		InitialAdvertisePeerURLs: urlsJoin(cfg.AdvertisePeerURLs),
		AdvertiseClientURLs:  urlsJoin(cfg.AdvertiseClientURLs),
		InitialCluster:       cfg.InitialCluster,
		InitialClusterToken:  "etcd-cluster",
		ClusterState:         cfg.ClusterState,
		ForceNewCluster:      cfg.ForceNewCluster,
		StrictReconfigCheck:  cfg.StrictReconfigCheck,
		// TLS — client
		CertFile:       cfg.ServerCertFile,
		KeyFile:        cfg.ServerKeyFile,
		TrustedCAFile:  cfg.ServerCAFile,
		ClientCertAuth: cfg.ClientCertAuth,
		// TLS — peer
		PeerCertFile:       cfg.PeerCertFile,
		PeerKeyFile:        cfg.PeerKeyFile,
		PeerTrustedCAFile:  cfg.PeerCAFile,
		PeerClientCertAuth: cfg.PeerClientCertAuth,
		// Logging
		Logger:      cfg.Logger,
		LogOutputs:  cfg.LogOutputs,
		LogLevel:    "info",
		EnablePprof: cfg.EnablePprof,
		// Feature gates
		InitialCorruptCheck:         cfg.InitialCorruptCheck,
		WatchProgressNotifyInterval: cfg.WatchProgressNotifyInterval,
		// Keepalive
		GRPCKeepAliveMinTime:  5 * time.Second,
		GRPCKeepAliveInterval: 2 * time.Hour,
		GRPCKeepAliveTimeout:  20 * time.Second,
		// Extra args merged via yaml:",inline"
		Extra: cfg.ExtraLines,
	}

	data, err := yaml.Marshal(&y)
	if err != nil {
		return fmt.Errorf("failed to marshal etcd config: %w", err)
	}

	// Write the config file
	if err := os.WriteFile(path, data, 0600); err != nil {
		return fmt.Errorf("failed to write etcd config file: %w", err)
	}

	return nil
}