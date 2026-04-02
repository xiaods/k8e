package sandboxmcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

// Server is the MCP stdio server.
type Server struct {
	client *Client
}

func NewServer(client *Client) *Server { return &Server{client: client} }

type rpcRequest struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      any             `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params,omitempty"`
}

type rpcResponse struct {
	JSONRPC string `json:"jsonrpc"`
	ID      any    `json:"id,omitempty"`
	Result  any    `json:"result,omitempty"`
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
		enc.Encode(resp)
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
		return rpcResponse{} // no response needed for notifications
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
			result, err := t.Handler(ctx, s.client, args)
			if err != nil {
				return err.Error(), true
			}
			return result, false
		}
	}
	return fmt.Sprintf("unknown tool: %s", name), true
}

func errResp(id any, code int, msg string) rpcResponse {
	return rpcResponse{JSONRPC: "2.0", ID: id, Error: &rpcError{Code: code, Message: msg}}
}

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

// discardWriter is used to suppress output for notification responses.
var _ io.Writer = (*discardWriter)(nil)

type discardWriter struct{}

func (discardWriter) Write(p []byte) (int, error) { return len(p), nil }
