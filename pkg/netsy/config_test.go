package netsy

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestWithDefaults(t *testing.T) {
	got := Config{}.WithDefaults()

	if got.Binary != DefaultBinary {
		t.Errorf("Binary = %q, want %q", got.Binary, DefaultBinary)
	}
	if got.ClusterID != DefaultClusterID {
		t.Errorf("ClusterID = %q, want %q", got.ClusterID, DefaultClusterID)
	}
	if got.NodeID != DefaultNodeID {
		t.Errorf("NodeID = %q, want %q", got.NodeID, DefaultNodeID)
	}
	if got.ClientPort != DefaultClientPort || got.PeerPort != DefaultPeerPort ||
		got.ElectionPort != DefaultElectionPort || got.HealthPort != DefaultHealthPort {
		t.Errorf("ports = %d/%d/%d/%d, want defaults",
			got.ClientPort, got.PeerPort, got.ElectionPort, got.HealthPort)
	}
	if got.Storage.Provider != "s3" || got.Storage.Class != "STANDARD" || got.Storage.Encryption != "provider-managed" {
		t.Errorf("storage defaults = %+v", got.Storage)
	}
	if got.ReadyTimeout != defaultReadyTimeout {
		t.Errorf("ReadyTimeout = %s, want %s", got.ReadyTimeout, defaultReadyTimeout)
	}
	if got.HealthPollInterval != defaultPollInterval {
		t.Errorf("HealthPollInterval = %s, want %s", got.HealthPollInterval, defaultPollInterval)
	}
}

func validConfig() Config {
	return Config{
		ClusterID: "k8e",
		NodeID:    "k8e",
		DataDir:   "/var/lib/k8e/server/netsy",
		CertDir:   "/var/lib/k8e/server/netsy/tls",
		Storage:   Storage{Bucket: "k8e-netsy"},
	}.WithDefaults()
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{name: "valid"},
		{name: "uppercase cluster id", mutate: func(c *Config) { c.ClusterID = "K8E" }, wantErr: "cluster-id"},
		{name: "cluster id too long", mutate: func(c *Config) { c.ClusterID = strings.Repeat("a", 33) }, wantErr: "at most 32"},
		{name: "cluster id consecutive hyphens", mutate: func(c *Config) { c.ClusterID = "a--b" }, wantErr: "consecutive hyphens"},
		{name: "cluster id trailing hyphen", mutate: func(c *Config) { c.ClusterID = "k8e-" }, wantErr: "cluster-id"},
		{name: "empty cluster id", mutate: func(c *Config) { c.ClusterID = "" }, wantErr: "cluster-id is required"},
		{name: "node id too long", mutate: func(c *Config) { c.NodeID = strings.Repeat("b", 33) }, wantErr: "node-id"},
		{name: "missing binary", mutate: func(c *Config) { c.Binary = "" }, wantErr: "binary is required"},
		{name: "missing data dir", mutate: func(c *Config) { c.DataDir = "" }, wantErr: "data directory is required"},
		{name: "missing cert dir", mutate: func(c *Config) { c.CertDir = "" }, wantErr: "certificate directory is required"},
		{name: "missing bucket", mutate: func(c *Config) { c.Storage.Bucket = "" }, wantErr: "bucket is required"},
		{name: "bad provider", mutate: func(c *Config) { c.Storage.Provider = "azure" }, wantErr: "provider"},
		{name: "bad client port", mutate: func(c *Config) { c.ClientPort = 70000 }, wantErr: "client-port"},
		{name: "bad peer port", mutate: func(c *Config) { c.PeerPort = -1 }, wantErr: "peer-port"},
		{name: "bad election port", mutate: func(c *Config) { c.ElectionPort = 0 }, wantErr: "election-port"},
		{name: "bad health port", mutate: func(c *Config) { c.HealthPort = 99999 }, wantErr: "health-port"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := validConfig()
			if tt.mutate != nil {
				tt.mutate(&cfg)
			}
			err := cfg.Validate()
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("Validate() = %v, want nil", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("Validate() = %v, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

func TestRenderClusterConfig(t *testing.T) {
	cfg := validConfig()
	cfg.Storage = Storage{
		Provider: "s3", Bucket: "bucket", KeyPrefix: "prefix",
		Class: "STANDARD_IA", Encryption: "customer-managed", KMSKeyID: "kms-1",
	}

	data, err := cfg.RenderClusterConfig()
	if err != nil {
		t.Fatalf("RenderClusterConfig() error = %v", err)
	}
	if !json.Valid(data) {
		t.Fatalf("rendered config is not valid JSON: %s", data)
	}

	var doc clusterConfigDoc
	if err := json.Unmarshal(data, &doc); err != nil {
		t.Fatalf("Unmarshal() error = %v", err)
	}
	if doc.ClusterID != "k8e" {
		t.Errorf("cluster_id = %q, want k8e", doc.ClusterID)
	}
	if doc.Storage.BucketName != "bucket" || doc.Storage.KeyPrefix != "prefix" ||
		doc.Storage.Class != "STANDARD_IA" || doc.Storage.Encryption != "customer-managed" ||
		doc.Storage.KMSKeyID != "kms-1" || doc.Storage.Provider != "s3" {
		t.Errorf("storage = %+v", doc.Storage)
	}
	if doc.Replication.Quorum != 0 {
		t.Errorf("replication.quorum = %d, want 0 for the single-node datastore", doc.Replication.Quorum)
	}
	if doc.HeartbeatInterval == "" || doc.CompactionInterval == "" {
		t.Errorf("intervals must be set: %+v", doc)
	}
}

func TestAddresses(t *testing.T) {
	cfg := validConfig()
	if got, want := cfg.BindClient(), "127.0.0.1:2378"; got != want {
		t.Errorf("BindClient() = %q, want %q", got, want)
	}
	if got, want := cfg.AdvertisePeer(), "127.0.0.1:2381"; got != want {
		t.Errorf("AdvertisePeer() = %q, want %q", got, want)
	}
	if got, want := cfg.BindElection(), "127.0.0.1:8443"; got != want {
		t.Errorf("BindElection() = %q, want %q", got, want)
	}
	if got, want := cfg.BindHealth(), "127.0.0.1:8080"; got != want {
		t.Errorf("BindHealth() = %q, want %q", got, want)
	}
	if got, want := cfg.Endpoint(), "https://127.0.0.1:2378"; got != want {
		t.Errorf("Endpoint() = %q, want %q", got, want)
	}
	if got, want := cfg.AdvertiseElection(), "127.0.0.1:8443"; got != want {
		t.Errorf("AdvertiseElection() = %q, want %q", got, want)
	}

	cfg.AdvertiseHost = "10.0.0.7"
	if got, want := cfg.Endpoint(), "https://10.0.0.7:2378"; got != want {
		t.Errorf("Endpoint() with advertise host = %q, want %q", got, want)
	}
	hosts := cfg.ServerHosts()
	for _, want := range []string{"127.0.0.1", "::1", "localhost", "k8e", "10.0.0.7"} {
		if !contains(hosts, want) {
			t.Errorf("ServerHosts() = %v, missing %q", hosts, want)
		}
	}
}

func TestEnviron(t *testing.T) {
	cfg := validConfig()
	certs := CertPaths{
		CA: "/tls/ca.crt", ServerCert: "/tls/server.crt", ServerKey: "/tls/server.key",
		PeerClientCert: "/tls/peer.crt", PeerClientKey: "/tls/peer.key",
	}
	env := cfg.Environ("/data/netsy.jsonc", certs)
	joined := strings.Join(env, "\n")
	for _, want := range []string{
		"NETSY_CONFIG=/data/netsy.jsonc",
		"NETSY_NODE_ID=k8e",
		"NETSY_BIND_CLIENT=127.0.0.1:2378",
		"NETSY_ADVERTISE_CLIENT=127.0.0.1:2378",
		"NETSY_BIND_PEER=127.0.0.1:2381",
		"NETSY_ADVERTISE_PEER=127.0.0.1:2381",
		"NETSY_BIND_ELECTION=127.0.0.1:8443",
		"NETSY_ADVERTISE_ELECTION=127.0.0.1:8443",
		"NETSY_BIND_HEALTH=127.0.0.1:8080",
		"NETSY_TLS_CA_CERT=/tls/ca.crt",
		"NETSY_TLS_SERVER_CERT=/tls/server.crt",
		"NETSY_TLS_SERVER_KEY=/tls/server.key",
		"NETSY_TLS_CLIENT_CERT=/tls/peer.crt",
		"NETSY_TLS_CLIENT_KEY=/tls/peer.key",
		"NETSY_DATA_DIR=/var/lib/k8e/server/netsy",
		"NETSY_DEBUG=false",
	} {
		if !strings.Contains(joined, want+"\n") && !strings.HasSuffix(joined, want) {
			t.Errorf("Environ() missing %q in\n%s", want, joined)
		}
	}
}

func contains(list []string, want string) bool {
	for _, v := range list {
		if v == want {
			return true
		}
	}
	return false
}
