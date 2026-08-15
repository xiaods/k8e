package e2b

import "time"

// StateStore is the exported form of stateStore, used by the embedded
// k8e-server to inject a CRD-backed store (see NewCRDStateStore).
type StateStore = stateStore

// stateStore is the persistence contract for the E2B layer's per-sandbox
// bookkeeping. In single-process mode (standalone `k8e e2b-server`) the
// in-memory sandboxRegistry implements it. In the embedded multi-node
// architecture (sandbox-matrix + e2b both inside k8e-server, fronted by the
// Cilium Gateway API) the same contract is backed by SandboxSession CRD
// annotations (crdStateStore) so every control-plane node reads the same
// deadline / pause / metadata state — no cross-node divergence.
//
// All methods must be safe for concurrent use.
type stateStore interface {
	// put records a sandbox's E2B bookkeeping at create time.
	put(e *registryEntry)
	// get returns the entry for a sandbox, if known to this store.
	get(sandboxID string) (*registryEntry, bool)
	// byKeyName resolves a metadata.name to its sandbox ID (idempotent
	// create), if the store tracks names.
	byKeyName(name string) (string, bool)
	// del removes a sandbox's bookkeeping (after destroy).
	del(sandboxID string)
	// ids returns all known sandbox IDs.
	ids() []string

	// deadlineOf returns the entry's kill deadline, or the zero time.
	deadlineOf(sandboxID string) time.Time
	// extendDeadline moves the deadline to now+timeoutSeconds if later.
	extendDeadline(sandboxID string, timeoutSeconds int) time.Time
	// clearDeadline makes the sandbox immortal (NEVER_TIMEOUT).
	clearDeadline(sandboxID string)

	// markPaused records an explicit user pause/resume.
	markPaused(sandboxID string, paused bool)
	// isPaused reports whether the sandbox is explicitly paused by the user.
	isPaused(sandboxID string) bool
}
