package rqlitecompat

import (
	"bytes"
	"context"
	"fmt"
	"testing"
)

// requireNoError fails the test when err is non-nil, naming what was attempted.
// The requirement helpers keep the scenarios readable and their cyclomatic
// complexity low: one assertion is one statement, not one branch.
func requireNoError(t *testing.T, what string, err error) {
	t.Helper()
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
}

// requireTrue fails the test when ok is false; what must read as a sentence
// describing the violated expectation.
func requireTrue(t *testing.T, what string, ok bool) {
	t.Helper()
	if !ok {
		t.Fatalf("%s", what)
	}
}

// requireEqual fails the test when got differs from want.
func requireEqual[T comparable](t *testing.T, what string, got, want T) {
	t.Helper()
	if got != want {
		t.Fatalf("%s = %v, want %v", what, got, want)
	}
}

// requireBytes fails the test when two byte strings differ.
func requireBytes(t *testing.T, what string, got, want []byte) {
	t.Helper()
	if !bytes.Equal(got, want) {
		t.Fatalf("%s = %x, want %x", what, got, want)
	}
}

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

// assertHistoryValue checks that the retained history of a key has an entry at
// modRevision holding exactly value. Newer revisions written concurrently
// exist sometimes, so the tail is deliberately not checked.
func (s *Store) assertHistoryValue(ctx context.Context, key []byte, modRevision int64, value []byte) error {
	entries, err := s.History(ctx, key)
	if err != nil {
		return fmt.Errorf("read history of %q: %w", key, err)
	}
	found := false
	for _, entry := range entries {
		if entry.ModRevision != modRevision {
			continue
		}
		found = true
		if !bytes.Equal(entry.Value, value) {
			return fmt.Errorf("history of %q holds %q for revision %d, want %q", key, entry.Value, modRevision, value)
		}
	}
	if !found {
		return fmt.Errorf("history of %q (%+v) has no row for the committed revision %d", key, entries, modRevision)
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
func (s *Store) assertKV(ctx context.Context, t *testing.T, key, value []byte, create, mod, version int64) {
	t.Helper()
	kv, _, err := s.Range(ctx, key)
	requireNoError(t, fmt.Sprintf("read %q", key), err)
	requireTrue(t, fmt.Sprintf("key %q does not exist, want value %q", key, value), kv.Exists)
	requireBytes(t, fmt.Sprintf("value of key %q", key), kv.Value, value)
	requireTrue(t, fmt.Sprintf("key %q = create=%d mod=%d version=%d, want %d/%d/%d",
		key, kv.CreateRevision, kv.ModRevision, kv.Version, create, mod, version),
		kv.CreateRevision == create && kv.ModRevision == mod && kv.Version == version)
}
