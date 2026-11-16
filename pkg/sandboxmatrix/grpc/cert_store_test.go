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

func TestRevocationListPersistsAcrossRestart(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "revoked.json")

	rl := newRevocationList(path)
	rl.Revoke("fp-1")
	if !rl.IsRevoked("fp-1") {
		t.Fatal("fingerprint should be revoked in memory")
	}

	// A new list (a gateway restart) must load the persisted revocation:
	// an in-memory-only list would silently make the cert valid again.
	reloaded := newRevocationList(path)
	if !reloaded.IsRevoked("fp-1") {
		t.Fatal("revocation must survive a restart")
	}
	if reloaded.IsRevoked("fp-2") {
		t.Fatal("unknown fingerprint must not be revoked")
	}

	// A blank fingerprint must not revoke anything (no wildcard).
	blank := newRevocationList(filepath.Join(dir, "blank.json"))
	blank.Revoke("")
	if blank.IsRevoked("") || blank.IsRevoked("anything") {
		t.Fatal("blank fingerprint must not revoke anything")
	}
}

func TestRevokeRemovedKeysReconcilesInactiveKeys(t *testing.T) {
	dir := t.TempDir()
	s := &Server{}
	s.issuedStore = newIssuedCertStore(filepath.Join(dir, "issued.json"))
	s.revocList = newRevocationList(filepath.Join(dir, "revoked.json"))

	now := time.Now()
	s.issuedStore.Add("deleted-key", "fp-deleted", now, now.Add(24*time.Hour))
	s.issuedStore.Add("live-key", "fp-live", now, now.Add(24*time.Hour))

	// A load whose active set only contains live-key must revoke the other —
	// this is the "key deleted while the gateway was down" case, which the old
	// prev!=nil diff never reached on the first load after a restart.
	s.revokeRemovedKeys(map[string]string{"live-key": "{{SECRET_orb4cf10}}"})

	if !s.revocList.IsRevoked("fp-deleted") {
		t.Fatal("certificate of an inactive key must be revoked")
	}
	if s.revocList.IsRevoked("fp-live") {
		t.Fatal("certificate of an active key must not be revoked")
	}

	// The reconciled revocation is persisted, so a restart keeps it.
	if !newRevocationList(filepath.Join(dir, "revoked.json")).IsRevoked("fp-deleted") {
		t.Fatal("reconciled revocation must be persisted")
	}
}
