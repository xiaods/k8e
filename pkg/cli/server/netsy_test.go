package server

import (
	"os"
	"path/filepath"
	"testing"
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
