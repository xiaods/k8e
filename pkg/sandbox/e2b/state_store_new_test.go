package e2b

import (
	"errors"
	"strings"
	"testing"
)

// TestNewStateStoreRequiresClusterAccess pins the policy: a server that
// cannot reach the SandboxSession CRDs must not silently continue with the
// in-memory registry, because the two have different semantics and the
// difference is invisible from the outside.
//
// The in-memory registry loses the E2B bookkeeping on restart (auto-pause,
// the metadata.name index, the E2B deadline) and cannot be shared between
// control-plane nodes.
func TestNewStateStoreRequiresClusterAccess(t *testing.T) {
	// A kubeconfig that does not exist: building the CRD store must fail.
	_, err := NewStateStore(StateStoreOptions{
		Kubeconfig: "/nonexistent/kubeconfig",
		Namespace:  DefaultNamespace,
	})
	if err == nil {
		t.Fatalf("expected the CRD state store to fail without a usable kubeconfig")
	}
	if !errors.Is(err, ErrNoStateStore) {
		t.Fatalf("expected ErrNoStateStore, got %v", err)
	}
	// The error must tell the operator how to proceed.
	if !strings.Contains(err.Error(), "in-memory-state") {
		t.Fatalf("the error must mention the --in-memory-state escape hatch, got %v", err)
	}
}

// TestNewStateStoreAllowsInMemoryWhenAsked is the opt-in counterpart.
func TestNewStateStoreAllowsInMemoryWhenAsked(t *testing.T) {
	store, err := NewStateStore(StateStoreOptions{
		Kubeconfig:    "/nonexistent/kubeconfig",
		Namespace:     DefaultNamespace,
		AllowInMemory: true,
	})
	if err != nil {
		t.Fatalf("expected the in-memory fallback to be allowed, got %v", err)
	}
	if store == nil {
		t.Fatalf("expected a store")
	}
	// It must be the in-memory registry, and it must work.
	store.put(&registryEntry{sandboxID: "s1", metadata: map[string]string{"name": "n1"}})
	if _, ok := store.get("s1"); !ok {
		t.Fatalf("the in-memory store must hold the entry")
	}
}

// TestNewStateStoreRejectsNilStore guards the contract the callers rely on:
// a returned store is never nil, and an error always accompanies a nil store.
func TestNewStateStoreRejectsNilStore(t *testing.T) {
	store, err := NewStateStore(StateStoreOptions{
		Kubeconfig:    "/nonexistent/kubeconfig",
		AllowInMemory: true,
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if store == nil {
		t.Fatalf("the store must not be nil")
	}
}

// TestDefaultNamespaceMatchesSandboxNamespace keeps the e2b package's copy
// of the default namespace in step with the rest of the tree. The e2b package
// cannot import pkg/sandboxmatrix/grpc (it would be an import cycle), so the
// constant is repeated - this test is what stops the copy from drifting.
func TestDefaultNamespaceMatchesSandboxNamespace(t *testing.T) {
	// The sandbox matrix's default is "sandbox-matrix" (see
	// config.DefaultSandboxNamespace). If that ever changes, this test must
	// be updated with it.
	if DefaultNamespace != "sandbox-matrix" {
		t.Fatalf("DefaultNamespace = %q, want %q", DefaultNamespace, "sandbox-matrix")
	}
}
