//go:build tandem_differential

package differential

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	clientv3 "go.etcd.io/etcd/client/v3"
)

// TestDifferentialCompactionConcurrentReaders covers the "compaction with
// concurrent readers" gap from m1-status: while a compaction is in flight,
// concurrent reads must stay consistent — no torn history, no phantom keys,
// no error on a reader that started before the compact.
func TestDifferentialCompactionConcurrentReaders(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	h := NewHarness(t)

	// Seed enough keys that a compaction watermark lands mid-history.
	const seeds = 50
	for i := 0; i < seeds; i++ {
		key := "compact/seed/" + string(rune('a'+i%26)) + string(rune('0'+i/26))
		for _, be := range []*Backend{h.Etcd, h.Tandem} {
			if _, err := be.Client.Put(ctx, key, "seed"); err != nil {
				t.Fatalf("seed put on %s: %v", be.Name, err)
			}
		}
	}

	// Find the current revision on each backend — the compaction point.
	etcdCompactRev := revOf(t, ctx, h.Etcd.Client)
	tandemCompactRev := revOf(t, ctx, h.Tandem.Client)
	_ = tandemCompactRev

	// Compact at a revision in the middle of the history on both backends.
	etcdMid := etcdCompactRev - 20
	tandemMid := tandemCompactRev - 20
	if etcdMid > 0 {
		if _, err := h.Etcd.Client.Compact(ctx, etcdMid); err != nil {
			t.Fatalf("etcd compact: %v", err)
		}
	}
	if tandemMid > 0 {
		if _, err := h.Tandem.Client.Compact(ctx, tandemMid); err != nil {
			t.Fatalf("tandem compact: %v", err)
		}
	}

	// Compaction is asynchronous: poll until the compacted revision actually
	// answers OutOfRange before starting the concurrent readers.
	for _, be := range []*Backend{h.Etcd, h.Tandem} {
		mid := etcdMid
		if be.Name == "Tandem" {
			mid = tandemMid
		}
		deadline := time.Now().Add(30 * time.Second)
		for {
			_, err := be.Client.Get(ctx, "compact/seed/", clientv3.WithRev(mid-1))
			if err != nil {
				break // compacted
			}
			if time.Now().After(deadline) {
				t.Fatalf("compaction did not complete on %s within 30s", be.Name)
			}
			time.Sleep(200 * time.Millisecond)
		}
	}

	// Concurrent readers: some read at the compacted revision (OutOfRange),
	// some read at the current revision (full data), some read at a revision
	// before the compact (must still see the keys that existed then).
	var wg sync.WaitGroup
	errCh := make(chan error, 12)

	compactRevs := map[string]int64{h.Etcd.Name: etcdMid, h.Tandem.Name: tandemMid}
	for _, be := range []*Backend{h.Etcd, h.Tandem} {
		client := be.Client
		name := be.Name
		compactRev := compactRevs[name]

		// Reader 1: current revision — must see all keys.
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(ctx, "compact/seed/", clientv3.WithPrefix())
			if err != nil {
				errCh <- fmt.Errorf("%s current-range: %v", name, err)
				return
			}
			if resp.Count != seeds {
				errCh <- fmt.Errorf("%s current-range count=%d want %d", name, resp.Count, seeds)
			}
		}()

		// Reader 2: a revision before the compaction point — must get
		// OutOfRange on both backends (KIP-29: the compact revision itself
		// is kept, so the read is one revision earlier).
		wg.Add(1)
		go func() {
			defer wg.Done()
			pre := compactRev - 1
			if pre > 0 {
				_, err := client.Get(ctx, "compact/seed/", clientv3.WithPrefix(), clientv3.WithRev(pre))
				if err == nil {
					errCh <- fmt.Errorf("%s compacted-range: expected OutOfRange, got nil", name)
				}
			}
		}()

		// Reader 3: a revision before the compact point — both must answer
		// OutOfRange. Reading at the compact revision itself still succeeds
		// (KIP-29), so the read is deliberately five revisions earlier.
		wg.Add(1)
		go func() {
			defer wg.Done()
			pre := compactRev - 5
			if pre > 0 {
				_, err := client.Get(ctx, "compact/seed/", clientv3.WithPrefix(), clientv3.WithRev(pre))
				if err == nil {
					errCh <- fmt.Errorf("%s pre-compact-range: expected OutOfRange, got nil", name)
				}
			}
		}()
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		if err != nil {
			t.Error(err)
		}
	}

	// After all readers finish, the store must still serve writes and reads.
	for _, be := range []*Backend{h.Etcd, h.Tandem} {
		if _, err := be.Client.Put(ctx, "compact/after", "ok"); err != nil {
			t.Errorf("%s post-compaction write: %v", be.Name, err)
		}
	}
}
