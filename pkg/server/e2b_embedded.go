package server

import (
	"context"
	"fmt"

	"github.com/sirupsen/logrus"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/sandbox/client"
	sandboxe2b "github.com/xiaods/k8e/pkg/sandbox/e2b"
)

// runEmbeddedE2B starts the KIP-18 E2B-compatible HTTP server inside the
// k8e-server process (on by default; --disable-e2b to turn off). It dials
// the in-process sandbox gRPC gateway over loopback — the gateway is started
// by sandboxmatrix.Register on 127.0.0.1:<GRPCPort> — and serves the E2B HTTP
// surface on cfg.E2BListen (0.0.0.0:3676 by default). External exposure is
// exclusively via the Cilium Gateway API (HTTPRoute :80/:443 → host e2b,
// TCPRoute :50051 → host gateway).
//
// Multi-node consistency: the E2B bookkeeping (deadline, pause, metadata,
// idempotent-create name index) is persisted on the SandboxSession CRD via a
// CRD-backed state store, so every control-plane node reads the same state —
// no per-process maps that diverge when the Gateway API routes a request to
// a different node. kubeconfig supplies the dynamic client.
//
// Blocking: intended to run in its own goroutine; returns when ctx is
// cancelled (server shutdown).
func runEmbeddedE2B(ctx context.Context, cfg config.SandboxConfig, kubeconfig string) {
	if cfg.E2BListen == "" {
		cfg.E2BListen = "0.0.0.0:3676"
	}
	gatewayAddr := fmt.Sprintf("127.0.0.1:%d", cfg.GRPCPort)

	// The in-process gateway uses mTLS with loopback LocalAuth (the sandbox
	// client's NewClient resolves local credentials from the server TLS dir).
	c, err := client.NewClientWithEndpoint(gatewayAddr, "")
	if err != nil {
		logrus.Errorf("e2b (embedded): connect to local gateway %s: %v", gatewayAddr, err)
		return
	}
	defer c.Close()

	// CRD-backed state store for multi-node consistency. Fall back to the
	// in-memory store if the cluster is unreachable (e.g. kubeconfig missing
	// in an unusual embedding) — degrade to single-node semantics.
	store, err := newEmbeddedStateStore(kubeconfig, cfg.Namespace)
	if err != nil {
		logrus.Warnf("e2b (embedded): CRD state store unavailable (%v); using in-memory (single-node semantics)", err)
	}

	srv := sandboxe2b.NewServer(sandboxe2b.Config{
		Listen:          cfg.E2BListen,
		Endpoint:        gatewayAddr,
		DefaultCPUs:     1,
		DefaultMemoryMB: 512,
		DefaultDiskMB:   10 * 1024,
		StateStore:      store,
	}, sandboxe2b.GatewayFromClient(c))

	logrus.Infof("e2b (embedded): serving on %s via gateway %s (Gateway API fronted; state=%T)", cfg.E2BListen, gatewayAddr, store)
	if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
		logrus.Errorf("e2b (embedded): %v", err)
	}
}

// newEmbeddedStateStore builds the CRD-backed E2B state store for the
// embedded architecture. A nil return with a non-nil error means the store
// could not be constructed (caller falls back to in-memory).
func newEmbeddedStateStore(kubeconfig, namespace string) (sandboxe2b.StateStore, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return nil, err
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, err
	}
	return sandboxe2b.NewCRDStateStore(dyn, namespace), nil
}
