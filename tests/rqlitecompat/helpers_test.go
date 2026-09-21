package rqlitecompat

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// assertEqualHistory checks the retained revisions of a key field by field,
// so a wrong revision, version, tombstone flag or value is reported by value.
func (s *Store) assertEqualHistory(ctx context.Context, key []byte, want []HistoryEntry) error {
	got, err := s.History(ctx, key)
	if err != nil {
		return fmt.Errorf("read history of %q: %w", key, err)
	}
	if len(got) != len(want) {
		return fmt.Errorf("history of %q has %d entries (%+v), want %d (%+v)", key, len(got), got, len(want), want)
	}
	for i := range want {
		if got[i].ModRevision != want[i].ModRevision || got[i].Version != want[i].Version ||
			got[i].Deleted != want[i].Deleted || !bytes.Equal(got[i].Value, want[i].Value) {
			return fmt.Errorf("history[%d] of %q = %+v, want %+v", i, key, got[i], want[i])
		}
	}
	return nil
}

// assertOrderedPrefix checks that a prefix scan returns exactly the expected
// keys in byte order.
func (s *Store) assertOrderedPrefix(ctx context.Context, prefix []byte, want [][]byte) error {
	entries, revision, err := s.RangePrefix(ctx, prefix, int64(len(want)+8))
	if err != nil {
		return fmt.Errorf("prefix scan %x: %w", prefix, err)
	}
	if len(entries) != len(want) {
		return fmt.Errorf("prefix scan %x returned %d entries, want %d", prefix, len(entries), len(want))
	}
	for i := range want {
		if !bytes.Equal(entries[i].Key, want[i]) {
			return fmt.Errorf("prefix scan %x entry %d = %x, want %x", prefix, i, entries[i].Key, want[i])
		}
	}
	if revision < 1 {
		return fmt.Errorf("prefix scan %x reported revision %d", prefix, revision)
	}
	return nil
}

// assertKV checks the full entry state after a write.
func (s *Store) assertKV(t *testing.T, ctx context.Context, key, value []byte, create, mod, version int64) {
	t.Helper()
	kv, _, err := s.Range(ctx, key)
	if err != nil {
		t.Fatalf("read %q: %v", key, err)
	}
	if !kv.Exists {
		t.Fatalf("key %q does not exist, want value %q", key, value)
	}
	if !bytes.Equal(kv.Value, value) {
		t.Fatalf("key %q value = %q, want %q", key, kv.Value, value)
	}
	if kv.CreateRevision != create || kv.ModRevision != mod || kv.Version != version {
		t.Fatalf("key %q = create=%d mod=%d version=%d, want %d/%d/%d",
			key, kv.CreateRevision, kv.ModRevision, kv.Version, create, mod, version)
	}
}
