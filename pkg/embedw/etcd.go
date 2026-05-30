package embedw

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
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
	MaxSnapFiles         uint    `yaml:"max-snapshots"`
	MaxWALFiles          uint    `yaml:"max-wals"`
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

// yamlSecurityConfig matches etcd's securityConfig for client/server-transport-security YAML sections.
type yamlSecurityConfig struct {
	CertFile       string `yaml:"cert-file"`
	KeyFile        string `yaml:"key-file"`
	TrustedCAFile  string `yaml:"trusted-ca-file"`
	ClientCertAuth bool   `yaml:"client-cert-auth"`
}

// etcdReservedYAMLTags lists all YAML keys managed by etcdYAMLConfig struct fields.
// ExtraLines keys that match any of these are filtered out to prevent inline map conflicts.
var etcdReservedYAMLTags = map[string]bool{
	"name": true, "data-dir": true, "wal-dir": true,
	"snapshot-count": true, "snapshot-catchup-entries": true,
	"max-snapshots": true, "max-wals": true,
	"heartbeat-interval": true, "election-timeout": true,
	"initial-election-tick-advance": true,
	"max-request-bytes": true, "max-concurrent-streams": true,
	"max-txn-ops": true, "max-learners": true,
	"listen-peer-urls": true, "listen-client-urls": true,
	"listen-client-http-urls": true, "listen-metrics-urls": true,
	"initial-advertise-peer-urls": true, "advertise-client-urls": true,
	"initial-cluster": true, "initial-cluster-token": true,
	"initial-cluster-state": true,
	"force-new-cluster": true, "strict-reconfig-check": true,
	"auto-compaction-mode": true, "auto-compaction-retention": true,
	"quota-backend-bytes": true, "backend-bbolt-freelist-type": true,
	"backend-batch-limit": true, "backend-batch-interval": true,
	"client-transport-security": true, "peer-transport-security": true,
	"tls-min-version": true, "tls-max-version": true,
	"cipher-suites": true,
	"auth-token": true, "bcrypt-cost": true, "auth-token-ttl": true,
	"cors": true, "host-whitelist": true,
	"logger": true, "log-outputs": true, "log-level": true,
	"log-rotation-config-json": true,
	"enable-pprof": true, "enable-log-rotation": true,
	"grpc-keepalive-min-time": true, "grpc-keepalive-interval": true,
	"grpc-keepalive-timeout": true,
	"experimental-initial-corrupt-check": true,
	"experimental-watch-progress-notify-interval": true,
}

// etcdYAMLConfig mirrors the format expected by embed.ConfigFromFile.
type etcdYAMLConfig struct {
	Name                 string            `yaml:"name"`
	DataDir              string            `yaml:"data-dir"`
	WalDir               string            `yaml:"wal-dir"`
	SnapshotCount        int               `yaml:"snapshot-count"`
	SnapshotCatchUpEntries uint64          `yaml:"snapshot-catchup-entries,omitempty"`
	MaxSnapFiles         uint              `yaml:"max-snapshots"`
	MaxWALFiles          uint              `yaml:"max-wals"`
	TickMs               int               `yaml:"heartbeat-interval"`
	ElectionMs           int               `yaml:"election-timeout"`
	InitialElectionTickAdvance bool         `yaml:"initial-election-tick-advance,omitempty"`
	MaxRequestBytes      uint              `yaml:"max-request-bytes,omitempty"`
	MaxConcurrentStreams uint32            `yaml:"max-concurrent-streams,omitempty"`
	MaxTxnOps            uint              `yaml:"max-txn-ops,omitempty"`
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
	AutoCompactionMode   string            `yaml:"auto-compaction-mode,omitempty"`
	AutoCompactionRetention string           `yaml:"auto-compaction-retention,omitempty"`
	QuotaBackendBytes    int64             `yaml:"quota-backend-bytes"`
	BackendFreelistType  string            `yaml:"backend-bbolt-freelist-type,omitempty"`
	BackendBatchLimit    int               `yaml:"backend-batch-limit,omitempty"`
	BackendBatchInterval int64              `yaml:"backend-batch-interval,omitempty"`
	MaxLearners          int               `yaml:"max-learners,omitempty"`
	// TLS — client (nested section)
	ClientTransportSecurity yamlSecurityConfig `yaml:"client-transport-security"`
	// TLS — peer (nested section)
	PeerTransportSecurity yamlSecurityConfig `yaml:"peer-transport-security"`
	// TLS general (top-level fields)
	TlsMinVersion        string            `yaml:"tls-min-version,omitempty"`
	TlsMaxVersion        string            `yaml:"tls-max-version,omitempty"`
	CipherSuites         []string          `yaml:"cipher-suites"`
	// Auth
	AuthToken            string            `yaml:"auth-token,omitempty"`
	BcryptCost           uint              `yaml:"bcrypt-cost,omitempty"`
	AuthTokenTTL         uint              `yaml:"auth-token-ttl,omitempty"`
	// CORS / host
	CORS                 string            `yaml:"cors,omitempty"`
	HostWhitelist        string            `yaml:"host-whitelist,omitempty"`
	// Logging
	Logger               string            `yaml:"logger"`
	LogOutputs           []string          `yaml:"log-outputs"`
	LogLevel             string            `yaml:"log-level"`
	LogRotationConfig    string            `yaml:"log-rotation-config-json,omitempty"`
	EnablePprof          bool              `yaml:"enable-pprof"`
	EnableLogRotation    bool              `yaml:"enable-log-rotation"`
	// GRPC
	GRPCKeepAliveMinTime  int64            `yaml:"grpc-keepalive-min-time,omitempty"`
	GRPCKeepAliveInterval int64            `yaml:"grpc-keepalive-interval,omitempty"`
	GRPCKeepAliveTimeout  int64            `yaml:"grpc-keepalive-timeout,omitempty"`
	// Feature gates
	InitialCorruptCheck bool                `yaml:"experimental-initial-corrupt-check"`
	WatchProgressNotifyInterval int64         `yaml:"experimental-watch-progress-notify-interval,omitempty"`

	Extra map[string]interface{} `yaml:",inline"`
}

func writeConfigFile(path string, cfg Config) error {
	y := etcdYAMLConfig{
		Name:                 cfg.Name,
		DataDir:              cfg.DataDir,
		WalDir:               cfg.WALDir,
		SnapshotCount:        cfg.SnapshotCount,
		MaxSnapFiles:         cfg.MaxSnapFiles,
		MaxWALFiles:          cfg.MaxWALFiles,
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
		ClientTransportSecurity: yamlSecurityConfig{
			CertFile:       cfg.ServerCertFile,
			KeyFile:        cfg.ServerKeyFile,
			TrustedCAFile:  cfg.ServerCAFile,
			ClientCertAuth: cfg.ClientCertAuth,
		},
		// TLS — peer
		PeerTransportSecurity: yamlSecurityConfig{
			CertFile:       cfg.PeerCertFile,
			KeyFile:        cfg.PeerKeyFile,
			TrustedCAFile:  cfg.PeerCAFile,
			ClientCertAuth: cfg.PeerClientCertAuth,
		},
		// Logging
		Logger:      cfg.Logger,
		LogOutputs:  cfg.LogOutputs,
		LogLevel:    "info",
		EnablePprof: cfg.EnablePprof,
	// Feature gates
		InitialCorruptCheck:         cfg.InitialCorruptCheck,
		WatchProgressNotifyInterval: int64(cfg.WatchProgressNotifyInterval),
		// Extra args merged via yaml:",inline" — filter out reserved keys to prevent conflicts
		Extra: filterExtraLines(cfg.ExtraLines),
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

// filterExtraLines removes keys that conflict with etcdYAMLConfig struct fields.
// yaml.v2 panics when inline map keys overlap with struct yaml tags.
func filterExtraLines(extra map[string]interface{}) map[string]interface{} {
	if len(extra) == 0 {
		return nil
	}
	filtered := make(map[string]interface{}, len(extra))
	for k, v := range extra {
		if etcdReservedYAMLTags[k] {
			logrus.Warnf("Skipping etcd extra arg %q: conflicts with k8e-managed config field", k)
			continue
		}
		filtered[k] = v
	}
	if len(filtered) == 0 {
		return nil
	}
	return filtered
}