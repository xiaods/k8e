package e2b

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
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

	req, _ := http.NewRequest("GET", ts.URL+"/e2b/envd/health", nil)
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
	req2, _ := http.NewRequest("GET", ts.URL+"/e2b/envd/health", nil)
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
	for _, f := range frames {
		if ev, ok := f.json["event"].(map[string]any); ok {
			if _, ok := ev["end"]; ok {
				endFrame = &f
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

func TestEnvdProcessStartRejectsPTY(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	msg := map[string]any{
		"process": map[string]any{
			"cmd":  "/bin/bash",
			"args": []string{"-i", "-l"},
			"envs": map[string]string{},
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
	if len(frames) != 1 {
		t.Fatalf("expected single error frame, got %d", len(frames))
	}
	errObj := frames[0].json["error"].(map[string]any)
	if errObj["code"] != "invalid_argument" {
		t.Fatalf("expected invalid_argument, got %v", errObj)
	}
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

	// SendSignal/SendInput/CloseStdin and the polling watch trio
	// (CreateWatcher/GetWatcherEvents/RemoveWatcher) are now implemented;
	// the streaming WatchDir, PTY resize, and streamed stdin stay 501.
	cases := []struct {
		path string
		body string
	}{
		{"/filesystem.Filesystem/WatchDir", `{}`},
		{"/process.Process/Update", `{}`},
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
		t.Fatalf("expected 1 process from sandboxd, got %v", out.Processes)
	}
	p := out.Processes[0]
	if p["pid"] != float64(4242) {
		t.Fatalf("pid = %v, want 4242", p["pid"])
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
	if err := json.Unmarshal([]byte(body), &cr); err != nil {
		t.Fatalf("create body: %s", body)
	}
	if cr.WatcherID == 0 {
		t.Fatal("watcher id missing")
	}

	// Seed an event on the watcher.
	stub.mu.Lock()
	stub.watchers[cr.WatcherID] = []watchEvent{{Name: "/workspace/new.txt", Type: 1}}
	stub.mu.Unlock()

	// GetWatcherEvents.
	status, body = postRPC("/filesystem.Filesystem/GetWatcherEvents",
		fmt.Sprintf(`{"watcherId":%d}`, cr.WatcherID))
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

	// RemoveWatcher.
	status, body = postRPC("/filesystem.Filesystem/RemoveWatcher",
		fmt.Sprintf(`{"watcherId":%d}`, cr.WatcherID))
	if status != 200 {
		t.Fatalf("RemoveWatcher: %d %s", status, body)
	}

	// GetWatcherEvents after remove → not_found.
	status, _ = postRPC("/filesystem.Filesystem/GetWatcherEvents",
		fmt.Sprintf(`{"watcherId":%d}`, cr.WatcherID))
	if status != 404 {
		t.Fatalf("events after remove: want 404, got %d", status)
	}
}
