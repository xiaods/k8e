package e2b

import (
	"errors"
	"fmt"

	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
)

// DefaultNamespace is the namespace holding the SandboxSession CRDs.
//
// It matches config.DefaultSandboxNamespace. The value is repeated here
// rather than imported so the e2b package keeps its small dependency set
// (it must not import pkg/sandboxmatrix/grpc, which would be a cycle).
const DefaultNamespace = "sandbox-matrix"

// ErrNoStateStore is returned when a state store was required but could not
// be built. Callers surface it instead of silently falling back to the
// in-memory registry: the two have different semantics, and the difference
// is not something an operator can discover from the outside.
var ErrNoStateStore = errors.New("e2b: no state store")

// StateStoreOptions describes how to build the E2B state store.
type StateStoreOptions struct {
	// Kubeconfig is the path to the kubeconfig used to reach the cluster.
	// Empty means the in-cluster config (the usual case for a server
	// running inside k8e-server) or the default resolution rules.
	Kubeconfig string
	// Namespace is the namespace holding the SandboxSession CRDs. Empty
	// means DefaultNamespace.
	Namespace string
	// AllowInMemory permits the in-memory registry as a fallback. It is
	// off by default: the in-memory registry loses the bookkeeping on
	// restart and cannot be shared between control-plane nodes, so a
	// deployment that falls back to it silently loses auto-pause, the
	// idempotent-create index and the E2B deadline.
	AllowInMemory bool
}

// NewStateStore builds the E2B state store for a server.
//
// The CRD-backed store is the only one that keeps the E2B bookkeeping
// consistent across restarts and across control-plane nodes, so it is the
// default and the only supported mode for a real deployment.
//
// The in-memory registry exists for tests and for a single-node experiment.
// It is only used when AllowInMemory is set: without it, a failure to reach
// the cluster is an error rather than a silent downgrade, because the
// downgraded server would look healthy while losing auto-pause semantics,
// the metadata.name index and the E2B deadline.
func NewStateStore(opts StateStoreOptions) (StateStore, error) {
	store, err := newCRDStateStoreFromConfig(opts.Kubeconfig, opts.Namespace)
	if err == nil {
		return store, nil
	}
	if !opts.AllowInMemory {
		return nil, fmt.Errorf("%w: %w (pass --in-memory-state to run without cluster access, "+
			"at the cost of losing the E2B bookkeeping on restart and on multi-node deployments)",
			ErrNoStateStore, err)
	}
	return newSandboxRegistry(), nil
}

// newCRDStateStoreFromConfig builds a CRD-backed store from a kubeconfig.
func newCRDStateStoreFromConfig(kubeconfig, namespace string) (StateStore, error) {
	if namespace == "" {
		namespace = DefaultNamespace
	}
	restConfig, err := restConfigFromKubeconfig(kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return NewCRDStateStore(dyn, namespace), nil
}

// restConfigFromKubeconfig builds a rest config from an explicit kubeconfig
// path, falling back to the in-cluster config when the path is empty.
func restConfigFromKubeconfig(kubeconfig string) (*rest.Config, error) {
	if kubeconfig != "" {
		return clientcmd.BuildConfigFromFlags("", kubeconfig)
	}
	// An empty path means "the default rules": the in-cluster service
	// account when running inside a pod, otherwise the user's kubeconfig.
	cfg, err := rest.InClusterConfig()
	if err == nil {
		return cfg, nil
	}
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	return clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
}
