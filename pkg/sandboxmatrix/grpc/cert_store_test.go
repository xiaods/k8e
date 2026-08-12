package grpc

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestReplaceAPIKeysReverseIndex(t *testing.T) {
	s := &Server{}
	s.replaceAPIKeys(map[string]string{
		"agent-a": "k8e-aaa",
		"agent-b": "k8e-bbb",
	})
	if got := s.lookupAPIKeyName("k8e-bbb"); got != "agent-b" {
		t.Fatalf("lookup: got %q want agent-b", got)
	}
	if got := s.lookupAPIKeyName("missing"); got != "" {
		t.Fatalf("missing key should be empty, got %q", got)
	}

	// Swap to a smaller set and ensure old tokens are gone.
	s.replaceAPIKeys(map[string]string{"agent-a": "k8e-aaa"})
	if got := s.lookupAPIKeyName("k8e-bbb"); got != "" {
		t.Fatalf("revoked token still resolves: %q", got)
	}
	if got := s.lookupAPIKeyName("k8e-aaa"); got != "agent-a" {
		t.Fatalf("remaining token: got %q", got)
	}

	s.replaceAPIKeys(nil)
	if got := s.lookupAPIKeyName("k8e-aaa"); got != "" {
		t.Fatalf("nil store should clear reverse index, got %q", got)
	}
}

func TestIssuedCertStorePruneAndAtomicSave(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "issued.json")
	store := newIssuedCertStore(path)

	now := time.Now()
	store.Add("a", "fp-old", now.Add(-48*time.Hour), now.Add(-time.Hour))
	store.Add("a", "fp-live", now, now.Add(24*time.Hour))
	store.PruneExpired()

	// Reload from disk — only the live record should remain.
	reloaded := newIssuedCertStore(path)
	recs := reloaded.FindByKeyName("a")
	if len(recs) != 1 {
		t.Fatalf("expected 1 live record, got %d", len(recs))
	}
	if recs[0].Fingerprint != "fp-live" {
		t.Fatalf("unexpected record: %+v", recs[0])
	}

	// No leftover temp files from atomic save.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if filepath.Ext(e.Name()) == ".tmp" || len(e.Name()) > 4 && e.Name()[len(e.Name())-4:] == ".tmp" {
			t.Fatalf("temp file leaked: %s", e.Name())
		}
	}
}
