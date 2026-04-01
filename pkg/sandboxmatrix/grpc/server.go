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
	"time"

	"github.com/sirupsen/logrus"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/status"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
)

const sandboxdPort = 2024

// SandboxdPort is exported for use by the controller.
const SandboxdPort = sandboxdPort

// Server implements the SandboxService gRPC interface.
type Server struct {
	pb.UnimplementedSandboxServiceServer
	k8s      kubernetes.Interface
	dyn      dynamic.Interface
	orch     *Orchestrator
	lisAddr  string
	certFile string
	keyFile  string
}

func NewServer(k8s kubernetes.Interface, dyn dynamic.Interface, certFile, keyFile string, grpcPort int) *Server {
	if grpcPort == 0 {
		grpcPort = 50051
	}
	s := &Server{
		k8s:      k8s,
		dyn:      dyn,
		lisAddr:  fmt.Sprintf("0.0.0.0:%d", grpcPort),
		certFile: certFile,
		keyFile:  keyFile,
	}
	s.orch = NewOrchestrator(k8s, dyn)
	return s
}

// Start registers the gRPC server and begins listening on lisAddr (default 0.0.0.0:50051).
func (s *Server) Start(ctx context.Context) error {
	lis, err := net.Listen("tcp", s.lisAddr)
	if err != nil {
		return fmt.Errorf("grpc listen: %w", err)
	}
	creds, err := credentials.NewServerTLSFromFile(s.certFile, s.keyFile)
	if err != nil {
		return fmt.Errorf("grpc tls credentials: %w", err)
	}
	gs := grpc.NewServer(grpc.Creds(creds))
	pb.RegisterSandboxServiceServer(gs, s)
	logrus.Infof("sandbox gRPC gateway listening on %s", s.lisAddr)
	go func() {
		<-ctx.Done()
		gs.GracefulStop()
	}()
	return gs.Serve(lis)
}

func (s *Server) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.CreateSessionResponse, error) {
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

	body, _ := json.Marshal(map[string]any{"command": req.Command, "timeout": timeout, "workdir": workdir})
	httpCtx, cancel := context.WithTimeout(ctx, time.Duration(timeout+5)*time.Second)
	defer cancel()

	httpReq, _ := http.NewRequestWithContext(httpCtx, http.MethodPost,
		fmt.Sprintf("http://%s:%d/exec", podIP, sandboxdPort), bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")

	resp, err := http.DefaultClient.Do(httpReq)
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
	body, _ := json.Marshal(map[string]any{"command": req.Command})
	httpReq, _ := http.NewRequestWithContext(stream.Context(), http.MethodGet,
		fmt.Sprintf("http://%s:%d/exec/stream", podIP, sandboxdPort), bytes.NewReader(body))

	resp, err := http.DefaultClient.Do(httpReq)
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
	body, _ := json.Marshal(map[string]any{"path": req.Path, "content": req.Content, "mode": mode})
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("http://%s:%d/files/write", podIP, sandboxdPort), bytes.NewReader(body))
	httpReq.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(httpReq)
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
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://%s:%d/files/read?path=%s", podIP, sandboxdPort, req.Path), http.NoBody)
	resp, err := http.DefaultClient.Do(httpReq)
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
	httpReq, _ := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("http://%s:%d/files/list?since=%d", podIP, sandboxdPort, req.Since), http.NoBody)
	resp, err := http.DefaultClient.Do(httpReq)
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
	pkgList := ""
	for i, p := range req.Packages {
		if i > 0 {
			pkgList += " "
		}
		pkgList += p
	}
	execResp, err := s.Exec(ctx, &pb.ExecRequest{SessionId: req.SessionId, Command: "pip install " + pkgList, Timeout: 120})
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

func (s *Server) getPodIP(ctx context.Context, sessionID string) (string, error) {
	u, err := s.dyn.Resource(sessionGVR).Namespace(sandboxNS).Get(ctx, sessionID, metav1.GetOptions{})
	if err != nil {
		return "", status.Errorf(codes.NotFound, "session %s not found", sessionID)
	}
	podIP, _, _ := unstructured.NestedString(u.Object, "status", "podIP")
	if podIP == "" {
		return "", status.Errorf(codes.Unavailable, "session %s has no pod IP yet", sessionID)
	}
	return podIP, nil
}
