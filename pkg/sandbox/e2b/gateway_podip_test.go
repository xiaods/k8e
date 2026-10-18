package e2b

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// asyncPodIPGateway models the real resume path: ResumeSession marks the
// session Active but the pod IP is only assigned a few polls later, exactly as
// the scheduler behaves. A single GetSession immediately after resume therefore
// reports an empty PodIp — which is what made the E2B layer answer
// "session ... has no pod IP".
type asyncPodIPGateway struct {
	*fakeGateway
	mu       sync.Mutex
	gets     int
	assignAt int // GetSession call number at/after which PodIp appears
}

func (g *asyncPodIPGateway) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	g.mu.Lock()
	g.gets++
	n := g.gets
	assign := n >= g.assignAt
	g.mu.Unlock()

	sess, err := g.fakeGateway.GetSession(ctx, req)
	if err != nil {
		return nil, err
	}
	if assign && sess.GetPodIp() == "" {
		// Simulate the scheduler finally assigning an address.
		g.fakeGateway.mu.Lock()
		g.fakeGateway.sessions[req.SessionId].PodIp = "10.42.0.99"
		g.fakeGateway.mu.Unlock()
		return g.fakeGateway.GetSession(ctx, req)
	}
	return sess, nil
}

// PauseSession drops the pod address, mirroring the real controller: a paused
// sandbox has no running pod, so status.podIP is cleared until it is resumed.
// Without this the fake would leave a stale address behind and the pod-IP race
// would never be exercised.
func (g *asyncPodIPGateway) PauseSession(ctx context.Context, req *pb.PauseSessionRequest) (*pb.PauseSessionResponse, error) {
	resp, err := g.fakeGateway.PauseSession(ctx, req)
	if err != nil {
		return nil, err
	}
	g.fakeGateway.mu.Lock()
	if s, ok := g.fakeGateway.sessions[req.SessionId]; ok {
		s.PodIp = ""
	}
	g.fakeGateway.mu.Unlock()
	return resp, nil
}

// listDir issues an authenticated envd ListDir, which is the request that
// triggers wakeForTraffic's auto-resume and then must reach sandboxd by pod IP.
func listDir(t *testing.T, ts *httptest.Server, s *Server, id string) int {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost,
		ts.URL+"/e2b/envd/filesystem.Filesystem/ListDir",
		strings.NewReader(`{"path":"/workspace","depth":1}`))
	if err != nil {
		t.Fatal(err)
	}
	for k, v := range envdHeaders(t, s, id) {
		req.Header.Set(k, v)
	}
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("ListDir after resume: %d %s", resp.StatusCode, readBody(t, resp))
	}
	return resp.StatusCode
}

// TestWakeForTrafficWaitsForPodIPAfterResume is the regression test for the E2B
// bug where the first call after a pause failed with "session has no pod IP".
func TestWakeForTrafficWaitsForPodIPAfterResume(t *testing.T) {
	base := newFakeGateway()
	gw := &asyncPodIPGateway{fakeGateway: base, assignAt: 6}
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	defer ts.Close()

	id := createSandboxID(t, ts)

	// Seed the workspace so ListDir has something real to stat.
	gw.mu.Lock()
	gw.files["/workspace/probe.txt"] = "x"
	gw.mu.Unlock()
	stub.mu.Lock()
	stub.dirs["/workspace/"] = true
	stub.files["/workspace/probe.txt"] = "x"
	stub.mu.Unlock()

	// Pause: the pod address goes away and the phase becomes Paused.
	if resp := controlReq(t, ts, "POST", "/sandboxes/"+id+"/pause", nil); resp.StatusCode != 204 {
		t.Fatalf("pause: %d %s", resp.StatusCode, readBody(t, resp))
	}

	// The call must auto-resume AND resolve to a usable pod IP. Before the fix
	// it returned the pre-resume view whose PodIp was empty, and the sandboxd
	// client answered "session ... has no pod IP".
	listDir(t, ts, s, id)

	if got := len(base.resumed); got != 1 {
		t.Fatalf("expected exactly one auto-resume, got %d", got)
	}
	if ip := base.sessions[id].GetPodIp(); ip == "" {
		t.Fatal("auto-resume must leave the session with an assigned pod IP")
	}
}

// TestWakeForTrafficRunningWithoutPodIPWaits covers the explicit-resume case:
// the phase is already running while the pod is still being created.
func TestWakeForTrafficRunningWithoutPodIPWaits(t *testing.T) {
	base := newFakeGateway()
	gw := &asyncPodIPGateway{fakeGateway: base, assignAt: 3}
	stub := newSandboxdStub()
	s, ts := testServerWithSandboxd(t, gw, stub)
	defer ts.Close()

	id := createSandboxID(t, ts)

	gw.mu.Lock()
	gw.files["/workspace/probe.txt"] = "x"
	gw.mu.Unlock()
	stub.mu.Lock()
	stub.dirs["/workspace/"] = true
	stub.files["/workspace/probe.txt"] = "x"
	stub.mu.Unlock()

	// A running session whose pod has no address yet (what an explicit resume
	// leaves behind) must not fail the request outright.
	base.mu.Lock()
	base.sessions[id].PodIp = ""
	base.mu.Unlock()

	listDir(t, ts, s, id)

	if len(base.resumed) != 0 {
		t.Fatalf("a running session must not be resumed again, got %d resumes", len(base.resumed))
	}
	if ip := base.sessions[id].GetPodIp(); ip == "" {
		t.Fatal("the call must have waited for the pod IP to be assigned")
	}
}

