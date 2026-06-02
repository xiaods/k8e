// Package grpc implements the SandboxService gRPC gateway.
package grpc

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"regexp"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"github.com/xiaods/k8e/pkg/sandboxmatrix/ratelimit"
	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
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

// Server implements the SandboxService gRPC interface.
type Server struct {
	pb.UnimplementedSandboxServiceServer
	k8s         kubernetes.Interface
	dyn         dynamic.Interface
	orch        *Orchestrator
	lisAddr     string
	certFile    string
	keyFile     string
	apiKeys     map[string]string // name → key for validation
	rateLimiter *ratelimit.Limiter
}

func NewServer(k8s kubernetes.Interface, dyn dynamic.Interface, certFile, keyFile string, grpcPort int) *Server {
	if grpcPort == 0 {
		grpcPort = 50051
	}
	s := &Server{
		k8s:         k8s,
		dyn:         dyn,
		lisAddr:     fmt.Sprintf("0.0.0.0:%d", grpcPort),
		certFile:    certFile,
		keyFile:     keyFile,
		rateLimiter: ratelimit.NewLimiter(ratelimit.DefaultRateConfig()),
	}
	s.orch = NewOrchestrator(k8s, dyn)
	return s
}

// loadAPIKeys reads API keys from the sandbox-apikeys Secret.
func (s *Server) loadAPIKeys(ctx context.Context) {
	secret, err := s.k8s.CoreV1().Secrets(apiKeySecretNS).Get(ctx, apiKeySecretName, metav1.GetOptions{})
	if err != nil {
		logrus.Debugf("sandbox gRPC: no api-key secret found, all requests allowed")
		return
	}
	data, ok := secret.Data["keys.json"]
	if !ok {
		return
	}
	var store map[string]string
	if err := json.Unmarshal(data, &store); err != nil {
		logrus.Warnf("sandbox gRPC: api-key secret corrupted: %v", err)
		return
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

	creds, err := credentials.NewServerTLSFromFile(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("grpc tls credentials: %w", err)
	}

	opts := []grpc.ServerOption{grpc.Creds(creds)}
	if len(s.apiKeys) > 0 {
		opts = append(opts,
			grpc.ChainUnaryInterceptor(s.rateLimiter.UnaryInterceptor, s.apiKeyInterceptor),
			grpc.ChainStreamInterceptor(s.rateLimiter.StreamInterceptor, s.apiStreamInterceptor),
		)
	} else {
		opts = append(opts,
			grpc.UnaryInterceptor(s.rateLimiter.UnaryInterceptor),
			grpc.StreamInterceptor(s.rateLimiter.StreamInterceptor),
		)
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
	if err := s.orch.CheckCapacity(ctx); err != nil {
		return nil, err
	}
	session, err := s.orch.CreateSession(ctx, req)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "create session: %v", err)
	}
	return &pb.CreateSessionResponse{SessionId: session.Name, PodIp: session.Status.PodIP}, nil
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
		runID, err := s.orch.ExecBackground(ctx, req.SessionId, req.Command, req.Timeout, req.Workdir)
		if err != nil {
			return nil, status.Errorf(codes.Internal, "background submit: %v", err)
		}
		return &pb.ExecResponse{RunId: runID, Status: "started", SessionId: req.SessionId}, nil
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

	body := map[string]any{"command": req.Command, "timeout": timeout, "workdir": workdir}
	httpCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()

	resp, err := sandboxdPost(httpCtx, podIP, "/exec", body)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd exec: %v", err)
	}
	defer resp.Body.Close()

	var result struct {
		Stdout   string `json:"stdout"`
		Stderr   string `json:"stderr"`
		ExitCode int32  `json:"exit_code"`
	}
	json.NewDecoder(resp.Body).Decode(&result)
	return &pb.ExecResponse{Stdout: result.Stdout, Stderr: result.Stderr, ExitCode: result.ExitCode}, nil
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
	body := map[string]any{"command": req.Command, "timeout": timeout, "workdir": workdir}
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
	var result struct{ Content string `json:"content"` }
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

// GetCACert returns the server's CA certificate for TLS verification.
// Requires API key authentication — the cert is only served to authenticated clients.
func (s *Server) GetCACert(ctx context.Context, req *pb.GetCACertRequest) (*pb.GetCACertResponse, error) {
	pem, err := os.ReadFile(s.certFile)
	if err != nil {
		return nil, status.Errorf(codes.Internal, "read ca cert: %v", err)
	}
	return &pb.GetCACertResponse{Cert: string(pem)}, nil
}

// PollRun checks the status of a background execution.
func (s *Server) PollRun(ctx context.Context, req *pb.PollRunRequest) (*pb.PollRunResponse, error) {
	return s.orch.PollRun(ctx, req.RunId)
}

func (s *Server) getPodIP(ctx context.Context, sessionID string) (string, error) {
	u, err := s.dyn.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sessionID, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}
	podIP, _, _ := unstructured.NestedString(u.Object, "status", "podIP")
	if podIP != "" {
		return podIP, nil
	}
	// pod just created — poll until IP is assigned (up to 60s, exponential backoff)
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

// validateAPIKey extracts and validates the API key from the gRPC context metadata.
func (s *Server) validateAPIKey(ctx context.Context) error {
	md, ok := metadata.FromIncomingContext(ctx)
	if !ok {
		return status.Error(codes.Unauthenticated, "missing metadata")
	}
	auth := md.Get("authorization")
	if len(auth) == 0 {
		return status.Error(codes.Unauthenticated, "missing authorization header")
	}
	token := strings.TrimPrefix(auth[0], "Bearer ")
	if token == auth[0] {
		return status.Error(codes.Unauthenticated, "invalid authorization format, expected 'Bearer <key>'")
	}
	for _, key := range s.apiKeys {
		if key == token {
			return nil
		}
	}
	return status.Error(codes.Unauthenticated, "invalid api key")
}

// apiKeyInterceptor validates the authorization header against known API keys for unary RPCs.
func (s *Server) apiKeyInterceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	if err := s.validateAPIKey(ctx); err != nil {
		return nil, err
	}
	return handler(ctx, req)
}

// apiStreamInterceptor validates the authorization header for streaming RPCs.
func (s *Server) apiStreamInterceptor(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
	if err := s.validateAPIKey(ss.Context()); err != nil {
		return err
	}
	return handler(srv, ss)
}
