package e2b

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/client"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// ExecStreamReader is the subset of a gRPC server-streaming client the e2b
// layer consumes. The real gateway client satisfies it; tests fake it.
type ExecStreamReader interface {
	Recv() (*pb.ExecStreamResponse, error)
}

// TerminalStreamReader is the subset of the gRPC server-streaming client for
// TerminalStream: Recv returns the next terminal frame (data or exit).
type TerminalStreamReader interface {
	Recv() (*pb.TerminalStreamResponse, error)
}

// Gateway is the k8e backend contract the E2B layer translates to: the
// sandbox gRPC gateway. A narrow subset of pb.SandboxServiceClient so tests
// can fake it with a small stub.
type Gateway interface {
	CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.CreateSessionResponse, error)
	GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error)
	ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error)
	DestroySession(ctx context.Context, req *pb.DestroySessionRequest) (*pb.DestroySessionResponse, error)
	Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error)
	ExecStream(ctx context.Context, req *pb.ExecRequest) (ExecStreamReader, error)
	WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error)
	ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error)
	ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error)
	PauseSession(ctx context.Context, req *pb.PauseSessionRequest) (*pb.PauseSessionResponse, error)
	ResumeSession(ctx context.Context, req *pb.ResumeSessionRequest) (*pb.ResumeSessionResponse, error)
	// KIP-19 PTY terminal primitive surface (closed by the E2B pty.* compat).
	CreateTerminal(ctx context.Context, req *pb.CreateTerminalRequest) (*pb.CreateTerminalResponse, error)
	TerminalStream(ctx context.Context, req *pb.TerminalStreamRequest) (TerminalStreamReader, error)
	TerminalWrite(ctx context.Context, req *pb.TerminalWriteRequest) (*pb.TerminalWriteResponse, error)
	TerminalResize(ctx context.Context, req *pb.TerminalResizeRequest) (*pb.TerminalResizeResponse, error)
	TerminalSignal(ctx context.Context, req *pb.TerminalSignalRequest) (*pb.TerminalSignalResponse, error)
	TerminalDestroy(ctx context.Context, req *pb.TerminalDestroyRequest) (*pb.TerminalDestroyResponse, error)
	// KIP-24: gateway-side expose registry (used by the /k8e/expose proxy
	// route to authorize which ports may be proxied into the pod).
	ListExposed(ctx context.Context, req *pb.ListExposedRequest) (*pb.ListExposedResponse, error)
}

// grpcGateway adapts the real k8e gRPC client to the Gateway contract.
type grpcGateway struct {
	client *client.Client
}

func (g *grpcGateway) CreateSession(ctx context.Context, req *pb.CreateSessionRequest) (*pb.CreateSessionResponse, error) {
	return g.client.SandboxServiceClient.CreateSession(ctx, req)
}

func (g *grpcGateway) GetSession(ctx context.Context, req *pb.GetSessionRequest) (*pb.GetSessionResponse, error) {
	return g.client.SandboxServiceClient.GetSession(ctx, req)
}

func (g *grpcGateway) ListSessions(ctx context.Context, req *pb.ListSessionsRequest) (*pb.ListSessionsResponse, error) {
	return g.client.SandboxServiceClient.ListSessions(ctx, req)
}

func (g *grpcGateway) DestroySession(ctx context.Context, req *pb.DestroySessionRequest) (*pb.DestroySessionResponse, error) {
	return g.client.SandboxServiceClient.DestroySession(ctx, req)
}

func (g *grpcGateway) Exec(ctx context.Context, req *pb.ExecRequest) (*pb.ExecResponse, error) {
	return g.client.SandboxServiceClient.Exec(ctx, req)
}

func (g *grpcGateway) ExecStream(ctx context.Context, req *pb.ExecRequest) (ExecStreamReader, error) {
	return g.client.SandboxServiceClient.ExecStream(ctx, req)
}

func (g *grpcGateway) WriteFile(ctx context.Context, req *pb.WriteFileRequest) (*pb.WriteFileResponse, error) {
	return g.client.SandboxServiceClient.WriteFile(ctx, req)
}

func (g *grpcGateway) ReadFile(ctx context.Context, req *pb.ReadFileRequest) (*pb.ReadFileResponse, error) {
	return g.client.SandboxServiceClient.ReadFile(ctx, req)
}

func (g *grpcGateway) ListFiles(ctx context.Context, req *pb.ListFilesRequest) (*pb.ListFilesResponse, error) {
	return g.client.SandboxServiceClient.ListFiles(ctx, req)
}

func (g *grpcGateway) PauseSession(ctx context.Context, req *pb.PauseSessionRequest) (*pb.PauseSessionResponse, error) {
	return g.client.SandboxServiceClient.PauseSession(ctx, req)
}

func (g *grpcGateway) ResumeSession(ctx context.Context, req *pb.ResumeSessionRequest) (*pb.ResumeSessionResponse, error) {
	return g.client.SandboxServiceClient.ResumeSession(ctx, req)
}

func (g *grpcGateway) CreateTerminal(ctx context.Context, req *pb.CreateTerminalRequest) (*pb.CreateTerminalResponse, error) {
	return g.client.SandboxServiceClient.CreateTerminal(ctx, req)
}

func (g *grpcGateway) TerminalStream(ctx context.Context, req *pb.TerminalStreamRequest) (TerminalStreamReader, error) {
	return g.client.SandboxServiceClient.TerminalStream(ctx, req)
}

func (g *grpcGateway) TerminalWrite(ctx context.Context, req *pb.TerminalWriteRequest) (*pb.TerminalWriteResponse, error) {
	return g.client.SandboxServiceClient.TerminalWrite(ctx, req)
}

func (g *grpcGateway) TerminalResize(ctx context.Context, req *pb.TerminalResizeRequest) (*pb.TerminalResizeResponse, error) {
	return g.client.SandboxServiceClient.TerminalResize(ctx, req)
}

func (g *grpcGateway) TerminalSignal(ctx context.Context, req *pb.TerminalSignalRequest) (*pb.TerminalSignalResponse, error) {
	return g.client.SandboxServiceClient.TerminalSignal(ctx, req)
}

func (g *grpcGateway) TerminalDestroy(ctx context.Context, req *pb.TerminalDestroyRequest) (*pb.TerminalDestroyResponse, error) {
	return g.client.SandboxServiceClient.TerminalDestroy(ctx, req)
}

func (g *grpcGateway) ListExposed(ctx context.Context, req *pb.ListExposedRequest) (*pb.ListExposedResponse, error) {
	return g.client.SandboxServiceClient.ListExposed(ctx, req)
}

// --- session views --------------------------------------------------------

// sandboxState is the logical E2B state of a session.
type sandboxState string

const (
	stateRunning sandboxState = "running"
	statePaused  sandboxState = "paused"
	stateDead    sandboxState = "dead"
)

// sessionState maps a k8e session phase to the logical E2B state. A session
// is `running` iff the k8e CRD phase is Active (pod claimed and serving);
// `paused` maps to E2B's paused state (pod released, PVC survives, resume
// restores it). Everything colder or protocol-dead is `dead` — the SDK
// treats it as "sandbox not found / timed out".
func sessionState(phase string) sandboxState {
	switch phase {
	case "Active", "":
		// Empty phase from a just-created session reports running; the
		// gateway polls for the pod IP on first use anyway.
		return stateRunning
	case "Paused":
		return statePaused
	default:
		return stateDead
	}
}

// sessionView is what create/connect answer with.
func (s *Server) sessionView(sess *pb.GetSessionResponse) map[string]any {
	return map[string]any{
		"sandboxID":       sess.SessionId,
		"clientID":        s.nodeID,
		"templateID":      s.templateIDFor(sess.RuntimeClass),
		"envdVersion":     EnvdVersion,
		"envdAccessToken": mintEnvdToken(s.signingSecret, sess.SessionId),
	}
}

// infoView is what getInfo and list answer with.
func (s *Server) infoView(sess *pb.GetSessionResponse, state sandboxState) map[string]any {
	now := time.Now()
	created := now
	if e, ok := s.registry.get(sess.SessionId); ok && !e.createdAt.IsZero() {
		created = e.createdAt
	}
	meta := map[string]string{}
	if e, ok := s.registry.get(sess.SessionId); ok {
		for k, v := range e.metadata {
			meta[k] = v
		}
	}
	view := map[string]any{
		"sandboxID":   sess.SessionId,
		"clientID":    s.nodeID,
		"templateID":  s.templateIDFor(sess.RuntimeClass),
		"metadata":    meta,
		"state":       state,
		"startedAt":   created.Format(time.RFC3339),
		"cpuCount":    s.defaultCPUs,
		"memoryMB":    s.defaultMemoryMB,
		"diskSizeMB":  s.defaultDiskMB,
		"envdVersion": EnvdVersion,
	}
	// endAt is the projected kill deadline. The E2B SDK data model treats
	// it as a required field (it parses it with isoparse), so never-timeout
	// sandboxes answer with a far-future sentinel instead of omitting it —
	// omission made getInfo()/list() fail with KeyError in the SDK.
	if endAt := s.registry.deadlineOf(sess.SessionId); !endAt.IsZero() {
		view["endAt"] = endAt.Format(time.RFC3339)
	} else {
		view["endAt"] = time.Now().Add(100 * 365 * 24 * time.Hour).UTC().Format(time.RFC3339)
	}
	return view
}

// templateIDFor is the inverse of create's template resolution: a session
// whose runtime is a known template name echoes that name; anything else
// falls back to 'base' (the honest default).
func (s *Server) templateIDFor(runtimeClass string) string {
	if runtimeClass == "" {
		return "base"
	}
	if _, ok := s.runtimes[runtimeClass]; ok {
		return runtimeClass
	}
	return "base"
}

// logf writes a log line when a hook is installed (logrus in production,
// no-op in tests). Kept as a method-free helper to avoid shadowing the
// Server.logf field.
func (s *Server) log(format string, args ...any) {
	if s.logf != nil {
		s.logf(format, args...)
	}
}

// wakeForTraffic resolves a sandbox for envd use, auto-resuming a paused one
// (CubeSandbox's auto_resume: any request to a paused sandbox wakes it before
// it lands — callers never see the pause). Returns the live session view and
// whether a resume happened.
func (s *Server) wakeForTraffic(r *http.Request, sandboxID string) (*pb.GetSessionResponse, bool, *E2bError) {
	sess, err := s.gw.GetSession(r.Context(), &pb.GetSessionRequest{SessionId: sandboxID})
	if err != nil {
		return nil, false, connectError("unavailable", "sandbox is not running")
	}
	st := sessionState(sess.GetPhase())
	if st == stateDead {
		return nil, false, connectError("unavailable", "sandbox is not running")
	}
	if st == statePaused {
		if _, rerr := s.gw.ResumeSession(r.Context(), &pb.ResumeSessionRequest{SessionId: sandboxID}); rerr != nil {
			return nil, false, connectError("unavailable", "auto-resume failed: "+rerr.Error())
		}
		s.registry.markPaused(sandboxID, false)
		// Resume re-creates the pod asynchronously, so the session view we
		// already hold still reports the pre-pause state: PodIp is empty
		// because the old pod is gone and the new one has not been assigned an
		// address yet. Callers address sandboxd by pod IP, so returning that
		// stale view made every post-resume call fail with
		// "session ... has no pod IP" (E2B watch_dir, files.stat, ...).
		//
		// Poll until the new pod reports an address. Bounded: a resume that
		// never schedules a pod is an infrastructure failure, and blocking the
		// request forever is worse than reporting it.
		fresh, perr := s.waitForPodIP(r, sandboxID)
		if perr != nil {
			return nil, false, connectError("unavailable", "sandbox resumed but has no pod IP: "+perr.Error())
		}
		return fresh, true, nil
	}
	// Running: normally return the live view. But a sandbox that an explicit
	// resume() just brought back is already marked running while its pod is
	// still being created, so its PodIp is momentarily empty. Callers address
	// sandboxd by pod IP, so wait briefly rather than failing the request —
	// otherwise the first files/process call after resume() reports
	// "has no pod IP".
	if st == stateRunning && sess.GetPodIp() == "" {
		if fresh, perr := s.waitForPodIP(r, sandboxID); perr == nil {
			return fresh, false, nil
		}
	}
	return sess, false, nil
}

// waitForPodIP polls the session until the scheduler has assigned the
// (re)created pod an address, returning the fresh session view.
//
// Poll rather than a single read because resume is asynchronous: the pod is
// claimed and scheduled after ResumeSession returns, and the pod IP cache in
// the sandboxd client is deliberately short-lived (e2bPodIPTTL) so it cannot
// paper over the gap.
func (s *Server) waitForPodIP(r *http.Request, sandboxID string) (*pb.GetSessionResponse, error) {
	const (
		// podIPWaitTimeout bounds the whole wait. Warm-pool claims normally
		// land in well under a second; a cold pod needs scheduling time.
		podIPWaitTimeout = 30 * time.Second
		podIPWaitPoll    = 200 * time.Millisecond
	)
	deadline := time.Now().Add(podIPWaitTimeout)
	for {
		sess, err := s.gw.GetSession(r.Context(), &pb.GetSessionRequest{SessionId: sandboxID})
		if err == nil && sess.GetPodIp() != "" {
			// The pod changed, so any cached address for this session is stale.
			s.invalidatePodIP(sandboxID)
			return sess, nil
		}
		if !time.Now().Before(deadline) {
			if err != nil {
				return nil, err
			}
			return nil, fmt.Errorf("pod IP not assigned after %s", podIPWaitTimeout)
		}
		time.Sleep(podIPWaitPoll)
	}
}

// invalidatePodIP drops any cached sandboxd address for a session. Pause,
// resume and pod replacement all change the address, so the short pod-IP cache
// must be dropped at those boundaries or it keeps answering with an IP that no
// longer exists.
func (s *Server) invalidatePodIP(sandboxID string) {
	if s.sandboxd != nil {
		s.sandboxd.invalidatePodIP(sandboxID)
	}
}

// unixNow returns the current unix time in seconds.
func unixNow() int64 { return time.Now().Unix() }

// jsonWriter writes a JSON response body.
func jsonWriter(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(body)
}

// errorBody renders an E2bError in the wire dialect.
func errorBody(e *E2bError) map[string]any {
	return map[string]any{"code": e.Code, "message": e.Message}
}

// trimSSEFraming strips the `data: ` / `\n\n` framing that sandboxd's
// /exec/stream SSE leaks through the gateway into gRPC chunks. The gateway
// proxies the raw body, so each chunk may contain zero or more SSE frames
// (and a frame may straddle chunks). The output is the concatenation of the
// frame payloads.
type sseFramer struct {
	buf []byte
}

func (f *sseFramer) push(chunk string) []byte {
	frames := f.pushFrames(chunk)
	var out []byte
	for _, frame := range frames {
		out = append(out, frame...)
	}
	return out
}

// pushFrames appends a chunk and returns each complete SSE frame payload as a
// separate byte slice (preserving frame boundaries). A partial trailing frame
// stays in the buffer for the next call or drain.
func (f *sseFramer) pushFrames(chunk string) [][]byte {
	f.buf = append(f.buf, chunk...)
	var frames [][]byte
	for {
		idx := indexOf(f.buf, "\n\n")
		if idx < 0 {
			break
		}
		line := f.buf[:idx]
		f.buf = f.buf[idx+2:]
		payload := stripDataPrefix(line)
		cp := append([]byte(nil), payload...)
		frames = append(frames, cp)
	}
	return frames
}

func (f *sseFramer) drain() []byte {
	out := stripDataPrefix(f.buf)
	f.buf = nil
	return out
}

func stripDataPrefix(line []byte) []byte {
	l := strings.TrimPrefix(string(line), "data:")
	return []byte(strings.TrimPrefix(l, " "))
}

func indexOf(b []byte, sub string) int {
	return bytes.Index(b, []byte(sub))
}

// resolveSandboxPath maps an E2B-relative path onto the k8e workspace root.
// E2B's default user home (/home/user/...) is /workspace in k8e; '..' and
// absolute escape attempts are rejected.
func resolveSandboxPath(raw string) (string, error) {
	if raw == "" {
		return "", fmt.Errorf("empty path")
	}
	p := raw
	if strings.HasPrefix(p, "/workspace") {
		p = strings.TrimPrefix(p, "/workspace")
		p = strings.TrimPrefix(p, "/")
	}
	for _, part := range strings.Split(p, "/") {
		if part == ".." || part == "." {
			return "", fmt.Errorf("path traversal not allowed: %s", raw)
		}
	}
	return "/workspace/" + strings.TrimPrefix(p, "/"), nil
}
