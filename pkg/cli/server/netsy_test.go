package server

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/server"
)

// TestHasEmbeddedEtcdData pins the detection setupNetsy uses to refuse
// --netsy on a node whose control plane would keep booting from an embedded
// etcd datastore: the WAL directory pkg/etcd.ETCD.IsInitialized stats.
func TestHasEmbeddedEtcdData(t *testing.T) {
	fresh := t.TempDir()
	if hasEmbeddedEtcdData(fresh) {
		t.Fatalf("hasEmbeddedEtcdData(%q) = true for a data directory that never ran etcd", fresh)
	}

	// etcd creates db/etcd before it writes the WAL; an interrupted first run
	// must not be mistaken for an initialized datastore.
	partial := t.TempDir()
	if err := os.MkdirAll(filepath.Join(partial, "db", "etcd"), 0700); err != nil {
		t.Fatal(err)
	}
	if hasEmbeddedEtcdData(partial) {
		t.Fatalf("hasEmbeddedEtcdData(%q) = true without a WAL directory", partial)
	}

	initialized := t.TempDir()
	if err := os.MkdirAll(filepath.Join(initialized, "db", "etcd", "member", "wal"), 0700); err != nil {
		t.Fatal(err)
	}
	if !hasEmbeddedEtcdData(initialized) {
		t.Fatalf("hasEmbeddedEtcdData(%q) = false with an etcd WAL directory", initialized)
	}
}

// TestRefuseSilentBackendSwitch pins the guard that stops a node which stored
// its Kubernetes data in Netsy from silently restarting embedded etcd (an old
// or empty datastore) once --netsy is dropped.
func TestRefuseSilentBackendSwitch(t *testing.T) {
	dataDir := t.TempDir()
	serverDataDir, err := server.ResolveDataDir(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	cfg := &cmds.Server{DataDir: dataDir}
	serverConfig := &server.Config{}

	if err := refuseSilentBackendSwitch(cfg, serverConfig); err != nil {
		t.Fatalf("refuseSilentBackendSwitch() before any Netsy run = %v, want nil", err)
	}

	if err := markNetsyBackend(serverDataDir); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(netsyBackendMarker(serverDataDir)); err != nil {
		t.Fatalf("backend marker not persisted: %v", err)
	}
	err = refuseSilentBackendSwitch(cfg, serverConfig)
	if err == nil {
		t.Fatal("refuseSilentBackendSwitch() = nil after the node stored its data in Netsy, want a refusal")
	}
	if !strings.Contains(err.Error(), netsyBackendMarker(serverDataDir)) {
		t.Errorf("error %q does not name the marker that has to be removed", err)
	}

	// An explicit endpoint is a deliberate migration to another datastore.
	serverConfig.ControlConfig.Datastore.Endpoint = "https://127.0.0.1:2379"
	if err := refuseSilentBackendSwitch(cfg, serverConfig); err != nil {
		t.Errorf("refuseSilentBackendSwitch() with an explicit --datastore-endpoint = %v, want nil", err)
	}

	// So is continuing to run the Netsy datastore.
	serverConfig.ControlConfig.Datastore.Endpoint = ""
	cfg.Netsy = true
	if err := refuseSilentBackendSwitch(cfg, serverConfig); err != nil {
		t.Errorf("refuseSilentBackendSwitch() with --netsy = %v, want nil", err)
	}
}
