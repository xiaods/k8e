package e2b

import (
	"context"
	"strconv"
	"sync"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

func itoa(n int) string { return strconv.Itoa(n) }

// registryEntry is the daemon-side bookkeeping for an E2B-created sandbox:
// its E2B kill deadline, when it was created (the k8e CRD has no createdAt
// on the proto view), and the metadata that E2B echoes back.
type registryEntry struct {
	sandboxID    string
	deadlineAt   time.Time
	onDeadline   string // "kill" | "pause"
	createdAt    time.Time
	metadata     map[string]string
	runtimeName  string
	pausedByUser bool
}

// sandboxRegistry is the in-memory E2B bookkeeping. In-memory is honest and
// documented (KIP-18): a server restart loses the registry — sessions
// survive, GC defaults to k8e's own ExpiresAt.
type sandboxRegistry struct {
	mu     sync.Mutex
	byID   map[string]*registryEntry
	byName map[string]string // metadata.name → sandboxID (idempotent create)
}

func newSandboxRegistry() *sandboxRegistry {
	return &sandboxRegistry{
		byID:   map[string]*registryEntry{},
		byName: map[string]string{},
	}
}

func (r *sandboxRegistry) put(e *registryEntry) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.byID[e.sandboxID] = e
	if name, ok := e.metadata["name"]; ok && name != "" {
		r.byName[name] = e.sandboxID
	}
}

func (r *sandboxRegistry) get(sandboxID string) (*registryEntry, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[sandboxID]
	return e, ok
}

func (r *sandboxRegistry) byKeyName(name string) (string, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	id, ok := r.byName[name]
	return id, ok
}

func (r *sandboxRegistry) del(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[sandboxID]; ok {
		if name, ok := e.metadata["name"]; ok && name != "" {
			delete(r.byName, name)
		}
	}
	delete(r.byID, sandboxID)
}

// deadlineOf returns the entry's kill deadline, or the zero time.
func (r *sandboxRegistry) deadlineOf(sandboxID string) time.Time {
	e, ok := r.get(sandboxID)
	if !ok {
		return time.Time{}
	}
	return e.deadlineAt
}

// extendDeadline moves the deadline to now+timeoutSeconds if that is later
// than the current deadline (only ever extended, never shortened — Dormice's
// connect TTL rule). Returns the new deadline.
func (r *sandboxRegistry) extendDeadline(sandboxID string, timeoutSeconds int) time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[sandboxID]
	if !ok {
		return time.Time{}
	}
	candidate := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	if candidate.After(e.deadlineAt) {
		e.deadlineAt = candidate
	}
	return e.deadlineAt
}

// clearDeadline makes the sandbox immortal (NEVER_TIMEOUT): no kill deadline,
// no endAt in views.
func (r *sandboxRegistry) clearDeadline(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[sandboxID]; ok {
		e.deadlineAt = time.Time{}
	}
}

// markPaused records an explicit user pause/resume.
func (r *sandboxRegistry) markPaused(sandboxID string, paused bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[sandboxID]; ok {
		e.pausedByUser = paused
	}
}

// isPaused reports whether the sandbox is explicitly paused by the user.
func (r *sandboxRegistry) isPaused(sandboxID string) bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	e, ok := r.byID[sandboxID]
	return ok && e.pausedByUser
}

// expireForTest forces a sandbox's deadline into the past (used by GC tests
// to time-travel without waiting). Only meaningful for the in-memory store.
func (r *sandboxRegistry) expireForTest(sandboxID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if e, ok := r.byID[sandboxID]; ok {
		e.deadlineAt = time.Now().Add(-time.Second)
	}
}

// ids returns a snapshot of all registered sandbox IDs (used by list and by
// signed-URL identity scanning).
func (r *sandboxRegistry) ids() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, 0, len(r.byID))
	for id := range r.byID {
		out = append(out, id)
	}
	return out
}

// gcLoop enforces E2B kill deadlines: a sandbox whose kill deadline has
// passed is destroyed for real (the protocol-dead view is answered before
// the physical teardown, exactly like Dormice's scanner).
func (s *Server) gcLoop(ctx context.Context) {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.gcExpired(ctx)
		}
	}
}

func (s *Server) gcExpired(ctx context.Context) {
	now := time.Now()
	for _, id := range s.registry.ids() {
		e, ok := s.registry.get(id)
		if !ok || e.deadlineAt.IsZero() || now.Before(e.deadlineAt) {
			continue
		}
		if e.onDeadline == "pause" {
			// autoPause at the deadline: release the pod (CPU/memory) via the
			// gateway, keep the PVC + session for a later resume. Ephemeral
			// sessions (no PVC) cannot pause — degrade to kill, honestly.
			s.log("e2b: auto-pausing sandbox %s (pause deadline passed)", id)
			if _, err := s.gw.PauseSession(ctx, &pb.PauseSessionRequest{SessionId: id}); err != nil {
				s.log("e2b: auto-pause of %s failed (%v); destroying", id, err)
				if _, derr := s.gw.DestroySession(ctx, &pb.DestroySessionRequest{SessionId: id}); derr == nil {
					s.registry.del(id)
				}
			} else {
				s.registry.markPaused(id, true)
			}
			continue
		}
		s.log("e2b: destroying sandbox %s (kill deadline passed)", id)
		if _, err := s.gw.DestroySession(ctx, &pb.DestroySessionRequest{SessionId: id}); err == nil {
			s.registry.del(id)
		}
	}
}
