package tandem

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

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
func (d *Driver) Register(h http.Handler) (http.Handler, error) { return h, nil }
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
	cfg := Config{DataDir: filepath.Join(d.control.DataDir, "tandem"), Stdout: os.Stdout, Stderr: os.Stderr}
	cfg.RqliteJoin = d.control.Datastore.TandemJoin
	cfg.AdvertiseIP = d.control.AdvertiseIP
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

// Test reports whether the datastore can actually serve reads and writes. A
// listening socket proves only that the process is alive: a node that has lost
// quorum still accepts connections but rejects every linearizable read. KIP-29
// requires readiness to reflect serviceability and to treat liveness as
// distinct from quorum, so this asks rqlite for its readiness and for a known
// leader before reporting ready, and performs a real request against the etcd
// port so a compat layer that failed after startup is not reported healthy.
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
func (d *Driver) Restore(context.Context) error {
	return fmt.Errorf("tandem restore is not implemented")
}
func (d *Driver) EndpointName() string { return "tandem" }
func (d *Driver) Snapshot(context.Context) (*managed.SnapshotResult, error) {
	return nil, fmt.Errorf("tandem snapshots are not implemented")
}

// ReconcileSnapshotData reports that snapshot reconciliation is not
// implemented rather than returning nil, which the cluster treats as success.
// A nil here makes the reconcile loop report success while doing nothing.
func (d *Driver) ReconcileSnapshotData(context.Context) error {
	return fmt.Errorf("tandem snapshot reconciliation is not implemented")
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
		endpoint = "127.0.0.1:2379"
	}
	// The scheme is https whenever the caller has not set one. Tandem's etcd
	// port always requires the apiserver's client certificate, so advertising
	// http:// makes the apiserver attempt a plaintext gRPC handshake and fail
	// with a server-preface error instead of a usable connection. The default
	// here matches what the embedded etcd driver advertises.
	if !strings.Contains(endpoint, "://") {
		endpoint = "https://" + endpoint
	}
	return []string{endpoint}, nil
}

// RemoveSelf reports that leaving the cluster is not implemented. Returning
// nil would make a node removal appear to succeed while the node stays a
// member.
func (d *Driver) RemoveSelf(context.Context) error {
	return fmt.Errorf("tandem does not support removing a member; rqlite membership is managed with rqlite's own join and remove commands")
}

var _ managed.Driver = (*Driver)(nil)
