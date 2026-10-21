package e2b

import (
	"sync"
	"testing"
	"time"
)

// TestRegistryConcurrentReadWrite is the regression for the data race the
// -race detector reported: sandboxRegistry.get() handed out the stored
// pointer, so request handlers read deadlineAt/createdAt/pausedByUser without
// the lock while extendDeadline/clearDeadline/markPaused wrote them under
// the lock.
//
// The test drives the readers and the writers concurrently; with the pointer
// escaping it fails under -race, with the snapshot copy it passes.
func TestRegistryConcurrentReadWrite(t *testing.T) {
	t.Helper()
	r := newSandboxRegistry()
	const id = "concurrent-sandbox"
	r.put(&registryEntry{
		sandboxID:  id,
		createdAt:  time.Now(),
		deadlineAt: time.Now().Add(time.Hour),
		metadata:   map[string]string{"name": "concurrent-sandbox"},
	})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Writers: the three mutating operations, exactly as the handlers call them.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				r.extendDeadline(id, 60)
				r.clearDeadline(id)
				r.markPaused(id, true)
				r.markPaused(id, false)
			}
		}()
	}

	// Readers: the field accesses the request handlers perform.
	for range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				if e, ok := r.get(id); ok {
					_ = e.deadlineAt
					_ = e.createdAt
					_ = e.pausedByUser
					_ = e.metadata["name"]
				}
				_ = r.deadlineOf(id)
				_ = r.isPaused(id)
			}
		}()
	}

	time.Sleep(200 * time.Millisecond)
	close(stop)
	wg.Wait()

	// The entry must still be intact after all that concurrent access: a
	// reader that saw a torn entry would have read a corrupted metadata map
	// (the deadline is legitimately zero here - the writers call
	// clearDeadline).
	e, ok := r.get(id)
	if !ok {
		t.Fatalf("the entry must survive the concurrent access")
	}
	if e.metadata["name"] != "concurrent-sandbox" {
		t.Fatalf("the metadata must not have been corrupted: %v", e.metadata)
	}
	if e.sandboxID != id {
		t.Fatalf("the sandbox id must not have been corrupted: %q", e.sandboxID)
	}
}

// TestRegistryGetReturnsCopy pins the contract: get() must hand out a copy,
// so a caller cannot reach into the registry's stored entry.
func TestRegistryGetReturnsCopy(t *testing.T) {
	r := newSandboxRegistry()
	r.put(&registryEntry{
		sandboxID:  "copy-sandbox",
		createdAt:  time.Now(),
		deadlineAt: time.Now().Add(time.Hour),
		metadata:   map[string]string{"name": "copy-sandbox"},
	})

	e, ok := r.get("copy-sandbox")
	if !ok {
		t.Fatalf("expected the entry to be found")
	}
	original := e.deadlineAt
	e.deadlineAt = time.Time{}
	e.metadata["name"] = "mutated"

	again, ok := r.get("copy-sandbox")
	if !ok {
		t.Fatalf("expected the entry to still be found")
	}
	if !again.deadlineAt.Equal(original) {
		t.Fatalf("mutating the returned entry changed the stored one: got %v, want %v", again.deadlineAt, original)
	}
	if again.metadata["name"] != "copy-sandbox" {
		t.Fatalf("mutating the returned metadata changed the stored one: got %q", again.metadata["name"])
	}
}

// TestRegistryPutCopiesEntry pins the other half: put() must not keep the
// caller's pointer, or the caller could mutate the registry behind its lock.
func TestRegistryPutCopiesEntry(t *testing.T) {
	r := newSandboxRegistry()
	entry := &registryEntry{
		sandboxID:  "put-copy",
		createdAt:  time.Now(),
		deadlineAt: time.Now().Add(time.Hour),
		metadata:   map[string]string{"name": "put-copy"},
	}
	r.put(entry)

	entry.deadlineAt = time.Time{}
	entry.metadata["name"] = "mutated-after-put"

	stored, ok := r.get("put-copy")
	if !ok {
		t.Fatalf("expected the entry to be found")
	}
	if stored.deadlineAt.IsZero() {
		t.Fatalf("mutating the caller's entry after put() cleared the stored deadline")
	}
	if stored.metadata["name"] != "put-copy" {
		t.Fatalf("mutating the caller's metadata after put() changed the stored name: got %q", stored.metadata["name"])
	}
}
