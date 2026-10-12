package tandem

import (
	"context"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/cluster/managed"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

// Driver exposes the k8e-owned Tandem+rqlite process pair through the managed
// datastore lifecycle. Advanced etcd cluster-management operations are
// intentionally rejected until Tandem implements their protocol semantics.
type Driver struct {
	control    *config.Control
	supervisor Supervisor
}

func NewDriver() *Driver                                   { return &Driver{} }
func (d *Driver) SetControlConfig(c *config.Control) error { d.control = c; return nil }
func (d *Driver) IsInitialized() (bool, error) {
	if d.control == nil {
		return false, fmt.Errorf("tandem driver has no control config")
	}
	info, err := os.Stat(filepath.Join(d.control.DataDir, "tandem", "rqlite", "initialized"))
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return info.Mode().IsRegular(), nil
}

// IsReset reports whether a cluster reset has already been performed. The
// cluster uses this to decide whether post-reset recovery has to run — the
// node password cleanup and the dynamic listener cache invalidation — so
// answering a constant false silently skipped that recovery after a reset.
func (d *Driver) IsReset() (bool, error) {
	if d.control == nil {
		return false, fmt.Errorf("tandem driver has no control config")
	}
	if _, err := os.Stat(d.ResetFile()); err != nil {
		if os.IsNotExist(err) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}
func (d *Driver) ResetFile() string {
	if d.control == nil {
		return ""
	}
	return filepath.Join(d.control.DataDir, "tandem", "reset")
}

// config builds the launch configuration for the supervised pair from the
// control config. Start and Reset both need it: a reset restarts the same
// process pair, and rebuilding the configuration in two places would let the
// two drift — TLS in particular is assembled from several files and a
// difference would be invisible until a member failed to join.
func (d *Driver) config() (Config, error) {
	if d.control == nil {
		return Config{}, fmt.Errorf("tandem driver has no control config")
	}
	cfg := Config{DataDir: filepath.Join(d.control.DataDir, "tandem"), Stdout: os.Stdout, Stderr: os.Stderr}
	cfg.RqliteJoin = d.control.Datastore.TandemJoin
	cfg.AdvertiseIP = d.control.AdvertiseIP
	// The node identity and the peer set were previously never passed down, so
	// every member fell back to its hostname and no cluster could be formed.
	cfg.NodeID = d.control.Datastore.TandemNodeID
	cfg.RqlitePeers = d.control.Datastore.TandemRaftPeers
	cfg.BootstrapExpect = d.control.Datastore.TandemBootstrapExpect
	cfg.RqliteRaftPort = d.control.Datastore.TandemRaftPort
	// The peer set decides whether Raft traffic leaves the host, so it has to
	// be known before the Raft credentials are chosen.
	if cfg.BootstrapExpect == 0 && len(cfg.RqlitePeers) > 1 {
		cfg.BootstrapExpect = len(cfg.RqlitePeers)
	}
	if err := d.configureRaftTLS(&cfg); err != nil {
		return Config{}, err
	}
	// Terminate the etcd port with the same certificates the apiserver and the
	// bootstrap client present. KIP-29 requires the compatibility layer to
	// serve mTLS; serving it in plaintext would both violate that contract and
	// leave the datastore unauthenticated on any interface the port reaches.
	if err := d.configureTLS(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (d *Driver) Start(ctx context.Context, _ *clientaccess.Info) error {
	cfg, err := d.config()
	if err != nil {
		return err
	}
	if err := d.supervisor.Start(ctx, cfg); err != nil {
		return err
	}
	// Record successful startup inside the datastore directory so restart can
	// use local bootstrap data even when the original join server is offline.
	if err := os.WriteFile(filepath.Join(cfg.DataDir, "rqlite", "initialized"), nil, 0600); err != nil {
		_ = d.supervisor.Close()
		return fmt.Errorf("record tandem initialization: %w", err)
	}
	return nil
}

// configureTLS points the compat layer at K8E's etcd server credentials and
// requires client certificates, so the apiserver's mTLS client is the only
// thing that can talk to the datastore. A partially configured set of
// credentials is an error rather than a silent downgrade to plaintext.
func (d *Driver) configureTLS(cfg *Config) error {
	runtime := d.control.Runtime
	cert, key, ca := runtime.ServerETCDCert, runtime.ServerETCDKey, runtime.ETCDServerCA
	if cert == "" && key == "" && ca == "" {
		return fmt.Errorf("tandem requires etcd server credentials, but none are configured")
	}
	if cert == "" || key == "" || ca == "" {
		return fmt.Errorf("tandem requires a complete etcd credential set (cert, key and CA); got cert=%q key=%q ca=%q", cert, key, ca)
	}
	for name, path := range map[string]string{"certificate": cert, "key": key, "CA": ca} {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("tandem etcd %s %s: %w", name, path, err)
		}
	}
	cfg.TandemTLSCert, cfg.TandemTLSKey, cfg.TandemTLSCA = cert, key, ca
	cfg.TandemMTLS = true
	return nil
}

// configureRaftTLS protects the rqlite Raft listener between members.
//
// KIP-29 §4 requires cross-node rqlite traffic to be protected: it carries
// every committed write, and anything that can reach the port can join the
// membership or read the log. An explicit credential set always wins so an
// operator can use their own CA; otherwise K8E issues a node certificate for
// the advertised Raft address from the etcd peer CA, which every server
// already shares through the bootstrap data, so a joining member is trusted
// without an extra distribution step.
//
// A lone member's Raft listener never leaves the host, so it is the only shape
// allowed to run without credentials. A clustered node without them is refused
// rather than started in plaintext, and a partial set is refused everywhere:
// rqlite turns on TLS only when -node-cert is given, so half a credential set
// would produce a node that looks configured and is not.
func (d *Driver) configureRaftTLS(cfg *Config) error {
	datastore := d.control.Datastore
	runtime := d.control.Runtime
	cert, key, ca := datastore.TandemRaftCert, datastore.TandemRaftKey, datastore.TandemRaftCA
	explicit := cert != "" || key != "" || ca != ""
	if !explicit && runtime != nil {
		cert, key, ca = runtime.TandemRaftNodeCert, runtime.TandemRaftNodeKey, runtime.ETCDPeerCA
	}
	clustered := len(cfg.RqlitePeers) > 1 || cfg.RqliteJoin != "" || cfg.BootstrapExpect > 1
	if cert == "" && key == "" && ca == "" {
		if clustered {
			return fmt.Errorf("tandem requires Raft credentials to form or join a cluster; pass --tandem-raft-cert, --tandem-raft-key and --tandem-raft-ca, or let K8E issue them")
		}
		return nil
	}
	if cert == "" || key == "" || ca == "" {
		return fmt.Errorf("tandem requires a complete Raft credential set (cert, key and CA); got cert=%q key=%q ca=%q", cert, key, ca)
	}
	for name, path := range map[string]string{"certificate": cert, "key": key, "CA": ca} {
		if _, err := os.Stat(path); err != nil {
			// Only a loopback-only member may come up without them; a
			// clustered node that cannot present a certificate cannot join.
			if !clustered && !explicit {
				logrus.Warnf("tandem: no usable Raft credentials at %s; the Raft listener stays unencrypted on loopback", path)
				return nil
			}
			return fmt.Errorf("tandem Raft %s %s: %w", name, path, err)
		}
	}
	cfg.RqliteNodeCert, cfg.RqliteNodeKey, cfg.RqliteNodeCA = cert, key, ca
	// Mutual authentication is the default: every member holds a certificate
	// from the same CA, so requiring one costs nothing and means a host that
	// merely reaches the port cannot join the membership.
	cfg.RqliteNodeVerifyClient = datastore.TandemRaftVerifyClient || !explicit
	cfg.RqliteNodeServerName = datastore.TandemRaftServerName
	return nil
}

// Test reports whether the datastore can actually serve reads and writes. A
// listening socket proves only that the process is alive: a node that has lost
// quorum still accepts connections but rejects every linearizable read. KIP-29
// requires readiness to reflect serviceability and to treat liveness as
// distinct from quorum, so this asks rqlite for its readiness, for a known
// leader and for a quorum of voters before reporting ready, and confirms the
// compat layer's etcd port still accepts a connection so a layer that died
// after startup is not reported healthy.
func (d *Driver) Test(ctx context.Context) error {
	if d.control == nil {
		return fmt.Errorf("tandem driver has no control config")
	}
	if err := d.supervisor.Ready(ctx); err != nil {
		return err
	}
	addr := "127.0.0.1:2379"
	if d.supervisor.cfg.TandemListen != "" {
		addr = dialAddress(d.supervisor.cfg.TandemListen)
	}
	conn, err := (&net.Dialer{Timeout: time.Second}).DialContext(ctx, "tcp", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

// Restore loads the backup named by --cluster-reset-restore-path into this
// node's rqlite store, replacing its contents. It is the restore half of the
// backup contract: what "k8e etcd-snapshot" wrote can be read back. The file is
// an rqlite (SQLite) image rather than an etcd snapshot, which is why KIP-29
// records the etcd snapshot restore path as a call migration.
func (d *Driver) Restore(ctx context.Context) error {
	if d.control == nil {
		return fmt.Errorf("tandem driver has no control config")
	}
	if d.control.ClusterResetRestorePath == "" {
		return fmt.Errorf("tandem restore requires a backup path; restart with --cluster-reset-restore-path")
	}
	return d.supervisor.RestoreFrom(ctx, d.control.ClusterResetRestorePath)
}

// Reset rebuilds this node as a standalone Tandem store loaded from a backup.
//
// rqlite has no equivalent of etcd's force-new-cluster: a store that lost
// quorum cannot be revived from its own files, only from a backup. A reset
// without --cluster-reset-restore-path is therefore refused rather than wiping
// the store, because the operator asking for one would silently lose every
// object the node holds. A reset node also drops the peer set: it becomes its
// own single-member group, and keeping the old address list would ask it to
// join a group that no longer exists.
func (d *Driver) Reset(ctx context.Context, rebootstrap func() error) error {
	if d.control == nil {
		return fmt.Errorf("tandem driver has no control config")
	}
	if d.control.ClusterResetRestorePath == "" {
		return fmt.Errorf("tandem cannot reset without a backup: rqlite has no force-new-cluster, so restart with --cluster-reset-restore-path, or recover the group from its remaining members")
	}
	// The supervised pair may still be running; the store is about to be
	// replaced, so stop it before anything touches the data directory.
	if err := d.supervisor.Close(); err != nil {
		return err
	}
	cfg, err := d.config()
	if err != nil {
		return err
	}
	cfg.RqlitePeers, cfg.RqliteJoin, cfg.BootstrapExpect = nil, "", 0
	if err := os.RemoveAll(filepath.Join(cfg.DataDir, "rqlite")); err != nil {
		return err
	}
	if err := d.supervisor.Start(ctx, cfg); err != nil {
		return err
	}
	if err := d.supervisor.RestoreFrom(ctx, d.control.ClusterResetRestorePath); err != nil {
		return err
	}
	if rebootstrap != nil {
		if err := rebootstrap(); err != nil {
			return err
		}
	}
	// Touch the reset file last: it is what makes the next start skip the
	// reset, so it must not exist before the restore it describes succeeded.
	if err := os.WriteFile(d.ResetFile(), nil, 0600); err != nil {
		return err
	}
	logrus.Infof("tandem: this node was reset to a standalone store from %s; restart it without --cluster-reset", d.control.ClusterResetRestorePath)
	return nil
}

func (d *Driver) EndpointName() string { return "tandem" }

// ReconcileSnapshotData reports that snapshot reconciliation is not
// implemented rather than returning nil, which the cluster treats as success.
// A nil here makes the reconcile loop report success while doing nothing.
//
// The sentinel is managed.ErrNotImplemented so the caller can tell "this
// driver has nothing to reconcile" from "reconciling failed". A plain error
// is retried, which logged this once a second for the life of the process.
func (d *Driver) ReconcileSnapshotData(context.Context) error {
	return fmt.Errorf("tandem snapshot reconciliation: %w", managed.ErrNotImplemented)
}

// GetMembersClientURLs returns the etcd-compatible endpoint this node serves.
// Tandem exposes a single stable endpoint per node rather than a member list
// with etcd member IDs and peer URLs, which KIP-29 says must not be emulated.
func (d *Driver) GetMembersClientURLs(context.Context) ([]string, error) {
	if d.control == nil {
		return nil, fmt.Errorf("tandem driver has no control config")
	}
	endpoint := d.control.Datastore.Endpoint
	if endpoint == "" {
		endpoint = "http://127.0.0.1:2379"
	}
	return []string{endpoint}, nil
}

// RemoveSelf takes this node out of the rqlite voting group.
//
// Returning nil without doing it would make a removal appear to have happened
// while the node is still a member: the group keeps trying to reach it, and it
// keeps counting towards quorum, so a later failure is blamed on a node that
// was supposed to be gone.
//
// Removal is itself a Raft operation, so it needs a leader. Against a group
// that has lost quorum it is refused here rather than attempted, and the
// refusal names the counts — the remedy is to bring members back, not to retry.
// A node that is not a member at all is already out, so that is a success.
func (d *Driver) RemoveSelf(ctx context.Context) error {
	if d.control == nil {
		return fmt.Errorf("tandem driver has no control config")
	}
	cfg := d.supervisor.cfg
	if cfg.RqliteHTTP == "" {
		return fmt.Errorf("tandem cannot remove this node: the local rqlite node is not running, and removal has to be requested through a member")
	}
	if cfg.NodeID == "" {
		return fmt.Errorf("tandem cannot remove this node: no rqlite node identity is configured")
	}
	client := newMembershipClient(rqliteURL(cfg.RqliteHTTP))
	// Reading the group first is what makes the two failures distinguishable.
	// If it cannot be read the removal is still attempted: rqlite's own answer
	// is then the most the caller can be told.
	if membership, err := client.nodes(ctx); err == nil {
		if !membership.HasVoter(cfg.NodeID) {
			return nil
		}
		if !membership.HasQuorum() {
			return fmt.Errorf("tandem cannot remove %s: %w", cfg.NodeID, membership.QuorumError())
		}
	}
	if err := client.remove(ctx, cfg.NodeID); err != nil {
		return fmt.Errorf("tandem remove %s: %w", cfg.NodeID, err)
	}
	logrus.Infof("tandem: removed %s from the rqlite voting group", cfg.NodeID)
	return nil
}

var _ managed.Driver = (*Driver)(nil)
