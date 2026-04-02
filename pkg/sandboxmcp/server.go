package sandboxmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sync"
)

// Server is the MCP stdio server.
type Server struct {
	client        *Client
	mu            sync.Mutex
	defaultSessID string // reused across the conversation
}

func NewServer(client *Client) *Server { return &Server{client: client} }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string    `json:"jsonrpc"`
	ID      any       `json:"id,omitempty"`
	Result  any       `json:"result,omitempty"`
	Error   *rpcError `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Run reads JSON-RPC requests from stdin and writes responses to stdout until ctx is done or EOF.
func (s *Server) Run(ctx context.Context) error {
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	// destroy default session on exit
	defer func() {
		s.mu.Lock()
		sid := s.defaultSessID
		s.mu.Unlock()
		if sid != "" {
			s.client.SandboxServiceClient.DestroySession(context.Background(), destroyReq(sid)) //nolint:errcheck
		}
	}()

	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if !scanner.Scan() {
			return scanner.Err()
		}
		var req rpcRequest
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			enc.Encode(errResp(nil, -32700, "parse error"))
			continue
		}
		resp := s.dispatch(ctx, &req)
		if resp.JSONRPC != "" { // skip notifications (no response)
			enc.Encode(resp)
		}
	}
}

func (s *Server) dispatch(ctx context.Context, req *rpcRequest) rpcResponse {
	switch req.Method {
	case "initialize":
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"protocolVersion": "2024-11-05",
			"serverInfo":      map[string]any{"name": "k8e-sandbox", "version": "1.0.0"},
			"capabilities":    map[string]any{"tools": map[string]any{}},
		}}
	case "notifications/initialized":
		return rpcResponse{} // no response for notifications
	case "tools/list":
		tools := make([]map[string]any, len(allTools))
		for i, t := range allTools {
			tools[i] = map[string]any{
				"name":        t.Name,
				"description": t.Description,
				"inputSchema": t.InputSchema,
			}
		}
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{"tools": tools}}
	case "tools/call":
		var p struct {
			Name      string         `json:"name"`
			Arguments map[string]any `json:"arguments"`
		}
		if err := json.Unmarshal(req.Params, &p); err != nil {
			return errResp(req.ID, -32602, "invalid params")
		}
		result, isErr := s.callTool(ctx, p.Name, p.Arguments)
		return rpcResponse{JSONRPC: "2.0", ID: req.ID, Result: map[string]any{
			"content": []map[string]any{{"type": "text", "text": result}},
			"isError": isErr,
		}}
	default:
		return errResp(req.ID, -32601, fmt.Sprintf("method not found: %s", req.Method))
	}
}

func (s *Server) callTool(ctx context.Context, name string, args map[string]any) (string, bool) {
	for _, t := range allTools {
		if t.Name == name {
			result, err := t.Handler(ctx, s, args)
			if err != nil {
				return friendlyError(err), true
			}
			return result, false
		}
	}
	return fmt.Sprintf("unknown tool: %s", name), true
}

// defaultSession returns the reused session ID, creating one lazily if needed.
func (s *Server) defaultSession(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.defaultSessID != "" {
		return s.defaultSessID, nil
	}
	resp, err := s.client.SandboxServiceClient.CreateSession(ctx, &createReqDefault)
	if err != nil {
		return "", fmt.Errorf("sandbox not available: %w", err)
	}
	s.defaultSessID = resp.SessionId
	return s.defaultSessID, nil
}

func friendlyError(err error) string {
	msg := err.Error()
	if contains(msg, "connection refused") || contains(msg, "unavailable") {
		return "K8E sandbox service is not reachable. Is k8e running? Check: systemctl status k8e"
	}
	if contains(msg, "not found") {
		return "Sandbox session not found. It may have expired — use sandbox_create_session to start a new one."
	}
	if contains(msg, "permission denied") || contains(msg, "PermissionDenied") {
		return "Permission denied. Check K8E_SANDBOX_CERT or TLS configuration."
	}
	return msg
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(s) > 0 && containsStr(s, sub))
}

func containsStr(s, sub string) bool {
	for i := 0; i <= len(s)-len(sub); i++ {
		if s[i:i+len(sub)] == sub {
			return true
		}
	}
	return false
}

func errResp(id any, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

// helpers shared with tools.go
func str(m map[string]any, k string) string {
	if v, ok := m[k].(string); ok {
		return v
	}
	return ""
}

func int32val(m map[string]any, k string, def int32) int32 {
	switch v := m[k].(type) {
	case float64:
		return int32(v)
	case int32:
		return v
	}
	return def
}

func strs(m map[string]any, k string) []string {
	raw, ok := m[k].([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func int64val(m map[string]any, k string) int64 {
	if v, ok := m[k].(float64); ok {
		return int64(v)
	}
	return 0
}

// suppress unused interface warning
var _ io.Writer = (*discardWriter)(nil)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
