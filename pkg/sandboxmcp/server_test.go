package sandboxmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func newRequest(method string, args any) *http.Request {
	params := map[string]any{"_meta": map[string]any{metaPrefix + "protocolVersion": ProtocolVersion, metaPrefix + "clientCapabilities": map[string]any{}}}
	if method == "tools/call" {
		params["name"] = "exec"
		params["arguments"] = args
	}
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": "req-1", "method": method, "params": params})
	r := httptest.NewRequest("POST", "/mcp", strings.NewReader(string(body)))
	r.Header.Set("Content-Type", "application/json")
	r.Header.Set("Accept", "application/json, text/event-stream")
	r.Header.Set("MCP-Protocol-Version", ProtocolVersion)
	r.Header.Set("Mcp-Method", method)
	if method == "tools/call" {
		r.Header.Set("Mcp-Name", "exec")
	}
	r.Header.Set("Authorization", "alice")
	return r
}
func testServer(t *testing.T, call func(context.Context, Principal, json.RawMessage) (CallResult, error)) *Server {
	t.Helper()
	s, err := New(Config{Version: "test", AllowedOrigins: []string{"https://allowed.example"}, Authenticate: func(r *http.Request) (Principal, error) {
		id := r.Header.Get("Authorization")
		if id == "" {
			return Principal{}, errors.New("missing credential")
		}
		return Principal{ID: id}, nil
	}, Tools: []Tool{{Name: "exec", Description: "Execute", InputSchema: json.RawMessage(`{"type":"object"}`), Call: call}}})
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestRejectedRequestsNeverInvokeBackend(t *testing.T) {
	tests := []struct {
		name         string
		mutate       func(*http.Request)
		status, code int
	}{
		{"anonymous", func(r *http.Request) { r.Header.Del("Authorization") }, 401, -32000},
		{"origin", func(r *http.Request) { r.Header.Set("Origin", "https://evil.example") }, 403, -32600},
		{"null origin", func(r *http.Request) { r.Header.Set("Origin", "null") }, 403, -32600},
		{"duplicate origin", func(r *http.Request) {
			r.Header.Add("Origin", "https://allowed.example")
			r.Header.Add("Origin", "https://allowed.example")
		}, 403, -32600},
		{"wrong method header", func(r *http.Request) { r.Header.Set("Mcp-Method", "tools/list") }, 400, -32020},
		{"wrong tool header", func(r *http.Request) { r.Header.Set("Mcp-Name", "destroy") }, 400, -32020},
		{"duplicate version", func(r *http.Request) { r.Header.Add("MCP-Protocol-Version", ProtocolVersion) }, 400, -32020},
		{"missing capabilities", func(r *http.Request) {
			*r = *httptest.NewRequest("POST", "/mcp", strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{}}`))
			r.Header = newRequest("tools/call", nil).Header
		}, 400, -32602},
		{"wrong content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 415, -32600},
		{"missing accept", func(r *http.Request) { r.Header.Del("Accept") }, 406, -32600},
		{"null arguments", func(r *http.Request) { *r = *newRequest("tools/call", nil) }, 400, -32602},
		{"GET", func(r *http.Request) { r.Method = "GET" }, 405, -32600},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			called := false
			s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) {
				called = true
				return CallResult{}, nil
			})
			r := newRequest("tools/call", map[string]any{})
			tt.mutate(r)
			w := httptest.NewRecorder()
			s.ServeHTTP(w, r)
			var out struct{ Error rpcError }
			if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			if w.Code != tt.status || out.Error.Code != tt.code || called {
				t.Fatalf("status=%d body=%s called=%v", w.Code, w.Body, called)
			}
		})
	}
}

// rpcEnvelope is the subset of a JSON-RPC response these tests assert on.
type rpcEnvelope struct {
	ID     string         `json:"id"`
	Result map[string]any `json:"result"`
}

// assertCompleteResult verifies one stateless success response.
func assertCompleteResult(t *testing.T, method string, w *httptest.ResponseRecorder) {
	t.Helper()
	var out rpcEnvelope
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || out.ID != "req-1" || out.Result["resultType"] != "complete" {
		t.Fatalf("%s: %s", method, w.Body)
	}
	if w.Header().Get("Mcp-Session-Id") != "" {
		t.Error("protocol session created")
	}
	if method != "tools/call" && out.Result["cacheScope"] != "private" {
		t.Error("unsafe caching")
	}
	if method == "tools/call" && out.Result["isError"] != false {
		t.Error("program failure became protocol error")
	}
}

func TestDiscoveryAndCallWithoutHandshake(t *testing.T) {
	s := testServer(t, func(ctx context.Context, p Principal, _ json.RawMessage) (CallResult, error) {
		if p.ID != "alice" {
			t.Fatalf("principal=%v", p)
		}
		if _, ok := ctx.Deadline(); !ok {
			t.Error("missing deadline")
		}
		return CallResult{Content: []TextContent{{Type: "text", Text: "failed"}}, StructuredContent: map[string]any{"exit_code": 17}}, nil
	})
	for _, method := range []string{"server/discover", "tools/list", "tools/call"} {
		w := httptest.NewRecorder()
		s.ServeHTTP(w, newRequest(method, map[string]any{}))
		assertCompleteResult(t, method, w)
	}
}

func TestGatewayTerminatedHTTP(t *testing.T) {
	s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) {
		return CallResult{Content: []TextContent{{Type: "text", Text: "ready"}}}, nil
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", s)
	original := newRequest("server/discover", nil)
	body, err := io.ReadAll(original.Body)
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/mcp", bytes.NewReader(body))
	request.Host = "k8e-mcp.k8e-mcp.svc"
	if request.TLS != nil {
		t.Fatal("gateway backend request unexpectedly used TLS")
	}
	request.Header = original.Header.Clone()
	response := httptest.NewRecorder()
	mux.ServeHTTP(response, request)
	var result struct {
		Result struct {
			ResultType string `json:"resultType"`
		} `json:"result"`
	}
	if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK || result.Result.ResultType != "complete" {
		t.Fatalf("status=%d result=%+v", response.Code, result)
	}
}
func TestBackendErrorsAreSanitized(t *testing.T) {
	s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) {
		return CallResult{}, errors.New("secret-backend-credential")
	})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, newRequest("tools/call", map[string]any{}))
	if w.Code != 200 || strings.Contains(w.Body.String(), "secret-backend") || !strings.Contains(w.Body.String(), `"isError":true`) {
		t.Fatal(w.Body)
	}
}
func TestContextCancellationAndConcurrentIdentities(t *testing.T) {
	s := testServer(t, func(ctx context.Context, p Principal, args json.RawMessage) (CallResult, error) {
		if ctx.Err() == nil {
			t.Error("cancelled request context not propagated")
		}
		var in struct{ ID string }
		json.Unmarshal(args, &in)
		if in.ID != p.ID {
			t.Errorf("identity crossed requests: %s != %s", in.ID, p.ID)
		}
		return CallResult{}, ctx.Err()
	})
	var wg sync.WaitGroup
	for _, id := range []string{"alice", "bob"} {
		wg.Add(1)
		go func(id string) {
			defer wg.Done()
			r := newRequest("tools/call", map[string]any{"ID": id})
			r.Header.Set("Authorization", id)
			ctx, cancel := context.WithCancel(r.Context())
			cancel()
			s.ServeHTTP(httptest.NewRecorder(), r.WithContext(ctx))
		}(id)
	}
	wg.Wait()
}
func TestBodyLimitAndInvalidJSON(t *testing.T) {
	s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) {
		t.Error("unexpected call")
		return CallResult{}, nil
	})
	s.maxBytes = 8
	w := httptest.NewRecorder()
	s.ServeHTTP(w, newRequest("tools/call", map[string]any{}))
	if w.Code != 413 {
		t.Fatal(w.Code)
	}
	s.maxBytes = 1024
	for _, body := range []string{`{`, `[]`, `null`, `{"jsonrpc":"2.0","id":null,"method":"tools/list"}`, "\xff"} {
		r := httptest.NewRequest("POST", "/mcp", strings.NewReader(body))
		r.Header = newRequest("tools/list", nil).Header
		w := httptest.NewRecorder()
		s.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("%q: %d", body, w.Code)
		}
	}
}
func TestSchemaSnapshotAndConfig(t *testing.T) {
	if _, err := New(Config{}); err == nil {
		t.Fatal("anonymous server allowed")
	}
	s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) { return CallResult{}, nil })
	if s.timeout != 30*time.Second {
		t.Fatal("timeout default")
	}
}

// unsupportedVersionResponse mirrors the -32022 error payload asserted below.
type unsupportedVersionResponse struct {
	Error unsupportedVersionError `json:"error"`
}

type unsupportedVersionError struct {
	Code int                    `json:"code"`
	Data unsupportedVersionData `json:"data"`
}

type unsupportedVersionData struct {
	Supported []string `json:"supported"`
	Requested string   `json:"requested"`
}

func TestUnsupportedVersionResponse(t *testing.T) {
	s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) {
		t.Fatal("called")
		return CallResult{}, nil
	})
	r := newRequest("server/discover", nil)
	body := `{"jsonrpc":"2.0","id":1,"method":"server/discover","params":{"_meta":{"io.modelcontextprotocol/protocolVersion":"1900-01-01","io.modelcontextprotocol/clientCapabilities":{}}}}`
	r.Body = io.NopCloser(strings.NewReader(body))
	r.Header.Set("MCP-Protocol-Version", "1900-01-01")
	w := httptest.NewRecorder()
	s.ServeHTTP(w, r)
	var out unsupportedVersionResponse
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if w.Code != 400 || out.Error.Code != -32022 || len(out.Error.Data.Supported) != 1 || out.Error.Data.Supported[0] != ProtocolVersion || out.Error.Data.Requested != "1900-01-01" {
		t.Fatal(w.Body)
	}
}
func TestToolSchemaSnapshotAndStableOrder(t *testing.T) {
	schema := json.RawMessage(`{"type":"object"}`)
	call := func(context.Context, Principal, json.RawMessage) (CallResult, error) { return CallResult{}, nil }
	s, err := New(Config{Authenticate: func(*http.Request) (Principal, error) { return Principal{ID: "a"}, nil }, Tools: []Tool{
		{Name: "z", InputSchema: schema, Call: call}, {Name: "a", InputSchema: schema, Call: call},
	}})
	if err != nil {
		t.Fatal(err)
	}
	schema[0] = '!'
	w := httptest.NewRecorder()
	s.ServeHTTP(w, newRequest("tools/list", nil))
	var out struct{ Result struct{ Tools []Tool } }
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Result.Tools) != 2 || out.Result.Tools[0].Name != "a" || out.Result.Tools[1].Name != "z" {
		t.Fatal(w.Body)
	}
}
func TestInvalidParamsAndUnknownMethod(t *testing.T) {
	s := testServer(t, func(context.Context, Principal, json.RawMessage) (CallResult, error) {
		return CallResult{}, &InvalidParams{Message: "session_id required"}
	})
	w := httptest.NewRecorder()
	s.ServeHTTP(w, newRequest("tools/call", map[string]any{}))
	if w.Code != 400 || !strings.Contains(w.Body.String(), `"code":-32602`) {
		t.Fatal(w.Body)
	}
	w = httptest.NewRecorder()
	s.ServeHTTP(w, newRequest("tasks/get", nil))
	if w.Code != 404 || !strings.Contains(w.Body.String(), `"code":-32601`) {
		t.Fatal(w.Body)
	}
}
