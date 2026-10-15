package tandem

import (
	
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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

	// snapshotMu serializes backups and restores. A backup streams the whole
	// SQLite file, so overlapping with a restore would race on the store and
	// overlap with another backup would race on the output files.
	snapshotMu sync.Mutex
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
// Register makes the Tandem snapshot API available on the cluster's database
// HTTP handler. The k8e etcd-snapshot CLI POSTs a SnapshotRequest to
// /db/snapshot; this handler dispatches save/list/prune/delete onto the
// driver, mirroring the etcd handler in pkg/etcd/snapshot_handler.go so the
// CLI and the retention controller work unchanged on this backend.
//
// The handler chains with the incoming handler: /db/snapshot goes to the
// tandem dispatch, and everything else falls through, the same way the etcd
// handler attaches under the mux router. pkg/cluster/managed.go calls
// Register with the handler the cluster already built, and rechains the
// result in.
func (d *Driver) Register(h http.Handler) (http.Handler, error) {
	mux := http.NewServeMux()
	mux.Handle("/db/snapshot", d.snapshotHandler())
	// Everything that is not /db/snapshot falls through to the cluster's
	// existing handler. ServeMux has no NotFoundHandler, so this wrapper
	// dispatches on the path instead.
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/db/snapshot" {
			mux.ServeHTTP(w, r)
			return
		}
		h.ServeHTTP(w, r)
	}), nil
}

// snapshotHandler translates the CLI's SnapshotRequest into driver calls. Its
// shape copies pkg/etcd/snapshot_handler.go's dispatch so the CLI, which
// marshals the same request body, does not need to know which backend it is
// talking to.
func (d *Driver) snapshotHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req snapshotRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil && err != io.EOF {
			http.Error(w, fmt.Sprintf("tandem: invalid snapshot request: %v", err), http.StatusBadRequest)
			return
		}
		switch req.Operation {
		case snapshotSave:
			d.handleSaveSnapshot(w, r)
		case snapshotList:
			d.handleListSnapshots(w, r)
		case snapshotPrune:
			d.handlePruneSnapshots(w, r)
		case snapshotDelete:
			d.handleDeleteSnapshots(w, r, req.Name)
		default:
			http.Error(w, fmt.Sprintf("tandem: unknown snapshot operation %q", req.Operation), http.StatusBadRequest)
		}
	})
}

const (
	snapshotSave   = "save"
	snapshotList   = "list"
	snapshotPrune  = "prune"
	snapshotDelete = "delete"
)

// SnapshotRequest mirrors the k8e etcd-snapshot CLI's request shape. It is a
// local type because importing pkg/etcd into the tandem driver would pull the
// whole etcd dependency into the build.
type snapshotRequest struct {
	Operation string    `json:"operation"`
	Name      []string  `json:"name,omitempty"`
	Dir       *string   `json:"dir,omitempty"`
	Compress  *bool     `json:"compress,omitempty"`
	Retention *int      `json:"retention,omitempty"`
}

func writeSnapshotResponse(w http.ResponseWriter, res *managed.SnapshotResult) {
	w.Header().Set("Content-Type", "application/json")
	if res == nil {
		res = &managed.SnapshotResult{}
	}
	json.NewEncoder(w).Encode(res)
}

func (d *Driver) handleSaveSnapshot(w http.ResponseWriter, r *http.Request) {
	res, err := d.Snapshot(r.Context())
	if err != nil {
		logrus.Errorf("tandem snapshot failed: %v", err)
		http.Error(w, fmt.Sprintf("tandem snapshot failed: %v", err), http.StatusInternalServerError)
		return
	}
	writeSnapshotResponse(w, res)
}

func (d *Driver) handleListSnapshots(w http.ResponseWriter, r *http.Request) {
	dir, err := d.snapshotDir(false)
	if err != nil {
		http.Error(w, fmt.Sprintf("tandem snapshot list: %v", err), http.StatusInternalServerError)
		return
	}
	entries, err := os.ReadDir(dir)
	if err != nil && !os.IsNotExist(err) {
		http.Error(w, fmt.Sprintf("tandem snapshot list: %v", err), http.StatusInternalServerError)
		return
	}
	var res managed.SnapshotResult
	for _, e := range entries {
		if !e.IsDir() && strings.HasPrefix(e.Name(), d.snapshotPrefix()) {
			res.Created = append(res.Created, e.Name())
		}
	}
	writeSnapshotResponse(w, &res)
}

func (d *Driver) handlePruneSnapshots(w http.ResponseWriter, r *http.Request) {
	dir, err := d.snapshotDir(false)
	if err != nil {
		http.Error(w, fmt.Sprintf("tandem snapshot prune: %v", err), http.StatusInternalServerError)
		return
	}
	res := &managed.SnapshotResult{}
	deleted, err := tandemSnapshotRetention(d.control.EtcdSnapshotRetention, d.snapshotPrefix(), dir)
	if err != nil {
		http.Error(w, fmt.Sprintf("tandem snapshot prune: %v", err), http.StatusInternalServerError)
		return
	}
	res.Deleted = append(res.Deleted, deleted...)
	writeSnapshotResponse(w, res)
}

func (d *Driver) handleDeleteSnapshots(w http.ResponseWriter, r *http.Request, names []string) {
	dir, err := d.snapshotDir(false)
	if err != nil {
		http.Error(w, fmt.Sprintf("tandem snapshot delete: %v", err), http.StatusInternalServerError)
		return
	}
	res := &managed.SnapshotResult{}
	for _, name := range names {
		if err := os.Remove(filepath.Join(dir, name)); err != nil && !os.IsNotExist(err) {
			logrus.Warnf("tandem: failed to delete snapshot %s: %v", name, err)
			continue
		}
		res.Deleted = append(res.Deleted, name)
	}
	writeSnapshotResponse(w, res)
}
func (d *Driver) Reset(context.Context, func() error) error {
	return fmt.Errorf("tandem reset is not implemented")
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
func (d *Driver) Start(ctx context.Context, _ *clientaccess.Info) error {
	if d.control == nil {
		return fmt.Errorf("tandem driver has no control config")
	}
	// A cluster reset with a restore path replaces the store before the
	// supervised pair starts. rqlite's /db/load requires a node with no
	// existing data and no other writes, so the load happens here, before any
	// process touches the data directory. This mirrors the etcd driver's
	// restore-before-start in pkg/etcd/etcd.go.
	if d.control.ClusterResetRestorePath != "" {
		if err := d.Restore(ctx); err != nil {
			return fmt.Errorf("tandem restore: %w", err)
		}
		if err := os.WriteFile(d.ResetFile(), []byte{}, 0600); err != nil {
			return fmt.Errorf("tandem: record reset: %w", err)
		}
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
		return err
	}
	// Terminate the etcd port with the same certificates the apiserver and the
	// bootstrap client present. KIP-29 requires the compatibility layer to
	// serve mTLS; serving it in plaintext would both violate that contract and
	// leave the datastore unauthenticated on any interface the port reaches.
	if err := d.configureTLS(&cfg); err != nil {
		return err
	}
	if err := d.supervisor.Start(ctx, cfg); err != nil {
		return err
	}
	// Point the apiserver at the compat layer's mTLS endpoint. The server
	// reads --etcd-servers from Datastore.Endpoint; without this the
	// apiserver fails with 'etcd-servers must be specified'.
	d.control.Datastore.Endpoint = fmt.Sprintf("https://%s", cfg.TandemListen)
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
// Restore replaces the local store with the SQLite file named by
// --cluster-reset-restore-path.
//
// This is the K8E restore entry point, and it runs before the store starts:
// rqlite's POST /db/load replaces everything in the store and requires a fresh
// cluster with no other write traffic, so loading into a node that is already
// serving would destroy live data and race with the apiserver's writes. The
// existing data directory is moved aside rather than deleted, so a restore
// that turns out to be the wrong file leaves the operator a way back.
//
// The restore path must be a real SQLite file: /db/load accepts nothing else,
// and accepting an etcd snapshot here — the formats are not interchangeable —
// would destroy the store and fail inside rqlite, after the original was
// already moved.
func (d *Driver) Restore(ctx context.Context) error {
	if d.control == nil {
		return fmt.Errorf("tandem driver has no control config")
	}
	src := d.control.ClusterResetRestorePath
	if src == "" {
		return fmt.Errorf("tandem restore: no restore path was specified")
	}
	if _, err := os.Stat(src); err != nil {
		return fmt.Errorf("tandem restore: %w", err)
	}
	if info, err := os.Stat(src); err == nil && info.IsDir() {
		return fmt.Errorf("tandem restore: %s is a directory, not a snapshot file", src)
	}
	ok, err := isSQLiteFile(src)
	if err != nil {
		return fmt.Errorf("tandem restore: %w", err)
	}
	if !ok {
		return fmt.Errorf("tandem restore: %s is not a SQLite database; a rqlite backup is required, and an etcd snapshot cannot be restored", src)
	}

	rqliteDir := filepath.Join(d.control.DataDir, "tandem", "rqlite")
	oldDir := rqliteDir + "-old-" + strconv.Itoa(int(time.Now().Unix()))
	if _, err := os.Stat(rqliteDir); err == nil {
		if err := os.Rename(rqliteDir, oldDir); err != nil {
			return fmt.Errorf("tandem restore: move existing data aside: %w", err)
		}
		logrus.Infof("Pre-restore tandem data moved to %s", oldDir)
	}
	// rqlite initializes a fresh store in an empty directory and then accepts
	// the load. prepareDataDir refuses a directory holding foreign state, so
	// the directory is created empty here rather than left to the load.
	if err := os.MkdirAll(rqliteDir, 0700); err != nil {
		return fmt.Errorf("tandem restore: %w", err)
	}
	// Load through a short-lived node, then stop it: the supervised pair that
	// Start brings up later reopens the same directory. Loading into the
	// running pair is not an option — it would replace the data the cluster
	// is serving. resolveBinary is consulted first so an operator can point
	// K8E at a rqlite that is not on PATH.
	rqliteBinary := resolveBinary("rqlited", "K8E_RQLITED_BINARY")
	return withTemporaryNode(ctx, rqliteBinary, rqliteDir, func(base string) error {
		return (&Supervisor{}).Load(ctx, base, src)
	})
}
func (d *Driver) EndpointName() string { return "tandem" }

// Snapshot saves a backup of the local rqlite node.
//
// rqlite's GET /db/backup streams the underlying SQLite database, which is
// the rqlite analogue of an etcd snapshot — the Maintenance.Snapshot RPC is
// Unimplemented on this layer precisely because the two formats are not
// interchangeable (see pkg/tandem/differential/known_differences.go). KIP-29
// §7 maps this endpoint onto the K8E snapshot flow.
//
// The file lands in the same snapshot directory the etcd backend uses, so the
// retention policy and the CLI's list/prune/delete operations work
// unchanged. It is named with the same prefix and timestamp convention so an
// operator's tooling cannot tell the backends apart from the filename.
func (d *Driver) Snapshot(ctx context.Context) (*managed.SnapshotResult, error) {
	if d.control == nil {
		return nil, fmt.Errorf("tandem driver has no control config")
	}
	if !d.snapshotMu.TryLock() {
		return nil, fmt.Errorf("tandem snapshot already in progress")
	}
	defer d.snapshotMu.Unlock()

	snapshotDir, err := d.snapshotDir(true)
	if err != nil {
		return nil, err
	}
	nodeName := os.Getenv("NODE_NAME")
	now := time.Now().Round(time.Second)
	snapshotName := fmt.Sprintf("%s-%s-%d", d.snapshotPrefix(), nodeName, now.Unix())
	snapshotPath := filepath.Join(snapshotDir, snapshotName)
	logrus.Infof("Saving tandem snapshot to %s", snapshotPath)

	res := &managed.SnapshotResult{}
	if err := d.supervisor.Backup(ctx, d.supervisor.HTTPAddr(), snapshotPath); err != nil {
		_ = os.Remove(snapshotPath)
		return nil, err
	}
	info, err := os.Stat(snapshotPath)
	if err != nil {
		return nil, fmt.Errorf("tandem snapshot: %w", err)
	}
	res.Created = append(res.Created, snapshotName)
	logrus.Infof("Saved tandem snapshot %s (%d bytes)", snapshotName, info.Size())

	// Retention is shared with the etcd backend's local file policy. Failing
	// to prune is not fatal — the snapshot is on disk either way.
	deleted, err := tandemSnapshotRetention(d.control.EtcdSnapshotRetention, d.snapshotPrefix(), snapshotDir)
	if err != nil {
		logrus.Warnf("Failed to apply tandem snapshot retention policy: %v", err)
	}
	res.Deleted = append(res.Deleted, deleted...)
	return res, nil
}

// snapshotDir resolves the directory backups are written to. It mirrors the
// etcd backend's snapshotDir so a cluster that switches backends keeps one
// backup location, and so the shared retention and listing code sees one
// directory.
func (d *Driver) snapshotDir(create bool) (string, error) {
	dir := d.control.EtcdSnapshotDir
	if dir == "" {
		dir = filepath.Join(d.control.DataDir, "db", "snapshots")
	}
	s, err := os.Stat(dir)
	if err != nil {
		if os.IsNotExist(err) && create {
			return dir, os.MkdirAll(dir, 0700)
		}
		return "", err
	}
	if !s.IsDir() {
		return "", fmt.Errorf("snapshot path %s is not a directory", dir)
	}
	return dir, nil
}

// snapshotPrefix is the name retention and pruning match backups by. It is
// the configured snapshot name when one is set, so an operator's naming
// convention applies to both backends, and otherwise a tandem-specific
// default that does not collide with the etcd backend's "etcd-snapshot"
// prefix — a directory holding both would otherwise prune one backend's
// backups under the other's policy.
func (d *Driver) snapshotPrefix() string {
	if d.control.EtcdSnapshotName != "" {
		return d.control.EtcdSnapshotName
	}
	return "tandem-snapshot"
}

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
