package netsy

import (
	"encoding/json"
	"fmt"
	"net"
	"regexp"
	"strconv"
	"strings"
	"time"
)

// identifierRegexp matches the identifiers Netsy accepts for cluster_id and
// node_id: lowercase alphanumerics and hyphens, no leading/trailing/consecutive
// hyphens, at most 32 characters (internal/config.ValidateIdentifier).
var identifierRegexp = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]*[a-z0-9])?$`)

// Storage describes the object storage Netsy persists to. It mirrors the
// "storage" section of the Netsy cluster config. Credentials are NOT part of
// this struct: Netsy reads the standard provider SDK environment variables
// (AWS_*, GOOGLE_APPLICATION_CREDENTIALS).
type Storage struct {
	Provider   string
	Bucket     string
	KeyPrefix  string
	Class      string
	Encryption string
	KMSKeyID   string
}

// Config is the per-node Netsy configuration k8e renders and runs.
type Config struct {
	// Binary is the netsy executable to run (PATH lookup or absolute path).
	Binary string
	// ClusterID is the Netsy cluster identifier. It is embedded in the TLS
	// certificates and must match the cluster_id in the rendered config.
	ClusterID string
	// NodeID is this Netsy node's identifier. It is embedded in the TLS
	// certificates and the per-node environment.
	NodeID string
	// DataDir holds the rendered JSONC config and the local SQLite database.
	DataDir string
	// CertDir holds the generated mTLS PKI.
	CertDir string
	// AdvertiseHost is the host other nodes and clients use to reach this
	// Netsy node. Defaults to the loopback address for the single-node case.
	AdvertiseHost string
	// ClientPort serves the etcd-compatible client API (kube-apiserver).
	ClientPort int
	// PeerPort serves the replication/peer API.
	PeerPort int
	// ElectionPort serves the election health server.
	ElectionPort int
	// HealthPort serves the HTTP /health endpoint.
	HealthPort int
	// Storage is the object storage Netsy persists to.
	Storage Storage
	// Verbose enables Netsy debug logging (NETSY_DEBUG).
	Verbose bool
	// ReadyTimeout bounds how long Start waits for the client API health
	// endpoint before giving up.
	ReadyTimeout time.Duration
	// HealthPollInterval is the delay between readiness probes.
	HealthPollInterval time.Duration
}

// WithDefaults fills in the Netsy defaults for any unset field.
// The defaults are applied to a copy so the receiver stays untouched.
func (c Config) WithDefaults() Config {
	d := c
	if d.Binary == "" {
		d.Binary = DefaultBinary
	}
	if d.ClusterID == "" {
		d.ClusterID = DefaultClusterID
	}
	if d.NodeID == "" {
		d.NodeID = DefaultNodeID
	}
	if d.ClientPort == 0 {
		d.ClientPort = DefaultClientPort
	}
	if d.PeerPort == 0 {
		d.PeerPort = DefaultPeerPort
	}
	if d.ElectionPort == 0 {
		d.ElectionPort = DefaultElectionPort
	}
	if d.HealthPort == 0 {
		d.HealthPort = DefaultHealthPort
	}
	if d.Storage.Provider == "" {
		d.Storage.Provider = "s3"
	}
	if d.Storage.Class == "" {
		d.Storage.Class = "STANDARD"
	}
	if d.Storage.Encryption == "" {
		d.Storage.Encryption = "provider-managed"
	}
	if d.ReadyTimeout <= 0 {
		d.ReadyTimeout = defaultReadyTimeout
	}
	if d.HealthPollInterval <= 0 {
		d.HealthPollInterval = defaultPollInterval
	}
	return d
}

// Validate checks the settings Netsy requires before the process is started.
func (c Config) Validate() error {
	if err := validateIdentifier(c.ClusterID, "cluster-id"); err != nil {
		return err
	}
	if err := validateIdentifier(c.NodeID, "node-id"); err != nil {
		return err
	}
	if c.Binary == "" {
		return fmt.Errorf("netsy binary is required")
	}
	if c.DataDir == "" {
		return fmt.Errorf("netsy data directory is required")
	}
	if c.CertDir == "" {
		return fmt.Errorf("netsy certificate directory is required")
	}
	if c.Storage.Bucket == "" {
		return fmt.Errorf("netsy storage bucket is required")
	}
	switch c.Storage.Provider {
	case "s3", "gcs":
	default:
		return fmt.Errorf("netsy storage provider must be \"s3\" or \"gcs\", got %q", c.Storage.Provider)
	}
	for _, p := range []struct {
		name string
		port int
	}{
		{"client-port", c.ClientPort},
		{"peer-port", c.PeerPort},
		{"election-port", c.ElectionPort},
		{"health-port", c.HealthPort},
	} {
		if p.port < 1 || p.port > 65535 {
			return fmt.Errorf("netsy %s must be between 1 and 65535, got %d", p.name, p.port)
		}
	}
	return nil
}

func validateIdentifier(value, field string) error {
	if value == "" {
		return fmt.Errorf("netsy %s is required", field)
	}
	if len(value) > 32 {
		return fmt.Errorf("netsy %s must be at most 32 characters", field)
	}
	if strings.Contains(value, "--") {
		return fmt.Errorf("netsy %s must not contain consecutive hyphens", field)
	}
	if !identifierRegexp.MatchString(value) {
		return fmt.Errorf("netsy %s must be lowercase alphanumeric with hyphens, no leading/trailing hyphens", field)
	}
	return nil
}

// advertiseHost returns the host others dial for this node.
func (c Config) advertiseHost() string {
	if c.AdvertiseHost != "" {
		return c.AdvertiseHost
	}
	return "127.0.0.1"
}

// BindClient is the client API bind address.
func (c Config) BindClient() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.ClientPort))
}

// AdvertiseClient is the client API advertise address.
func (c Config) AdvertiseClient() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.ClientPort))
}

// BindPeer is the peer API bind address.
func (c Config) BindPeer() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.PeerPort))
}

// AdvertisePeer is the peer API advertise address.
func (c Config) AdvertisePeer() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.PeerPort))
}

// BindElection is the election health server bind address.
func (c Config) BindElection() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.ElectionPort))
}

// AdvertiseElection is the election health server advertise address.
func (c Config) AdvertiseElection() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.ElectionPort))
}

// BindHealth is the HTTP health endpoint bind address.
func (c Config) BindHealth() string {
	return net.JoinHostPort(c.advertiseHost(), strconv.Itoa(c.HealthPort))
}

// Endpoint is the etcd-compatible client endpoint kube-apiserver and
// etcdstorage connect to.
func (c Config) Endpoint() string { return "https://" + c.AdvertiseClient() }

// ServerHosts lists the hosts the Netsy server certificate must be valid for:
// every advertise address plus the loopback aliases k8e itself connects to.
func (c Config) ServerHosts() []string {
	hosts := []string{"127.0.0.1", "::1", "localhost", c.NodeID}
	if h := c.AdvertiseHost; h != "" {
		hosts = append(hosts, h)
	}
	return hosts
}

// clusterConfigDoc is the on-disk JSONC document rendering. It is a subset of
// the Netsy cluster config schema; the values are the single-node defaults.
type clusterConfigDoc struct {
	ClusterID          string         `json:"cluster_id"`
	Storage            storageDoc     `json:"storage"`
	HeartbeatInterval  string         `json:"heartbeat_interval"`
	Elector            electorDoc     `json:"elector"`
	Replication        replicationDoc `json:"replication"`
	Snapshot           snapshotDoc    `json:"snapshot"`
	CompactionInterval string         `json:"compaction_interval"`
}

type storageDoc struct {
	Provider   string `json:"provider"`
	BucketName string `json:"bucket_name"`
	KeyPrefix  string `json:"key_prefix,omitempty"`
	Class      string `json:"class"`
	Encryption string `json:"encryption"`
	KMSKeyID   string `json:"kms_key_id,omitempty"`
}

type electorDoc struct {
	DegradationCount      int    `json:"degradation_count"`
	DeregistrationTimeout string `json:"deregistration_timeout"`
	PrimaryPriorTimeout   string `json:"primary_prior_timeout"`
}

type replicationDoc struct {
	Quorum           int            `json:"quorum"`
	DegradationCount int            `json:"degradation_count"`
	ChunkBuffer      chunkBufferDoc `json:"chunk_buffer"`
}

type chunkBufferDoc struct {
	ThresholdSizeMB     int `json:"threshold_size_mb"`
	ThresholdAgeMinutes int `json:"threshold_age_minutes"`
}

type snapshotDoc struct {
	ThresholdRecords    int64 `json:"threshold_records"`
	ThresholdSizeMB     int64 `json:"threshold_size_mb"`
	ThresholdAgeMinutes int64 `json:"threshold_age_minutes"`
}

// RenderClusterConfig renders the Netsy JSONC cluster config. The document is
// plain JSON, which is a valid JSONC document, so Netsy's parser accepts it.
//
// The single-node defaults disable quorum replication (quorum 0) so writes are
// acknowledged after being durably stored in object storage, which is what the
// datastore needs on a single-server k8e node. Multi-node clusters replace this
// by running Netsy out of band and setting --datastore-endpoint.
func (c Config) RenderClusterConfig() ([]byte, error) {
	doc := clusterConfigDoc{
		ClusterID: c.ClusterID,
		Storage: storageDoc{
			Provider:   c.Storage.Provider,
			BucketName: c.Storage.Bucket,
			KeyPrefix:  c.Storage.KeyPrefix,
			Class:      c.Storage.Class,
			Encryption: c.Storage.Encryption,
			KMSKeyID:   c.Storage.KMSKeyID,
		},
		HeartbeatInterval: "1s",
		Elector: electorDoc{
			DegradationCount:      2,
			DeregistrationTimeout: "3m",
			PrimaryPriorTimeout:   "5s",
		},
		Replication: replicationDoc{
			Quorum:           0,
			DegradationCount: 2,
			ChunkBuffer: chunkBufferDoc{
				ThresholdSizeMB:     4,
				ThresholdAgeMinutes: 1,
			},
		},
		Snapshot: snapshotDoc{
			ThresholdRecords:    10000,
			ThresholdSizeMB:     10000,
			ThresholdAgeMinutes: 0,
		},
		CompactionInterval: "5m",
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("failed to render netsy cluster config: %w", err)
	}
	return append(data, '\n'), nil
}

// Environ returns the Netsy per-node environment variables (NETSY_*) for the
// given rendered config file and PKI.
func (c Config) Environ(configPath string, certs CertPaths) []string {
	return []string{
		"NETSY_CONFIG=" + configPath,
		"NETSY_NODE_ID=" + c.NodeID,
		"NETSY_BIND_CLIENT=" + c.BindClient(),
		"NETSY_ADVERTISE_CLIENT=" + c.AdvertiseClient(),
		"NETSY_BIND_PEER=" + c.BindPeer(),
		"NETSY_ADVERTISE_PEER=" + c.AdvertisePeer(),
		"NETSY_BIND_ELECTION=" + c.BindElection(),
		"NETSY_ADVERTISE_ELECTION=" + c.AdvertiseElection(),
		"NETSY_BIND_HEALTH=" + c.BindHealth(),
		"NETSY_TLS_CA_CERT=" + certs.CA,
		"NETSY_TLS_SERVER_CERT=" + certs.ServerCert,
		"NETSY_TLS_SERVER_KEY=" + certs.ServerKey,
		"NETSY_TLS_CLIENT_CERT=" + certs.PeerClientCert,
		"NETSY_TLS_CLIENT_KEY=" + certs.PeerClientKey,
		"NETSY_DATA_DIR=" + c.DataDir,
		"NETSY_DEBUG=" + strconv.FormatBool(c.Verbose),
	}
}
