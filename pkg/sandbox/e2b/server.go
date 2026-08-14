package e2b

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"net/http"
	"os"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/sandbox/client"
)

// Server is the E2B-compatible HTTP server (KIP-18). It speaks the E2B
// protocol on three surfaces and translates to the k8e sandbox gRPC gateway.
type Server struct {
	gw Gateway

	listen        string
	nodeID        string
	signingSecret string
	apiKeys       []string

	defaultCPUs     int
	defaultMemoryMB int
	defaultDiskMB   int

	runtimes map[string]struct{}

	registry  stateStore
	processes *ProcessTable

	// sandboxd is the direct in-pod HTTP client for native operations the
	// gRPC gateway does not expose (filesystem stat/mkdir/mv/rm, process
	// stdin/signal) — KIP-18 "ability downshift".
	sandboxd *sandboxdClient

	// lastErr remembers the most recent gateway error per sandbox so 404
	// paths can distinguish gone (404) from mid-lifecycle (503+Retry-After).
	mu      sync.Mutex
	lastErr map[string]error

	logf func(string, ...any)
}

// Config holds the E2B server configuration.
type Config struct {
	// Listen is the HTTP listen address (default 127.0.0.1:3676).
	Listen string
	// Endpoint is the k8e gRPC gateway endpoint ("" = local auto-discovery).
	Endpoint string
	// APIKey authenticates to the gateway; also the accepted E2B API key.
	APIKey string
	// NodeID is the value reported as clientID (default "k8e").
	NodeID string
	// SigningSecret keys envd access tokens and signed file URLs.
	SigningSecret string
	// DefaultCPUs / DefaultMemoryMB / DefaultDiskMB are reported in info
	// views (k8e has no per-session spec on the proto view).
	DefaultCPUs     int
	DefaultMemoryMB int
	DefaultDiskMB   int
	// AllowedRuntimeClasses are the templateIDs accepted at create
	// (default gvisor, kata, firecracker).
	AllowedRuntimeClasses []string
	// StateStore persists the E2B bookkeeping (deadline, pause, metadata).
	// Defaults to an in-memory store; the embedded k8e-server mode injects a
	// CRD-backed store so multi-node control planes share state.
	StateStore stateStore
}

// NewServer builds an E2B server against the given gateway.
func NewServer(cfg Config, gw Gateway) *Server {
	if cfg.Listen == "" {
		cfg.Listen = "127.0.0.1:3676"
	}
	if cfg.NodeID == "" {
		cfg.NodeID = "k8e"
	}
	if cfg.DefaultCPUs == 0 {
		cfg.DefaultCPUs = 1
	}
	if cfg.DefaultMemoryMB == 0 {
		cfg.DefaultMemoryMB = 512
	}
	if cfg.DefaultDiskMB == 0 {
		cfg.DefaultDiskMB = 10 * 1024
	}
	if cfg.SigningSecret == "" {
		cfg.SigningSecret = resolveSigningSecret()
	}
	runtimes := map[string]struct{}{}
	for _, r := range cfg.AllowedRuntimeClasses {
		runtimes[r] = struct{}{}
	}
	if len(runtimes) == 0 {
		for _, r := range []string{"gvisor", "kata", "firecracker"} {
			runtimes[r] = struct{}{}
		}
	}
	apiKeys := []string{}
	if cfg.APIKey != "" {
		apiKeys = append(apiKeys, cfg.APIKey)
	}
	registry := cfg.StateStore
	if registry == nil {
		registry = newSandboxRegistry()
	}
	s := &Server{
		gw:              gw,
		listen:          cfg.Listen,
		nodeID:          cfg.NodeID,
		signingSecret:   cfg.SigningSecret,
		apiKeys:         apiKeys,
		defaultCPUs:     cfg.DefaultCPUs,
		defaultMemoryMB: cfg.DefaultMemoryMB,
		defaultDiskMB:   cfg.DefaultDiskMB,
		runtimes:        runtimes,
		registry:        registry,
		processes:       NewProcessTable(),
		sandboxd:        newSandboxdClient(gw),
		lastErr:         map[string]error{},
		logf:            func(format string, args ...any) { logrus.Infof("e2b: "+format, args...) },
	}
	return s
}

// GatewayFromClient adapts a k8e sandbox client to the Gateway contract.
func GatewayFromClient(c *client.Client) Gateway {
	return &grpcGateway{client: c}
}

// Handle returns the HTTP handler (used by tests and by Start).
func (s *Server) Handle() http.Handler {
	r := mux.NewRouter()

	// Control plane. Mounted BOTH at the root (CubeSandbox style — the
	// official SDK points apiUrl at the bare origin, so /sandboxes etc. are
	// what it hits) and under the /e2b/api prefix (Dormice-style, kept for
	// clients already wired to it). Same handlers, same auth.
	registerControlRoutes(r, s)
	// /e2b/api keeps the Dormice-style prefix for clients already wired to
	// it; Subrouter() strips the prefix before the inner routes match.
	prefixed := r.PathPrefix("/e2b/api").Subrouter()
	registerControlRoutes(prefixed, s)

	// envd surface (E2b-Sandbox-Id + X-Access-Token auth, string error codes).
	envd := r.PathPrefix("/e2b/envd").Subrouter()
	envd.Use(s.envdKeyAuth)
	envd.HandleFunc("/health", s.handleEnvdHealth).Methods(http.MethodGet)
	envd.HandleFunc("/process.Process/Start", s.handleProcessStart)
	envd.HandleFunc("/process.Process/Connect", s.handleProcessConnect)
	envd.HandleFunc("/process.Process/List", s.handleProcessList)
	envd.HandleFunc("/process.Process/SendInput", s.handleProcessSendInput)
	envd.HandleFunc("/process.Process/CloseStdin", s.handleProcessCloseStdin)
	envd.HandleFunc("/process.Process/SendSignal", s.handleProcessSendSignal)
	envd.HandleFunc("/process.Process/Update", s.handleUnimplementedProcess("Update"))
	envd.HandleFunc("/process.Process/StreamInput", s.handleStreamInputUnimplemented)
	envd.HandleFunc("/process.Process/UpdatePTY", s.handleUnimplementedProcess("UpdatePTY"))
	envd.HandleFunc("/filesystem.Filesystem/Stat", s.handleFSStat)
	envd.HandleFunc("/filesystem.Filesystem/ListDir", s.handleFSListDir)
	envd.HandleFunc("/filesystem.Filesystem/MakeDir", s.handleFSMakeDir)
	envd.HandleFunc("/filesystem.Filesystem/Move", s.handleFSMove)
	envd.HandleFunc("/filesystem.Filesystem/Remove", s.handleFSRemove)
	// The streaming WatchDir stays unimplemented (the SDK uses the polling
	// trio below — CreateWatcher/GetWatcherEvents/RemoveWatcher — via
	// WatchHandle; the streaming RPC is a low-level path).
	envd.HandleFunc("/filesystem.Filesystem/WatchDir", s.handleUnimplementedFS("WatchDir"))
	envd.HandleFunc("/filesystem.Filesystem/CreateWatcher", s.handleFSCreateWatcher)
	envd.HandleFunc("/filesystem.Filesystem/GetWatcherEvents", s.handleFSGetWatcherEvents)
	envd.HandleFunc("/filesystem.Filesystem/RemoveWatcher", s.handleFSRemoveWatcher)
	envd.HandleFunc("/files", s.handleFiles).Methods(http.MethodGet, http.MethodPost, http.MethodOptions)
	envd.NotFoundHandler = http.HandlerFunc(s.envdNotFound)

	// Signed file URLs at the daemon root (the SDK builds
	// new URL('/files', sandboxUrl) which strips the /e2b/envd prefix).
	r.HandleFunc("/files", s.handleSignedFiles).Methods(http.MethodGet, http.MethodPost, http.MethodOptions)

	// Root health probe (CubeSandbox-style) for orchestrators and load
	// balancers; the envd health probe stays under /e2b/envd/health.
	r.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}).Methods(http.MethodGet)

	return s.timeoutLayers(r)
}

// registerControlRoutes wires the control-plane routes onto a router (used
// for both the root mount and the /e2b/api prefix mount). Auth is applied
// per-handler, NOT via router.Use(): the root router also hosts the envd
// and /files routes, and a router-level Use would leak control-plane auth
// onto them.
func registerControlRoutes(r *mux.Router, s *Server) {
	auth := func(h http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if !s.acceptKey(credentialFromHeaders(req)) {
				jsonWriter(w, http.StatusUnauthorized, errorBody(apiError(401, "invalid API key")))
				return
			}
			h(w, req)
		}
	}
	r.HandleFunc("/sandboxes", auth(s.handleCreate)).Methods(http.MethodPost)
	r.HandleFunc("/sandboxes/{id}/connect", auth(s.handleConnect)).Methods(http.MethodPost)
	r.HandleFunc("/sandboxes/{id}", auth(s.handleGet)).Methods(http.MethodGet)
	r.HandleFunc("/sandboxes/{id}", auth(s.handleKill)).Methods(http.MethodDelete)
	r.HandleFunc("/sandboxes/{id}/timeout", auth(s.handleTimeout)).Methods(http.MethodPost)
	r.HandleFunc("/sandboxes/{id}/pause", auth(s.handlePause)).Methods(http.MethodPost)
	r.HandleFunc("/sandboxes/{id}/resume", auth(s.handleResume)).Methods(http.MethodPost)
	r.HandleFunc("/sandboxes/{id}/metrics", auth(s.handleMetrics)).Methods(http.MethodGet)
	r.HandleFunc("/v2/sandboxes", auth(s.handleList)).Methods(http.MethodGet)
	r.NotFoundHandler = http.HandlerFunc(s.controlNotFound)
}

// --- time-layered timeouts (CubeSandbox design) ---------------------------

// Timeouts for the e2b HTTP surface, layered like CubeSandbox's router
// budgets. The gateway RPCs they front are synchronous and can legitimately
// exceed the default; streaming paths (process Start/Connect, file
// download) must never be cut — they are excluded here.
const (
	// defaultRouteTimeout bounds ordinary control-plane RPCs: create (which
	// waits for a pod IP), get, list, kill, metrics.
	defaultRouteTimeout = 60 * time.Second
	// lifecycleRouteTimeout bounds pause/resume/connect — lifecycle
	// transitions can take seconds (snapshot restore / pod re-claim).
	lifecycleRouteTimeout = 120 * time.Second
	// longRouteTimeout is reserved for snapshot/rollback style operations
	// (CubeSandbox uses 240s); no such route exists yet.
	longRouteTimeout = 240 * time.Second
)

// timeoutLayers applies a per-route timeout budget based on the path,
// mirroring CubeSandbox's 30s/120s/240s router split (adapted: create is
// slow on K8s so the standard lane is 60s). Streaming paths are untouched.
func (s *Server) timeoutLayers(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		// Streaming / long-lived paths: no artificial deadline.
		if strings.Contains(path, "/process.Process/") || strings.HasSuffix(path, "/files") && r.Method == http.MethodGet {
			next.ServeHTTP(w, r)
			return
		}
		budget := defaultRouteTimeout
		switch {
		case strings.HasSuffix(path, "/connect"), strings.HasSuffix(path, "/pause"), strings.HasSuffix(path, "/resume"):
			budget = lifecycleRouteTimeout
		case strings.Contains(path, "/snapshots"), strings.Contains(path, "/rollback"):
			budget = longRouteTimeout
		}
		ctx, cancel := context.WithTimeout(r.Context(), budget)
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// Start serves HTTP until ctx is canceled.
func (s *Server) Start(ctx context.Context) error {
	srv := &http.Server{
		Addr:              s.Listen(),
		Handler:           s.Handle(),
		ReadHeaderTimeout: 30 * time.Second,
	}
	if s.gw == nil {
		return os.ErrInvalid
	}
	go s.gcLoop(ctx)
	errCh := make(chan error, 1)
	go func() {
		s.log("listening on %s (e2b api=/e2b/api envd=/e2b/envd files=/files)", s.Listen())
		errCh <- srv.ListenAndServe()
	}()
	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return srv.Shutdown(shutdownCtx)
	case err := <-errCh:
		return err
	}
}

// Listen returns the configured listen address.
func (s *Server) Listen() string { return s.listen }

// --- auth -----------------------------------------------------------------

// credentialFromHeaders extracts the control-plane credential, mirroring
// CubeSandbox's priority: Authorization: Bearer wins over X-API-Key.
func credentialFromHeaders(r *http.Request) string {
	if auth := r.Header.Get("Authorization"); strings.HasPrefix(auth, "Bearer ") {
		return stripE2BPrefix(strings.TrimPrefix(auth, "Bearer "))
	}
	if key := r.Header.Get("X-API-Key"); key != "" {
		return stripE2BPrefix(key)
	}
	if key := r.Header.Get("X-API-KEY"); key != "" {
		return stripE2BPrefix(key)
	}
	return ""
}

func (s *Server) acceptKey(bare string) bool {
	if bare == "" {
		return false
	}
	for _, k := range s.apiKeys {
		if constantTimeEqual(k, bare) {
			return true
		}
	}
	return false
}

func constantTimeEqual(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	var v byte
	for i := 0; i < len(a); i++ {
		v |= a[i] ^ b[i]
	}
	return v == 0
}

// envdKeyAuth guards the envd surface. /health stays open like real envd's —
// it is how isRunning() probes. Signed file requests may present the signed
// query instead of the header token (judged in the file handler itself).
func (s *Server) envdKeyAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/health") {
			next.ServeHTTP(w, r)
			return
		}
		if r.URL.Path == "/files" && r.URL.Query().Get("signature") != "" {
			// Signed-URL requests carry no token; the signature is verified
			// in the file handler after the sandbox is identified.
			next.ServeHTTP(w, r)
			return
		}
		sandboxID := r.Header.Get("E2b-Sandbox-Id")
		token := r.Header.Get("X-Access-Token")
		if sandboxID == "" || token == "" || !verifyEnvdToken(s.signingSecret, sandboxID, token) {
			jsonWriter(w, http.StatusUnauthorized, errorBody(connectError("unauthenticated", "invalid envd access token")))
			return
		}
		ctx := withSandboxID(r.Context(), sandboxID)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

// sandboxIDKey is the context key for the authenticated E2b-Sandbox-Id.
type sandboxIDKey struct{}

func withSandboxID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, sandboxIDKey{}, id)
}

func sandboxIDOf(r *http.Request) string {
	if id, ok := r.Context().Value(sandboxIDKey{}).(string); ok {
		return id
	}
	return r.Header.Get("E2b-Sandbox-Id")
}

// resolveSigningSecret picks a stable signing secret: explicit env wins,
// then the server's sandbox CA key on the node, then a random per-process
// key (tokens die on restart, warned).
func resolveSigningSecret() string {
	if v := os.Getenv("K8E_E2B_SIGNING_SECRET"); v != "" {
		return v
	}
	for _, p := range []string{
		"/var/lib/k8e/server/tls/sandbox-ca.key",
		"/etc/k8e/tls/sandbox-ca.key",
	} {
		if b, err := os.ReadFile(p); err == nil && len(b) > 0 {
			return string(b)
		}
	}
	buf := make([]byte, 32)
	_, _ = rand.Read(buf)
	logrus.Warn("e2b: no signing secret configured (K8E_E2B_SIGNING_SECRET); using a random per-process key — envd tokens will not survive a restart")
	return hex.EncodeToString(buf)
}

// helpers ------------------------------------------------------------------

func (s *Server) controlNotFound(w http.ResponseWriter, r *http.Request) {
	jsonWriter(w, http.StatusNotFound, errorBody(apiError(404, "route "+r.Method+" "+r.URL.Path+" not found")))
}

func (s *Server) envdNotFound(w http.ResponseWriter, r *http.Request) {
	jsonWriter(w, http.StatusNotFound, errorBody(connectError("not_found", "route "+r.Method+" "+r.URL.Path+" not found")))
}

func (s *Server) writeControlError(w http.ResponseWriter, e *E2bError) {
	jsonWriter(w, e.StatusCode, errorBody(e))
}

func (s *Server) writeEnvdError(w http.ResponseWriter, e *E2bError) {
	jsonWriter(w, e.StatusCode, errorBody(e))
}

func normalizeEnvVars(raw map[string]string) map[string]string {
	if len(raw) == 0 {
		return nil
	}
	out := make(map[string]string, len(raw))
	for k, v := range raw {
		out[k] = v
	}
	return out
}

func sanitizeMetadata(meta map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range meta {
		if k == "name" {
			if !namePattern.MatchString(v) {
				continue
			}
		}
		out[k] = v
	}
	return out
}

var namePattern = regexp.MustCompile(`^[a-zA-Z0-9._-]{1,64}$`)
