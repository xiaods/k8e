package e2b

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
)

// E2B state annotations on the SandboxSession CRD. Because sandbox-matrix
// and e2b-server both embed in k8e-server, every control-plane node shares
// the same CRD — persisting the E2B bookkeeping here makes the deadline,
// pause and metadata state consistent across nodes (no per-process maps that
// diverge when the Gateway API routes a request to a different node).
const (
	// e2bStateAnnotation carries the JSON-serialized registryEntry fields
	// that must survive across nodes: deadline, onDeadline, createdAt,
	// metadata, runtimeName, pausedByUser.
	e2bStateAnnotation = "sandbox.k8e.io/e2b-state"
	// e2bNameIndexAnnotation maps metadata.name → sandboxID (idempotent
	// create) so any node can resolve the name without its own map.
	e2bNameIndexAnnotation = "sandbox.k8e.io/e2b-name"
)

var (
	sessionGVR = schema.GroupVersionResource{
		Group:    "k8e.sh",
		Version:  "v1alpha1",
		Resource: "sandboxsessions",
	}
)

// crdStateStore implements stateStore on the SandboxSession CRD via the
// dynamic client. Reads hit the cluster; writes update annotations. It keeps
// a small in-memory name index cache for byKeyName (the authoritative index
// lives on the session CRD itself, so the cache is only an optimization).
type crdStateStore struct {
	dyn       dynamic.Interface
	namespace string

	mu     sync.Mutex
	byName map[string]string // metadata.name → sandboxID
}

// NewCRDStateStore builds a CRD-backed state store (exported for the
// embedded k8e-server). namespace defaults to sandbox-matrix.
func NewCRDStateStore(dyn dynamic.Interface, namespace string) StateStore {
	return newCRDStateStore(dyn, namespace)
}

// newCRDStateStore builds a CRD-backed state store. namespace defaults to
// sandbox-matrix.
func newCRDStateStore(dyn dynamic.Interface, namespace string) *crdStateStore {
	if namespace == "" {
		namespace = "sandbox-matrix"
	}
	return &crdStateStore{
		dyn:       dyn,
		namespace: namespace,
		byName:    map[string]string{},
	}
}

// e2bState is the JSON payload stored under e2bStateAnnotation.
type e2bState struct {
	DeadlineAt   string            `json:"deadlineAt,omitempty"` // RFC3339, empty = immortal
	OnDeadline   string            `json:"onDeadline,omitempty"`
	CreatedAt    string            `json:"createdAt,omitempty"` // RFC3339
	Metadata     map[string]string `json:"metadata,omitempty"`
	RuntimeName  string            `json:"runtimeName,omitempty"`
	PausedByUser bool              `json:"pausedByUser,omitempty"`
}

func (s *crdStateStore) entry(e *e2bState) *registryEntry {
	return &registryEntry{
		sandboxID:    "", // caller sets
		deadlineAt:   parseRFC3339(e.DeadlineAt),
		onDeadline:   e.OnDeadline,
		createdAt:    parseRFC3339(e.CreatedAt),
		metadata:     e.Metadata,
		runtimeName:  e.RuntimeName,
		pausedByUser: e.PausedByUser,
	}
}

func (s *crdStateStore) state(ent *registryEntry) *e2bState {
	st := &e2bState{
		OnDeadline:   ent.onDeadline,
		Metadata:     ent.metadata,
		RuntimeName:  ent.runtimeName,
		PausedByUser: ent.pausedByUser,
	}
	if !ent.deadlineAt.IsZero() {
		st.DeadlineAt = ent.deadlineAt.Format(time.RFC3339)
	}
	if !ent.createdAt.IsZero() {
		st.CreatedAt = ent.createdAt.Format(time.RFC3339)
	}
	return st
}

func parseRFC3339(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	t, _ := time.Parse(time.RFC3339, s)
	return t
}

// getSession fetches the session CRD, or nil.
func (s *crdStateStore) getSession(ctx context.Context, sandboxID string) *unstructured.Unstructured {
	u, err := s.dyn.Resource(sessionGVR).Namespace(s.namespace).Get(ctx, sandboxID, metav1.GetOptions{})
	if err != nil {
		return nil
	}
	return u
}

func (s *crdStateStore) readState(u *unstructured.Unstructured) *e2bState {
	ann := u.GetAnnotations()
	if ann == nil {
		return &e2bState{}
	}
	raw, ok := ann[e2bStateAnnotation]
	if !ok || raw == "" {
		return &e2bState{}
	}
	var st e2bState
	if err := json.Unmarshal([]byte(raw), &st); err != nil {
		return &e2bState{}
	}
	return &st
}

// writeState persists the state annotation plus the name index on the CRD.
func (s *crdStateStore) writeState(ctx context.Context, sandboxID string, st *e2bState) error {
	u := s.getSession(ctx, sandboxID)
	if u == nil {
		return fmt.Errorf("session %s not found", sandboxID)
	}
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	raw, err := json.Marshal(st)
	if err != nil {
		return err
	}
	ann[e2bStateAnnotation] = string(raw)
	// Maintain the name index: drop it if the entry has no name.
	if name := st.Metadata["name"]; name != "" {
		ann[e2bNameIndexAnnotation] = sandboxID
	} else {
		delete(ann, e2bNameIndexAnnotation)
	}
	u.SetAnnotations(ann)
	_, err = s.dyn.Resource(sessionGVR).Namespace(s.namespace).Update(ctx, u, metav1.UpdateOptions{})
	if err != nil {
		return err
	}
	// Update the in-memory name cache on success.
	s.mu.Lock()
	defer s.mu.Unlock()
	if name := st.Metadata["name"]; name != "" {
		s.byName[name] = sandboxID
	} else {
		for k, v := range s.byName {
			if v == sandboxID {
				delete(s.byName, k)
			}
		}
	}
	return nil
}

// --- stateStore interface -------------------------------------------------

func (s *crdStateStore) put(e *registryEntry) {
	_ = s.writeState(context.Background(), e.sandboxID, s.state(e))
}

func (s *crdStateStore) get(sandboxID string) (*registryEntry, bool) {
	u := s.getSession(context.Background(), sandboxID)
	if u == nil {
		return nil, false
	}
	e := s.entry(s.readState(u))
	e.sandboxID = sandboxID
	return e, true
}

func (s *crdStateStore) byKeyName(name string) (string, bool) {
	s.mu.Lock()
	id, ok := s.byName[name]
	s.mu.Unlock()
	if ok {
		return id, true
	}
	// Fall back to a cluster scan (name index may be stale after restart).
	ctx := context.Background()
	list, err := s.dyn.Resource(sessionGVR).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return "", false
	}
	for _, item := range list.Items {
		ann := item.GetAnnotations()
		if ann == nil {
			continue
		}
		if ann[e2bNameIndexAnnotation] == "" {
			continue
		}
		// The annotation value IS the sandboxID; the name→ID mapping is
		// only recoverable by scanning state payloads. Optimize: skip when
		// the annotation holds the ID but we need name→ID, so instead match
		// the state payload's metadata.name.
		st := s.readState(&item)
		if st.Metadata["name"] == name {
			return item.GetName(), true
		}
	}
	return "", false
}

func (s *crdStateStore) del(sandboxID string) {
	ctx := context.Background()
	u := s.getSession(ctx, sandboxID)
	if u == nil {
		return
	}
	ann := u.GetAnnotations()
	if ann == nil {
		ann = map[string]string{}
	}
	delete(ann, e2bStateAnnotation)
	delete(ann, e2bNameIndexAnnotation)
	u.SetAnnotations(ann)
	_, _ = s.dyn.Resource(sessionGVR).Namespace(s.namespace).Update(ctx, u, metav1.UpdateOptions{})
	s.mu.Lock()
	for k, v := range s.byName {
		if v == sandboxID {
			delete(s.byName, k)
		}
	}
	s.mu.Unlock()
}

func (s *crdStateStore) ids() []string {
	ctx := context.Background()
	list, err := s.dyn.Resource(sessionGVR).Namespace(s.namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil
	}
	var out []string
	for _, item := range list.Items {
		if item.GetAnnotations() != nil {
			if _, ok := item.GetAnnotations()[e2bStateAnnotation]; ok {
				out = append(out, item.GetName())
			}
		}
	}
	return out
}

func (s *crdStateStore) deadlineOf(sandboxID string) time.Time {
	e, ok := s.get(sandboxID)
	if !ok {
		return time.Time{}
	}
	return e.deadlineAt
}

func (s *crdStateStore) extendDeadline(sandboxID string, timeoutSeconds int) time.Time {
	ctx := context.Background()
	e, ok := s.get(sandboxID)
	if !ok {
		return time.Time{}
	}
	candidate := time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	if candidate.After(e.deadlineAt) {
		e.deadlineAt = candidate
		_ = s.writeState(ctx, sandboxID, s.state(e))
	}
	return e.deadlineAt
}

func (s *crdStateStore) clearDeadline(sandboxID string) {
	ctx := context.Background()
	e, ok := s.get(sandboxID)
	if !ok {
		return
	}
	e.deadlineAt = time.Time{}
	_ = s.writeState(ctx, sandboxID, s.state(e))
}

func (s *crdStateStore) markPaused(sandboxID string, paused bool) {
	ctx := context.Background()
	e, ok := s.get(sandboxID)
	if !ok {
		return
	}
	e.pausedByUser = paused
	_ = s.writeState(ctx, sandboxID, s.state(e))
}

func (s *crdStateStore) isPaused(sandboxID string) bool {
	e, ok := s.get(sandboxID)
	return ok && e.pausedByUser
}
