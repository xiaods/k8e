package e2b

import (
	"bufio"
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// ptySetup creates a sandbox and one PTY session via the SDK's pty.create
// shape. The terminal stream hangs (long-running session), so the pid -
// > terminal_id bridge stays alive for SendInput/Update/SendSignal tests.
func ptySetup(t *testing.T, s *Server, ts *httptest.Server) (int, string) {
	t.Helper()
	gw := s.gw.(*fakeGateway)
	gw.mu.Lock()
	gw.hangTerminals = true
	gw.mu.Unlock()
	id := createSandboxID(t, ts)

	msg := map[string]any{
		"process": map[string]any{"cmd": "/bin/bash", "args": []string{"-i", "-l"}},
		"pty":     map[string]any{"size": map[string]int{"cols": 80, "rows": 24}},
	}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Start", bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// Read only the first (start) frame; the stream stays open (hanging)
	// so the pty row is not dropped.
	f, err := readOneEnvelope(bufio.NewReader(resp.Body))
	if err != nil {
		t.Fatalf("read start frame: %v", err)
	}
	ev, ok := f.json["event"].(map[string]any)
	if !ok {
		t.Fatalf("first frame must be an event, got %v", f.json)
	}
	start, ok := ev["start"].(map[string]any)
	if !ok {
		t.Fatalf("first frame must be start, got %v", f.json)
	}
	return int(start["pid"].(float64)), id
}

// readOneEnvelope reads a single connect envelope frame from a stream.
func readOneEnvelope(br *bufio.Reader) (frame, error) {
	header := make([]byte, 5)
	if _, err := io.ReadFull(br, header); err != nil {
		return frame{}, err
	}
	length := int(binary.BigEndian.Uint32(header[1:]))
	body := make([]byte, length)
	if _, err := io.ReadFull(br, body); err != nil {
		return frame{}, err
	}
	var payload map[string]any
	if length > 0 {
		if err := json.Unmarshal(body, &payload); err != nil {
			return frame{}, err
		}
	}
	return frame{flags: int(header[0]), json: payload}, nil
}

// ptyRequest posts a unary envd RPC with the sandbox headers and returns
// the status code.
func ptyRequest(t *testing.T, ts *httptest.Server, s *Server, sid, path string, body any) int {
	t.Helper()
	payload, _ := json.Marshal(body)
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd"+path, bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range envdHeaders(t, s, sid) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.StatusCode
}

func TestEnvdPtySendInput(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	pid, sid := ptySetup(t, s, ts)

	// SDK sendInput selector shape: input routed to TerminalWrite.
	code := ptyRequest(t, ts, s, sid, "/process.Process/SendInput", map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"input":   map[string]any{"input": map[string]any{"case": "pty", "value": base64.StdEncoding.EncodeToString([]byte("ls\r"))}},
	})
	if code != 200 {
		t.Fatalf("sendInput: want 200, got %d", code)
	}
	if len(gw.term.writes) != 1 {
		t.Fatalf("expected 1 TerminalWrite, got %d", len(gw.term.writes))
	}
	if string(gw.term.writes[0].Data) != "ls\r" {
		t.Fatalf("TerminalWrite data = %q, want %q", gw.term.writes[0].Data, "ls\r")
	}
}

func TestEnvdPtyResize(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	pid, sid := ptySetup(t, s, ts)

	// SDK resize shape: process.Process/Update with pty.size.
	code := ptyRequest(t, ts, s, sid, "/process.Process/Update", map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"pty":     map[string]any{"size": map[string]int{"cols": 132, "rows": 43}},
	})
	if code != 200 {
		t.Fatalf("resize: want 200, got %d", code)
	}
	if len(gw.term.resizes) != 1 {
		t.Fatalf("expected 1 TerminalResize, got %d", len(gw.term.resizes))
	}
	r := gw.term.resizes[0]
	if r.Rows != 43 || r.Cols != 132 {
		t.Fatalf("resize = %dx%d, want 43x132", r.Rows, r.Cols)
	}
}

func TestEnvdPtyKill(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	pid, sid := ptySetup(t, s, ts)

	// SDK pty.kill: sendSignal with numeric 9 (SIGKILL).
	code := ptyRequest(t, ts, s, sid, "/process.Process/SendSignal", map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"signal":  9,
	})
	if code != 200 {
		t.Fatalf("kill: want 200, got %d", code)
	}
	if len(gw.term.signals) != 1 {
		t.Fatalf("expected 1 TerminalSignal, got %d", len(gw.term.signals))
	}
	if gw.term.signals[0].Signal != pb.TerminalSignal_TERMINAL_SIGNAL_KILL {
		t.Fatalf("signal = %v, want KILL", gw.term.signals[0].Signal)
	}
}

// TestEnvdPtyOwnershipEnforced verifies a pty RPC from a different sandbox
// credential cannot drive the terminal (Greptile security review).
func TestEnvdPtyOwnershipEnforced(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	pid, _ := ptySetup(t, s, ts)
	otherID := createSandboxID(t, ts) // different sandbox

	code := ptyRequest(t, ts, s, otherID, "/process.Process/SendInput", map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"input":   map[string]any{"input": map[string]any{"case": "pty", "value": base64.StdEncoding.EncodeToString([]byte("ls\r"))}},
	})
	if code != 404 {
		t.Fatalf("cross-sandbox sendInput: want 404, got %d", code)
	}
	if len(gw.term.writes) != 0 {
		t.Fatalf("cross-sandbox sendInput must not reach TerminalWrite, got %d writes", len(gw.term.writes))
	}

	code = ptyRequest(t, ts, s, otherID, "/process.Process/Update", map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"pty":     map[string]any{"size": map[string]int{"cols": 100, "rows": 40}},
	})
	if code != 404 && code != 400 {
		t.Fatalf("cross-sandbox resize: want 4xx, got %d", code)
	}
	if len(gw.term.resizes) != 0 {
		t.Fatalf("cross-sandbox resize must not reach TerminalResize, got %d", len(gw.term.resizes))
	}
}

// TestEnvdPtyDestroyedOnExit verifies the backend terminal is released after
// the pty.create stream completes (Greptile security review).
func TestEnvdPtyDestroyedOnExit(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	msg := map[string]any{
		"process": map[string]any{"cmd": "/bin/bash", "args": []string{"-i", "-l"}},
		"pty":     map[string]any{"size": map[string]int{"cols": 80, "rows": 24}},
	}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Start", bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	_ = readBody(t, resp)

	if len(gw.term.created) != 1 {
		t.Fatalf("expected 1 CreateTerminal, got %d", len(gw.term.created))
	}
	// fakeGateway derives the terminal id from the session + counter.
	want := id + "/t1"
	if len(gw.term.destroyed) != 1 || gw.term.destroyed[0] != want {
		t.Fatalf("expected TerminalDestroy(%q), got %v", want, gw.term.destroyed)
	}
}
