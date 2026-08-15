package e2b

import (
	"context"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	dynfake "k8s.io/client-go/dynamic/fake"
)

// newTestCRDStore builds a crdStateStore over a fake dynamic client that
// knows the SandboxSession GVR.
func newTestCRDStore(t *testing.T) *crdStateStore {
	t.Helper()
	scheme := runtime.NewScheme()
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "k8e.sh", Version: "v1alpha1", Kind: "SandboxSession"},
		&unstructured.Unstructured{},
	)
	scheme.AddKnownTypeWithName(
		schema.GroupVersionKind{Group: "k8e.sh", Version: "v1alpha1", Kind: "SandboxSessionList"},
		&unstructured.UnstructuredList{},
	)
	listKinds := map[schema.GroupVersionResource]string{
		{Group: "k8e.sh", Version: "v1alpha1", Resource: "sandboxsessions"}: "SandboxSessionList",
	}
	dyn := dynfake.NewSimpleDynamicClientWithCustomListKinds(scheme, listKinds)
	return newCRDStateStore(dyn, "sandbox-matrix")
}

// seedSession creates a session CRD object in the fake cluster.
func seedSession(t *testing.T, s *crdStateStore, id string) {
	t.Helper()
	u := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": "k8e.sh/v1alpha1",
			"kind":       "SandboxSession",
			"metadata": map[string]interface{}{
				"name":        id,
				"namespace":   "sandbox-matrix",
				"annotations": map[string]interface{}{},
			},
		},
	}
	_, err := s.dyn.Resource(sessionGVR).Namespace("sandbox-matrix").Create(context.Background(), u, metav1.CreateOptions{})
	if err != nil {
		t.Fatalf("seed session: %v", err)
	}
}

func TestCRDStateStorePutGetDeadline(t *testing.T) {
	s := newTestCRDStore(t)
	seedSession(t, s, "s1")

	// put records deadline + metadata.
	now := time.Now().Add(60 * time.Second).Truncate(time.Second)
	s.put(&registryEntry{
		sandboxID:   "s1",
		deadlineAt:  now,
		onDeadline:  "pause",
		createdAt:   time.Now().Add(-time.Minute),
		metadata:    map[string]string{"name": "my-sandbox"},
		runtimeName: "gvisor",
	})

	// get returns the persisted entry (round-trip through the CRD).
	e, ok := s.get("s1")
	if !ok {
		t.Fatal("get(s1) not found")
	}
	if !e.deadlineAt.Equal(now) {
		t.Fatalf("deadline = %v, want %v", e.deadlineAt, now)
	}
	if e.onDeadline != "pause" {
		t.Fatalf("onDeadline = %q, want pause", e.onDeadline)
	}
	if e.metadata["name"] != "my-sandbox" {
		t.Fatalf("metadata name = %q", e.metadata["name"])
	}

	// deadlineOf reads through.
	if got := s.deadlineOf("s1"); !got.Equal(now) {
		t.Fatalf("deadlineOf = %v", got)
	}

	// byKeyName resolves via the name index.
	if id, ok := s.byKeyName("my-sandbox"); !ok || id != "s1" {
		t.Fatalf("byKeyName = %q,%v, want s1,true", id, ok)
	}
}

func TestCRDStateStoreExtendClearDeadline(t *testing.T) {
	s := newTestCRDStore(t)
	seedSession(t, s, "s1")
	base := time.Now().Add(30 * time.Second).Truncate(time.Second)
	s.put(&registryEntry{sandboxID: "s1", deadlineAt: base})

	// extend moves the deadline forward.
	ext := s.extendDeadline("s1", 120)
	if ext.Before(base) {
		t.Fatalf("extendDeadline went backwards: %v < %v", ext, base)
	}
	if got := s.deadlineOf("s1"); got.Equal(base) {
		t.Fatal("deadline did not extend")
	}

	// clear makes it immortal.
	s.clearDeadline("s1")
	if got := s.deadlineOf("s1"); !got.IsZero() {
		t.Fatalf("deadline after clear = %v, want zero", got)
	}
}

func TestCRDStateStorePauseFlag(t *testing.T) {
	s := newTestCRDStore(t)
	seedSession(t, s, "s1")
	s.put(&registryEntry{sandboxID: "s1"})

	if s.isPaused("s1") {
		t.Fatal("should not be paused initially")
	}
	s.markPaused("s1", true)
	if !s.isPaused("s1") {
		t.Fatal("should be paused after markPaused(true)")
	}
	s.markPaused("s1", false)
	if s.isPaused("s1") {
		t.Fatal("should not be paused after markPaused(false)")
	}
}

func TestCRDStateStoreIdsAndDel(t *testing.T) {
	s := newTestCRDStore(t)
	seedSession(t, s, "s1")
	seedSession(t, s, "s2")
	s.put(&registryEntry{sandboxID: "s1", metadata: map[string]string{"name": "a"}})
	s.put(&registryEntry{sandboxID: "s2", metadata: map[string]string{"name": "b"}})

	ids := s.ids()
	if len(ids) != 2 {
		t.Fatalf("ids = %v, want 2", ids)
	}

	s.del("s1")
	ids = s.ids()
	if len(ids) != 1 || ids[0] != "s2" {
		t.Fatalf("ids after del = %v, want [s2]", ids)
	}
	if _, ok := s.byKeyName("a"); ok {
		t.Fatal("name index for deleted sandbox still present")
	}
	if _, ok := s.byKeyName("b"); !ok {
		t.Fatal("name index for surviving sandbox missing")
	}
}

func TestCRDStateStoreMissingSession(t *testing.T) {
	s := newTestCRDStore(t)
	if _, ok := s.get("no-such"); ok {
		t.Fatal("get on missing session should be false")
	}
	if got := s.deadlineOf("no-such"); !got.IsZero() {
		t.Fatal("deadlineOf on missing session should be zero")
	}
	// put on a missing session must not panic (best-effort write fails).
	s.put(&registryEntry{sandboxID: "no-such"})
}
