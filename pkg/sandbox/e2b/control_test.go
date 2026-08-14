package e2b

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// testServer builds a Server with a fake gateway and returns it plus the
// httptest server.
// sandboxdStub is an in-process fake of the in-pod sandboxd HTTP API used by
// the native fs + process-control endpoints (KIP-18 ability downshift).
type sandboxdStub struct {
	mu      sync.Mutex
	files   map[string]string // path -> content
	dirs    map[string]bool
	stdin   map[int][]string // pid -> list of base64 inputs
	signals map[int][]string // pid -> signals
	closed  map[int]bool
	// procTable mirrors the sandbox-owned process table (KIP-18 P1):
	// pid -> alive + command snapshot.
	procTable map[int]sandboxProcess
	// attachOutputs holds the buffered output an attach replays (pid -> text).
	attachOutputs map[int]string
	// watchers: watcher id -> buffered events (name -> type name).
	watchers      map[int][]watchEvent
	nextWatcherID int
	nextPID       int
}

func newSandboxdStub() *sandboxdStub {
	return &sandboxdStub{
		files: map[string]string{}, dirs: map[string]bool{},
		stdin: map[int][]string{}, signals: map[int][]string{}, closed: map[int]bool{},
		procTable:     map[int]sandboxProcess{},
		attachOutputs: map[int]string{},
		watchers:      map[int][]watchEvent{},
		nextWatcherID: 1,
		nextPID:       1000,
	}
}

// sandboxdBody is the JSON envelope the E2B server sends to sandboxd; the
// stub decodes the union of fields and each handler reads what it needs.
type sandboxdBody struct {
	Path        string `json:"path"`
	Source      string `json:"source"`
	Destination string `json:"destination"`
	Pid         int    `json:"pid"`
	Data        string `json:"data"`
	Signal      string `json:"signal"`
	WatcherID   int    `json:"watcher_id"`
}

func (st *sandboxdStub) handle(w http.ResponseWriter, r *http.Request) {
	var body sandboxdBody
	_ = json.NewDecoder(r.Body).Decode(&body)
	switch r.URL.Path {
	case "/files/stat":
		st.handleStat(w, body)
	case "/files/mkdir":
		st.handleMkdir(w, body)
	case "/files/move":
		st.handleMove(w, body)
	case "/files/remove":
		st.handleRemove(w, body)
	case "/exec/stdin":
		st.handleStdin(w, body)
	case "/exec/stdin/close":
		st.handleStdinClose(w, body)
	case "/exec/signal":
		st.handleSignal(w, body)
	case "/exec/processes":
		st.handleProcesses(w)
	case "/exec/attach":
		st.handleAttach(w, r)
	case "/watch/create":
		st.handleWatchCreate(w, r)
	case "/watch/events":
		st.handleWatchEvents(w, r)
	case "/watch/remove":
		st.handleWatchRemove(w, body)
	default:
		http.NotFound(w, r)
	}
}

func (st *sandboxdStub) writeOK(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func (st *sandboxdStub) handleStat(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if content, ok := st.files[body.Path]; ok {
		st.writeOK(w, map[string]any{
			"type": "file", "size": len(content), "mode": "644",
			"uid": 1000, "gid": 1000, "mtime": 1720000000, "name": body.Path,
		})
		return
	}
	if st.dirs[body.Path] {
		st.writeOK(w, map[string]any{
			"type": "dir", "size": 0, "mode": "755",
			"uid": 1000, "gid": 1000, "mtime": 1720000000, "name": body.Path,
		})
		return
	}
	http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
}

func (st *sandboxdStub) handleMkdir(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.dirs[body.Path] {
		http.Error(w, `{"error":"already exists"}`, http.StatusConflict)
		return
	}
	st.dirs[body.Path] = true
	st.writeOK(w, map[string]any{"ok": true})
}

func (st *sandboxdStub) handleMove(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if content, ok := st.files[body.Source]; ok {
		delete(st.files, body.Source)
		st.files[body.Destination] = content
		st.writeOK(w, map[string]any{"ok": true})
		return
	}
	if st.dirs[body.Source] {
		delete(st.dirs, body.Source)
		st.dirs[body.Destination] = true
		st.writeOK(w, map[string]any{"ok": true})
		return
	}
	http.Error(w, `{"error":"source not found"}`, http.StatusNotFound)
}

func (st *sandboxdStub) handleRemove(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	delete(st.files, body.Path)
	delete(st.dirs, body.Path)
	st.writeOK(w, map[string]any{"ok": true})
}

func (st *sandboxdStub) handleStdin(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stdin[body.Pid] = append(st.stdin[body.Pid], body.Data)
	st.writeOK(w, map[string]any{"ok": true})
}

func (st *sandboxdStub) handleStdinClose(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.closed[body.Pid] = true
	st.writeOK(w, map[string]any{"ok": true})
}

func (st *sandboxdStub) handleSignal(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.signals[body.Pid] = append(st.signals[body.Pid], body.Signal)
	st.writeOK(w, map[string]any{"ok": true})
}

func (st *sandboxdStub) handleProcesses(w http.ResponseWriter) {
	st.mu.Lock()
	defer st.mu.Unlock()
	procs := make([]sandboxProcess, 0, len(st.procTable))
	for pid, p := range st.procTable {
		procs = append(procs, sandboxProcess{PID: pid, Alive: p.Alive, Config: p.Config})
	}
	st.writeOK(w, map[string]any{"processes": procs})
}

func (st *sandboxdStub) handleAttach(w http.ResponseWriter, r *http.Request) {
	// GET /exec/attach?pid=N → SSE replay of buffered output.
	st.mu.Lock()
	defer st.mu.Unlock()
	pidStr := r.URL.Query().Get("pid")
	pid, _ := strconv.Atoi(pidStr)
	if pid == 0 {
		http.Error(w, `{"error":"pid required"}`, http.StatusBadRequest)
		return
	}
	if _, known := st.procTable[pid]; !known {
		// Unknown pid: the sandbox has no such process.
		http.Error(w, `{"error":"process not found"}`, http.StatusNotFound)
		return
	}
	output := st.attachOutputs[pid]
	w.Header().Set("Content-Type", "text/event-stream")
	// Skip the pid frame: the Connect handler sends its own start frame.
	if len(output) > 0 {
		_, _ = w.Write([]byte("data: " + output + "\n\n"))
	}
	_, _ = w.Write([]byte("data: {\"done\":true}\n\n"))
}

func (st *sandboxdStub) handleWatchCreate(w http.ResponseWriter, r *http.Request) {
	var wb struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&wb)
	st.mu.Lock()
	defer st.mu.Unlock()
	id := st.nextWatcherID
	st.nextWatcherID++
	st.watchers[id] = nil
	st.writeOK(w, map[string]any{"watcher_id": id})
}

func (st *sandboxdStub) handleWatchEvents(w http.ResponseWriter, r *http.Request) {
	st.mu.Lock()
	defer st.mu.Unlock()
	id, _ := strconv.Atoi(r.URL.Query().Get("watcher_id"))
	evs, ok := st.watchers[id]
	if !ok {
		http.Error(w, `{"error":"watcher not found"}`, http.StatusNotFound)
		return
	}
	st.writeOK(w, map[string]any{"events": evs})
}

func (st *sandboxdStub) handleWatchRemove(w http.ResponseWriter, body sandboxdBody) {
	st.mu.Lock()
	defer st.mu.Unlock()
	if _, ok := st.watchers[body.WatcherID]; !ok {
		http.Error(w, `{"error":"watcher not found"}`, http.StatusNotFound)
		return
	}
	delete(st.watchers, body.WatcherID)
	st.writeOK(w, map[string]any{"ok": true})
}

func testServer(t *testing.T, gw Gateway) (*Server, *httptest.Server) {
	t.Helper()
	return testServerWithSandboxd(t, gw, nil)
}

// testServerWithSandboxd builds a server whose sandboxd client targets the
// given stub (or a fresh one when nil).
func testServerWithSandboxd(t *testing.T, gw Gateway, stub *sandboxdStub) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(Config{
		Listen:        "127.0.0.1:0",
		APIKey:        "test-key",
		NodeID:        "node-test",
		SigningSecret: "signing-secret",
	}, gw)
	if stub == nil {
		stub = newSandboxdStub()
	}
	sbx := httptest.NewServer(http.HandlerFunc(stub.handle))
	t.Cleanup(sbx.Close)
	s.sandboxd.baseURL = sbx.URL
	ts := httptest.NewServer(s.Handle())
	t.Cleanup(ts.Close)
	return s, ts
}

func controlReq(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+"/e2b/api"+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("X-API-KEY", "e2b_test-key")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func readBody(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	var b bytes.Buffer
	_, err := b.ReadFrom(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestControlAuth(t *testing.T) {
	_, ts := testServer(t, newFakeGateway())

	// Wrong key → 401 with numeric code.
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/api/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-API-KEY", "e2b_wrong")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 401 {
		t.Fatalf("want 401, got %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	var errBody map[string]any
	if err := json.Unmarshal([]byte(body), &errBody); err != nil {
		t.Fatalf("error body not JSON: %s", body)
	}
	if errBody["code"] != float64(401) {
		t.Fatalf("want numeric code 401, got %v", errBody["code"])
	}
}

func TestControlCreate(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)

	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{
		"templateID": "base",
		"timeout":    600,
		"envVars":    map[string]string{"FOO": "bar"},
		"metadata":   map[string]string{"team": "blue"},
	})
	if resp.StatusCode != 201 {
		t.Fatalf("want 201, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	if session["sandboxID"] == "" {
		t.Fatal("sandboxID missing")
	}
	if session["clientID"] != "node-test" {
		t.Fatalf("clientID=%v", session["clientID"])
	}
	if session["envdVersion"] != EnvdVersion {
		t.Fatalf("envdVersion=%v", session["envdVersion"])
	}
	if session["envdAccessToken"] != mintEnvdToken("signing-secret", session["sandboxID"].(string)) {
		t.Fatal("envdAccessToken not minted from signing secret")
	}
	if len(gw.created) != 1 {
		t.Fatalf("expected 1 CreateSession call, got %d", len(gw.created))
	}
	req := gw.created[0]
	if req.Env["FOO"] != "bar" {
		t.Fatalf("env not passed through: %v", req.Env)
	}
}

func TestControlCreateIdempotentByName(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)

	r1 := controlReq(t, ts, "POST", "/sandboxes", map[string]any{
		"metadata": map[string]string{"name": "agent-7"},
	})
	if r1.StatusCode != 201 {
		t.Fatalf("first create: %d %s", r1.StatusCode, readBody(t, r1))
	}
	s1 := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, r1)), &s1)

	// Same name → same sandbox.
	r2 := controlReq(t, ts, "POST", "/sandboxes", map[string]any{
		"metadata": map[string]string{"name": "agent-7"},
	})
	if r2.StatusCode != 201 {
		t.Fatalf("second create: %d %s", r2.StatusCode, readBody(t, r2))
	}
	s2 := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, r2)), &s2)
	if s1["sandboxID"] != s2["sandboxID"] {
		t.Fatalf("same key should reuse the same sandbox: %v vs %v", s1["sandboxID"], s2["sandboxID"])
	}
	if len(gw.created) != 1 {
		t.Fatalf("expected no second CreateSession, got %d", len(gw.created))
	}
}

func TestControlGetInfo(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)

	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": 600})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	info := controlReq(t, ts, "GET", "/sandboxes/"+id, nil)
	if info.StatusCode != 200 {
		t.Fatalf("getInfo: %d %s", info.StatusCode, readBody(t, info))
	}
	iv := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, info)), &iv)
	// The Python SDK hard-requires this full field set.
	for _, f := range []string{"clientID", "cpuCount", "diskSizeMB", "endAt", "envdVersion", "memoryMB", "sandboxID", "startedAt", "state", "templateID"} {
		if _, ok := iv[f]; !ok {
			t.Fatalf("info.%s missing", f)
		}
	}
	if iv["state"] != "running" {
		t.Fatalf("state=%v", iv["state"])
	}
	_ = s
}

func TestControlKill(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)

	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	kill := controlReq(t, ts, "DELETE", "/sandboxes/"+id, nil)
	if kill.StatusCode != 204 {
		t.Fatalf("kill: %d %s", kill.StatusCode, readBody(t, kill))
	}
	if len(gw.destroyed) != 1 || gw.destroyed[0] != id {
		t.Fatalf("destroyed=%v", gw.destroyed)
	}

	again := controlReq(t, ts, "DELETE", "/sandboxes/"+id, nil)
	if again.StatusCode != 404 {
		t.Fatalf("second kill should 404, got %d", again.StatusCode)
	}
	body := readBody(t, again)
	if !strings.Contains(body, `"code":404`) {
		t.Fatalf("404 body should carry numeric code: %s", body)
	}
}

func TestControlTimeout(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)

	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": 60})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	timeout := controlReq(t, ts, "POST", "/sandboxes/"+id+"/timeout", map[string]any{"timeout": 600})
	if timeout.StatusCode != 204 {
		t.Fatalf("timeout: %d %s", timeout.StatusCode, readBody(t, timeout))
	}
	d := s.registry.deadlineOf(id)
	if d.Before(time.Now().Add(9 * time.Minute)) {
		t.Fatalf("deadline not extended: %v", d)
	}
}

func TestControlPauseResume(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Pause → 204, gateway PauseSession called, registry marked paused.
	pause := controlReq(t, ts, "POST", "/sandboxes/"+id+"/pause", map[string]any{})
	if pause.StatusCode != 204 {
		t.Fatalf("pause: %d %s", pause.StatusCode, readBody(t, pause))
	}
	if len(gw.paused) != 1 || gw.paused[0] != id {
		t.Fatalf("paused=%v", gw.paused)
	}
	if !s.registry.isPaused(id) {
		t.Fatal("registry should mark paused")
	}

	// Resume → 201 (created), gateway ResumeSession called.
	resume := controlReq(t, ts, "POST", "/sandboxes/"+id+"/resume", map[string]any{})
	if resume.StatusCode != 201 {
		t.Fatalf("resume: %d %s", resume.StatusCode, readBody(t, resume))
	}
	if len(gw.resumed) != 1 || gw.resumed[0] != id {
		t.Fatalf("resumed=%v", gw.resumed)
	}
	if s.registry.isPaused(id) {
		t.Fatal("registry should clear paused")
	}
}

func TestControlPauseEphemeralRefused(t *testing.T) {
	gw := newFakeGateway()
	gw.pauseErr = status.Error(codes.FailedPrecondition, "session is ephemeral (no workspace PVC)")
	_, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	pause := controlReq(t, ts, "POST", "/sandboxes/"+id+"/pause", map[string]any{})
	if pause.StatusCode != 409 {
		t.Fatalf("ephemeral pause should be 409, got %d: %s", pause.StatusCode, readBody(t, pause))
	}
}

func TestControlMetricsEmpty(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	m := controlReq(t, ts, "GET", "/sandboxes/"+id+"/metrics", nil)
	if m.StatusCode != 200 {
		t.Fatalf("metrics: %d", m.StatusCode)
	}
	if strings.TrimSpace(readBody(t, m)) != "[]" {
		t.Fatal("metrics should be empty (honest absence)")
	}
}

func TestControlTemplateNotFound(t *testing.T) {
	_, ts := testServer(t, newFakeGateway())
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"templateID": "never-registered"})
	if resp.StatusCode != 404 {
		t.Fatalf("unknown template should 404, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestListPagination(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	for i := 0; i < 3; i++ {
		controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	}
	page := controlReq(t, ts, "GET", "/v2/sandboxes?limit=2", nil)
	if page.StatusCode != 200 {
		t.Fatalf("list: %d", page.StatusCode)
	}
	var arr []map[string]any
	_ = json.Unmarshal([]byte(readBody(t, page)), &arr)
	if len(arr) != 2 {
		t.Fatalf("want 2, got %d", len(arr))
	}
	if page.Header.Get("x-next-token") == "" {
		t.Fatal("expected x-next-token on a non-final page")
	}
}

func TestDeadlineKillGC(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": 1})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Time-travel the deadline (in-memory store used by testServer).
	s.registry.(*sandboxRegistry).expireForTest(id)

	s.gcExpired(t.Context())
	if len(gw.destroyed) != 1 || gw.destroyed[0] != id {
		t.Fatalf("expected GC destroy, got %v", gw.destroyed)
	}
	// Protocol-dead after GC: 404.
	again := controlReq(t, ts, "GET", "/sandboxes/"+id, nil)
	if again.StatusCode != 404 {
		t.Fatalf("expected 404 after GC, got %d", again.StatusCode)
	}
}

func TestBearerAuth(t *testing.T) {
	_, ts := testServer(t, newFakeGateway())
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/api/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("Bearer auth should pass, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestBareXAPIKeyAuth(t *testing.T) {
	_, ts := testServer(t, newFakeGateway())
	// Bare key without the e2b_ prefix, via X-API-Key (CubeSandbox's accepted form).
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/api/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-API-Key", "test-key")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 201 {
		t.Fatalf("bare X-API-Key should pass, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestNeverTimeoutEndAtOmitted(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": -1})
	if resp.StatusCode != 201 {
		t.Fatalf("create with -1: %d %s", resp.StatusCode, readBody(t, resp))
	}
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	info := controlReq(t, ts, "GET", "/sandboxes/"+id, nil)
	iv := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, info)), &iv)
	if _, has := iv["endAt"]; has {
		t.Fatalf("never-timeout sandbox must omit endAt, got %v", iv["endAt"])
	}
	// The registry has no deadline.
	if d := s.registry.deadlineOf(id); !d.IsZero() {
		t.Fatalf("never-timeout must have zero deadline, got %v", d)
	}
}

func TestSetTimeoutNever(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": 60})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	timeout := controlReq(t, ts, "POST", "/sandboxes/"+id+"/timeout", map[string]any{"timeout": -1})
	if timeout.StatusCode != 204 {
		t.Fatalf("setTimeout -1: %d %s", timeout.StatusCode, readBody(t, timeout))
	}
	if d := s.registry.deadlineOf(id); !d.IsZero() {
		t.Fatalf("deadline should be cleared, got %v", d)
	}
}

// rootControlReq hits the root-mounted control plane (CubeSandbox style:
// apiUrl points at the bare origin).
func rootControlReq(t *testing.T, ts *httptest.Server, method, path string, body any) *http.Response {
	t.Helper()
	var reader *bytes.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		reader = bytes.NewReader(b)
	} else {
		reader = bytes.NewReader(nil)
	}
	req, err := http.NewRequest(method, ts.URL+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer test-key")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestRootPathControlPlane(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)

	// Root-mounted create (no /e2b/api prefix).
	resp := rootControlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": 600})
	if resp.StatusCode != 201 {
		t.Fatalf("root create: %d %s", resp.StatusCode, readBody(t, resp))
	}
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Root-mounted get / list / kill.
	info := rootControlReq(t, ts, "GET", "/sandboxes/"+id, nil)
	if info.StatusCode != 200 {
		t.Fatalf("root get: %d", info.StatusCode)
	}
	list := rootControlReq(t, ts, "GET", "/v2/sandboxes", nil)
	if list.StatusCode != 200 {
		t.Fatalf("root list: %d", list.StatusCode)
	}
	kill := rootControlReq(t, ts, "DELETE", "/sandboxes/"+id, nil)
	if kill.StatusCode != 204 {
		t.Fatalf("root kill: %d", kill.StatusCode)
	}
}

func TestRootPathHealth(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	resp := rootControlReq(t, ts, "GET", "/health", nil)
	if resp.StatusCode != 200 {
		t.Fatalf("root health: %d", resp.StatusCode)
	}
	body := readBody(t, resp)
	if !strings.Contains(body, `"status":"ok"`) {
		t.Fatalf("health body: %s", body)
	}
}

func TestPrefixedAndRootBothWork(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	// The same sandbox is reachable through both mounts.
	a := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	s1 := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, a)), &s1)
	id := s1["sandboxID"].(string)

	b := rootControlReq(t, ts, "GET", "/sandboxes/"+id, nil)
	if b.StatusCode != 200 {
		t.Fatalf("prefixed-created sandbox via root: %d", b.StatusCode)
	}
}

func TestCapacityRefused409(t *testing.T) {
	gw := newFakeGateway()
	gw.createErr = status.Error(codes.ResourceExhausted, "warm pool full")
	_, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	if resp.StatusCode != 409 {
		t.Fatalf("capacity should map to 409, got %d: %s", resp.StatusCode, readBody(t, resp))
	}
}

func TestLifecycleConflict503RetryAfter(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Sandbox mid-lifecycle: GetSession fails with a paused/unavailable error.
	gw.mu.Lock()
	gw.getErrs[id] = status.Error(codes.FailedPrecondition, "sandbox is pausing")
	gw.mu.Unlock()

	kill := controlReq(t, ts, "DELETE", "/sandboxes/"+id, nil)
	if kill.StatusCode != 503 {
		t.Fatalf("mid-lifecycle kill should be 503, got %d: %s", kill.StatusCode, readBody(t, kill))
	}
	if kill.Header.Get("Retry-After") != "2" {
		t.Fatalf("Retry-After=%q, want 2", kill.Header.Get("Retry-After"))
	}
}

func TestGoneSandbox410or404(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Destroy it once, then the second kill must 404 (SDK's kill()===false).
	kill := controlReq(t, ts, "DELETE", "/sandboxes/"+id, nil)
	if kill.StatusCode != 204 {
		t.Fatalf("first kill: %d", kill.StatusCode)
	}
	again := controlReq(t, ts, "DELETE", "/sandboxes/"+id, nil)
	if again.StatusCode != 404 {
		t.Fatalf("second kill should 404, got %d", again.StatusCode)
	}
}

func TestAutoPauseAtDeadline(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{"timeout": 1, "autoPause": true})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Time-travel the deadline; the GC must call PauseSession (not destroy).
	s.registry.(*sandboxRegistry).expireForTest(id)

	s.gcExpired(t.Context())
	if len(gw.paused) != 1 || gw.paused[0] != id {
		t.Fatalf("expected PauseSession, got paused=%v destroyed=%v", gw.paused, gw.destroyed)
	}
	if len(gw.destroyed) != 0 {
		t.Fatalf("autoPause must not destroy, got %v", gw.destroyed)
	}
	// The sandbox is still gettable (paused state).
	info := controlReq(t, ts, "GET", "/sandboxes/"+id, nil)
	if info.StatusCode != 200 {
		t.Fatalf("paused sandbox should still be gettable, got %d", info.StatusCode)
	}
	iv := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, info)), &iv)
	if iv["state"] != "paused" {
		t.Fatalf("state=%v, want paused", iv["state"])
	}
}

func TestConnectAutoResumePaused(t *testing.T) {
	gw := newFakeGateway()
	_, ts := testServer(t, gw)
	resp := controlReq(t, ts, "POST", "/sandboxes", map[string]any{})
	session := map[string]any{}
	_ = json.Unmarshal([]byte(readBody(t, resp)), &session)
	id := session["sandboxID"].(string)

	// Manually pause via the API.
	pause := controlReq(t, ts, "POST", "/sandboxes/"+id+"/pause", map[string]any{})
	if pause.StatusCode != 204 {
		t.Fatalf("pause: %d", pause.StatusCode)
	}

	// Connect auto-resumes → 201.
	conn := controlReq(t, ts, "POST", "/sandboxes/"+id+"/connect", map[string]any{"timeout": 300})
	if conn.StatusCode != 201 {
		t.Fatalf("connect to paused should be 201, got %d: %s", conn.StatusCode, readBody(t, conn))
	}
	if len(gw.resumed) != 1 || gw.resumed[0] != id {
		t.Fatalf("resumed=%v", gw.resumed)
	}

	// Connect to a running sandbox → 200.
	conn2 := controlReq(t, ts, "POST", "/sandboxes/"+id+"/connect", map[string]any{"timeout": 300})
	if conn2.StatusCode != 200 {
		t.Fatalf("connect to running should be 200, got %d", conn2.StatusCode)
	}
}

func TestEnvdAutoResumePaused(t *testing.T) {
	gw := newFakeGateway()
	s, ts := testServer(t, gw)
	id := createSandboxID(t, ts)

	// Pause it.
	pause := controlReq(t, ts, "POST", "/sandboxes/"+id+"/pause", map[string]any{})
	if pause.StatusCode != 204 {
		t.Fatalf("pause: %d", pause.StatusCode)
	}

	// envd filesystem Stat on a paused sandbox auto-resumes (the sandbox
	// wakes before the request lands, CubeSandbox auto_resume).
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/envd/filesystem.Filesystem/Stat", bytes.NewReader([]byte(`{"path":"x"}`)))
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// The Stat itself 404s (the fake has no stat output for that path) — what
	// matters is that the paused sandbox was auto-resumed and the request
	// reached the exec path instead of being refused as unavailable.
	resp.Body.Close()
	if len(gw.resumed) != 1 || gw.resumed[0] != id {
		t.Fatalf("expected auto-resume via envd, got %v", gw.resumed)
	}
	if s.registry.isPaused(id) {
		t.Fatal("registry should no longer mark paused after auto-resume")
	}
}
