package sandboxlayer

import (
	"bytes"
	"os"
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
	// Layers are zstd-compressed on disk (KIP-16 M2); SizeBytes sums the
	// on-disk files, so it matches the actual layer directory.
	entries, err := os.ReadDir(s.layerDir())
	if err != nil {
		t.Fatal(err)
	}
	var want int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			want += info.Size()
		}
	}
	if total != want {
		t.Fatalf("SizeBytes %d != on-disk %d", total, want)
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

// TestStore_Autosquash verifies SaveManifest squashes over-threshold layer
// counts into a single consolidated layer (KIP-16 M2 autosquash, mirroring
// ephemeral-sandbox squash_at_n_layers).
func TestStore_Autosquash(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// Exactly at threshold: no squash.
	atThreshold := make([]string, SquashThreshold)
	for i := range atThreshold {
		d, _ := s.Put([]byte{byte(i)})
		atThreshold[i] = d
	}
	if err := s.SaveManifest("snap-at", atThreshold); err != nil {
		t.Fatalf("save at threshold: %v", err)
	}
	mAt, _ := s.LoadManifest("snap-at")
	if len(mAt.Layers) != SquashThreshold {
		t.Fatalf("at threshold must not squash: %d layers", len(mAt.Layers))
	}

	// One over threshold: squashed to a single consolidated layer.
	over := make([]string, SquashThreshold+1)
	for i := range over {
		d, _ := s.Put([]byte{byte(200 + i)})
		over[i] = d
	}
	if err := s.SaveManifest("snap-over", over); err != nil {
		t.Fatalf("save over threshold: %v", err)
	}
	mOver, _ := s.LoadManifest("snap-over")
	if len(mOver.Layers) != 1 {
		t.Fatalf("over threshold must squash to 1 layer, got %d", len(mOver.Layers))
	}
	// Consolidated content must reconstruct the concatenation.
	got, err := s.Get(mOver.Layers[0])
	if err != nil {
		t.Fatalf("get consolidated: %v", err)
	}
	var want []byte
	for i := range over {
		want = append(want, byte(200+i))
	}
	if !bytes.Equal(got, want) {
		t.Fatal("squashed layer content mismatch")
	}
}

// TestStore_Autosquash_ReleasesOriginals verifies the original over-threshold
// layers become unreferenced and are reclaimed by GC.
func TestStore_Autosquash_ReleasesOriginals(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	layers := make([]string, SquashThreshold+1)
	for i := range layers {
		d, _ := s.Put([]byte{byte(100 + i)})
		layers[i] = d
	}
	if err := s.SaveManifest("snap-sq", layers); err != nil {
		t.Fatal(err)
	}
	// GC should reclaim the squashed-away originals (only the consolidated
	// layer is referenced now).
	if _, err := s.GC(); err != nil {
		t.Fatalf("gc: %v", err)
	}
	for _, d := range layers {
		if s.Has(d) {
			t.Fatalf("squashed-original layer %s must be reclaimed", d)
		}
	}
	m, _ := s.LoadManifest("snap-sq")
	if !s.Has(m.Layers[0]) {
		t.Fatal("consolidated layer must survive GC")
	}
}
func TestStore_ZstdCompressedOnDisk(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// Highly compressible payload: repeated text.
	content := []byte(strings.Repeat("k8e-sandbox-layer-content-", 5000)) // ~125KB
	d, err := s.Put(content)
	if err != nil {
		t.Fatalf("put: %v", err)
	}
	got, err := s.Get(d)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatal("zstd round-trip mismatch")
	}
	info, err := os.Stat(s.layerPath(d))
	if err != nil {
		t.Fatalf("stat layer: %v", err)
	}
	if info.Size() >= int64(len(content)) {
		t.Fatalf("expected compressed layer smaller than raw (%d >= %d)", info.Size(), len(content))
	}
}

// TestStore_GetLegacyUncompressedLayer verifies decompress falls back to raw
// bytes for layers written before zstd storage (backward compatibility).
func TestStore_GetLegacyUncompressedLayer(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	legacy := []byte("legacy uncompressed layer content")
	d := Digest(legacy)
	if err := os.WriteFile(s.layerPath(d), legacy, 0o600); err != nil {
		t.Fatalf("write legacy layer: %v", err)
	}
	got, err := s.Get(d)
	if err != nil {
		t.Fatalf("get legacy: %v", err)
	}
	if !bytes.Equal(got, legacy) {
		t.Fatal("legacy layer fallback mismatch")
	}
}
func TestStore_PutChunksAssemble(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	// 10 bytes with 4-byte chunks → 3 layers (4+4+2).
	content := []byte("0123456789")
	digests, err := s.PutChunks(content, 4)
	if err != nil {
		t.Fatalf("put chunks: %v", err)
	}
	if len(digests) != 3 {
		t.Fatalf("expected 3 chunks, got %d", len(digests))
	}
	got, err := s.Assemble(digests)
	if err != nil {
		t.Fatalf("assemble: %v", err)
	}
	if !bytes.Equal(got, content) {
		t.Fatalf("chunk round-trip mismatch: %q vs %q", got, content)
	}
}

// TestStore_DeltaSharedChunks verifies two snapshots with a common prefix share
// layers, so Delta reports only the unique tail as missing.
func TestStore_DeltaSharedChunks(t *testing.T) {
	s, err := New(t.TempDir())
	if err != nil {
		t.Fatalf("new store: %v", err)
	}
	base := []byte("AAAA-BBBB")
	next := []byte("AAAA-BBBB-CCCC") // shares first 8 bytes → chunks overlap
	baseLayers, err := s.PutChunks(base, 5)
	if err != nil {
		t.Fatal(err)
	}
	nextLayers, err := s.PutChunks(next, 5)
	if err != nil {
		t.Fatal(err)
	}
	have := &Manifest{Layers: baseLayers}
	want := &Manifest{Layers: nextLayers}
	missing := Delta(have, want)
	// Shared prefix "AAAA-" is one chunk → at most len(next)-1 missing.
	if len(missing) >= len(nextLayers) {
		t.Fatalf("expected some shared chunks, missing=%d of %d", len(missing), len(nextLayers))
	}
}
