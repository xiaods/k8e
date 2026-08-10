package sandboxcli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/xiaods/k8e/pkg/sandboxlayer"
)

// TestSnapshotStore_ContentAddressedDedup verifies saving the same payload
// twice yields the same layer digest and no duplicated storage.
func TestSnapshotStore_ContentAddressedDedup(t *testing.T) {
	dir := t.TempDir()
	store, err := sandboxlayer.New(filepath.Join(dir, ".layers"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	payload := []byte("same workspace tar content")
	d1, err := store.Put(payload)
	if err != nil {
		t.Fatalf("put 1: %v", err)
	}
	d2, err := store.Put(payload)
	if err != nil {
		t.Fatalf("put 2: %v", err)
	}
	if d1 != d2 {
		t.Fatalf("same content must dedup to same digest: %s vs %s", d1, d2)
	}
	// Dedup: exactly one on-disk layer file despite two Put calls.
	entries, err := os.ReadDir(filepath.Join(dir, ".layers", "layers"))
	if err != nil {
		t.Fatalf("read layer dir: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected exactly 1 deduped layer file, got %d", len(entries))
	}
	// Round-trip intact (zstd stored, transparently decompressed).
	got, err := store.Get(d1)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if string(got) != string(payload) {
		t.Fatal("zstd round-trip mismatch")
	}
}

// TestSnapshotStore_ManifestLeaseProtectsFromGC verifies a saved snapshot's
// layer survives GC while an unreferenced layer is reclaimed.
func TestSnapshotStore_ManifestLeaseProtectsFromGC(t *testing.T) {
	dir := t.TempDir()
	store, err := sandboxlayer.New(filepath.Join(dir, ".layers"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}

	kept, _ := store.Put([]byte("leased layer"))
	orphan, _ := store.Put([]byte("unreferenced layer"))

	if err := store.SaveManifest("snap-1", []string{kept}); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	removed, err := store.GC()
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if removed == 0 {
		t.Fatal("expected unreferenced layer removed")
	}
	if !store.Has(kept) {
		t.Fatal("leased layer must survive GC")
	}
	if store.Has(orphan) {
		t.Fatal("orphan layer must be reclaimed")
	}
}

// TestSnapshotStore_DeleteReleasesLease verifies deleting a snapshot's
// manifest releases its layers for GC (snapshot delete path).
func TestSnapshotStore_DeleteReleasesLease(t *testing.T) {
	dir := t.TempDir()
	store, err := sandboxlayer.New(filepath.Join(dir, ".layers"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	d, _ := store.Put([]byte("transient snapshot"))
	if err := store.SaveManifest("snap-del", []string{d}); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteManifest("snap-del"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.GC(); err != nil {
		t.Fatal(err)
	}
	if store.Has(d) {
		t.Fatal("deleted snapshot's layer must be reclaimed")
	}
}

// TestSnapshotStore_LargePayload verifies payloads beyond the old 64KiB
// WriteFile/ReadFile limit round-trip through the layerstore (KIP-16 M2 driver).
func TestSnapshotStore_LargePayload(t *testing.T) {
	dir := t.TempDir()
	store, err := sandboxlayer.New(filepath.Join(dir, ".layers"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	// 1 MiB payload.
	big := make([]byte, 1024*1024)
	for i := range big {
		big[i] = byte(i % 251)
	}
	d, err := store.Put(big)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := store.Get(d)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if len(got) != len(big) {
		t.Fatalf("size mismatch: %d vs %d", len(got), len(big))
	}
	_ = os.RemoveAll(dir)
}
