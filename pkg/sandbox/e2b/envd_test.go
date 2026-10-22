package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// envdHeaders builds the header set the SDK sends (token minted for exactly
// this sandbox).
func envdHeaders(t *testing.T, s *Server, sandboxID string) map[string]string {
	t.Helper()
	return map[string]string{
		"E2b-Sandbox-Id": sandboxID,
		"X-Access-Token": mintEnvdToken(s.signingSecret, sandboxID),
	}
}

func createSandboxID(t *testing.T, ts *httptest.Server) string {
	t.Helper()
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	if resp.StatusCode != 201 {
		t.Fatalf("create: %d %s", resp.StatusCode, readBody(t, resp))
	}
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	return session["sandboxID"].(string)
}

func TestEnvdHealth(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	req, _ := http.NewRequest("GET", ts.URL+"/e2b/envd/health", http.NoBody)
	req.Header.Set("E2b-Sandbox-Id", id)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 204 {
		t.Fatalf("health on running sandbox: want 204, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Unknown sandbox → 502.
	req2, _ := http.NewRequest("GET", ts.URL+"/e2b/envd/health", http.NoBody)
	req2.Header.Set("E2b-Sandbox-Id", "no-such")
	resp2, _ := ts.Client().Do(req2)
	if resp2.StatusCode != 502 {
		t.Fatalf("health on unknown: want 502, got %d", resp2.StatusCode)
	}
	resp2.Body.Close()
}

func TestEnvdAuth(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	// Wrong token → 401.
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/List", bytes.NewReader([]byte("{}")))
	req.Header.Set("E2b-Sandbox-Id", id)
	req.Header.Set("X-Access-Token", "wrong")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	resp.Body.Close()

	// Correct token → 200.
	req2, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/List", bytes.NewReader([]byte("{}")))
	for k, v := range envdHeaders(t, s, id) {
		req2.Header.Set(k, v)
	}
	resp2, err := ts.Client().Do(req2)
	if err != nil {
		t.Fatal(err)
	}
	if resp2.StatusCode != 200 {
		t.Fatalf("want 200, got %d: %s", resp2.StatusCode, readBody(t, resp2))
	}
	resp2.Body.Close()
}

func TestEnvdProcessListEmpty(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/List", bytes.NewReader([]byte("{}")))
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	var out struct {
		Processes []map[string]any `json:"processes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("list body: %s", body)
	}
	if len(out.Processes) != 0 {
		t.Fatalf("expected empty process list, got %v", out.Processes)
	}
}

func TestEnvdProcessStartStreamsFrames(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	// Exit code arrives in-stream (KIP-18 P1): the closing {"exit":N} frame.
	gw.mu.Lock()
	gw.streams["echo hi"] = []*pb.ExecStreamResponse{
		{Chunk: "data: hi\n\n"},
		{Chunk: "data: \n\n"},
		{Chunk: "data: {\"exit\":0}\n\n"},
	}
	gw.mu.Unlock()

	req := buildStartRequest(t, ts, s, id, "echo hi")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	frames, err := parseEnvelopes([]byte(body))
	if err != nil {
		t.Fatalf("parse frames: %v", err)
	}
	if len(frames) < 2 {
		t.Fatalf("expected start + data + end frames, got %d", len(frames))
	}
	// Frame 0: start with pid.
	ev0 := frames[0].json["event"].(map[string]any)
	if _, ok := ev0["start"]; !ok {
		t.Fatalf("first frame must be start, got %v", frames[0].json)
	}
	// A data frame carrying the streamed bytes base64-decoded.
	var gotData []byte
	for _, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if data, ok := ev["data"].(map[string]any); ok {
				if b64, ok := data["stdout"].(string); ok {
					dec := mustB64Decode(t, b64)
					gotData = append(gotData, dec...)
				}
			}
		}
	}
	if string(gotData) != "hi" {
		t.Fatalf("streamed data = %q, want %q", gotData, "hi")
	}
	// End frame with exit code 0.
	var endFrame *frame
	for i, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if _, ok := ev["end"]; ok {
				endFrame = &frames[i]
			}
		}
	}
	if endFrame == nil {
		t.Fatal("missing end frame")
	}
	end := endFrame.json["event"].(map[string]any)["end"].(map[string]any)
	if end["exitCode"] != float64(0) {
		t.Fatalf("exitCode=%v", end["exitCode"])
	}
}

func buildStartRequest(t *testing.T, ts *httptest.Server, s *Server, sandboxID, command string) *http.Request {
	t.Helper()
	msg := map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-l", "-c", command},
			"envs": map[string]string{},
		},
	}
	payload := envelope(FlagMessage, msg)
	req, err := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Start", bytes.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, sandboxID) {
		req.Header.Set(k, v)
	}
	return req
}

func TestEnvdProcessStartWithPTY(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	msg := map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
			"envs": map[string]string{"TERM": "xterm-256color"},
		},
		"pty": map[string]any{"size": map[string]int{"cols": 80, "rows": 24}},
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
	body := readBody(t, resp)
	frames, err := parseEnvelopes([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	if len(frames) < 1 {
		t.Fatalf("expected frames, got %d", len(frames))
	}

	// First frame: start with the E2B-facing pid.
	startPID := envelopeStartPID(t, frames[0])
	if startPID == 0 {
		t.Fatalf("pty start pid must be non-zero")
	}

	// The gateway received the terminal create with the right argv/size.
	assertPtyCreateRequest(t, gw, id)

	// Terminal streamed a data frame then exited.
	if got := ptyStreamedData(t, frames); string(got) != "hello tty" {
		t.Fatalf("pty streamed data = %q, want %q", got, "hello tty")
	}
	if !envelopeHasEvent(frames, "end") {
		t.Fatalf("missing end frame")
	}
	// The pty row is dropped after the stream exits.
	if _, ok := s.ptyFor(startPID); ok {
		t.Fatalf("pty row must be dropped after exit")
	}
}

// assertPtyCreateRequest checks the gateway received exactly one
// CreateTerminal for the sandbox with the SDK's argv and window size.
func assertPtyCreateRequest(t *testing.T, gw *fakeGateway, sandboxID string) {
	t.Helper()
	if len(gw.terminalCreated()) != 1 {
		t.Fatalf("expected 1 CreateTerminal, got %d", len(gw.terminalCreated()))
	}
	cr := gw.terminalCreated()[0]
	if cr.SessionId != sandboxID || len(cr.Argv) != 3 || cr.Argv[0] != "/bin/bash" {
		t.Fatalf("unexpected CreateTerminalRequest: %+v", cr)
	}
	if cr.Rows != 24 || cr.Cols != 80 {
		t.Fatalf("pty size = %dx%d, want 24x80", cr.Rows, cr.Cols)
	}
}

// envelopeStartPID extracts the pid from a start event frame, or 0.
func envelopeStartPID(t *testing.T, f frame) int {
	t.Helper()
	ev, ok := f.json["event"].(map[string]any)
	if !ok {
		return 0
	}
	start, ok := ev["start"].(map[string]any)
	if !ok {
		return 0
	}
	return int(start["pid"].(float64))
}

// envelopeHasEvent reports whether any frame carries the named event.
func envelopeHasEvent(frames []frame, event string) bool {
	for _, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if _, ok := ev[event]; ok {
				return true
			}
		}
	}
	return false
}

// ptyStreamedData concatenates base64 stdout data frames.
func ptyStreamedData(t *testing.T, frames []frame) []byte {
	t.Helper()
	var got []byte
	for _, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if data, ok := ev["data"].(map[string]any); ok {
				if b64, ok := data["stdout"].(string); ok {
					got = append(got, mustB64Decode(t, b64)...)
				}
			}
		}
	}
	return got
}

func TestEnvdProcessStartUnknownShape(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	msg := map[string]any{"process": map[string]any{"cmd": "/bin/ls", "args": []string{"-l"}}}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Start", bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := parseEnvelopes([]byte(readBody(t, resp)))
	errObj := frames[0].json["error"].(map[string]any)
	if !strings.Contains(errObj["message"].(string), "only shell commands") {
		t.Fatalf("message=%v", errObj["message"])
	}
}

func TestEnvdProcessConnectUnknownPid(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	msg := map[string]any{"process": map[string]any{"pid": 424242}}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Connect", bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	frames, _ := parseEnvelopes([]byte(readBody(t, resp)))
	errObj := frames[0].json["error"].(map[string]any)
	if errObj["code"] != "not_found" {
		t.Fatalf("expected not_found, got %v", errObj)
	}
}

func TestUnimplementedSurfaces(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	// SendSignal/SendInput/CloseStdin and PTY Update (resize) are
	// implemented; the polling watch trio
	// (CreateWatcher/GetWatcherEvents/RemoveWatcher) is implemented too.
	// The streaming WatchDir is implemented as well: the JS SDK's
	// files.watchDir() uses it (the Python SDK uses the polling trio),
	// so answering it with a 501 broke the JS SDK's watch.
	// Streamed stdin stays 501.
	cases := []struct {
		path string
		body string
	}{
		{"/process.Process/StreamInput", `{}`},
	}
	for _, c := range cases {
		req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd"+c.path, bytes.NewReader([]byte(c.body)))
		for k, v := range envdHeaders(t, s, id) {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		if resp.StatusCode != 501 {
			t.Fatalf("%s: want 501, got %d", c.path, resp.StatusCode)
		}
		body := readBody(t, resp)
		if !strings.Contains(body, "unimplemented") {
			t.Fatalf("%s: body %s", c.path, body)
		}
	}
}

func mustB64Decode(t *testing.T, s string) []byte {
	t.Helper()
	out, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func TestExitStatusKilled(t *testing.T) {
	if exitStatus(137) != "killed" {
		t.Fatalf("137 should be killed, got %q", exitStatus(137))
	}
	if exitStatus(-1) != "killed" {
		t.Fatalf("-1 (marker missing = SIGKILLed) should be killed, got %q", exitStatus(-1))
	}
	if exitStatus(0) != "exited" {
		t.Fatalf("0 should be exited, got %q", exitStatus(0))
	}
	if exitStatus(7) != "exited" {
		t.Fatalf("7 should be exited, got %q", exitStatus(7))
	}
}

func TestProcessSendInputSignal(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// Register a live process with a known guest pid (simulating what the
	// stream would set).
	rec := s.processes.Start(id, ProcessConfig{Cmd: "/bin/bash", Args: []string{"-c", "cat"}})
	gp := 4242
	s.processes.SetGuestPID(rec.PID, gp)

	envdHeadersMap := envdHeaders(t, s, id)
	postRPC := func(path, payload string) (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd"+path, bytes.NewReader([]byte(payload)))
		for k, v := range envdHeadersMap {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, readBody(t, resp)
	}

	// SendInput: base64 "hello".
	status, body := postRPC("/process.Process/SendInput",
		`{"process":{"pid":1000},"input":{"stdin":"`+b64("hello")+`"}}`)
	if status != 200 {
		t.Fatalf("SendInput: %d %s", status, body)
	}
	stub.mu.Lock()
	got := stub.stdin[gp]
	stub.mu.Unlock()
	if len(got) != 1 || got[0] != b64("hello") {
		t.Fatalf("stdin=%v", got)
	}

	// SendSignal: SIGKILL.
	status, body = postRPC("/process.Process/SendSignal",
		`{"process":{"pid":1000},"signal":"SIGNAL_SIGKILL"}`)
	if status != 200 {
		t.Fatalf("SendSignal: %d %s", status, body)
	}
	stub.mu.Lock()
	sigs := stub.signals[gp]
	stub.mu.Unlock()
	if len(sigs) != 1 || sigs[0] != "SIGKILL" {
		t.Fatalf("signals=%v", sigs)
	}

	// CloseStdin.
	status, body = postRPC("/process.Process/CloseStdin", `{"process":{"pid":1000}}`)
	if status != 200 {
		t.Fatalf("CloseStdin: %d %s", status, body)
	}
	stub.mu.Lock()
	closed := stub.closed[gp]
	stub.mu.Unlock()
	if !closed {
		t.Fatal("stdin should be closed")
	}

	// Unknown synthetic pid → not_found.
	status, body = postRPC("/process.Process/SendSignal",
		`{"process":{"pid":999999},"signal":"SIGKILL"}`)
	if status != 404 {
		t.Fatalf("unknown pid SendSignal: %d %s", status, body)
	}
}

func TestProcessSendInputUnknownGuestPID(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// A process registered but the stream never reported a guest pid: the
	// control verb must answer not_found (cannot address sandboxd yet).
	rec := s.processes.Start(id, ProcessConfig{Cmd: "/bin/bash"})
	_ = rec

	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/SendInput",
		bytes.NewReader([]byte(`{"process":{"pid":1000},"input":{"stdin":"aGk="}}`)))
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 404 {
		t.Fatalf("want 404 (no guest pid yet), got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

func b64(s string) string {
	return base64.StdEncoding.EncodeToString([]byte(s))
}

// TestEnvdStreamGuestPIDCapture locks the guest-pid bridge: sandboxd's
// /exec/stream opens with a `data: {"pid":N}\n\n` frame; the e2b layer must
// (a) record it on the process record so SendInput/SendSignal can address
// the real in-sandbox process, and (b) never leak the frame into the SDK's
// output stream. A gated stream holds the process alive while we assert on
// the table record.
func TestEnvdStreamGuestPIDCapture(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	release := gw.gatedExecStream("echo hi", []*pb.ExecStreamResponse{
		{Chunk: "data: {\"pid\":4242}\n\n"},
		{Chunk: "data: hi\n\n"},
		{Chunk: "data: \n\n"},
		{Chunk: "data: {\"exit\":0}\n\n"},
	})
	defer release()

	req := buildStartRequest(t, ts, s, id, "echo hi")
	done := make(chan string, 1)
	go func() {
		resp, err := ts.Client().Do(req)
		if err != nil {
			done <- "err: " + err.Error()
			return
		}
		done <- readBody(t, resp)
	}()

	// Wait for the stream to open and the pid frame to be consumed.
	deadline := time.Now().Add(5 * time.Second)
	for {
		if guest, ok := s.processes.GuestPID(1000); ok {
			if guest != 4242 {
				t.Fatalf("guest pid = %d, want 4242", guest)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("guest pid never captured")
		}
		time.Sleep(10 * time.Millisecond)
	}

	release()

	body := <-done
	if strings.HasPrefix(body, "err:") {
		t.Fatal(body)
	}
	frames, err := parseEnvelopes([]byte(body))
	if err != nil {
		t.Fatalf("parse frames: %v", err)
	}
	var gotData []byte
	for _, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if data, ok := ev["data"].(map[string]any); ok {
				if b64, ok := data["stdout"].(string); ok {
					gotData = append(gotData, mustB64Decode(t, b64)...)
				}
			}
		}
	}
	if string(gotData) != "hi" {
		t.Fatalf("streamed data = %q, want %q (pid frame leaked into output)", gotData, "hi")
	}
}

// TestEnvdProcessListFromSandboxd verifies Process/List reads the sandbox-
// owned process table (KIP-18 P1): pids are the sandbox's own, so the view
// is node-independent — any control-plane node returns the same list.
func TestEnvdProcessListFromSandboxd(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// Seed the sandbox-owned process table with a live process.
	stub.mu.Lock()
	stub.procTable[4242] = sandboxProcess{PID: 4242, Alive: true, Config: "echo hi"}
	stub.mu.Unlock()

	// Register the same process in the e2b layer's own table, mapping the
	// SDK-facing pid to the guest pid (4242). Process/List must report the
	// SDK-facing pid — it is the one the SDK received in the Start frame
	// and the one it must pass to Connect/SendInput.
	rec := s.processes.Start(id, ProcessConfig{Cmd: "echo hi"})
	s.processes.SetGuestPID(rec.PID, 4242)

	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/List", bytes.NewReader([]byte("{}")))
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	var out struct {
		Processes []map[string]any `json:"processes"`
	}
	if err := json.Unmarshal([]byte(body), &out); err != nil {
		t.Fatalf("list body: %s", body)
	}
	if len(out.Processes) != 1 {
		t.Fatalf("expected 1 process, got %v", out.Processes)
	}
	p := out.Processes[0]
	// The SDK-facing pid, NOT the guest pid: the two spaces used to be
	// mixed, so a client that took the pid from commands.list() and passed
	// it to commands.connect() always got "process not found".
	if p["pid"] != float64(rec.PID) {
		t.Fatalf("pid = %v, want %d (the pid the SDK was given at Start)", p["pid"], rec.PID)
	}
	if p["alive"] != true {
		t.Fatalf("alive = %v, want true", p["alive"])
	}
}

// TestEnvdProcessConnectCrossNode verifies Process/Connect when the local
// process table has no record (the Start stream ran on another control-plane
// node): it falls back to the sandbox-owned process table via /exec/attach,
// which replays the buffered output — node-independent pids make Connect work
// across nodes even though the subscriber broadcast stays local.
func TestEnvdProcessConnectCrossNode(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// The process exists in the sandbox (another node served its Start), with
	// buffered output available for attach. No local process-table record.
	stub.mu.Lock()
	stub.procTable[4242] = sandboxProcess{PID: 4242, Alive: true, Config: "echo hi"}
	stub.attachOutputs[4242] = "hello from sandbox"
	stub.mu.Unlock()

	msg := map[string]any{"process": map[string]any{"pid": 4242}}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Connect",
		bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	frames, err := parseEnvelopes([]byte(body))
	if err != nil {
		t.Fatalf("parse frames: %v", err)
	}

	// Frame 0: start with pid 4242.
	ev0 := frames[0].json["event"].(map[string]any)
	start, ok := ev0["start"].(map[string]any)
	if !ok {
		t.Fatalf("first frame must be start, got %v", frames[0].json)
	}
	if start["pid"] != float64(4242) {
		t.Fatalf("start pid = %v, want 4242", start["pid"])
	}

	// The attach output must arrive as a stdout data frame.
	var gotData []byte
	for _, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if data, ok := ev["data"].(map[string]any); ok {
				if b64, ok := data["stdout"].(string); ok {
					gotData = append(gotData, mustB64Decode(t, b64)...)
				}
			}
		}
	}
	if string(gotData) != "hello from sandbox" {
		t.Fatalf("attached data = %q, want %q", gotData, "hello from sandbox")
	}
}

// TestKeepaliveSubscriber verifies the idle heartbeat: after keepaliveInterval
// with no output, a ProcessEvent keepalive frame is written; output resets the
// clock.
func TestKeepaliveSubscriber(t *testing.T) {
	var mu sync.Mutex
	var buf bytes.Buffer
	lockedWrite := func(p []byte) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		return buf.Write(p)
	}
	inner := &streamSubscriber{pid: 1, w: &buf}
	ksub, stop := newKeepaliveSubscriber(inner, writerFunc(lockedWrite))
	defer stop()

	// With no output, the ticker fires and writes a keepalive frame.
	deadline := time.Now().Add(keepaliveInterval + 5*time.Second)
	for {
		mu.Lock()
		has := bytes.Contains(buf.Bytes(), []byte(`"keepalive"`))
		mu.Unlock()
		if has || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	mu.Lock()
	hasKeepalive := bytes.Contains(buf.Bytes(), []byte(`"keepalive"`))
	mu.Unlock()
	if !hasKeepalive {
		t.Fatal("no keepalive frame after idle interval")
	}

	// Output resets the clock and is forwarded (base64-encoded by the inner
	// streamSubscriber).
	ksub.OnOutput(ChannelStdout, []byte("hi"))
	mu.Lock()
	fwd := bytes.Contains(buf.Bytes(), []byte(b64("hi")))
	mu.Unlock()
	if !fwd {
		t.Fatal("output not forwarded through keepalive subscriber")
	}
}

// writerFunc adapts a func to io.Writer.
type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// TestEnvdWatchDirFamily exercises the polling watch trio the SDK's
// watch_dir uses (CreateWatcher → GetWatcherEvents → RemoveWatcher), the
// last protocol gap (KIP-18 P1).
func TestEnvdWatchDirFamily(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	envdHdrs := envdHeaders(t, s, id)
	postRPC := func(path, payload string) (int, string) {
		req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd"+path, bytes.NewReader([]byte(payload)))
		for k, v := range envdHdrs {
			req.Header.Set(k, v)
		}
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, readBody(t, resp)
	}

	// CreateWatcher.
	status, body := postRPC("/filesystem.Filesystem/CreateWatcher",
		`{"path":"/workspace","recursive":false}`)
	if status != 200 {
		t.Fatalf("CreateWatcher: %d %s", status, body)
	}
	var cr struct {
		WatcherID int `json:"watcherId"`
	}
	// The E2B proto declares watcher_id as a string, so the server returns
	// one; sandboxd's int id is encoded as decimal text.
	var crStr struct {
		WatcherID string `json:"watcherId"`
	}
	if err := json.Unmarshal([]byte(body), &crStr); err != nil {
		t.Fatalf("create body: %s", body)
	}
	watcherID, convErr := strconv.Atoi(crStr.WatcherID)
	if convErr != nil || watcherID == 0 {
		t.Fatalf("watcher id missing: %s", body)
	}
	cr.WatcherID = watcherID

	// Seed an event on the watcher.
	stub.mu.Lock()
	stub.watchers[cr.WatcherID] = []watchEvent{{Name: "/workspace/new.txt", Type: 1}}
	stub.mu.Unlock()

	// GetWatcherEvents (the SDK sends watcher_id as a string).
	status, body = postRPC("/filesystem.Filesystem/GetWatcherEvents",
		fmt.Sprintf(`{"watcherId":%q}`, crStr.WatcherID))
	if status != 200 {
		t.Fatalf("GetWatcherEvents: %d %s", status, body)
	}
	var ge struct {
		Events []map[string]any `json:"events"`
	}
	if err := json.Unmarshal([]byte(body), &ge); err != nil {
		t.Fatalf("events body: %s", body)
	}
	if len(ge.Events) != 1 {
		t.Fatalf("events = %v, want 1", ge.Events)
	}
	if ge.Events[0]["type"] != "EVENT_TYPE_CREATE" {
		t.Fatalf("event type = %v, want EVENT_TYPE_CREATE", ge.Events[0]["type"])
	}
	if ge.Events[0]["name"] != "/workspace/new.txt" {
		t.Fatalf("event name = %v", ge.Events[0]["name"])
	}

	// RemoveWatcher (string watcher_id).
	status, body = postRPC("/filesystem.Filesystem/RemoveWatcher",
		fmt.Sprintf(`{"watcherId":%q}`, crStr.WatcherID))
	if status != 200 {
		t.Fatalf("RemoveWatcher: %d %s", status, body)
	}

	// GetWatcherEvents after remove → not_found.
	status, _ = postRPC("/filesystem.Filesystem/GetWatcherEvents",
		fmt.Sprintf(`{"watcherId":%q}`, crStr.WatcherID))
	if status != 404 {
		t.Fatalf("events after remove: want 404, got %d", status)
	}
}

// TestParseChannelFrame covers the stdout/stderr separation (2026-10-21).
//
// sandboxd used to dup2 stderr onto the same pipe as stdout, so the stream
// carried no channel information: the E2B SDK's stderr was always empty and
// its on_stderr callback never fired. The stream now tags each frame with
// its channel and base64-encodes the payload.
func TestParseChannelFrame(t *testing.T) {
	// stdout frame
	ch, payload, ok := parseChannelFrame([]byte(`{"stdout":"aGVsbG8="}`))
	if !ok {
		t.Fatal("expected a stdout frame to parse")
	}
	if ch != ChannelStdout {
		t.Fatalf("channel = %q, want %q", ch, ChannelStdout)
	}
	if string(payload) != "hello" {
		t.Fatalf("payload = %q, want %q", payload, "hello")
	}

	// stderr frame
	ch, payload, ok = parseChannelFrame([]byte(`{"stderr":"d29ybGQ="}`))
	if !ok {
		t.Fatal("expected a stderr frame to parse")
	}
	if ch != ChannelStderr {
		t.Fatalf("channel = %q, want %q", ch, ChannelStderr)
	}
	if string(payload) != "world" {
		t.Fatalf("payload = %q, want %q", payload, "world")
	}

	// A frame from an older sandboxd carries raw output and no channel:
	// it must not be mistaken for a tagged frame.
	if _, _, ok := parseChannelFrame([]byte("plain output")); ok {
		t.Fatal("expected an untagged frame not to parse as a channel frame")
	}
	// A pid frame and an exit frame are handled elsewhere.
	if _, _, ok := parseChannelFrame([]byte(`{"pid":42}`)); ok {
		t.Fatal("expected a pid frame not to parse as a channel frame")
	}
	if _, _, ok := parseChannelFrame([]byte(`{"exit":0}`)); ok {
		t.Fatal("expected an exit frame not to parse as a channel frame")
	}
}

// TestSSEFramerChannelFramesKeepsBoundaries covers the frame-boundary bug
// that came with the shared stdout/stderr pipe: a raw payload containing a
// blank line split the SSE frame and leaked the "data: " prefix into the
// client's stdout. Base64 payloads cannot contain a blank line, so the
// boundary always holds.
func TestSSEFramerChannelFramesKeepsBoundaries(t *testing.T) {
	f := &sseFramer{}
	frames := f.pushFrames("data: {\"stdout\":\"bGluawpibGFuawpibGFuawo=\"}\n\n")
	if len(frames) != 1 {
		t.Fatalf("expected 1 frame, got %d: %q", len(frames), frames)
	}
	ch, payload, ok := parseChannelFrame(frames[0])
	if !ok {
		t.Fatalf("expected the frame to parse: %q", frames[0])
	}
	if ch != ChannelStdout {
		t.Fatalf("channel = %q, want %q", ch, ChannelStdout)
	}
	if string(payload) != "link\nblank\nblank\n" {
		t.Fatalf("payload = %q", payload)
	}
}

// TestEnvdProcessConnectDeliversEndEvent covers the attach path (2026-10-21).
//
// Process/Connect opened a stream, subscribed to the process and then waited
// for it to finish — but ProcessTable.End() DELETES the record, so Done()
// reported "no channel" for a process that had already finished, and the
// subscriber never got an end event. The SDK's CommandHandle.wait() then
// raised "Command ended without an end event", so attaching to a running
// process never reported its output or exit code.
func TestEnvdProcessConnectDeliversEndEvent(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// A live process with buffered output: Connect must replay the output
	// AND carry the end event. Without the end event the SDK's
	// CommandHandle.wait() raises "Command ended without an end event", so
	// attaching to a running process reported neither output nor an exit
	// code.
	rec := s.processes.Start(id, ProcessConfig{Cmd: "echo hi"})
	s.processes.SetGuestPID(rec.PID, 4242)
	stub.mu.Lock()
	stub.procTable[4242] = sandboxProcess{PID: 4242, Alive: true, Config: "echo hi"}
	stub.attachOutputs[4242] = "hello from sandbox"
	stub.mu.Unlock()

	msg := map[string]any{"process": map[string]any{"pid": rec.PID}}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/process.Process/Connect",
		bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	// The stream follows the process to completion, so end it.
	go func() {
		time.Sleep(200 * time.Millisecond)
		s.processes.End(rec.PID, ProcessEnd{ExitCode: 0})
	}()
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body := readBody(t, resp)
	frames, err := parseEnvelopes([]byte(body))
	if err != nil {
		t.Fatalf("parse frames: %v", err)
	}
	foundEnd := false
	for _, f := range frames {
		if event, ok := f.json["event"].(map[string]any); ok {
			if _, hasEnd := event["end"]; hasEnd {
				foundEnd = true
			}
		}
	}
	if !foundEnd {
		t.Fatalf("expected an end event in the connect stream, got %q", body)
	}
}

// TestEnvdFSWatchDirStreaming covers the STREAMING filesystem WatchDir.
//
// KIP-18 answered the streaming WatchDir with an honest 501 on the
// assumption that "no SDK uses it" — true for the Python SDK (which uses the
// polling trio), but NOT for the JS SDK: its files.watchDir() calls the
// streaming RPC, so the JS SDK's watchDir() failed with
// "501: WatchDir is not supported: K8E has no watch".
//
// The stream carries the oneof member name at the TOP level of the frame
// (the connect client parses the whole frame as the WatchDirResponse), and
// the event type is the NUMERIC enum the SDK maps to its names.
func TestEnvdFSWatchDirStreaming(t *testing.T) {
	gw := newFakeGateway()
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	id := createSandboxID(t, ts)

	// A watcher with one pending event.
	stub.mu.Lock()
	stub.nextWatcherID = 7
	stub.watchers[7] = []watchEvent{{Name: "/workspace/watched/a.txt", Type: 1}}
	stub.mu.Unlock()

	// A streaming request carries its payload inside a connect envelope.
	msg := map[string]any{"path": "/workspace/watched"}
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/filesystem.Filesystem/WatchDir",
		bytes.NewReader(envelope(FlagMessage, msg)))
	req.Header.Set("Content-Type", "application/connect+json")
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}

	// The watch stream stays open until the client goes away (that is the
	// point of a watch), so read it incrementally and stop on a deadline
	// rather than waiting for the response to end.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	req = req.WithContext(ctx)
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	body := readStreamFor(t, resp, cancel)
	frames, err := parseEnvelopes([]byte(body))
	if err != nil {
		t.Fatalf("parse frames: %v (body %q)", err, body)
	}

	// The first frame is the start event the SDK's watch handle requires.
	if len(frames) == 0 {
		t.Fatal("expected at least a start frame")
	}
	start, ok := frames[0].json["start"]
	if !ok {
		t.Fatalf("expected a start frame, got %v", frames[0].json)
	}
	_ = start

	// The event frame names the "filesystem" oneof member and carries the
	// numeric event type.
	found := false
	for _, f := range frames {
		ev, ok := f.json["filesystem"].(map[string]any)
		if !ok {
			continue
		}
		if ev["name"] != "/workspace/watched/a.txt" {
			t.Fatalf("event name = %v", ev["name"])
		}
		if ev["type"] != float64(1) {
			t.Fatalf("event type = %v, want the numeric enum 1", ev["type"])
		}
		found = true
	}
	if !found {
		t.Fatalf("no filesystem event frame in %q", body)
	}
}

// readStreamFor reads a streaming response for a short while and returns
// what arrived. A watch stream stays open until the client goes away, so the
// caller cancels the request instead of waiting for the response to end.
func readStreamFor(t *testing.T, resp *http.Response, cancel context.CancelFunc) string {
	t.Helper()
	var mu sync.Mutex
	raw := make([]byte, 0, 512)
	go func() {
		buf := make([]byte, 512)
		for {
			n, rerr := resp.Body.Read(buf)
			if n > 0 {
				mu.Lock()
				raw = append(raw, buf[:n]...)
				mu.Unlock()
			}
			if rerr != nil {
				return
			}
		}
	}()
	time.Sleep(500 * time.Millisecond)
	cancel()
	time.Sleep(200 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return string(raw)
}
