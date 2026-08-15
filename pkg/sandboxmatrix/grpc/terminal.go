package grpc

import (
	"bufio"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// maxTerminals bounds the gateway-side terminal registry so a runaway client
// cannot grow it without bound. Entries are removed on TerminalDestroy and on
// lazy cleanup when their session no longer resolves.
const maxTerminals = 10000

// terminalEntry maps a gateway-owned branded terminal_id to the sandboxd
// terminal it proxies. The branded id is opaque; the sandboxd id is a
// pod-local numeric handle.
type terminalEntry struct {
	sessionID  string
	sandboxdID uint32
}

// terminalSignalName maps the proto enum to the signal names sandboxd accepts.
func terminalSignalName(sig pb.TerminalSignal) (string, error) {
	switch sig {
	case pb.TerminalSignal_TERMINAL_SIGNAL_INT:
		return "SIGINT", nil
	case pb.TerminalSignal_TERMINAL_SIGNAL_TERM:
		return "SIGTERM", nil
	case pb.TerminalSignal_TERMINAL_SIGNAL_KILL:
		return "SIGKILL", nil
	case pb.TerminalSignal_TERMINAL_SIGNAL_TSTP:
		return "SIGTSTP", nil
	case pb.TerminalSignal_TERMINAL_SIGNAL_HUP:
		return "SIGHUP", nil
	default:
		return "", status.Errorf(codes.InvalidArgument, "unsupported terminal signal %v", sig)
	}
}

// resolveTerminal returns the pod IP and sandboxd terminal id for a branded
// terminal_id. A terminal whose session no longer resolves is dropped lazily.
func (s *Server) resolveTerminal(ctx context.Context, terminalID string) (string, uint32, error) {
	s.terminalsMu.RLock()
	e, ok := s.terminals[terminalID]
	s.terminalsMu.RUnlock()
	if !ok {
		return "", 0, status.Error(codes.NotFound, "terminal not found")
	}
	podIP, err := s.getPodIP(ctx, e.sessionID)
	if err != nil {
		s.terminalsMu.Lock()
		delete(s.terminals, terminalID)
		s.terminalsMu.Unlock()
		return "", 0, err
	}
	return podIP, e.sandboxdID, nil
}

func (s *Server) dropTerminal(terminalID string) {
	s.terminalsMu.Lock()
	delete(s.terminals, terminalID)
	s.terminalsMu.Unlock()
}

// CreateTerminal allocates a sandbox PTY and starts argv as a controlling-
// terminal session leader, then records a branded terminal_id for routing.
func (s *Server) CreateTerminal(ctx context.Context, req *pb.CreateTerminalRequest) (*pb.CreateTerminalResponse, error) {
	if req.SessionId == "" {
		return nil, status.Error(codes.InvalidArgument, "session_id required")
	}
	if len(req.Argv) == 0 {
		return nil, status.Error(codes.InvalidArgument, "argv must be non-empty")
	}
	podIP, err := s.getPodIP(ctx, req.SessionId)
	if err != nil {
		return nil, err
	}

	rows := req.Rows
	cols := req.Cols
	if rows <= 0 {
		rows = 24
	}
	if cols <= 0 {
		cols = 80
	}
	body := map[string]any{
		"argv":    req.Argv,
		"workdir": req.Workdir,
		"env":     req.Env,
		"rows":    rows,
		"cols":    cols,
	}
	resp, err := sandboxdPost(ctx, podIP, "/pty/create", body)
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd pty create: %v", err)
	}
	defer resp.Body.Close()
	var created struct {
		TerminalID uint32 `json:"terminal_id"`
		PID        int32  `json:"pid"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, status.Errorf(codes.Internal, "sandboxd pty create decode: %v", err)
	}

	s.terminalsMu.Lock()
	if len(s.terminals) >= maxTerminals {
		s.terminalsMu.Unlock()
		return nil, status.Error(codes.ResourceExhausted, "terminal registry full")
	}
	s.terminalSeq++
	terminalID := fmt.Sprintf("term-%d", s.terminalSeq)
	s.terminals[terminalID] = terminalEntry{sessionID: req.SessionId, sandboxdID: created.TerminalID}
	s.terminalsMu.Unlock()

	return &pb.CreateTerminalResponse{TerminalId: terminalID, Pid: created.PID}, nil
}

// TerminalWrite delivers raw bytes to the terminal input (no implicit newline).
func (s *Server) TerminalWrite(ctx context.Context, req *pb.TerminalWriteRequest) (*pb.TerminalWriteResponse, error) {
	podIP, sandboxdID, err := s.resolveTerminal(ctx, req.TerminalId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdPost(ctx, podIP, "/pty/input", map[string]any{
		"terminal_id": sandboxdID,
		"data":        string(req.Data),
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd pty input: %v", err)
	}
	defer resp.Body.Close()
	return &pb.TerminalWriteResponse{Ok: resp.StatusCode == http.StatusOK}, nil
}

// TerminalResize applies a new window size to the terminal.
func (s *Server) TerminalResize(ctx context.Context, req *pb.TerminalResizeRequest) (*pb.TerminalResizeResponse, error) {
	podIP, sandboxdID, err := s.resolveTerminal(ctx, req.TerminalId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdPost(ctx, podIP, "/pty/resize", map[string]any{
		"terminal_id": sandboxdID,
		"rows":        req.Rows,
		"cols":        req.Cols,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd pty resize: %v", err)
	}
	defer resp.Body.Close()
	return &pb.TerminalResizeResponse{Ok: resp.StatusCode == http.StatusOK}, nil
}

// TerminalForeground reports the current foreground process group.
func (s *Server) TerminalForeground(ctx context.Context, req *pb.TerminalForegroundRequest) (*pb.TerminalForegroundResponse, error) {
	podIP, sandboxdID, err := s.resolveTerminal(ctx, req.TerminalId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdGet(ctx, podIP, fmt.Sprintf("/pty/foreground?terminal_id=%d", sandboxdID))
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd pty foreground: %v", err)
	}
	defer resp.Body.Close()
	var fg struct {
		ProcessGroupID int32 `json:"process_group_id"`
		InputWaiting   bool  `json:"input_waiting"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&fg); err != nil {
		return nil, status.Errorf(codes.Internal, "sandboxd pty foreground decode: %v", err)
	}
	return &pb.TerminalForegroundResponse{ProcessGroupId: fg.ProcessGroupID, InputWaiting: fg.InputWaiting}, nil
}

// TerminalSignal delivers a signal to the terminal's foreground process group.
func (s *Server) TerminalSignal(ctx context.Context, req *pb.TerminalSignalRequest) (*pb.TerminalSignalResponse, error) {
	name, err := terminalSignalName(req.Signal)
	if err != nil {
		return nil, err
	}
	podIP, sandboxdID, err := s.resolveTerminal(ctx, req.TerminalId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdPost(ctx, podIP, "/pty/signal", map[string]any{
		"terminal_id": sandboxdID,
		"signal":      name,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd pty signal: %v", err)
	}
	defer resp.Body.Close()
	var out struct {
		ProcessGroupID int32 `json:"process_group_id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return nil, status.Errorf(codes.Internal, "sandboxd pty signal decode: %v", err)
	}
	return &pb.TerminalSignalResponse{ProcessGroupId: out.ProcessGroupID}, nil
}

// TerminalDestroy terminates the terminal session tree and drops its registry entry.
func (s *Server) TerminalDestroy(ctx context.Context, req *pb.TerminalDestroyRequest) (*pb.TerminalDestroyResponse, error) {
	podIP, sandboxdID, err := s.resolveTerminal(ctx, req.TerminalId)
	if err != nil {
		return nil, err
	}
	resp, err := sandboxdPost(ctx, podIP, "/pty/destroy", map[string]any{
		"terminal_id": sandboxdID,
		"grace_ms":    req.GraceMs,
	})
	if err != nil {
		return nil, status.Errorf(codes.Unavailable, "sandboxd pty destroy: %v", err)
	}
	defer resp.Body.Close()
	s.dropTerminal(req.TerminalId)
	return &pb.TerminalDestroyResponse{Ok: resp.StatusCode == http.StatusOK}, nil
}

// TerminalStream streams terminal output as data frames and closes with one
// exit frame. sandboxd emits SSE: a pid frame, base64 data frames, then a JSON
// exit frame.
func (s *Server) TerminalStream(req *pb.TerminalStreamRequest, stream pb.SandboxService_TerminalStreamServer) error {
	podIP, sandboxdID, err := s.resolveTerminal(stream.Context(), req.TerminalId)
	if err != nil {
		return err
	}
	resp, err := sandboxdGet(stream.Context(), podIP, fmt.Sprintf("/pty/stream?terminal_id=%d", sandboxdID))
	if err != nil {
		return status.Errorf(codes.Unavailable, "sandboxd pty stream: %v", err)
	}
	defer resp.Body.Close()

	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 256*1024)
	for scanner.Scan() {
		line := scanner.Text()
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		payload := strings.TrimPrefix(line, "data: ")

		frame, err := parsePTYStreamFrame(payload)
		if err != nil {
			return status.Errorf(codes.Internal, "pty stream frame: %v", err)
		}
		if frame.exit != nil {
			if err := stream.Send(&pb.TerminalStreamResponse{
				Frame: &pb.TerminalStreamResponse_Exit{Exit: frame.exit},
			}); err != nil {
				return err
			}
			return nil
		}
		if frame.data != nil {
			if err := stream.Send(&pb.TerminalStreamResponse{
				Frame: &pb.TerminalStreamResponse_Data{Data: frame.data},
			}); err != nil {
				return err
			}
		}
		// pid frame (both fields nil): nothing to forward
	}
	if err := scanner.Err(); err != nil {
		return status.Errorf(codes.Internal, "pty stream read: %v", err)
	}
	return nil
}

// ptyStreamFrame is one parsed /pty/stream SSE data payload.
type ptyStreamFrame struct {
	data []byte           // base64-decoded output (nil for control frames)
	exit *pb.TerminalExit // terminal exit frame (nil until the final frame)
}

// parsePTYStreamFrame decodes one sandboxd SSE data payload into a stream
// frame. JSON payloads carry control facts (pid, then exit); any other payload
// is a base64-encoded output chunk.
func parsePTYStreamFrame(payload string) (ptyStreamFrame, error) {
	if strings.HasPrefix(payload, "{") {
		var frame struct {
			PID    *int32 `json:"pid"`
			Exit   *int32 `json:"exit"`
			Signal string `json:"signal"`
		}
		if err := json.Unmarshal([]byte(payload), &frame); err != nil {
			return ptyStreamFrame{}, fmt.Errorf("frame decode: %w", err)
		}
		if frame.Exit != nil {
			return ptyStreamFrame{exit: &pb.TerminalExit{ExitCode: *frame.Exit, Signal: frame.Signal}}, nil
		}
		return ptyStreamFrame{}, nil // pid frame
	}
	decoded, err := base64.StdEncoding.DecodeString(payload)
	if err != nil {
		return ptyStreamFrame{}, fmt.Errorf("base64 decode: %w", err)
	}
	return ptyStreamFrame{data: decoded}, nil
}
