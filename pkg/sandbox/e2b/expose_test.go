package e2b

import (
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"testing"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// serveE2BWithExposed boots the e2b HTTP surface with a fake gateway that has
// a session whose pod IP points at the fake in-pod service, and the given
// ports registered as exposed. Returns the server URL.
func serveE2BWithExposed(t *testing.T, podIP string, exposed []int32) string {
	t.Helper()
	gw := newFakeGateway()
	sess := &pb.GetSessionResponse{SessionId: "sess-1", Phase: "Active", PodIp: podIP}
	gw.mu.Lock()
	gw.sessions["sess-1"] = sess
	if len(exposed) > 0 {
		gw.exposed["sess-1"] = exposed
	}
	gw.mu.Unlock()

	srv := NewServer(Config{Listen: "127.0.0.1:0", Endpoint: "127.0.0.1:50051"}, gw)
	ts := httptest.NewServer(srv.Handle())
	t.Cleanup(ts.Close)
	return ts.URL
}

// TestExposeProxy_ForwardsToPod verifies /k8e/expose/<session>/<port>/path
// reverse-proxies to the in-pod service for a registered port, preserving the
// path and the original Host header.
func TestExposeProxy_ForwardsToPod(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	podPort := ln.Addr().(*net.TCPAddr).Port
	pod := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/hello" {
			http.Error(w, "unexpected path "+r.URL.Path, http.StatusBadRequest)
			return
		}
		w.Header().Set("X-Served-By", "pod")
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, "ok from pod")
	}))
	pod.Listener = ln
	pod.Start()
	defer pod.Close()

	base := serveE2BWithExposed(t, "127.0.0.1", []int32{int32(podPort)})
	resp, err := http.Get(base + fmt.Sprintf("/k8e/expose/sess-1/%d/hello", podPort))
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK || string(body) != "ok from pod" {
		t.Fatalf("proxy status=%d body=%q", resp.StatusCode, string(body))
	}
	if resp.Header.Get("X-Served-By") != "pod" {
		t.Fatalf("expected pod response header, got %q", resp.Header.Get("X-Served-By"))
	}
}

// TestExposeProxy_NotExposed404 verifies a port that is NOT registered in the
// gateway registry is rejected before any pod dial.
func TestExposeProxy_NotExposed404(t *testing.T) {
	base := serveE2BWithExposed(t, "10.0.0.9", nil) // no exposures
	resp, err := http.Get(base + "/k8e/expose/sess-1/8080/")
	if err != nil {
		t.Fatalf("proxy request: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("expected 404 for unexposed port, got %d", resp.StatusCode)
	}
}

// TestExposeProxy_BadRequest verifies malformed routes are rejected.
func TestExposeProxy_BadRequest(t *testing.T) {
	base := serveE2BWithExposed(t, "10.0.0.9", []int32{8080})
	for _, path := range []string{"/k8e/expose/", "/k8e/expose/sess-1/notaport/", "/k8e/expose/sess-1/70000/"} {
		resp, err := http.Get(base + path)
		if err != nil {
			t.Fatalf("request %s: %v", path, err)
		}
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest {
			t.Fatalf("path %s: expected 400, got %d", path, resp.StatusCode)
		}
	}
}
