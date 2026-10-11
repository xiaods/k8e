package tandem

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func TestDriverInitializationMarker(t *testing.T) {
	d := NewDriver()
	if _, err := d.IsInitialized(); err == nil {
		t.Fatal("missing control config must fail")
	}
	d.SetControlConfig(&config.Control{DataDir: t.TempDir()})
	if initialized, err := d.IsInitialized(); err != nil || initialized {
		t.Fatalf("fresh datastore: initialized=%v, err=%v", initialized, err)
	}
	dir := filepath.Join(d.control.DataDir, "tandem", "rqlite")
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if initialized, err := d.IsInitialized(); err != nil || initialized {
		t.Fatalf("incomplete startup: initialized=%v, err=%v", initialized, err)
	}
	if err := os.WriteFile(filepath.Join(dir, "initialized"), nil, 0600); err != nil {
		t.Fatal(err)
	}
	if initialized, err := d.IsInitialized(); err != nil || !initialized {
		t.Fatalf("restart: initialized=%v, err=%v", initialized, err)
	}
}

// TestDriverConfiguresRaftTLS checks that the rqlite Raft listener between
// members is protected, and that a cluster is never formed in plaintext.
func TestDriverConfiguresRaftTLS(t *testing.T) {
	write := func(dir, name string) string {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, nil, 0600); err != nil {
			t.Fatal(err)
		}
		return path
	}
	cluster := Config{RqlitePeers: []string{"10.0.0.1:4002", "10.0.0.2:4002", "10.0.0.3:4002"}}

	t.Run("a cluster without any credential is refused", func(t *testing.T) {
		d := NewDriver()
		d.SetControlConfig(&config.Control{DataDir: t.TempDir(), Runtime: config.NewRuntime(nil)})
		cfg := cluster
		if err := d.configureRaftTLS(&cfg); err == nil {
			t.Fatal("expected a clustered node with no Raft credentials to be refused")
		}
	})

	t.Run("a partial credential set is refused", func(t *testing.T) {
		dir := t.TempDir()
		d := NewDriver()
		control := &config.Control{DataDir: dir, Runtime: config.NewRuntime(nil)}
		control.Datastore.TandemRaftCert = write(dir, "node.crt")
		d.SetControlConfig(control)
		cfg := cluster
		if err := d.configureRaftTLS(&cfg); err == nil {
			t.Fatal("expected a partial Raft credential set to be refused")
		}
	})

	t.Run("credential files that do not exist are refused", func(t *testing.T) {
		d := NewDriver()
		control := &config.Control{DataDir: t.TempDir(), Runtime: config.NewRuntime(nil)}
		control.Datastore.TandemRaftCert = filepath.Join(t.TempDir(), "absent.crt")
		control.Datastore.TandemRaftKey = filepath.Join(t.TempDir(), "absent.key")
		control.Datastore.TandemRaftCA = filepath.Join(t.TempDir(), "absent-ca.crt")
		d.SetControlConfig(control)
		cfg := cluster
		if err := d.configureRaftTLS(&cfg); err == nil {
			t.Fatal("expected absent credential files to be refused")
		}
	})

	// The common case: K8E issues the node certificate from the etcd peer CA
	// that every server already shares, so no operator step is involved.
	t.Run("K8E issues the credentials when none are supplied", func(t *testing.T) {
		dir := t.TempDir()
		runtime := config.NewRuntime(nil)
		runtime.TandemRaftNodeCert = write(dir, "node.crt")
		runtime.TandemRaftNodeKey = write(dir, "node.key")
		runtime.ETCDPeerCA = write(dir, "peer-ca.crt")
		d := NewDriver()
		d.SetControlConfig(&config.Control{DataDir: dir, Runtime: runtime})
		cfg := cluster
		if err := d.configureRaftTLS(&cfg); err != nil {
			t.Fatalf("configureRaftTLS: %v", err)
		}
		if cfg.RqliteNodeCert != runtime.TandemRaftNodeCert || cfg.RqliteNodeKey != runtime.TandemRaftNodeKey || cfg.RqliteNodeCA != runtime.ETCDPeerCA {
			t.Fatalf("Raft credentials not wired: %+v", cfg)
		}
		// Encryption alone would still let any host that reaches the port join
		// the membership.
		if !cfg.RqliteNodeVerifyClient {
			t.Error("mutual authentication must be on for issued credentials")
		}
	})

	t.Run("an explicit set wins over the issued one", func(t *testing.T) {
		dir := t.TempDir()
		runtime := config.NewRuntime(nil)
		runtime.TandemRaftNodeCert = write(dir, "issued.crt")
		runtime.TandemRaftNodeKey = write(dir, "issued.key")
		runtime.ETCDPeerCA = write(dir, "peer-ca.crt")
		d := NewDriver()
		control := &config.Control{DataDir: dir, Runtime: runtime}
		control.Datastore.TandemRaftCert = write(dir, "own.crt")
		control.Datastore.TandemRaftKey = write(dir, "own.key")
		control.Datastore.TandemRaftCA = write(dir, "own-ca.crt")
		control.Datastore.TandemRaftServerName = "raft.internal"
		d.SetControlConfig(control)
		cfg := cluster
		if err := d.configureRaftTLS(&cfg); err != nil {
			t.Fatalf("configureRaftTLS: %v", err)
		}
		if cfg.RqliteNodeCert != control.Datastore.TandemRaftCert || cfg.RqliteNodeCA != control.Datastore.TandemRaftCA {
			t.Fatalf("explicit credentials were not used: %+v", cfg)
		}
		if cfg.RqliteNodeServerName != "raft.internal" {
			t.Errorf("server name not wired: %+v", cfg)
		}
	})

	// A lone member's Raft listener never leaves the host, so it is the one
	// shape allowed to come up without credentials.
	t.Run("a single node does not require credentials", func(t *testing.T) {
		d := NewDriver()
		d.SetControlConfig(&config.Control{DataDir: t.TempDir(), Runtime: config.NewRuntime(nil)})
		var cfg Config
		if err := d.configureRaftTLS(&cfg); err != nil {
			t.Fatalf("a loopback-only member must start: %v", err)
		}
		if cfg.RqliteNodeCert != "" {
			t.Errorf("no credentials were supplied: %+v", cfg)
		}
	})
}

// TestDriverRequiresEtcdCredentials checks that the driver never starts the
// etcd port in plaintext. KIP-29 requires the compatibility layer to serve
// mTLS, and a missing or partial credential set must fail rather than silently
// downgrade.
func TestDriverRequiresEtcdCredentials(t *testing.T) {
	t.Run("no credentials", func(t *testing.T) {
		d := NewDriver()
		d.SetControlConfig(&config.Control{DataDir: t.TempDir(), Runtime: config.NewRuntime(nil)})
		var cfg Config
		if err := d.configureTLS(&cfg); err == nil {
			t.Fatal("expected missing etcd credentials to be refused")
		}
	})

	t.Run("partial credentials", func(t *testing.T) {
		dir := t.TempDir()
		cert := filepath.Join(dir, "server-client.crt")
		if err := os.WriteFile(cert, nil, 0600); err != nil {
			t.Fatal(err)
		}
		d := NewDriver()
		runtime := config.NewRuntime(nil)
		runtime.ServerETCDCert = cert
		d.SetControlConfig(&config.Control{DataDir: dir, Runtime: runtime})
		var cfg Config
		if err := d.configureTLS(&cfg); err == nil {
			t.Fatal("expected a partial credential set to be refused")
		}
	})

	t.Run("credentials that do not exist", func(t *testing.T) {
		runtime := config.NewRuntime(nil)
		runtime.ServerETCDCert = filepath.Join(t.TempDir(), "absent.crt")
		runtime.ServerETCDKey = filepath.Join(t.TempDir(), "absent.key")
		runtime.ETCDServerCA = filepath.Join(t.TempDir(), "absent-ca.crt")
		d := NewDriver()
		d.SetControlConfig(&config.Control{DataDir: t.TempDir(), Runtime: runtime})
		var cfg Config
		if err := d.configureTLS(&cfg); err == nil {
			t.Fatal("expected absent credential files to be refused")
		}
	})

	t.Run("complete credentials enable mTLS", func(t *testing.T) {
		dir := t.TempDir()
		cert, key, ca := filepath.Join(dir, "c.crt"), filepath.Join(dir, "c.key"), filepath.Join(dir, "ca.crt")
		for _, path := range []string{cert, key, ca} {
			if err := os.WriteFile(path, nil, 0600); err != nil {
				t.Fatal(err)
			}
		}
		runtime := config.NewRuntime(nil)
		runtime.ServerETCDCert, runtime.ServerETCDKey, runtime.ETCDServerCA = cert, key, ca
		d := NewDriver()
		d.SetControlConfig(&config.Control{DataDir: dir, Runtime: runtime})

		var cfg Config
		if err := d.configureTLS(&cfg); err != nil {
			t.Fatalf("configureTLS: %v", err)
		}
		if !cfg.TandemMTLS || cfg.TandemTLSCert != cert || cfg.TandemTLSKey != key || cfg.TandemTLSCA != ca {
			t.Fatalf("mTLS not configured: %+v", cfg)
		}
	})
}
