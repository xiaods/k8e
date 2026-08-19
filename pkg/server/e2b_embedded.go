package server

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/sandbox/client"
	sandboxe2b "github.com/xiaods/k8e/pkg/sandbox/e2b"
)

const (
	e2bAPIKeySecretNS   = "sandbox-matrix"
	e2bAPIKeySecretName = "sandbox-apikeys"
	e2bAPIKeyReload     = 30 * time.Second
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

	staticKey := resolveEmbeddedAPIKey(cfg.E2BAPIKey)
	srv := sandboxe2b.NewServer(sandboxe2b.Config{
		Listen:          cfg.E2BListen,
		Endpoint:        gatewayAddr,
		APIKey:          staticKey,
		DefaultCPUs:     1,
		DefaultMemoryMB: 512,
		DefaultDiskMB:   10 * 1024,
		StateStore:      store,
	}, sandboxe2b.GatewayFromClient(c))

	cache := &e2bAPIKeyCache{static: staticKey}
	applyE2BAPIKeys(ctx, srv, cache, kubeconfig)
	go reloadE2BAPIKeys(ctx, srv, cache, kubeconfig)

	if err := sandboxe2b.ValidateE2BAPIKey(staticKey); err != nil {
		logrus.Warnf("e2b (embedded): %v; official e2b SDK clients will not be able to authenticate — generate a hex key with `k8e sandbox-apikey create <name>` (pass the e2b_key field to the SDK)", err)
	}

	logrus.Infof("e2b (embedded): serving on %s via gateway %s (Gateway API fronted; state=%T)", cfg.E2BListen, gatewayAddr, store)
	if err := srv.Start(ctx); err != nil && ctx.Err() == nil {
		logrus.Errorf("e2b (embedded): %v", err)
	}
}

// resolveEmbeddedAPIKey prefers --e2b-apikey / K8E_E2B_APIKEY, then the
// shared sandbox CLI env K8E_SANDBOX_APIKEY so a single exported key works
// for `k8e sandbox`, standalone e2b-server, and the embedded surface.
func resolveEmbeddedAPIKey(configured string) string {
	if key := strings.TrimSpace(configured); key != "" {
		return key
	}
	return strings.TrimSpace(os.Getenv("K8E_SANDBOX_APIKEY"))
}

// e2bAPIKeyCache is the last authoritative sandbox-apikeys snapshot. The
// reload loop must inherit the startup snapshot: otherwise a failed first
// refresh leaves haveCache=false and an expired boot-time key stays accepted
// until a later successful Secret read.
type e2bAPIKeyCache struct {
	static    string
	snapshot  sandboxe2b.SecretKeySet
	haveCache bool
}

// refresh updates the snapshot when ok, then returns the merged keyring
// (static + currently-unexpired Secret tokens). apply=false means there is
// no snapshot yet, so the caller must leave the current keyring alone.
func (c *e2bAPIKeyCache) refresh(ok bool, set sandboxe2b.SecretKeySet, now time.Time) (keys []string, apply bool) {
	if ok {
		c.snapshot = set
		c.haveCache = true
	} else if !c.haveCache {
		return nil, false
	}
	return append([]string{c.static}, c.snapshot.Active(now)...), true
}

func applyE2BAPIKeys(ctx context.Context, srv *sandboxe2b.Server, cache *e2bAPIKeyCache, kubeconfig string) {
	set, ok := loadSandboxAPIKeys(ctx, kubeconfig)
	keys, apply := cache.refresh(ok, set, time.Now())
	if !apply {
		if cache.static == "" {
			logrus.Warnf("e2b (embedded): no --e2b-apikey / K8E_SANDBOX_APIKEY and sandbox-apikeys Secret not readable; official e2b SDK control-plane requests will be rejected (401) — run `k8e sandbox-apikey create <name>` and pass the e2b_key field to the SDK")
		}
		return
	}
	srv.ReplaceAPIKeys(keys)
	active := cache.snapshot.Active(time.Now())
	switch {
	case len(active) == 0 && cache.static == "":
		logrus.Warnf("e2b (embedded): no API keys configured; official e2b SDK control-plane requests will be rejected (401) — run `k8e sandbox-apikey create <name>` and pass the e2b_key field to the SDK")
	case len(active) > 0:
		logrus.Infof("e2b (embedded): accepting %d key(s) from sandbox-apikeys Secret (plus static=%t)", len(active), cache.static != "")
	}
}

func reloadE2BAPIKeys(ctx context.Context, srv *sandboxe2b.Server, cache *e2bAPIKeyCache, kubeconfig string) {
	ticker := time.NewTicker(e2bAPIKeyReload)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			set, ok := loadSandboxAPIKeys(ctx, kubeconfig)
			keys, apply := cache.refresh(ok, set, time.Now())
			if !apply {
				continue
			}
			srv.ReplaceAPIKeys(keys)
		}
	}
}

// loadSandboxAPIKeys reads the sandbox-apikeys Secret snapshot.
// ok=false means the snapshot is not authoritative (transient API error or
// corrupt payload); the caller must keep the last parsed set and re-filter it
// for expiry. ok=true with an empty set is a real empty/missing Secret and may
// replace the keyring.
func loadSandboxAPIKeys(ctx context.Context, kubeconfig string) (sandboxe2b.SecretKeySet, bool) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", kubeconfig)
	if err != nil {
		return sandboxe2b.SecretKeySet{}, false
	}
	k8s, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return sandboxe2b.SecretKeySet{}, false
	}
	secret, err := k8s.CoreV1().Secrets(e2bAPIKeySecretNS).Get(ctx, e2bAPIKeySecretName, metav1.GetOptions{})
	if apierrors.IsNotFound(err) {
		return sandboxe2b.SecretKeySet{}, true
	}
	if err != nil {
		logrus.Debugf("e2b (embedded): sandbox-apikeys Secret: %v", err)
		return sandboxe2b.SecretKeySet{}, false
	}
	data, exists := secret.Data["keys.json"]
	if !exists {
		return sandboxe2b.SecretKeySet{}, true
	}
	parsed, err := sandboxe2b.ParseSecretKeys(data)
	if err != nil {
		logrus.Warnf("e2b (embedded): sandbox-apikeys keys.json corrupted: %v", err)
		return sandboxe2b.SecretKeySet{}, false
	}
	return parsed, true
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
