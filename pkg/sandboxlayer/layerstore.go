// Package sandboxlayer implements a content-addressed layer store for sandbox
// snapshots (KIP-16 M2 / issue #511).
//
// It mirrors the mechanism of ephemeral-sandbox's layerstack without copying
// its delivery form: SHA-256 content addressing, immutable layers, atomic
// staging+publish, manifest-based references, and lease-driven GC. The store is
// pure Go and file-backed, so it stays inside the single-binary constraint.
package sandboxlayer

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"

	"github.com/klauspost/compress/zstd"
)

// Store is a content-addressed layer store rooted at dir.
//
// Layout:
//
//	<dir>/layers/<sha256>          immutable published layers
//	<dir>/staging/                 temporary files before publish
//	<dir>/manifests/<name>.json    snapshot manifests referencing layers
type Store struct {
	dir string
}

// New opens (creating if needed) a layer store at dir.
func New(dir string) (*Store, error) {
	for _, sub := range []string{"layers", "staging", "manifests"} {
		if err := os.MkdirAll(filepath.Join(dir, sub), 0o700); err != nil {
			return nil, fmt.Errorf("layerstore mkdir %s: %w", sub, err)
		}
	}
	return &Store{dir: dir}, nil
}

// Digest returns the hex sha256 of b.
func Digest(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

// Put stages content and publishes it atomically under its sha256. Idempotent:
// publishing an existing digest is a no-op. The layer is stored zstd-compressed
// (content-addressed by the UNCOMPRESSED digest, like OCI registries) so
// dedup works on content and disk usage stays compact. The file is fsync'd
// before rename so a crash never leaves a torn layer under the final name.
func (s *Store) Put(content []byte) (string, error) {
	digest := Digest(content)
	layerPath := s.layerPath(digest)
	if _, err := os.Stat(layerPath); err == nil {
		return digest, nil // already present
	}

	compressed := compress(content)
	tmp, err := os.CreateTemp(s.stagingDir(), "layer-")
	if err != nil {
		return "", fmt.Errorf("layerstore stage: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(compressed); err != nil {
		tmp.Close() //nolint:errcheck
		return "", fmt.Errorf("layerstore write: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return "", fmt.Errorf("layerstore fsync: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return "", fmt.Errorf("layerstore close: %w", err)
	}
	if err := os.Rename(tmpName, layerPath); err != nil {
		return "", fmt.Errorf("layerstore publish: %w", err)
	}
	return digest, nil
}

// Has reports whether a layer with digest exists.
func (s *Store) Has(digest string) bool {
	_, err := os.Stat(s.layerPath(digest))
	return err == nil
}

// PutChunks splits content into fixed-size chunks and stores each as a layer,
// returning the ordered digest list. Chunks dedup across calls, so snapshots
// sharing identical chunks reference the same layer (Delta-friendly).
func (s *Store) PutChunks(content []byte, chunkSize int) ([]string, error) {
	if chunkSize <= 0 {
		chunkSize = 4 * 1024 * 1024
	}
	var digests []string
	for start := 0; start < len(content); start += chunkSize {
		end := start + chunkSize
		if end > len(content) {
			end = len(content)
		}
		d, err := s.Put(content[start:end])
		if err != nil {
			return nil, err
		}
		digests = append(digests, d)
	}
	if len(content) == 0 {
		// Empty workspace: store one empty layer so the manifest is non-empty.
		d, err := s.Put(nil)
		if err != nil {
			return nil, err
		}
		digests = append(digests, d)
	}
	return digests, nil
}

// Assemble concatenates layer contents in digest order, reconstructing the
// original payload (each layer decompressed transparently).
func (s *Store) Assemble(digests []string) ([]byte, error) {
	var out []byte
	for _, d := range digests {
		b, err := s.Get(d)
		if err != nil {
			return nil, fmt.Errorf("layerstore assemble %s: %w", d, err)
		}
		out = append(out, b...)
	}
	return out, nil
}

// Get returns the (decompressed) layer content for digest.
func (s *Store) Get(digest string) ([]byte, error) {
	b, err := os.ReadFile(s.layerPath(digest))
	if err != nil {
		return nil, fmt.Errorf("layerstore get %s: %w", digest, err)
	}
	return decompress(b)
}

// Manifest is a content-addressed snapshot: an ordered list of layer digests
// whose concatenation reconstructs the workspace.
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	Layers        []string `json:"layers"`
}

// ManifestVersion is the current manifest schema.
const ManifestVersion = 1

// SquashThreshold is the max layers a manifest may hold before SaveManifest
// squashes it into a single consolidated layer (mirrors ephemeral-sandbox's
// squash_at_n_layers). Bounds manifest size and restore cost.
const SquashThreshold = 32

// SaveManifest writes a manifest under manifests/<name>.json (atomic: tmp +
// rename). Layers referenced by any saved manifest are protected from GC.
// When the layer count exceeds SquashThreshold, the layers are squashed into
// one consolidated layer first (KIP-16 M2 autosquash).
func (s *Store) SaveManifest(name string, layers []string) error {
	if len(layers) > SquashThreshold {
		consolidated, err := s.squash(layers)
		if err != nil {
			return err
		}
		layers = []string{consolidated}
	}
	m := Manifest{SchemaVersion: ManifestVersion, Layers: layers}
	b, err := json.Marshal(m)
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(s.manifestDir(), "manifest-")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if _, err := tmp.Write(b); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close() //nolint:errcheck
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, filepath.Join(s.manifestDir(), name+".json"))
}

// LoadManifest reads a manifest by name.
func (s *Store) LoadManifest(name string) (*Manifest, error) {
	b, err := os.ReadFile(filepath.Join(s.manifestDir(), name+".json"))
	if err != nil {
		return nil, fmt.Errorf("layerstore manifest %s: %w", name, err)
	}
	var m Manifest
	if err := json.Unmarshal(b, &m); err != nil {
		return nil, fmt.Errorf("layerstore manifest %s: %w", name, err)
	}
	return &m, nil
}

// DeleteManifest removes a manifest by name, releasing its layers for GC.
func (s *Store) DeleteManifest(name string) error {
	return os.Remove(filepath.Join(s.manifestDir(), name+".json"))
}

// ListManifests returns manifest names (sorted).
func (s *Store) ListManifests() ([]string, error) {
	entries, err := os.ReadDir(s.manifestDir())
	if err != nil {
		return nil, err
	}
	var names []string
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		names = append(names, trimExt(e.Name()))
	}
	sort.Strings(names)
	return names, nil
}

// Delta computes which layers of want are missing from have. Useful for
// incremental transfer: only the returned layers need to be sent/restored.
func Delta(have, want *Manifest) []string {
	present := make(map[string]bool, len(have.Layers))
	for _, d := range have.Layers {
		present[d] = true
	}
	var missing []string
	for _, d := range want.Layers {
		if !present[d] {
			missing = append(missing, d)
		}
	}
	return missing
}

// GC removes layers not referenced by any saved manifest (lease-driven: a
// snapshot's layers are its lease). Returns the number of bytes removed.
func (s *Store) GC() (int64, error) {
	referenced := s.referencedLayers()

	entries, err := os.ReadDir(s.layerDir())
	if err != nil {
		return 0, err
	}
	var removed int64
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if referenced[name] {
			continue
		}
		if removedBytes, err := s.removeLayer(e, name); err == nil {
			removed += removedBytes
		}
	}
	return removed, nil
}

// referencedLayers returns the set of layer digests leased by saved manifests.
func (s *Store) referencedLayers() map[string]bool {
	referenced := map[string]bool{}
	names, err := s.ListManifests()
	if err != nil {
		return referenced
	}
	for _, n := range names {
		m, err := s.LoadManifest(n)
		if err != nil {
			continue
		}
		for _, d := range m.Layers {
			referenced[d] = true
		}
	}
	return referenced
}

// removeLayer deletes one unreferenced layer entry, returning its size.
func (s *Store) removeLayer(e os.DirEntry, name string) (int64, error) {
	info, err := e.Info()
	if err != nil {
		return 0, err
	}
	if err := os.Remove(filepath.Join(s.layerDir(), name)); err != nil {
		return 0, err
	}
	return info.Size(), nil
}

// SizeBytes returns total bytes of all stored layers (for status surfacing).
func (s *Store) SizeBytes() (int64, error) {
	entries, err := os.ReadDir(s.layerDir())
	if err != nil {
		return 0, err
	}
	var total int64
	for _, e := range entries {
		if info, err := e.Info(); err == nil {
			total += info.Size()
		}
	}
	return total, nil
}

func (s *Store) layerDir() string   { return filepath.Join(s.dir, "layers") }
func (s *Store) stagingDir() string { return filepath.Join(s.dir, "staging") }
func (s *Store) manifestDir() string {
	return filepath.Join(s.dir, "manifests")
}
func (s *Store) layerPath(digest string) string {
	return filepath.Join(s.layerDir(), digest)
}

// squash consolidates layers into a single layer containing their
// concatenated (decompressed) content. Used by autosquash to bound manifest
// growth; the original layers become unreferenced and are reclaimed by GC.
func (s *Store) squash(layers []string) (string, error) {
	var out []byte
	for _, d := range layers {
		b, err := s.Get(d)
		if err != nil {
			return "", fmt.Errorf("layerstore squash get %s: %w", d, err)
		}
		out = append(out, b...)
	}
	digest, err := s.Put(out)
	if err != nil {
		return "", fmt.Errorf("layerstore squash put: %w", err)
	}
	return digest, nil
}

func trimExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}

// compress zstd-compresses layer content. The digest stays the sha256 of the
// UNCOMPRESSED bytes so dedup is content-based (OCI-style).
func compress(content []byte) []byte {
	enc, err := zstd.NewWriter(nil)
	if err != nil {
		return content // never fail a store on compression
	}
	defer enc.Close()
	return enc.EncodeAll(content, nil)
}

// decompress restores a layer stored zstd-compressed. Falls back to raw bytes
// for layers written by older (uncompressed) versions.
func decompress(b []byte) ([]byte, error) {
	dec, err := zstd.NewReader(nil)
	if err != nil {
		return b, nil
	}
	defer dec.Close()
	out, err := dec.DecodeAll(b, nil)
	if err != nil {
		// Not a valid zstd frame — treat as legacy uncompressed layer.
		return b, nil
	}
	return out, nil
}
