package rqlitecompat

import (
	"context"
	"fmt"
	"testing"
	"time"

	compat "github.com/xiaods/k8e/pkg/rqlitecompat"
	"google.golang.org/grpc/codes"
)

// newM1Store attaches the layer to a fresh rqlite cluster and bootstraps the
// schema, so a store test drives the real SQLite layout through the real
// rqlite HTTP API.
func newM1Store(t *testing.T, nodes int) (*Cluster, *compat.Store) {
	t.Helper()
	cluster := StartCluster(t, nodes)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	client := compat.NewClient(cluster.Endpoints()...)
	requireNoError(t, "bootstrap schema", compat.Bootstrap(ctx, client))
	return cluster, compat.NewStore(client, "store-test")
}

func m1Ctx(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestM1StoreRevisionSequence is the differential-behavior foundation: it
// pins the exact revision/version/create sequence, historical reads, deletes,
// compaction and durable events of the layer.
func TestM1StoreRevisionSequence(t *testing.T) {
	_, s := newM1Store(t, 1)
	ctx := m1Ctx(t)

	put := func(key, value string) *compat.PutResult {
		t.Helper()
		res, err := s.Put(ctx, compat.PutOptions{Key: []byte(key), Value: []byte(value)})
		requireNoError(t, "put "+key, err)
		return res
	}

	requireEqual(t, "first put revision", put("a", "1").Revision, int64(1))
	requireEqual(t, "second put revision", put("a", "2").Revision, int64(2))

	kv, err := s.Range(ctx, compat.RangeOptions{Key: []byte("a")})
	requireNoError(t, "range a", err)
	requireEqual(t, "range count", len(kv.KVs), 1)
	requireEqual(t, "version after two puts", kv.KVs[0].Version, int64(2))
	requireEqual(t, "create revision", kv.KVs[0].CreateRevision, int64(1))
	requireEqual(t, "mod revision", kv.KVs[0].ModRevision, int64(2))
	requireBytes(t, "value", kv.KVs[0].Value, []byte("2"))

	old, err := s.Range(ctx, compat.RangeOptions{Key: []byte("a"), Revision: 1})
	requireNoError(t, "historical range at revision 1", err)
	requireEqual(t, "historical count", len(old.KVs), 1)
	requireBytes(t, "historical value", old.KVs[0].Value, []byte("1"))

	del, err := s.DeleteRange(ctx, compat.DeleteOptions{Key: []byte("a")})
	requireNoError(t, "delete a", err)
	requireEqual(t, "deleted count", del.Deleted, int64(1))
	requireEqual(t, "delete revision", del.Revision, int64(3))

	gone, err := s.Range(ctx, compat.RangeOptions{Key: []byte("a")})
	requireNoError(t, "range deleted a", err)
	requireEqual(t, "deleted key still visible", len(gone.KVs), 0)

	events, more, err := s.EventsAfter(ctx, 0, 100)
	requireNoError(t, "read events", err)
	requireTrue(t, "event page is complete", !more)
	requireEqual(t, "event count", len(events), 3)
	requireEqual(t, "event 1 revision", events[0].Revision, int64(1))
	requireEqual(t, "event 2 revision", events[1].Revision, int64(2))
	requireEqual(t, "event 3 revision", events[2].Revision, int64(3))
	requireEqual(t, "event 3 type is delete", events[2].Type, compat.EventDelete)
	requireBytes(t, "event 3 key", events[2].KV.Key, []byte("a"))
	requireTrue(t, "event 3 carries the previous value", events[2].PrevKV != nil)
	requireBytes(t, "event 3 previous value", events[2].PrevKV.Value, []byte("2"))
}

// TestM1StoreCompactionDropsEventsAndHistory checks that a compacted store
// keeps serving the head state while the history and events at or below the
// compacted revision are gone.
func TestM1StoreCompactionDropsEventsAndHistory(t *testing.T) {
	_, s := newM1Store(t, 1)
	ctx := m1Ctx(t)

	for i, value := range []string{"1", "2", "3"} {
		_, err := s.Put(ctx, compat.PutOptions{Key: []byte("k"), Value: []byte(value)})
		requireNoError(t, fmt.Sprintf("put %d", i), err)
	}
	requireNoError(t, "compact to revision 2", s.Compact(ctx, 2, false))

	head, err := s.Range(ctx, compat.RangeOptions{Key: []byte("k")})
	requireNoError(t, "range head after compaction", err)
	requireEqual(t, "head count after compaction", len(head.KVs), 1)
	requireBytes(t, "head value after compaction", head.KVs[0].Value, []byte("3"))

	_, err = s.Range(ctx, compat.RangeOptions{Key: []byte("k"), Revision: 1})
	requireCode(t, "range below the compacted revision", err, codes.OutOfRange)
	at, err := s.Range(ctx, compat.RangeOptions{Key: []byte("k"), Revision: 2})
	requireNoError(t, "range at the compacted revision", err)
	requireEqual(t, "range at the compacted revision count", len(at.KVs), 1)

	events, _, err := s.EventsAfter(ctx, 0, 100)
	requireNoError(t, "read events after compaction", err)
	requireEqual(t, "surviving event count", len(events), 1)
	requireEqual(t, "surviving event revision", events[0].Revision, int64(3))
}

// TestM1StoreBinaryAndRange pins binary keys/values, prefix ranges, count-only
// and limit/more, the shapes the K8s apiserver depends on.
func TestM1StoreBinaryAndRange(t *testing.T) {
	_, s := newM1Store(t, 1)
	ctx := m1Ctx(t)

	binaryKey := []byte{0x00, 0xff, 0x41, 0x00}
	binaryValue := []byte{0xde, 0xad, 0xbe, 0xef, 0x00}
	_, err := s.Put(ctx, compat.PutOptions{Key: binaryKey, Value: binaryValue})
	requireNoError(t, "put binary key", err)
	got, err := s.Range(ctx, compat.RangeOptions{Key: binaryKey})
	requireNoError(t, "range binary key", err)
	requireEqual(t, "binary key count", len(got.KVs), 1)
	requireBytes(t, "binary key round-trip", got.KVs[0].Key, binaryKey)
	requireBytes(t, "binary value round-trip", got.KVs[0].Value, binaryValue)

	emptyValue, err := s.Put(ctx, compat.PutOptions{Key: []byte("empty"), Value: []byte{}})
	requireNoError(t, "put empty value", err)
	requireTrue(t, "empty value put returns the revision", emptyValue.Revision > 0)
	empty, err := s.Range(ctx, compat.RangeOptions{Key: []byte("empty")})
	requireNoError(t, "range empty value", err)
	requireEqual(t, "empty value count", len(empty.KVs), 1)
	requireBytes(t, "empty value is an empty BLOB, not NULL", empty.KVs[0].Value, []byte{})

	for i, key := range []string{"p/a", "p/b", "p/c", "p/d", "q/a"} {
		_, err := s.Put(ctx, compat.PutOptions{Key: []byte(key), Value: []byte(fmt.Sprint(i))})
		requireNoError(t, "put "+key, err)
	}
	page, err := s.Range(ctx, compat.RangeOptions{Key: []byte("p/"), RangeEnd: []byte("p0"), Limit: 2})
	requireNoError(t, "prefix range", err)
	requireEqual(t, "prefix page size", len(page.KVs), 2)
	requireTrue(t, "prefix range reports more", page.More)
	// Like etcd's unary Range (withTotalCount), Count is the number of matching
	// keys ignoring the limit, while the page itself is truncated.
	requireEqual(t, "prefix range count is the total match count", page.Count, int64(4))
	requireBytes(t, "prefix page key 0", page.KVs[0].Key, []byte("p/a"))
	requireBytes(t, "prefix page key 1", page.KVs[1].Key, []byte("p/b"))

	count, err := s.Range(ctx, compat.RangeOptions{Key: []byte("p/"), RangeEnd: []byte("p0"), CountOnly: true})
	requireNoError(t, "count-only range", err)
	requireEqual(t, "count-only ignores the limit", count.Count, int64(4))
	requireEqual(t, "count-only returns no keys", len(count.KVs), 0)
}
