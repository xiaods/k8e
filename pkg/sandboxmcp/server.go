package sandboxmcp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"time"
)

// Server is the MCP stdio server.
type Server struct {
	client        *Client
	mu            sync.Mutex
	defaultSessID string // reused across the conversation
	tenantID      string // if set, enables cross-process session reuse
}

func NewServer(client *Client) *Server { return &Server{client: client} }

func NewServerWithTenant(client *Client, tenantID string) *Server {
	return &Server{client: client, tenantID: tenantID}
}

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
// This is the legacy stdio transport — use RunSSE for long-lived connections.
func (s *Server) Run(ctx context.Context) error {
	enc := json.NewEncoder(os.Stdout)
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024) // 1MB — handles large file content in requests
	// destroy default session on exit — skip if tenant reuse is enabled
	defer func() {
		s.mu.Lock()
		sid := s.defaultSessID
		tenant := s.tenantID
		s.mu.Unlock()
		if sid != "" && tenant == "" {
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
// If tenantID is set, first tries to find an existing Active session for that tenant.
func (s *Server) defaultSession(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.defaultSessID != "" {
		return s.defaultSessID, nil
	}
	// cross-process reuse: look for existing tenant session
	if s.tenantID != "" {
		if sid, err := FindActiveSession(s.tenantID); err == nil && sid != "" {
			s.defaultSessID = sid
			return sid, nil
		}
	}
	resp, err := s.client.SandboxServiceClient.CreateSession(ctx, newCreateReq(s.tenantID))
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

// ── SSE / HTTP transport (MCP Streamable HTTP, spec 2025-03-26) ────────────
//
// Architecture:
//
//	GET  /mcp          → open SSE stream (server → client push)
//	POST /mcp          → send JSON-RPC request, get JSON response
//	                     header Mcp-Session-Id ties requests to an SSE stream
//
// One HTTP server is shared across all agent connections — no per-request
// process spawn, no initialize handshake per call.

// sseSession holds the SSE event channel for one connected agent.
type sseSession struct {
	ch     chan string
	cancel context.CancelFunc
}

// SSEServer is the HTTP/SSE MCP server. Create via NewSSEServer.
type SSEServer struct {
	*Server
	mu       sync.Mutex
	sessions map[string]*sseSession
}

func NewSSEServer(client *Client, tenantID string) *SSEServer {
	return &SSEServer{
		Server:   NewServerWithTenant(client, tenantID),
		sessions: make(map[string]*sseSession),
	}
}

// RunSSE starts the HTTP server on addr (e.g. ":8811").
// GET /mcp  → SSE stream
// POST /mcp → JSON-RPC, response in HTTP body + optional SSE push
func (s *SSEServer) RunSSE(ctx context.Context, addr string) error {
	mux := http.NewServeMux()
	mux.HandleFunc("/mcp", s.handler)

	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}
	go func() {
		<-ctx.Done()
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutCtx) //nolint:errcheck
	}()

	if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
		return err
	}
	return nil
}

func (s *SSEServer) handler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.handleSSE(w, r)
	case http.MethodPost:
		s.handlePost(w, r)
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

// handleSSE opens a long-lived SSE stream for one agent session.
// The client receives the assigned session ID as the first SSE event,
// then uses it in Mcp-Session-Id on subsequent POST requests.
func (s *SSEServer) handleSSE(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		http.Error(w, "streaming not supported", http.StatusInternalServerError)
		return
	}

	sid := newSessionToken()
	ch := make(chan string, 32)
	sessCtx, cancel := context.WithCancel(r.Context())

	s.mu.Lock()
	s.sessions[sid] = &sseSession{ch: ch, cancel: cancel}
	s.mu.Unlock()

	defer func() {
		cancel()
		s.mu.Lock()
		delete(s.sessions, sid)
		s.mu.Unlock()
	}()

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)

	// send session ID so client knows which header to use
	fmt.Fprintf(w, "event: session\ndata: %s\n\n", sid)
	flusher.Flush()

	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()

	for {
		select {
		case <-sessCtx.Done():
			return
		case msg := <-ch:
			fmt.Fprintf(w, "data: %s\n\n", msg)
			flusher.Flush()
		case <-ticker.C:
			fmt.Fprintf(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}

// handlePost processes a JSON-RPC request and returns the response in the HTTP body.
// If the client has an open SSE stream (Mcp-Session-Id header), the response is
// also pushed over SSE so the agent can receive it without polling.
func (s *SSEServer) handlePost(w http.ResponseWriter, r *http.Request) {
	var req rpcRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errResp(nil, -32700, "parse error"))
		return
	}

	resp := s.dispatch(r.Context(), &req)

	// push to SSE stream if session is open
	if sid := r.Header.Get("Mcp-Session-Id"); sid != "" {
		if data, err := json.Marshal(resp); err == nil {
			s.mu.Lock()
			sess, ok := s.sessions[sid]
			s.mu.Unlock()
			if ok {
				select {
				case sess.ch <- string(data):
				default: // drop if buffer full — client will read from HTTP body
				}
			}
		}
	}

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

func newSessionToken() string {
	b := make([]byte, 12)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(b)
}
