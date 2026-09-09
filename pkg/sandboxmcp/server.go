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

// ProtocolVersion is the MCP revision implemented by this HTTP boundary.
const ProtocolVersion = "2026-07-28"
const metaPrefix = "io.modelcontextprotocol/"

// Principal is established by authentication, never by tool arguments.
type Principal struct{ ID string }

// Authenticate validates an HTTP request and returns its stable caller identity.
type Authenticate func(*http.Request) (Principal, error)

// TextContent is a plain-text MCP content item.
type TextContent struct {
	// Type is the MCP content discriminator and is normally "text".
	Type string `json:"type"`
	// Text contains the public-safe tool response.
	Text string `json:"text"`
}

// CallResult is the result returned by an MCP tool handler.
type CallResult struct {
	// Content is the human-readable MCP result content.
	Content []TextContent `json:"content"`
	// StructuredContent carries the machine-readable result when available.
	StructuredContent any `json:"structuredContent,omitempty"`
	// IsError reports a tool-level failure inside a successful JSON-RPC response.
	IsError bool `json:"isError,omitempty"`
}

// Tool handlers must validate arguments and enforce principal ownership before
// side effects. InputSchema is descriptive; the protocol boundary is not a
// general-purpose JSON Schema interpreter.
type Tool struct {
	// Name is the stable identifier clients use with tools/call.
	Name string `json:"name"`
	// Description explains the operation and its retry behavior.
	Description string `json:"description"`
	// InputSchema describes the accepted JSON arguments.
	InputSchema json.RawMessage `json:"inputSchema"`
	// Call executes the tool for an authenticated principal.
	Call func(context.Context, Principal, json.RawMessage) (CallResult, error) `json:"-"`
}

// InvalidParams is a public-safe argument validation failure from a handler.
// Other errors are deliberately hidden to avoid leaking backend internals.
type InvalidParams struct{ Message string }

// Error returns the public-safe validation message.
func (e *InvalidParams) Error() string { return e.Message }

// Config defines the authenticated MCP handler and its transport limits.
type Config struct {
	// Authenticate is required and runs for every request.
	Authenticate Authenticate
	// Tools is snapshotted and sorted when the server is constructed.
	Tools []Tool
	// AllowedOrigins lists explicit browser origins accepted by the handler.
	AllowedOrigins []string
	// Version identifies the server implementation in MCP metadata.
	Version string
	// MaxRequestBytes caps the JSON request body size.
	MaxRequestBytes int64
	// Timeout caps each tool invocation.
	Timeout time.Duration
}

// Server is an authenticated MCP HTTP handler with immutable tool schemas.
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
	if err := normalizeConfig(&c); err != nil {
		return nil, err
	}
	tools, err := snapshotTools(c.Tools)
	if err != nil {
		return nil, err
	}
	origins, err := explicitOrigins(c.AllowedOrigins)
	if err != nil {
		return nil, err
	}
	return &Server{auth: c.Authenticate, tools: tools, origins: origins, version: c.Version, maxBytes: c.MaxRequestBytes, timeout: c.Timeout}, nil
}

func normalizeConfig(c *Config) error {
	if c.Authenticate == nil {
		return errors.New("MCP authentication is required")
	}
	if c.MaxRequestBytes == 0 {
		c.MaxRequestBytes = 1 << 20
	}
	if c.Timeout == 0 {
		c.Timeout = 30 * time.Second
	}
	if c.MaxRequestBytes < 1 || c.Timeout < 0 {
		return errors.New("invalid MCP limits")
	}
	return nil
}

func snapshotTools(configured []Tool) ([]Tool, error) {
	var tools []Tool
	seen := map[string]bool{}
	for _, t := range configured {
		if t.Name == "" || seen[t.Name] || t.Call == nil {
			return nil, fmt.Errorf("invalid or duplicate MCP tool %q", t.Name)
		}
		var schema map[string]any
		if err := json.Unmarshal(t.InputSchema, &schema); err != nil || schema == nil {
			return nil, fmt.Errorf("invalid schema for %q", t.Name)
		}
		t.InputSchema = append(json.RawMessage(nil), t.InputSchema...)
		seen[t.Name] = true
		tools = append(tools, t)
	}
	sort.Slice(tools, func(i, j int) bool { return tools[i].Name < tools[j].Name })
	return tools, nil
}

func explicitOrigins(configured []string) (map[string]bool, error) {
	origins := map[string]bool{}
	for _, o := range configured {
		if o == "" || o == "*" || o == "null" {
			return nil, errors.New("explicit origins required")
		}
		origins[o] = true
	}
	return origins, nil
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

func (s *Server) validateTransport(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		fail(w, 405, nil, -32600, "Only POST is supported")
		return false
	}
	if origins := r.Header.Values("Origin"); len(origins) > 0 && (len(origins) != 1 || !s.origins[origins[0]]) {
		fail(w, 403, nil, -32600, "Origin rejected")
		return false
	}
	return true
}

func validateMediaHeaders(w http.ResponseWriter, r *http.Request) bool {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" {
		fail(w, 415, nil, -32600, "Content-Type must be application/json")
		return false
	}
	if !accepts(r, "application/json") || !accepts(r, "text/event-stream") {
		fail(w, 406, nil, -32600, "Accept must include application/json and text/event-stream")
		return false
	}
	return true
}

func (s *Server) authenticateRequest(w http.ResponseWriter, r *http.Request) (Principal, bool) {
	principal, err := s.auth(r)
	if err == nil && principal.ID != "" {
		return principal, true
	}
	status := http.StatusUnauthorized
	var authErr *authenticationError
	if errors.As(err, &authErr) {
		status = authErr.status
		if authErr.challenge != "" {
			w.Header().Set("WWW-Authenticate", authErr.challenge)
		}
	}
	fail(w, status, nil, -32000, "Authentication required")
	return Principal{}, false
}

func (s *Server) decodeRequest(w http.ResponseWriter, r *http.Request) (request, map[string]json.RawMessage, bool) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, s.maxBytes))
	if err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			fail(w, 413, nil, -32600, "Request too large")
		} else {
			fail(w, 400, nil, -32700, "Cannot read request")
		}
		return request{}, nil, false
	}
	if !utf8.Valid(body) || !json.Valid(body) {
		fail(w, 400, nil, -32700, "Invalid JSON")
		return request{}, nil, false
	}
	var req request
	if err := json.Unmarshal(body, &req); err != nil || req.JSONRPC != "2.0" || req.Method == "" || !validID(req.ID) {
		fail(w, 400, nil, -32600, "Invalid request")
		return request{}, nil, false
	}
	var params map[string]json.RawMessage
	if json.Unmarshal(req.Params, &params) != nil || params == nil {
		fail(w, 400, req.ID, -32602, "Object params required")
		return request{}, nil, false
	}
	return req, params, true
}

func validateRequestMetadata(w http.ResponseWriter, r *http.Request, req request, params map[string]json.RawMessage) bool {
	var meta map[string]json.RawMessage
	if json.Unmarshal(params["_meta"], &meta) != nil || meta == nil {
		fail(w, 400, req.ID, -32602, "Request metadata required")
		return false
	}
	var version string
	if json.Unmarshal(meta[metaPrefix+"protocolVersion"], &version) != nil || version == "" {
		fail(w, 400, req.ID, -32602, "Version and client capabilities required")
		return false
	}
	var capabilities map[string]json.RawMessage
	if json.Unmarshal(meta[metaPrefix+"clientCapabilities"], &capabilities) != nil || capabilities == nil {
		fail(w, 400, req.ID, -32602, "Version and client capabilities required")
		return false
	}
	if !matchesHeader(r, "MCP-Protocol-Version", version) || !matchesHeader(r, "Mcp-Method", req.Method) {
		fail(w, 400, req.ID, -32020, "Request header mismatch")
		return false
	}
	if version != ProtocolVersion {
		reply(w, 400, req.ID, nil, &rpcError{Code: -32022, Message: "Unsupported protocol version", Data: map[string]any{"supported": []string{ProtocolVersion}, "requested": version}})
		return false
	}
	return true
}

// ServeHTTP validates the MCP transport envelope and dispatches one JSON-RPC request.
func (s *Server) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if !s.validateTransport(w, r) {
		return
	}
	principal, ok := s.authenticateRequest(w, r)
	if !ok {
		return
	}
	if !validateMediaHeaders(w, r) {
		return
	}
	req, params, ok := s.decodeRequest(w, r)
	if !ok || !validateRequestMetadata(w, r, req, params) {
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
		name, args, valid := toolCallArguments(w, r, req.ID, params)
		if !valid {
			return
		}
		s.invokeTool(w, r, principal, req.ID, name, args, info)
	default:
		fail(w, 404, req.ID, -32601, "Method not found")
	}
}

func (s *Server) invokeTool(w http.ResponseWriter, r *http.Request, principal Principal, id json.RawMessage, name string, args json.RawMessage, info map[string]any) {
	for _, tool := range s.tools {
		if tool.Name != name {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), s.timeout)
		defer cancel()
		result, err := tool.Call(ctx, principal, args)
		if err != nil {
			var invalid *InvalidParams
			if errors.As(err, &invalid) {
				fail(w, 400, id, -32602, invalid.Message)
				return
			}
			result = CallResult{IsError: true, Content: []TextContent{{Type: "text", Text: "Sandbox operation failed"}}}
		}
		if result.Content == nil {
			result.Content = []TextContent{}
		}
		reply(w, 200, id, map[string]any{"resultType": "complete", "content": result.Content, "structuredContent": result.StructuredContent, "isError": result.IsError, "_meta": info}, nil)
		return
	}
	fail(w, 400, id, -32602, "Unknown tool")
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

func toolCallArguments(w http.ResponseWriter, r *http.Request, id json.RawMessage, params map[string]json.RawMessage) (string, json.RawMessage, bool) {
	var name string
	if json.Unmarshal(params["name"], &name) != nil || name == "" {
		fail(w, 400, id, -32602, "Tool name required")
		return "", nil, false
	}
	if !matchesHeader(r, "Mcp-Name", name) {
		fail(w, 400, id, -32020, "Tool header mismatch")
		return "", nil, false
	}
	args := params["arguments"]
	if len(args) == 0 {
		args = json.RawMessage(`{}`)
	}
	var object map[string]json.RawMessage
	if json.Unmarshal(args, &object) != nil || object == nil {
		fail(w, 400, id, -32602, "Object arguments required")
		return "", nil, false
	}
	return name, args, true
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
