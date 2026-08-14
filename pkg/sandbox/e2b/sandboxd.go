package e2b

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// sandboxdClient talks directly to the in-pod sandboxd HTTP API (:2024) for
// the native operations that have no gRPC gateway equivalent (KIP-18
// "ability downshift"): filesystem stat/mkdir/move/remove and process
// stdin/signal control. The gateway RPCs (Exec, ReadFile, WriteFile) cover
// the rest; these endpoints are what the e2b layer needs on top.
//
// The pod IP comes from GetSession — the same source the gateway uses to
// reach sandboxd. sandboxd has no auth (KIP-16 M10 tracks that gap), but the
// pod IP is not routable from outside the cluster, and the e2b-server already
// holds the session credential, so this preserves the existing trust model.
type sandboxdClient struct {
	gw     Gateway
	client *http.Client
	// baseURL, when non-empty, overrides the per-session pod-IP resolution.
	// Tests inject an httptest server here; production leaves it empty so
	// every call resolves the live pod IP from the gateway.
	baseURL string
}

func newSandboxdClient(gw Gateway) *sandboxdClient {
	return &sandboxdClient{
		gw:     gw,
		client: &http.Client{Timeout: 30 * time.Second},
	}
}

// podIP resolves the sandbox pod IP via the gateway.
func (s *sandboxdClient) podIP(ctx context.Context, sessionID string) (string, error) {
	sess, err := s.gw.GetSession(ctx, &pb.GetSessionRequest{SessionId: sessionID})
	if err != nil {
		return "", err
	}
	if sess.PodIp == "" {
		return "", fmt.Errorf("session %s has no pod IP", sessionID)
	}
	return sess.PodIp, nil
}

// post sends a JSON POST to a sandboxd path and decodes the JSON response.
func (s *sandboxdClient) post(ctx context.Context, sessionID, path string, body any, out any) error {
	base := s.baseURL
	if base == "" {
		ip, err := s.podIP(ctx, sessionID)
		if err != nil {
			return err
		}
		base = fmt.Sprintf("http://%s:2024", ip)
	}
	data, _ := json.Marshal(body)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, base+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		var e struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &e)
		if e.Error == "already exists" {
			return errAlreadyExists
		}
		if resp.StatusCode == http.StatusNotFound {
			return errNotFound
		}
		return fmt.Errorf("sandboxd %s: http %d: %s", path, resp.StatusCode, e.Error)
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}

var (
	errAlreadyExists = fmt.Errorf("already exists")
	errNotFound      = fmt.Errorf("not found")
)

// statEntry is the sandboxd /files/stat response.
type statEntry struct {
	Type          string `json:"type"`
	Size          int64  `json:"size"`
	Mode          string `json:"mode"`
	UID           int    `json:"uid"`
	GID           int    `json:"gid"`
	Mtime         int64  `json:"mtime"`
	Name          string `json:"name"`
	SymlinkTarget string `json:"symlink_target"`
}

// stat resolves path metadata (E2B filesystem.Filesystem/Stat).
func (s *sandboxdClient) stat(ctx context.Context, sessionID, path string) (*statEntry, error) {
	var out statEntry
	if err := s.post(ctx, sessionID, "/files/stat", map[string]string{"path": path}, &out); err != nil {
		return nil, err
	}
	return &out, nil
}

// mkdir creates a directory (E2B filesystem.Filesystem/MakeDir).
func (s *sandboxdClient) mkdir(ctx context.Context, sessionID, path string) error {
	return s.post(ctx, sessionID, "/files/mkdir", map[string]string{"path": path}, nil)
}

// move renames a path (E2B filesystem.Filesystem/Move).
func (s *sandboxdClient) move(ctx context.Context, sessionID, source, destination string) error {
	return s.post(ctx, sessionID, "/files/move",
		map[string]string{"source": source, "destination": destination}, nil)
}

// remove deletes a path recursively (E2B filesystem.Filesystem/Remove).
func (s *sandboxdClient) remove(ctx context.Context, sessionID, path string) error {
	return s.post(ctx, sessionID, "/files/remove", map[string]string{"path": path}, nil)
}

// sendStdin writes bytes to a running process's stdin (E2B process SendInput).
// pid is sent as a JSON number: sandboxd's handler parses it into a pid_t.
func (s *sandboxdClient) sendStdin(ctx context.Context, sessionID string, pid int, data []byte) error {
	body := map[string]any{
		"pid":  pid,
		"data": base64.StdEncoding.EncodeToString(data),
	}
	return s.post(ctx, sessionID, "/exec/stdin", body, nil)
}

// closeStdin closes a process's stdin, signalling EOF (E2B process CloseStdin).
func (s *sandboxdClient) closeStdin(ctx context.Context, sessionID string, pid int) error {
	return s.post(ctx, sessionID, "/exec/stdin/close", map[string]any{"pid": pid}, nil)
}

// signal sends a signal to a running process (E2B process SendSignal).
func (s *sandboxdClient) signal(ctx context.Context, sessionID string, pid int, sig string) error {
	return s.post(ctx, sessionID, "/exec/signal",
		map[string]any{"pid": pid, "signal": sig}, nil)
}

// get sends a GET to a sandboxd path and decodes the JSON response.
func (s *sandboxdClient) get(ctx context.Context, sessionID, path string, out any) error {
	base := s.baseURL
	if base == "" {
		ip, err := s.podIP(ctx, sessionID)
		if err != nil {
			return err
		}
		base = fmt.Sprintf("http://%s:2024", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, base+path, nil)
	if err != nil {
		return err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return err
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("sandboxd %s: http %d: %s", path, resp.StatusCode, string(raw))
	}
	if out != nil {
		if err := json.Unmarshal(raw, out); err != nil {
			return err
		}
	}
	return nil
}

// sandboxProcess is one entry of the sandbox-owned process table
// (GET /exec/processes). Pids are the sandbox's own — node-independent, so
// the E2B Process/List view is consistent across control-plane nodes.
type sandboxProcess struct {
	PID    int    `json:"pid"`
	Alive  bool   `json:"alive"`
	Config string `json:"config"`
}

// processList returns the sandbox-owned process table (E2B Process/List).
func (s *sandboxdClient) processList(ctx context.Context, sessionID string) ([]sandboxProcess, error) {
	var out struct {
		Processes []sandboxProcess `json:"processes"`
	}
	if err := s.get(ctx, sessionID, "/exec/processes", &out); err != nil {
		return nil, err
	}
	return out.Processes, nil
}

// attach replays a process's buffered output (E2B Process/Connect) via
// GET /exec/attach?pid=N. Pids are the sandbox's own, so an attach from any
// control-plane node addresses the same process (multi-node embedded
// architecture). Returns the raw SSE body.
func (s *sandboxdClient) attach(ctx context.Context, sessionID string, pid int) ([]byte, error) {
	base := s.baseURL
	if base == "" {
		ip, err := s.podIP(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		base = fmt.Sprintf("http://%s:2024", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/exec/attach?pid=%d", base, pid), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sandboxd attach %d: http %d: %s", pid, resp.StatusCode, string(raw))
	}
	return raw, nil
}

// createWatcher starts an inotify watch on a path (E2B
// filesystem.Filesystem/CreateWatcher). Returns the watcher id.
func (s *sandboxdClient) createWatcher(ctx context.Context, sessionID, path string) (int, error) {
	var out struct {
		WatcherID int `json:"watcher_id"`
	}
	if err := s.post(ctx, sessionID, "/watch/create", map[string]string{"path": path}, &out); err != nil {
		return 0, err
	}
	return out.WatcherID, nil
}

// getWatcherEvents returns events since the last call (E2B
// filesystem.Filesystem/GetWatcherEvents, WatchHandle.get_new_events).
type watchEvent struct {
	Name string `json:"name"`
	Type int    `json:"type"`
}

func (s *sandboxdClient) getWatcherEvents(ctx context.Context, sessionID string, watcherID int) ([]watchEvent, error) {
	base := s.baseURL
	if base == "" {
		ip, err := s.podIP(ctx, sessionID)
		if err != nil {
			return nil, err
		}
		base = fmt.Sprintf("http://%s:2024", ip)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		fmt.Sprintf("%s/watch/events?watcher_id=%d", base, watcherID), nil)
	if err != nil {
		return nil, err
	}
	resp, err := s.client.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("sandboxd watch events %d: http %d: %s", watcherID, resp.StatusCode, string(raw))
	}
	var out struct {
		Events []watchEvent `json:"events"`
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return nil, err
	}
	return out.Events, nil
}

// removeWatcher stops a watch (E2B filesystem.Filesystem/RemoveWatcher).
func (s *sandboxdClient) removeWatcher(ctx context.Context, sessionID string, watcherID int) error {
	return s.post(ctx, sessionID, "/watch/remove", map[string]int{"watcher_id": watcherID}, nil)
}
