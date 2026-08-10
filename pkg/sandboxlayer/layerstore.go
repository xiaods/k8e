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
// publishing an existing digest is a no-op. The layer file is fsync'd before
// rename so a crash never leaves a torn layer under the final name.
func (s *Store) Put(content []byte) (string, error) {
	digest := Digest(content)
	layerPath := s.layerPath(digest)
	if _, err := os.Stat(layerPath); err == nil {
		return digest, nil // already present
	}

	tmp, err := os.CreateTemp(s.stagingDir(), "layer-")
	if err != nil {
		return "", fmt.Errorf("layerstore stage: %w", err)
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName) // no-op after successful rename

	if _, err := tmp.Write(content); err != nil {
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

// Get returns the layer content for digest.
func (s *Store) Get(digest string) ([]byte, error) {
	b, err := os.ReadFile(s.layerPath(digest))
	if err != nil {
		return nil, fmt.Errorf("layerstore get %s: %w", digest, err)
	}
	return b, nil
}

// Manifest is a content-addressed snapshot: an ordered list of layer digests
// whose concatenation reconstructs the workspace.
type Manifest struct {
	SchemaVersion int      `json:"schema_version"`
	Layers        []string `json:"layers"`
}

// ManifestVersion is the current manifest schema.
const ManifestVersion = 1

// SaveManifest writes a manifest under manifests/<name>.json (atomic: tmp +
// rename). Layers referenced by any saved manifest are protected from GC.
func (s *Store) SaveManifest(name string, layers []string) error {
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
	referenced := map[string]bool{}
	names, err := s.ListManifests()
	if err != nil {
		return 0, err
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
		info, err := e.Info()
		if err != nil {
			continue
		}
		if err := os.Remove(filepath.Join(s.layerDir(), name)); err == nil {
			removed += info.Size()
		}
	}
	return removed, nil
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

func trimExt(name string) string {
	for i := len(name) - 1; i >= 0; i-- {
		if name[i] == '.' {
			return name[:i]
		}
	}
	return name
}
