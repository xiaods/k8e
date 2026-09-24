package tandem

import (
	"os"
	"path/filepath"
	"strings"
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

// TestDriverAdvertisesTheEtcdPortOverTLS checks the scheme the driver hands
// the apiserver. Tandem's etcd port always requires a client certificate, so
// advertising http:// makes the apiserver start a plaintext gRPC handshake
// against a TLS listener; it fails with "error reading server preface" rather
// than anything that names the real cause, and the control plane never comes
// up. The embedded etcd driver already advertises https://.
func TestDriverAdvertisesTheEtcdPortOverTLS(t *testing.T) {
	for name, endpoint := range map[string]string{
		"unset":            "",
		"bare host:port":   "127.0.0.1:2379",
		"explicit https":   "https://127.0.0.1:2379",
		"explicit http":    "http://127.0.0.1:2379",
		"remote host:port": "10.0.0.5:2379",
	} {
		t.Run(name, func(t *testing.T) {
			d := NewDriver()
			d.SetControlConfig(&config.Control{Datastore: config.DatastoreConfig{Endpoint: endpoint}})
			urls, err := d.GetMembersClientURLs(t.Context())
			if err != nil {
				t.Fatal(err)
			}
			if len(urls) != 1 {
				t.Fatalf("endpoints = %v, want exactly one", urls)
			}
			// An operator who explicitly asked for http keeps it; the
			// default is the only thing this fixes.
			if endpoint == "http://127.0.0.1:2379" {
				if urls[0] != endpoint {
					t.Fatalf("explicit endpoint rewritten to %q", urls[0])
				}
				return
			}
			if !strings.HasPrefix(urls[0], "https://") {
				t.Fatalf("endpoint = %q, want an https:// scheme", urls[0])
			}
		})
	}
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
