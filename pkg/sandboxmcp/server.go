// Package sandboxmcp implements the MCP 2026-07-28 HTTP boundary. It does not
// authenticate users or own sandbox state: both are explicit dependencies.
package sandboxmcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

const ProtocolVersion = "2026-07-28"
const metaPrefix = "io.modelcontextprotocol/"

// Principal is established by authentication, never by tool arguments.
type Principal struct{ ID string }
type Authenticate func(*http.Request) (Principal, error)

type TextContent struct {
	Type string `json:"type"`
	Text string `json:"text"`
}
type CallResult struct {
	Content           []TextContent `json:"content"`
	StructuredContent any           `json:"structuredContent,omitempty"`
	IsError           bool          `json:"isError,omitempty"`
}

// Tool handlers must validate arguments and enforce principal ownership before
// side effects. InputSchema is descriptive; the protocol boundary is not a
// general-purpose JSON Schema interpreter.
type Tool struct {
	Name        string                                                                `json:"name"`
	Description string                                                                `json:"description"`
	InputSchema json.RawMessage                                                       `json:"inputSchema"`
	Call        func(context.Context, Principal, json.RawMessage) (CallResult, error) `json:"-"`
}

// InvalidParams is a public-safe argument validation failure from a handler.
// Other errors are deliberately hidden to avoid leaking backend internals.
type InvalidParams struct{ Message string }

func (e *InvalidParams) Error() string { return e.Message }

type Config struct {
	Authenticate    Authenticate
	Tools           []Tool
	AllowedOrigins  []string
	Version         string
	MaxRequestBytes int64
	Timeout         time.Duration
}

type Server struct {
	auth     Authenticate
	tools    []Tool
	origins  map[string]bool
	version  string
	maxBytes int64
	timeout  time.Duration
}

// New validates configuration and snapshots tool schemas. Mount the returned
// handler at /mcp on an HTTPS server with transport timeouts and request limits.
// No listener is started, and there is no anonymous fallback.
func New(c Config) (*Server, error) {
	if c.Authenticate == nil {
		return nil, errors.New("MCP authentication is required")
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 1 << 20
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxRequestBytes < 1 || c.Timeout < 0 {
		return nil, errors.New("invalid MCP limits")
	}
	s := &Server{auth: c.Authenticate, version: c.Version, maxBytes: c.MaxRequestBytes, timeout: c.Timeout, origins: map[string]bool{}}
	seen := map[string]bool{}
	for _, t := range c.Tools {
		if t.Name == "" || seen[t.Name] || t.Call == nil {
			return nil, fmt.Errorf("invalid or duplicate MCP tool %q", t.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema == nil {
			return nil, fmt.Errorf("invalid schema for %q", t.Name)
		}
		t.InputSchema = append(json.RawMessage(nil), t.InputSchema...)
		seen[t.Name] = true
		s.tools = append(s.tools, t)
	}
	sort.Slice(s.tools, func(i, j int) bool { return s.tools[i].Name < s.tools[j].Name })
	for _, o := range c.AllowedOrigins {
		if o == "" || o == "*" || o == "null" {
			return nil, errors.New("explicit origins required")
		}
		s.origins[o] = true
	}
	return s, nil
}

type request struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Method  string          `json:"method"`
	Params  json.RawMessage `json:"params"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

func reply(w http.ResponseWriter, status int, id json.RawMessage, result any, err *rpcError) {
	if len(id) == 0 {
		id = json.RawMessage("null")
	}
	body := map[string]any{"jsonrpc": "2.0", "id": id}
	if err != nil {
		body["error"] = err
	} else {
		body["result"] = result
	}
	data, encodeErr := json.Marshal(body)
	if encodeErr != nil {
		status = http.StatusInternalServerError
		data = []byte(`{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"Response encoding failed"}}`)
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_, _ = w.Write(data)
}
func fail(w http.ResponseWriter, status int, id json.RawMessage, code int, msg string) {
	reply(w, status, id, nil, &rpcError{Code: code, Message: msg})
}

func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, nil, -32600, "Only POST is supported")
		return
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || !s.origins[origins[0]]) {
		fail(w, 403, nil, -32600, "Origin rejected")
		return
	}
	principal, err := s.auth(r)
	if err != nil || principal.ID == "" {
		status := http.StatusUnauthorized
		var authErr *authenticationError
		if errors.As(err, &authErr) {
			status = authErr.status
			if authErr.challenge != "" {
				w.Header().Set("WWW-Authenticate", authErr.challenge)
			}
		}
		fail(w, status, nil, -32000, "Authentication required")
		return
	}
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 415, nil, -32600, "Content-Type must be application/json")
		return
	}
	if !accepts(r, "application/json") || !accepts(r, "text/event-stream") {
		fail(w, 406, nil, -32600, "Accept must include application/json and text/event-stream")
		return
	}
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, nil, -32600, "Request too large")
		} else {
			fail(w, 400, nil, -32700, "Cannot read request")
		}
		return
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		fail(w, 400, nil, -32700, "Invalid JSON")
		return
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
		fail(w, 400, nil, -32600, "Invalid request")
		return
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(req.Params, &params) != nil || params == nil {
		fail(w, 400, req.ID, -32602, "Object params required")
		return
	}
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil || meta == nil {
		fail(w, 400, req.ID, -32602, "Request metadata required")
		return
	}
	var version string
	var capabilities map[string]json.RawMessage
	if json.Unmarshal(meta[metaPrefix+"protocolVersion"], &version) != nil || version == "" || json.Unmarshal(meta[metaPrefix+"clientCapabilities"], &capabilities) != nil || capabilities == nil {
		fail(w, 400, req.ID, -32602, "Version and client capabilities required")
		return
	}
	if !matchesHeader(r, "MCP-Protocol-Version", version) || !matchesHeader(r, "Mcp-Method", req.Method) {
		fail(w, 400, req.ID, -32020, "Request header mismatch")
		return
	}
	if version != ProtocolVersion {
		reply(w, 400, req.ID, nil, &rpcError{Code: -32022, Message: "Unsupported protocol version", Data: map[string]any{"supported": []string{ProtocolVersion}, "requested": version}})
		return
	}
	info := map[string]any{metaPrefix + "serverInfo": map[string]string{"name": "k8e-sandbox", "version": s.version}}
	switch req.Method {
	case "server/discover":
		reply(w, 200, req.ID, map[string]any{"resultType": "complete", "supportedVersions": []string{ProtocolVersion}, "capabilities": map[string]any{"tools": map[string]any{}}, "_meta": info, "ttlMs": 0, "cacheScope": "private"}, nil)
	case "tools/list":
		if cursor, ok := params["cursor"]; ok && string(cursor) != `""` {
			fail(w, 400, req.ID, -32602, "Unsupported cursor")
			return
		}
		tools := s.tools
		if tools == nil {
			tools = []Tool{}
		}
		reply(w, 200, req.ID, map[string]any{"resultType": "complete", "tools": tools, "ttlMs": 0, "cacheScope": "private", "_meta": info}, nil)
	case "tools/call":
		var name string
		if json.Unmarshal(params["name"], &name) != nil || name == "" {
			fail(w, 400, req.ID, -32602, "Tool name required")
			return
		}
		if !matchesHeader(r, "Mcp-Name", name) {
			fail(w, 400, req.ID, -32020, "Tool header mismatch")
			return
		}
		args := params["arguments"]
		if len(args) == 0 {
			args = json.RawMessage(`{}`)
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(args, &object) != nil || object == nil {
			fail(w, 400, req.ID, -32602, "Object arguments required")
			return
		}
		for _, t := range s.tools {
			if t.Name == name {
				ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
				defer cancel()
				result, err := t.Call(ctx, principal, args)
				if err != nil {
					var invalid *InvalidParams
					if errors.As(err, &invalid) {
						fail(w, 400, req.ID, -32602, invalid.Message)
						return
					}
					result = CallResult{IsError: true, Content: []TextContent{{Type: "text", Text: "Sandbox operation failed"}}}
				}
				if result.Content == nil {
					result.Content = []TextContent{}
				}
				reply(w, 200, req.ID, map[string]any{"resultType": "complete", "content": result.Content, "structuredContent": result.StructuredContent, "isError": result.IsError, "_meta": info}, nil)
				return
			}
		}
		fail(w, 400, req.ID, -32602, "Unknown tool")
	default:
		fail(w, 404, req.ID, -32601, "Method not found")
	}
}

func validID(id json.RawMessage) bool {
	if len(id) == 0 || string(id) == "null" {
		return false
	}
	var v any
	if json.Unmarshal(id, &v) != nil {
		return false
	}
	switch v.(type) {
	case string, float64:
		return true
	}
	return false
}
func matchesHeader(r *http.Request, key, want string) bool {
	values := r.Header.Values(key)
	if len(values) != 1 {
		return false
	}
	value := values[0]
	if strings.HasPrefix(value, "=?base64?") && strings.HasSuffix(value, "?=") {
		decoded, err := base64.StdEncoding.DecodeString(strings.TrimSuffix(strings.TrimPrefix(value, "=?base64?"), "?="))
		if err != nil || !utf8.Valid(decoded) {
			return false
		}
		value = string(decoded)
	}
	return value == want
}
func accepts(r *http.Request, want string) bool {
	for _, line := range r.Header.Values("Accept") {
		for _, part := range strings.Split(line, ",") {
			typ, p, err := mime.ParseMediaType(strings.TrimSpace(part))
			quality := 1.0
			if raw, ok := p["q"]; ok {
				var qerr error
				quality, qerr = strconv.ParseFloat(raw, 64)
				if qerr != nil {
					continue
				}
			}
			if err == nil && typ == want && quality > 0 && quality <= 1 {
				return true
			}
		}
	}
	return false
}
