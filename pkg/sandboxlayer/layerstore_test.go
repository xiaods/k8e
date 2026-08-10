package sandboxlayer

import (
	"bytes"
	"strings"
	"testing"
)

func TestDigest_Deterministic(t *testing.T) {
	a := Digest([]byte("hello"))
	b := Digest([]byte("hello"))
	c := Digest([]byte("hello!"))
	if a != b {
		t.Fatalf("digest must be deterministic: %s vs %s", a, b)
	}
	if a == c {
		t.Fatalf("different content must differ: %s vs %s", a, c)
	}
	if len(a) != 64 {
		t.Fatalf("expected sha256 hex (64 chars), got %d", len(a))
	}
}

func TestStore_PutGetRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	content := []byte("layer one content")
	digest, err := s.Put(content)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(digest)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("round-trip mismatch")
	}
	if !s.Has(digest) {
		t.Fatal("expected Has true after put")
	}
}

func TestStore_PutIdempotent(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	d1, err := s.Put([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	d2, err := s.Put([]byte("same"))
	if err != nil {
		t.Fatal(err)
	}
	if d1 != d2 {
		t.Fatalf("same content must yield same digest: %s vs %s", d1, d2)
	}
}

// TestStore_ManifestRoundTrip exercises save/load/list in one flow.
func TestStore_ManifestRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	d1, _ := s.Put([]byte("part1"))
	d2, _ := s.Put([]byte("part2"))

	if err := s.SaveManifest("snap-b", []string{d1, d2}); err != nil {
		t.Fatalf("save manifest: %v", err)
	}
	m, err := s.LoadManifest("snap-b")
	if err != nil {
		t.Fatalf("load manifest: %v", err)
	}
	expectLayers(t, m, 2)
	names, err := s.ListManifests()
	if err != nil || len(names) != 1 || names[0] != "snap-b" {
		t.Fatalf("unexpected manifests: %v err=%v", names, err)
	}
}

// expectLayers asserts a manifest has the expected schema and layer count.
func expectLayers(t *testing.T, m *Manifest, n int) {
	t.Helper()
	if m.SchemaVersion != ManifestVersion || len(m.Layers) != n {
		t.Fatalf("unexpected manifest: %+v", m)
	}
}

// TestStore_ManifestDelete verifies delete removes the manifest lease.
func TestStore_ManifestDelete(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	d, _ := s.Put([]byte("part1"))
	if err := s.SaveManifest("snap-c", []string{d}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteManifest("snap-c"); err != nil {
		t.Fatalf("delete manifest: %v", err)
	}
	if _, err := s.LoadManifest("snap-c"); err == nil {
		t.Fatal("expected error loading deleted manifest")
	}
}

func TestStore_Delta(t *testing.T) {
	have := &Manifest{Layers: []string{"a", "b"}}
	want := &Manifest{Layers: []string{"a", "b", "c", "d"}}
	missing := Delta(have, want)
	if len(missing) != 2 || missing[0] != "c" || missing[1] != "d" {
		t.Fatalf("unexpected delta: %v", missing)
	}
	if got := Delta(want, want); len(got) != 0 {
		t.Fatalf("delta of identical manifests must be empty: %v", got)
	}
}

func TestStore_GC_OnlyUnreferencedRemoved(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	keep1, _ := s.Put([]byte("keep me 1"))
	keep2, _ := s.Put([]byte("keep me 2"))
	orphan, _ := s.Put([]byte("orphan layer"))

	// Reference only keep1+keep2 via a manifest.
	if err := s.SaveManifest("snap-keep", []string{keep1, keep2}); err != nil {
		t.Fatal(err)
	}

	removed, err := s.GC()
	if err != nil {
		t.Fatalf("gc: %v", err)
	}
	if removed == 0 {
		t.Fatal("expected orphan layer to be removed")
	}
	assertHasAll(t, s, keep1, keep2)
	if s.Has(orphan) {
		t.Fatal("orphan layer must be removed")
	}

	// Second GC removes nothing.
	removed2, _ := s.GC()
	if removed2 != 0 {
		t.Fatalf("expected no-op second GC, removed %d", removed2)
	}
}

// assertHasAll asserts every digest is present in the store.
func assertHasAll(t *testing.T, s *Store, digests ...string) {
	t.Helper()
	for _, d := range digests {
		if !s.Has(d) {
			t.Fatalf("layer %s missing after GC", d)
		}
	}
}

func TestStore_GC_AfterManifestDelete(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	d, _ := s.Put([]byte("transient"))
	if err := s.SaveManifest("snap-tmp", []string{d}); err != nil {
		t.Fatal(err)
	}
	// Delete the manifest → its layer becomes unreferenced.
	if err := s.DeleteManifest("snap-tmp"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GC(); err != nil {
		t.Fatalf("gc: %v", err)
	}
	if s.Has(d) {
		t.Fatal("layer of deleted manifest must be GC'd")
	}
}

func TestStore_SizeBytes(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	s.Put([]byte("12345")) // 5 bytes
	s.Put([]byte("1234567890"))
	total, err := s.SizeBytes()
	if err != nil {
		t.Fatal(err)
	}
	if total != 15 {
		t.Fatalf("expected 15 bytes, got %d", total)
	}
}

func TestStore_LargeContentRoundTrip(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// 1 MiB payload — beyond the old 64KiB WriteFile limit, proving the
	// layerstore path handles large snapshots (KIP-16 M2 driver).
	content := strings.Repeat("k8e-layer-", 128*1024) // 1 MiB
	digest, err := s.Put([]byte(content))
	if err != nil {
		t.Fatalf("put large: %v", err)
	}
	got, err := s.Get(digest)
	if err != nil {
		t.Fatalf("get large: %v", err)
	}
	if len(got) != len(content) {
		t.Fatalf("size mismatch: %d vs %d", len(got), len(content))
	}
}
