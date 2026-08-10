// Package grpc implements the SandboxService gRPC gateway.
package grpc

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/sandboxlayer"
	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"github.com/xiaods/k8e/pkg/sandboxmatrix/ratelimit"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/peer"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const apiKeySecretNS = "sandbox-matrix"
const apiKeySecretName = "sandbox-apikeys"

const sandboxdPort = 2024

// SandboxdPort is exported for use by the controller.
const SandboxdPort = sandboxdPort

// sandboxdClient is a dedicated HTTP client for sandboxd calls with a base timeout.
var sandboxdClient = &http.Client{Timeout: 5 * time.Minute}

// sandboxdURL builds a sandboxd HTTP URL for the given pod IP and path.
func sandboxdURL(podIP, path string) string {
	return fmt.Sprintf("http://%s:%d%s", podIP, sandboxdPort, path)
}

// sandboxdPost sends a JSON POST request to sandboxd and returns the response.
func sandboxdPost(ctx context.Context, podIP, path string, body interface{}) (*http.Response, error) {
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, sandboxdURL(podIP, path), bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	return sandboxdClient.Do(req)
}

// sandboxdGet sends a GET request to sandboxd and returns the response.
func sandboxdGet(ctx context.Context, podIP, path string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, sandboxdURL(podIP, path), http.NoBody)
	if err != nil {
		return nil, err
	}
	return sandboxdClient.Do(req)
}

// pipPkgNameRe validates Python package names per PEP 508.
var pipPkgNameRe = regexp.MustCompile(`^[a-zA-Z0-9]([a-zA-Z0-9._-]*[a-zA-Z0-9])?$`)

// sanitizePipPackage validates a pip package specifier against shell injection.
// Accepts bare names, version constraints (requests>=2.28), and extras (pkg[extra]).
func sanitizePipPackage(raw string) (string, error) {
	// Extract the base package name before any version/extras specifiers
	name := raw
	for _, sep := range []string{">=", "<=", "!=", "==", "~=", ">", "<", "=", "["} {
		if idx := strings.Index(name, sep); idx >= 0 {
			name = name[:idx]
			break
		}
	}
	if !pipPkgNameRe.MatchString(name) {
		return "", fmt.Errorf("invalid package name: %q", raw)
	}
	// Also reject shell metacharacters in the version/extras portion
	for _, ch := range []string{";", "&", "|", "`", "$", "(", ")", "\n", "\r"} {
		if strings.Contains(raw, ch) {
			return "", fmt.Errorf("invalid characters in package specifier: %q", raw)
		}
	}
	return raw, nil
}

// ServerConfig holds the configuration for a sandbox gRPC Server.
type ServerConfig struct {
	K8s            kubernetes.Interface
	Dyn            dynamic.Interface
	CACertFile     string
	CAKeyFile      string
	ServerCertFile string
	ServerKeyFile  string
	GRPCPort       int
	LocalAuth      bool // allow loopback connections without client cert
	// LayerStoreDir, when set, enables the server-side content-addressed
	// snapshot layer registry (KIP-16 M2 / issue #511).
	LayerStoreDir string
	// FQDNEnabled enables Cilium toFQDNs egress for sessions with allowedHosts
	// (requires Cilium DNS proxy; KIP-16 M10 / issue #510).
	FQDNEnabled bool
}

// Server implements the SandboxService gRPC interface.
type Server struct {
	pb.UnimplementedSandboxServiceServer
	k8s            kubernetes.Interface
	dyn            dynamic.Interface
	orch           *Orchestrator
	lisAddr        string
	caCertFile     string
	caKeyFile      string
	serverCertFile string
	serverKeyFile  string
	caCert         *x509.Certificate
	caKey          *ecdsa.PrivateKey
	apiKeys        map[string]string // name → key for validation
	issuedStore    *issuedCertStore
	revocList      *RevocationList
	localAuth      bool
	rateLimiter    *ratelimit.Limiter
	layerStore     *sandboxlayer.Store
}

func NewServer(cfg ServerConfig) *Server {
	port := cfg.GRPCPort
	if port == 0 {
		port = 50051
	}
	s := &Server{
		k8s:            cfg.K8s,
		dyn:            cfg.Dyn,
		lisAddr:        fmt.Sprintf("0.0.0.0:%d", port),
		caCertFile:     cfg.CACertFile,
		caKeyFile:      cfg.CAKeyFile,
		serverCertFile: cfg.ServerCertFile,
		serverKeyFile:  cfg.ServerKeyFile,
		localAuth:      cfg.LocalAuth,
		rateLimiter:    ratelimit.NewLimiter(ratelimit.DefaultRateConfig()),
	}
	s.orch = NewOrchestrator(cfg.K8s, cfg.Dyn)
	if cfg.FQDNEnabled {
		s.orch.SetFQDNEGressEnabled(true)
	}
	RegisterSandboxMetrics(s.orch)
	if cfg.LayerStoreDir != "" {
		if ls, err := sandboxlayer.New(cfg.LayerStoreDir); err == nil {
			s.layerStore = ls
		}
	}
	return s
}

// loadAPIKeys reads API keys from the sandbox-apikeys Secret.
func (s *Server) loadAPIKeys(ctx context.Context) {
	secret, err := s.k8s.CoreV1().Secrets(apiKeySecretNS).Get(ctx, apiKeySecretName, metav1.GetOptions{})
	if err != nil {
		logrus.Debugf("sandbox gRPC: no api-key secret found, all requests allowed")
		s.apiKeys = nil
		return
	}
	data, ok := secret.Data["keys.json"]
	if !ok {
		s.apiKeys = nil
		return
	}
	var store map[string]string
	if err := json.Unmarshal(data, &store); err != nil {
		logrus.Warnf("sandbox gRPC: api-key secret corrupted: %v", err)
		return
	}
	// Detect removed keys and revoke their certificates
	if s.revocList != nil && s.issuedStore != nil {
		for name := range s.apiKeys {
			if _, ok := store[name]; !ok {
				s.revocList.RevokeByKeyName(s.issuedStore, name)
				logrus.Infof("sandbox gRPC: revoked certificates for deleted API key %q", name)
			}
		}
	}
	s.apiKeys = store
	logrus.Infof("sandbox gRPC: loaded %d API key(s)", len(store))
}

// reloadConfigLoop periodically reloads API keys and rate limits from the SandboxMatrix CRD.
func (s *Server) reloadConfigLoop(ctx context.Context) {
	// Immediate cleanup goroutine for stale rate limit tenants
	go func() {
		reapTicker := time.NewTicker(5 * time.Minute)
		defer reapTicker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-reapTicker.C:
				s.rateLimiter.ReapStale(10 * time.Minute)
			}
		}
	}()

	ticker := time.NewTicker(30 * time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.loadAPIKeys(ctx)
			s.reloadRateLimits(ctx)
		}
	}
}

// reloadRateLimits reads rate limit config from the SandboxMatrix CRD and applies it.
func (s *Server) reloadRateLimits(ctx context.Context) {
	matrixGVR := schema.GroupVersionResource{Group: "k8e.sh", Version: "v1alpha1", Resource: "sandboxmatrices"}
	matrices, err := s.dyn.Resource(matrixGVR).Namespace("sandbox-matrix").List(ctx, metav1.ListOptions{})
	if err != nil || len(matrices.Items) == 0 {
		return
	}
	obj := matrices.Items[0].Object
	var spec sandboxv1.RateLimitSpec
	if raw, found, _ := unstructured.NestedFieldNoCopy(obj, "spec", "rateLimits"); found {
		if data, err := json.Marshal(raw); err == nil {
			json.Unmarshal(data, &spec)
		}
	}
	s.rateLimiter.ReloadConfig(&spec)
}

// Start registers the gRPC server and begins listening on lisAddr (default 0.0.0.0:50051).
func (s *Server) Start(ctx context.Context) error {
	// Check port availability before binding to give a clear error
	testConn, err := net.DialTimeout("tcp", s.lisAddr, 100*time.Millisecond)
	if err == nil {
		testConn.Close()
		return fmt.Errorf("grpc port %s is already in use — another gateway may be running", s.lisAddr)
	}

	lis, err := net.Listen("tcp", s.lisAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}

	// Load API keys from Secret for remote client authentication
	s.loadAPIKeys(ctx)
	// Reload API keys and rate limits every 30s
	go s.reloadConfigLoop(ctx)
	// Clean up expired pending approvals from disconnected clients
	go s.orch.StartApprovalGC(ctx)
	// Rebuild background run registry from existing Session CRDs
	go s.orch.RebuildRunRegistry(ctx, "sandbox-matrix")

	// Initialize sandbox CA and server certificate
	caKey, caCert, err := ensureCA(s.caCertFile, s.caKeyFile)
	if err != nil {
		return fmt.Errorf("sandbox CA: %w", err)
	}
	s.caKey = caKey
	s.caCert = caCert

	if err := ensureServerCert(s.caKey, s.caCert, s.serverCertFile, s.serverKeyFile); err != nil {
		return fmt.Errorf("sandbox server cert: %w", err)
	}

	serverTLS, err := tls.LoadX509KeyPair(s.serverCertFile, s.serverKeyFile)
	if err != nil {
		return fmt.Errorf("load server cert: %w", err)
	}

	creds, err := buildMTLSCreds(s.caCert, serverTLS)
	if err != nil {
		return fmt.Errorf("grpc mTLS credentials: %w", err)
	}

	s.issuedStore = newIssuedCertStore(s.caCertFile[:strings.LastIndex(s.caCertFile, "/")] + "/sandbox-issued.json")
	s.revocList = newRevocationList()

	opts := []grpc.ServerOption{
		grpc.Creds(creds),
		// Raise the gRPC message size limits from the 4MiB default: snapshot
		// restore / file payloads routinely exceed it (see KIP-16 M7).
		grpc.MaxRecvMsgSize(64 * 1024 * 1024),
		grpc.MaxSendMsgSize(64 * 1024 * 1024),
		grpc.ChainUnaryInterceptor(s.rateLimiter.UnaryInterceptor, s.mTLSAuthInterceptor),
		grpc.ChainStreamInterceptor(s.rateLimiter.StreamInterceptor, s.mTLSStreamInterceptor),
	}
	gs := grpc.NewServer(opts...)
	pb.RegisterSandboxServiceServer(gs, s)
	logrus.Infof("sandbox gRPC gateway listening on %s", s.lisAddr)
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	return gs.Serve(lis)
}

func (s *Server) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.CreateSessionResponse, error) {
	if err := validateSessionEnv(req.Env); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "env: %v", err)
	}
	if err := validateSecretRefs(req.SecretRefs); err != nil {
		return nil, status.Errorf(codes.InvalidArgument, "secret_refs: %v", err)
	}
	if err := s.orch.CheckCapacity(ctx); err != nil {
		return nil, err
	}
	session, err := s.orch.CreateSession(ctx, req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create session: %v", err)
	}
	return &pb.CreateSessionResponse{SessionId: session.Name, PodIp: session.Status.PodIP}, nil
}

func (s *Server) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id required")
	}
	sess, err := s.orch.getSession(ctx, req.SessionId)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "session %s not found", req.SessionId)
	}
	return sessionToProtoView(sess, s.orch.countBackgroundRuns(req.SessionId)), nil
}

func (s *Server) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	phase := req.Phase
	if phase == "" {
		phase = string(sandboxv1.SandboxPhaseActive)
	}
	sessions, err := s.orch.listSessions(ctx, sandboxNS, phase)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "list sessions: %v", err)
	}
	out := make([]*pb.GetSessionResponse, 0, len(sessions))
	for _, sess := range sessions {
		out = append(out, sessionToProtoView(sess, s.orch.countBackgroundRuns(sess.Name)))
	}
	return &pb.ListSessionsResponse{Sessions: out}, nil
}

func (s *Server) DestroySession(ctx context.Context, req *pb.DestroySessionRequest) (*pb.DestroySessionResponse, error) {
	if err := s.orch.DestroySession(ctx, req.SessionId); err != nil {
		return nil, status.Errorf(codes.Internal, "destroy session: %v", err)
	}
	return &pb.DestroySessionResponse{Ok: true}, nil
}

func (s *Server) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	// Background mode: submit async, return run_id immediately
	if req.Background {
		env, envErr := s.resolveSessionEnv(ctx, req.SessionId)
		if envErr != nil {
			return nil, envErr
		}
		runID, err := s.orch.ExecBackground(ctx, req.SessionId, req.Command, req.Timeout, req.Workdir, env)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "background submit: %v", err)
		}
		return &pb.ExecResponse{
			RunId: runID, Status: execStatusStarted, SessionId: req.SessionId, Language: req.Language,
		}, nil
	}

	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = "/workspace"
	}

	env, envErr := s.resolveSessionEnv(ctx, req.SessionId)
	if envErr != nil {
		return nil, envErr
	}
	body := sandboxdExecBody(req.SessionId, req.Command, timeout, workdir, env)
	start := time.Now()
	httpCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()

	resp, err := sandboxdPost(httpCtx, podIP, "/exec", body)
	elapsed := time.Since(start).Milliseconds()
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd exec: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Stdout     string `json:"stdout"`
		Stderr     string `json:"stderr"`
		ExitCode   int32  `json:"exit_code"`
		DurationMs int64  `json:"duration_ms"`
		Truncated  bool   `json:"truncated"`
		TimedOut   bool   `json:"timed_out"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	duration := result.DurationMs
	if duration <= 0 {
		duration = elapsed
	}
	// Prefer sandboxd truncation flag; also mark if streams hit gateway-side cap observation.
	truncated := result.Truncated || len(result.Stdout) >= maxExecOutputBytes || len(result.Stderr) >= maxExecOutputBytes
	timedOut := result.TimedOut || (timeout > 0 && duration >= int64(timeout)*1000 && result.ExitCode != 0)
	return &pb.ExecResponse{
		Stdout:     result.Stdout,
		Stderr:     result.Stderr,
		ExitCode:   result.ExitCode,
		SessionId:  req.SessionId,
		Status:     classifyExecStatus(timedOut, false),
		DurationMs: duration,
		Truncated:  truncated,
		Language:   req.Language,
	}, nil
}

func (s *Server) ExecStream(req *pb.ExecRequest, stream pb.SandboxService_ExecStreamServer) error {
	podIP, err := s.getPodIP(stream.Context(), req.SessionId)
	if err != nil {
		return err
	}
	timeout := req.Timeout
	if timeout == 0 {
		timeout = 30
	}
	workdir := req.Workdir
	if workdir == "" {
		workdir = "/workspace"
	}
	env, envErr := s.resolveSessionEnv(stream.Context(), req.SessionId)
	if envErr != nil {
		return envErr
	}
	body := sandboxdExecBody(req.SessionId, req.Command, timeout, workdir, env)
	httpCtx, cancel := context.WithTimeout(stream.Context(), time.Duration(timeout+5)*time.Second)
	defer cancel()

	resp, err := sandboxdPost(httpCtx, podIP, "/exec/stream", body)
	if err != nil {
		return status.Errorf(codes.Unavailable, "sandboxd stream: %v", err)
	}
	defer resp.Body.Close()

	buf := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if serr := stream.Send(&pb.ExecStreamResponse{Chunk: string(buf[:n])}); serr != nil {
				return serr
			}
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return status.Errorf(codes.Internal, "stream read: %v", err)
		}
	}
}

// resolveSessionEnv loads non-sensitive env and resolves secret_refs from K8s Secrets.
// Secret resolution failures return a gRPC error (fail closed for secrets).
func (s *Server) resolveSessionEnv(ctx context.Context, sessionID string) (map[string]string, error) {
	sess, err := s.orch.getSession(ctx, sessionID)
	if err != nil {
		logrus.WithError(err).WithField("session_id", sessionID).
			Warn("sandbox gRPC: failed to load session for env; continuing with sandboxd defaults")
		return nil, nil
	}
	var out map[string]string
	if len(sess.Spec.Env) > 0 {
		out = make(map[string]string, len(sess.Spec.Env)+len(sess.Spec.SecretRefs))
		for k, v := range sess.Spec.Env {
			out[k] = v
		}
	}
	if len(sess.Spec.SecretRefs) == 0 {
		return out, nil
	}
	if out == nil {
		out = make(map[string]string, len(sess.Spec.SecretRefs))
	}
	for _, ref := range sess.Spec.SecretRefs {
		val, rerr := s.readSecretKey(ctx, ref.SecretName, ref.Key)
		if rerr != nil {
			return nil, status.Errorf(codes.FailedPrecondition,
				"secret_ref %s/%s for env %s: %v", ref.SecretName, ref.Key, ref.EnvVar, rerr)
		}
		out[ref.EnvVar] = val
	}
	return out, nil
}

// getSessionEnv is a best-effort non-secret env load (tests / legacy helpers).
func (s *Server) getSessionEnv(ctx context.Context, sessionID string) map[string]string {
	env, err := s.resolveSessionEnv(ctx, sessionID)
	if err != nil {
		return nil
	}
	return env
}

func (s *Server) readSecretKey(ctx context.Context, secretName, key string) (string, error) {
	sec, err := s.k8s.CoreV1().Secrets(sandboxNS).Get(ctx, secretName, metav1.GetOptions{})
	if err != nil {
		return "", err
	}
	raw, ok := sec.Data[key]
	if !ok {
		return "", fmt.Errorf("key %q not found in secret %q", key, secretName)
	}
	return string(raw), nil
}

func (s *Server) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}
	mode := req.Mode
	if mode == "" {
		mode = "w"
	}
	body := map[string]any{"path": req.Path, "content": req.Content, "mode": mode}
	resp, err := sandboxdPost(ctx, podIP, "/files/write", body)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd write: %v", err)
	}
	resp.Body.Close()
	return &pb.WriteFileResponse{Ok: resp.StatusCode == http.StatusOK}, nil
}

func (s *Server) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdGet(ctx, podIP, fmt.Sprintf("/files/read?path=%s", req.Path))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd read: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		Content string `json:"content"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return &pb.ReadFileResponse{Content: result.Content}, nil
}

func (s *Server) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdGet(ctx, podIP, fmt.Sprintf("/files/list?since=%d", req.Since))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd list: %v", err)
	}
	defer resp.Body.Close()
	var result struct {
		Files []struct {
			Path     string `json:"path"`
			Modified int64  `json:"modified"`
		} `json:"files"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	entries := make([]*pb.FileEntry, len(result.Files))
	for i, f := range result.Files {
		entries[i] = &pb.FileEntry{Path: f.Path, Modified: f.Modified}
	}
	return &pb.ListFilesResponse{Files: entries}, nil
}

func (s *Server) PipInstall(ctx context.Context, req *pb.PipInstallRequest) (*pb.PipInstallResponse, error) {
	if len(req.Packages) == 0 {
		return nil, status.Error(codes.InvalidArgument, "no packages specified")
	}
	safe := make([]string, 0, len(req.Packages))
	for _, p := range req.Packages {
		validated, err := sanitizePipPackage(p)
		if err != nil {
			return nil, status.Errorf(codes.InvalidArgument, "%v", err)
		}
		safe = append(safe, validated)
	}
	cmd := "pip install --no-cache-dir " + strings.Join(safe, " ")
	execResp, err := s.Exec(ctx, &pb.ExecRequest{SessionId: req.SessionId, Command: cmd, Timeout: 120})
	if err != nil {
		return nil, err
	}
	return &pb.PipInstallResponse{Output: execResp.Stdout + execResp.Stderr, ExitCode: execResp.ExitCode}, nil
}

func (s *Server) RunSubAgent(ctx context.Context, req *pb.RunSubAgentRequest) (*pb.RunSubAgentResponse, error) {
	return s.orch.RunSubAgent(ctx, req)
}

func (s *Server) ConfirmAction(ctx context.Context, req *pb.ConfirmActionRequest) (*pb.ConfirmActionResponse, error) {
	return s.orch.ConfirmAction(ctx, req)
}

func (s *Server) ApproveAction(ctx context.Context, req *pb.ApproveActionRequest) (*pb.ApproveActionResponse, error) {
	return s.orch.ApproveAction(ctx, req)
}

// GetTranscript reads a bounded, offset-resumable window of a session's exec
// transcript (file-backed in sandboxd; KIP-16 M4 / issue #512).
func (s *Server) GetTranscript(ctx context.Context, req *pb.GetTranscriptRequest) (*pb.GetTranscriptResponse, error) {
	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}
	path := fmt.Sprintf("/transcript?session=%s&offset=%d&limit=%d",
		url.QueryEscape(req.SessionId), req.Offset, req.Limit)
	resp, err := sandboxdGet(ctx, podIP, path)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd transcript: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// No transcript yet for this session — return an empty window at 0.
		return &pb.GetTranscriptResponse{SessionId: req.SessionId, Eof: true}, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, status.Errorf(codes.Internal, "sandboxd transcript: http %d", resp.StatusCode)
	}
	var result struct {
		Output          string `json:"output"`
		Offset          int64  `json:"offset"`
		NextOffset      int64  `json:"next_offset"`
		TruncatedBefore bool   `json:"truncated_before"`
		Eof             bool   `json:"eof"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return &pb.GetTranscriptResponse{
		SessionId:       req.SessionId,
		Output:          result.Output,
		Offset:          result.Offset,
		NextOffset:      result.NextOffset,
		TruncatedBefore: result.TruncatedBefore,
		Eof:             result.Eof,
	}, nil
}

// GetEvents reads the daemon's NDJSON event stream (exec/files/bg events;
// KIP-16 M5 / issue #513).
func (s *Server) GetEvents(ctx context.Context, req *pb.GetEventsRequest) (*pb.GetEventsResponse, error) {
	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}
	limit := req.Limit
	if limit == 0 {
		limit = 500
	}
	path := fmt.Sprintf("/events?limit=%d", limit)
	resp, err := sandboxdGet(ctx, podIP, path)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd events: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		return &pb.GetEventsResponse{}, nil // no events yet
	}
	if resp.StatusCode != http.StatusOK {
		return nil, status.Errorf(codes.Internal, "sandboxd events: http %d", resp.StatusCode)
	}
	var raw []json.RawMessage
	if err := json.NewDecoder(resp.Body).Decode(&raw); err != nil {
		return nil, status.Errorf(codes.Internal, "sandboxd events decode: %v", err)
	}
	lines := make([]string, len(raw))
	for i, r := range raw {
		lines[i] = string(r)
	}
	return &pb.GetEventsResponse{
		Events:    lines,
		Returned:  int64(len(lines)),
		Truncated: int64(len(lines)) == limit,
	}, nil
}

// requireLayerStore returns an error when the server-side layer registry is
// not enabled (ServerConfig.LayerStoreDir unset).
func (s *Server) requireLayerStore() (*sandboxlayer.Store, error) {
	if s.layerStore == nil {
		return nil, status.Error(codes.FailedPrecondition,
			"server-side snapshot registry disabled; set ServerConfig.LayerStoreDir")
	}
	return s.layerStore, nil
}

// SnapshotPut publishes a snapshot manifest (its layer list) to the server-side
// registry (KIP-16 M2 / issue #511).
func (s *Server) SnapshotPut(ctx context.Context, req *pb.SnapshotPutRequest) (*pb.SnapshotPutResponse, error) {
	store, err := s.requireLayerStore()
	if err != nil {
		return nil, err
	}
	if req.Name == "" || len(req.Layers) == 0 {
		return nil, status.Error(codes.InvalidArgument, "name and layers required")
	}
	if err := store.SaveManifest(req.Name, req.Layers); err != nil {
		return nil, status.Errorf(codes.Internal, "snapshot put: %v", err)
	}
	return &pb.SnapshotPutResponse{Name: req.Name, Layers: int64(len(req.Layers))}, nil
}

// SnapshotGet returns a snapshot manifest's ordered layer list.
func (s *Server) SnapshotGet(ctx context.Context, req *pb.SnapshotGetRequest) (*pb.SnapshotGetResponse, error) {
	store, err := s.requireLayerStore()
	if err != nil {
		return nil, err
	}
	m, err := store.LoadManifest(req.Name)
	if err != nil {
		return nil, status.Errorf(codes.NotFound, "snapshot %s: %v", req.Name, err)
	}
	return &pb.SnapshotGetResponse{Name: req.Name, Layers: m.Layers}, nil
}

// SnapshotList returns all snapshot manifest names in the registry.
func (s *Server) SnapshotList(ctx context.Context, req *pb.SnapshotListRequest) (*pb.SnapshotListResponse, error) {
	store, err := s.requireLayerStore()
	if err != nil {
		return nil, err
	}
	names, err := store.ListManifests()
	if err != nil {
		return nil, status.Errorf(codes.Internal, "snapshot list: %v", err)
	}
	return &pb.SnapshotListResponse{Names: names}, nil
}

// PollRun checks the status of a background execution.
func (s *Server) PollRun(ctx context.Context, req *pb.PollRunRequest) (*pb.PollRunResponse, error) {
	return s.orch.PollRun(ctx, req.RunId)
}

// Login authenticates the client (via mTLS or API key) and returns a signed client certificate.
func (s *Server) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	keyName, _ := peerIdentity(ctx)

	if keyName == "" {
		md, ok := metadata.FromIncomingContext(ctx)
		if !ok {
			return nil, status.Error(codes.Unauthenticated, "missing metadata")
		}
		auth := md.Get("authorization")
		if len(auth) == 0 {
			return nil, status.Error(codes.Unauthenticated, "missing API key or client certificate")
		}
		token := strings.TrimPrefix(auth[0], "Bearer ")
		for name, key := range s.apiKeys {
			if key == token {
				keyName = name
				break
			}
		}
		if keyName == "" {
			return nil, status.Error(codes.Unauthenticated, "invalid API key")
		}
	}

	certPEM, fingerprint, err := signClientCert(s.caKey, s.caCert, req.Csr, keyName, 30)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "sign certificate: %v", err)
	}

	s.issuedStore.Add(keyName, fingerprint, time.Now(), time.Now().Add(30*24*time.Hour))

	logrus.WithFields(logrus.Fields{
		"key_name":         keyName,
		"device_name":      req.DeviceName,
		"client_version":   req.ClientVersion,
		"cert_fingerprint": fingerprint,
	}).Info("sandbox gRPC: client certificate issued")

	caPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: s.caCert.Raw})

	return &pb.LoginResponse{
		Cert:      certPEM,
		CaCert:    string(caPEM),
		ValidDays: 30,
	}, nil
}

func (s *Server) getPodIP(ctx context.Context, sessionID string) (string, error) {
	u, err := s.dyn.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sessionID, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}
	podIP, _, _ := unstructured.NestedString(u.Object, "status", "podIP")
	if podIP != "" {
		// Verify the pod still exists — it may have been deleted externally.
		pods, err := s.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
			LabelSelector: labelSessionID + "=" + sessionID,
		})
		if err == nil && len(pods.Items) == 0 {
			return "", status.Errorf(codes.NotFound, "session %s pod no longer exists", sessionID)
		}
		return podIP, nil
	}
	return s.pollForPodIP(ctx, sessionID)
}

func (s *Server) pollForPodIP(ctx context.Context, sessionID string) (string, error) {
	wait := 1 * time.Second
	maxWait := 5 * time.Second
	for i := 0; i < 12; i++ {
		select {
		case <-ctx.Done():
			return "", status.Errorf(codes.Canceled, "context cancelled waiting for pod IP")
		case <-time.After(wait):
		}
		pods, err := s.k8s.CoreV1().Pods(sandboxNS).List(ctx, metav1.ListOptions{
			LabelSelector: labelSessionID + "=" + sessionID,
		})
		if err == nil {
			for i := range pods.Items {
				if pods.Items[i].Status.PodIP != "" {
					return pods.Items[i].Status.PodIP, nil
				}
			}
		}
		wait *= 2
		if wait > maxWait {
			wait = maxWait
		}
	}
	return "", status.Errorf(codes.FailedPrecondition, "session %s has no pod IP after 60s", sessionID)
}

// mTLSAuthInterceptor enforces mTLS for all RPCs except Login.
func (s *Server) mTLSAuthInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if info.FullMethod == "/sandbox.v1.SandboxService/Login" {
		return handler(ctx, req)
	}
	if err := s.checkMTLSAuth(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// mTLSStreamInterceptor enforces mTLS for all streaming RPCs except ExecStream.
func (s *Server) mTLSStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.checkMTLSAuth(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}

func (s *Server) checkMTLSAuth(ctx context.Context) error {
	keyName, isLocal := peerIdentity(ctx)
	if isLocal && s.localAuth {
		return nil
	}
	if keyName == "" {
		return status.Error(codes.Unauthenticated, "client certificate required for mTLS")
	}
	if s.revocList.IsRevoked(certFingerprintFromContext(ctx)) {
		return status.Error(codes.PermissionDenied, "client certificate has been revoked")
	}
	return nil
}

func certFingerprintFromContext(ctx context.Context) string {
	p, ok := peer.FromContext(ctx)
	if !ok {
		return ""
	}
	tlsInfo, ok := p.AuthInfo.(credentials.TLSInfo)
	if !ok || len(tlsInfo.State.PeerCertificates) == 0 {
		return ""
	}
	h := sha256.Sum256(tlsInfo.State.PeerCertificates[0].Raw)
	return fmt.Sprintf("%x", h[:])
}
