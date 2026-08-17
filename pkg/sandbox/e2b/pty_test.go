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

func TestEnvdPtySendInput(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	pid, sid := ptySetup(t, s, ts)

	// SDK sendInput selector shape: input routed to TerminalWrite.
	payload, _ := json.Marshal(map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"input":   map[string]any{"input": map[string]any{"case": "pty", "value": base64.StdEncoding.EncodeToString([]byte("ls\r"))}},
	})
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/SendInput", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range envdHeaders(t, s, sid) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("sendInput: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
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
	payload, _ := json.Marshal(map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"pty":     map[string]any{"size": map[string]int{"cols": 132, "rows": 43}},
	})
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Update", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range envdHeaders(t, s, sid) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("resize: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
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
	payload, _ := json.Marshal(map[string]any{
		"process": map[string]any{"selector": map[string]any{"case": "pid", "value": pid}},
		"signal":  9,
	})
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/SendSignal", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range envdHeaders(t, s, sid) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("kill: want 200, got %d", resp.StatusCode)
	}
	resp.Body.Close()
	if len(gw.term.signals) != 1 {
		t.Fatalf("expected 1 TerminalSignal, got %d", len(gw.term.signals))
	}
	if gw.term.signals[0].Signal != pb.TerminalSignal_TERMINAL_SIGNAL_KILL {
		t.Fatalf("signal = %v, want KILL", gw.term.signals[0].Signal)
	}
}
