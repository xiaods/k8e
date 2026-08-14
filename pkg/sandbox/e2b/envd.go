package e2b

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// handleEnvdHealth implements GET /e2b/envd/health: 204 running, 502 not.
func (s *Server) handleEnvdHealth(w http.ResponseWriter, r *http.Request) {
	id := r.Header.Get("E2b-Sandbox-Id")
	if id == "" {
		w.WriteHeader(http.StatusBadRequest)
		return
	}
	sess, err := s.gw.GetSession(r.Context(), &pb.GetSessionRequest{SessionId: id})
	if err != nil || sessionState(sess.GetPhase()) == stateDead {
		jsonWriter(w, http.StatusBadGateway, errorBody(connectError("unavailable", "sandbox is not running")))
		return
	}
	if e, ok := s.registry.get(id); ok && !e.deadlineAt.IsZero() && unixNow()*1000 > e.deadlineAt.UnixMilli() {
		jsonWriter(w, http.StatusBadGateway, errorBody(connectError("unavailable", "sandbox is not running")))
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// handleProcessStart implements process.Process/Start (streaming). The SDK
// sends /bin/bash -l -c <command> (or -c); we translate the -c argument into
// a sandboxd command and stream output frames, persisting the exit code to a
// marker file (sandboxd's stream never reports it).
func (s *Server) handleProcessStart(w http.ResponseWriter, r *http.Request) {
	body, err := readFirstMessage(mustReadBody(r))
	if err != nil {
		writeEnvdStreamError(w, err.(*E2bError))
		return
	}
	proc, _ := body["process"].(map[string]any)
	if proc == nil {
		writeEnvdStreamError(w, connectError("invalid_argument", "missing process"))
		return
	}
	cmd, _ := proc["cmd"].(string)
	args := stringSlice(proc["args"])
	envs, _ := proc["envs"].(map[string]any)
	cwd, _ := proc["cwd"].(string)

	// A pty block makes this a terminal session — K8E has no PTY support.
	if _, hasPty := body["pty"]; hasPty {
		writeEnvdStreamError(w, connectError("invalid_argument", "PTY sessions are not supported"))
		return
	}
	// Without a pty, the SDK always sends /bin/bash [-l] -c <command>;
	// anything else is a shape this layer does not speak yet.
	command := extractShellCommand(cmd, args)
	if command == "" {
		writeEnvdStreamError(w, connectError("invalid_argument", "only shell commands are supported: expected /bin/bash [-l] -c <command>"))
		return
	}
	if stdin, _ := proc["stdin"].(bool); stdin {
		writeEnvdStreamError(w, connectError("unimplemented", "stdin is not supported: sandboxd /exec/stream has no stdin pipe"))
		return
	}

	// Authenticate + resolve the sandbox row.
	sandboxID := sandboxIDOf(r)
	if sandboxID == "" {
		writeEnvdStreamError(w, connectError("unauthenticated", "missing E2b-Sandbox-Id header"))
		return
	}
	// Auto-resume a paused sandbox before the exec lands (CubeSandbox's
	// auto_resume contract).
	if _, _, e2e := s.wakeForTraffic(r, sandboxID); e2e != nil {
		writeEnvdStreamError(w, e2e)
		return
	}

	// Register the process first (the SDK needs the pid back immediately).
	cfg := ProcessConfig{
		Cmd:  cmd,
		Args: args,
		Envs: toStringMap(envs),
		Cwd:  cwd,
	}
	rec := s.processes.Start(sandboxID, cfg)

	// The exec runs in the background; the HTTP stream is one subscriber.
	hijack, ok := w.(http.Hijacker)
	if !ok {
		s.processes.End(rec.PID, ProcessEnd{ExitCode: -1, Err: fmt.Errorf("response writer not hijackable")})
		return
	}
	conn, buf, err := hijack.Hijack()
	if err != nil {
		s.processes.End(rec.PID, ProcessEnd{ExitCode: -1, Err: err})
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\n\r\n")

	writeEnvelopeRaw(buf, envelope(FlagMessage, map[string]any{
		"event": map[string]any{"start": map[string]int{"pid": rec.PID}},
	}))

	sub := &streamSubscriber{pid: rec.PID, w: buf}
	if !s.processes.Subscribe(rec.PID, sub) {
		writeEnvelopeRaw(buf, envelope(FlagEndStream, map[string]any{
			"error": map[string]string{"code": "not_found", "message": fmt.Sprintf("process not found: %d", rec.PID)},
		}))
		return
	}
	defer s.processes.Unsubscribe(rec.PID, sub)

	// Keepalive heartbeats: long-running processes with no output must not
	// be cut by proxy timeouts (official envd sends ProcessEvent.KeepAlive).
	ksub, stopKeepalive := newKeepaliveSubscriber(sub, buf)
	s.processes.ReplaceSubscriber(rec.PID, sub, ksub)
	defer stopKeepalive()

	// Run the exec and stream frames until it ends.
	exitCode := s.runProcessStream(r, sandboxID, rec, command, cfg)
	// Deliver the end frame.
	writeEnvelopeRaw(buf, envelope(FlagMessage, map[string]any{
		"event": map[string]any{
			"end": map[string]any{
				"exitCode": exitCode,
				"exited":   true,
				"status":   exitStatus(exitCode),
			},
		},
	}))
	writeEnvelopeRaw(buf, envelope(FlagEndStream, map[string]any{}))
}

// runProcessStream executes the command via ExecStream, stripping sandboxd
// SSE framing and broadcasting data frames. Returns the process exit code
// read from the marker file.
func (s *Server) runProcessStream(r *http.Request, sandboxID string, rec *ProcessRecord, command string, cfg ProcessConfig) int {
	req := &pb.ExecRequest{
		SessionId: sandboxID,
		Command:   command,      // raw command; exit code arrives in-stream (KIP-18 P1)
		Timeout:   24 * 60 * 60, // in-container backstop only; the wire deadline detaches, never kills
		Workdir:   cfg.Cwd,
	}
	stream, err := s.gw.ExecStream(r.Context(), req)
	if err != nil {
		s.processes.End(rec.PID, ProcessEnd{ExitCode: -1, Err: err})
		return -1
	}
	framer := &sseFramer{}
	// exitCh carries the in-stream exit code reported by sandboxd's closing
	// {"exit":N} frame. Buffered so the reader never blocks on it.
	exitCh := make(chan int, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		pidSeen := false
		for {
			chunk, recvErr := stream.Recv()
			if recvErr != nil {
				break
			}
			if out := framer.push(chunk.Chunk); len(out) > 0 {
				// The first frame carries the in-sandbox pid
				// ({"pid":N}) so /exec/stdin and /exec/signal can address
				// the real process; the closing {"exit":N} frame carries
				// the exit code. Everything else is output.
				if !pidSeen {
					if pid := parsePidFrame(out); pid != nil {
						s.processes.SetGuestPID(rec.PID, *pid)
						pidSeen = true
						continue
					}
					pidSeen = true
				}
				if code := parseExitFrame(out); code != nil {
					exitCh <- *code
					continue
				}
				s.processes.Broadcast(rec.PID, ChannelStdout, out)
			}
		}
		if out := framer.drain(); len(out) > 0 {
			s.processes.Broadcast(rec.PID, ChannelStdout, out)
		}
	}()
	<-done
	exitCode := -1
	select {
	case code := <-exitCh:
		exitCode = code
	default:
		// No exit frame (e.g. stream cut): report killed.
	}
	s.processes.End(rec.PID, ProcessEnd{ExitCode: exitCode})
	return exitCode
}

// parseExitFrame extracts {"exit":N} from a frame, or nil.
func parseExitFrame(frame []byte) *int {
	frame = bytes.TrimSpace(frame)
	if !bytes.HasPrefix(frame, []byte(`{"exit":`)) {
		return nil
	}
	var v struct {
		Exit int `json:"exit"`
	}
	if err := json.Unmarshal(frame, &v); err != nil {
		return nil
	}
	return &v.Exit
}

// parsePidFrame extracts {"pid":N} from a frame, or nil.
func parsePidFrame(frame []byte) *int {
	frame = bytes.TrimSpace(frame)
	if !bytes.HasPrefix(frame, []byte(`{"pid":`)) {
		return nil
	}
	var v struct {
		PID int `json:"pid"`
	}
	if err := json.Unmarshal(frame, &v); err != nil || v.PID == 0 {
		return nil
	}
	return &v.PID
}

// handleProcessConnect implements process.Process/Connect (streaming):
// reattach to a living process, opening with a start frame like Start. No
// replay of past output.
func (s *Server) handleProcessConnect(w http.ResponseWriter, r *http.Request) {
	body, err := readFirstMessage(mustReadBody(r))
	if err != nil {
		writeEnvdStreamError(w, err.(*E2bError))
		return
	}
	proc, _ := body["process"].(map[string]any)
	pidFloat, ok := proc["pid"].(float64)
	if !ok {
		writeEnvdStreamError(w, connectError("invalid_argument", "missing process pid"))
		return
	}
	pid := int(pidFloat)
	sandboxID := sandboxIDOf(r)
	rec := s.processes.Get(sandboxID, pid)

	// Cross-node Connect: the process's Start stream may have been served by
	// a different control-plane node, so this node's local process table has
	// no record. Fall back to the sandbox-owned process table via
	// /exec/attach (pids are the sandbox's own — node-independent), which
	// replays the buffered output. This keeps Connect working across nodes
	// even though the subscriber broadcast stays local.
	if rec == nil {
		s.attachRemote(w, r, sandboxID, pid)
		return
	}
	hijack, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, buf, err := hijack.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\n\r\n")

	writeEnvelopeRaw(buf, envelope(FlagMessage, map[string]any{
		"event": map[string]any{"start": map[string]int{"pid": pid}},
	}))

	sub := &streamSubscriber{pid: pid, w: buf}
	if !s.processes.Subscribe(pid, sub) {
		writeEnvelopeRaw(buf, envelope(FlagEndStream, map[string]any{
			"error": map[string]string{"code": "not_found", "message": fmt.Sprintf("process not found: %d", pid)},
		}))
		return
	}
	defer s.processes.Unsubscribe(pid, sub)

	// Keepalive heartbeats for long-idle reconnects.
	ksub, stopKeepalive := newKeepaliveSubscriber(sub, buf)
	s.processes.ReplaceSubscriber(pid, sub, ksub)
	defer stopKeepalive()

	// Wait for the process to finalize, then close the stream.
	if done, ok := s.processes.Done(pid); ok {
		<-done
	}
	writeEnvelopeRaw(buf, envelope(FlagEndStream, map[string]any{}))
}

// handleProcessList implements process.Process/List (unary).
func (s *Server) handleProcessList(w http.ResponseWriter, r *http.Request) {
	sandboxID := sandboxIDOf(r)

	// Prefer the sandbox-owned process table (GET /exec/processes): pids are
	// the sandbox's own, node-independent, so the view is consistent across
	// control-plane nodes (multi-node embedded architecture). Fall back to
	// the local process table (subscriber records) if sandboxd is
	// unreachable — degraded but still useful.
	if sp, err := s.sandboxd.processList(r.Context(), sandboxID); err == nil {
		procs := make([]map[string]any, 0, len(sp))
		for _, p := range sp {
			procs = append(procs, map[string]any{
				"pid":    p.PID,
				"alive":  p.Alive,
				"config": map[string]any{"cmd": p.Config},
			})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"processes": procs})
		return
	}

	records := s.processes.List(sandboxID)
	procs := make([]map[string]any, 0, len(records))
	for _, rec := range records {
		cfg := map[string]any{
			"cmd":  rec.Config.Cmd,
			"args": rec.Config.Args,
			"envs": rec.Config.Envs,
		}
		if rec.Config.Cwd != "" {
			cfg["cwd"] = rec.Config.Cwd
		}
		procs = append(procs, map[string]any{
			"pid":    rec.PID,
			"config": cfg,
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"processes": procs})
}

// requireGuestPID resolves a sandbox's process and returns its in-sandbox
// pid (needed to address /exec/stdin and /exec/signal). The synthetic pid
// from the Start response maps to the record; the guest pid is what the
// sandboxd process table holds.
func (s *Server) requireGuestPID(sandboxID string, pid int) (int, *E2bError) {
	rec := s.processes.Get(sandboxID, pid)
	if rec == nil {
		return 0, connectError("not_found", fmt.Sprintf("process not found: %d", pid))
	}
	guest, ok := s.processes.GuestPID(pid)
	if !ok {
		return 0, connectError("not_found", fmt.Sprintf("process not found: %d", pid))
	}
	return guest, nil
}

// handleProcessSendInput implements process.Process/SendInput: write stdin
// to the running process via sandboxd's /exec/stdin.
func (s *Server) handleProcessSendInput(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Process struct {
			PID int `json:"pid"`
		} `json:"process"`
		Input struct {
			Stdin string `json:"stdin"`
		} `json:"input"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Process.PID == 0 {
		s.writeEnvdError(w, connectError("invalid_argument", "missing process pid"))
		return
	}
	sandboxID := sandboxIDOf(r)
	guestPID, e2e := s.requireGuestPID(sandboxID, body.Process.PID)
	if e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	data, err := base64.StdEncoding.DecodeString(body.Input.Stdin)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", "invalid base64 stdin"))
		return
	}
	if err := s.sandboxd.sendStdin(r.Context(), sandboxID, guestPID, data); err != nil {
		s.writeEnvdError(w, connectError("internal", "send stdin failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// handleProcessCloseStdin implements process.Process/CloseStdin: EOF.
func (s *Server) handleProcessCloseStdin(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Process struct {
			PID int `json:"pid"`
		} `json:"process"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Process.PID == 0 {
		s.writeEnvdError(w, connectError("invalid_argument", "missing process pid"))
		return
	}
	sandboxID := sandboxIDOf(r)
	guestPID, e2e := s.requireGuestPID(sandboxID, body.Process.PID)
	if e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	if err := s.sandboxd.closeStdin(r.Context(), sandboxID, guestPID); err != nil {
		s.writeEnvdError(w, connectError("internal", "close stdin failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// handleProcessSendSignal implements process.Process/SendSignal: SIGKILL /
// SIGTERM via sandboxd's /exec/signal.
func (s *Server) handleProcessSendSignal(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Process struct {
			PID int `json:"pid"`
		} `json:"process"`
		Signal string `json:"signal"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Process.PID == 0 {
		s.writeEnvdError(w, connectError("invalid_argument", "missing process pid"))
		return
	}
	sig := ""
	switch body.Signal {
	case "SIGNAL_SIGKILL", "SIGKILL", "9":
		sig = "SIGKILL"
	case "SIGNAL_SIGTERM", "SIGTERM", "15":
		sig = "SIGTERM"
	default:
		s.writeEnvdError(w, connectError("invalid_argument",
			"unsupported signal: only SIGKILL and SIGTERM are supported"))
		return
	}
	sandboxID := sandboxIDOf(r)
	guestPID, e2e := s.requireGuestPID(sandboxID, body.Process.PID)
	if e2e != nil {
		s.writeEnvdError(w, e2e)
		return
	}
	if err := s.sandboxd.signal(r.Context(), sandboxID, guestPID, sig); err != nil {
		if errors.Is(err, errNotFound) {
			s.writeEnvdError(w, connectError("not_found", fmt.Sprintf("process not found: %d", body.Process.PID)))
			return
		}
		s.writeEnvdError(w, connectError("internal", "signal failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// handleUnimplementedProcess answers a Connect RPC with the honest
// unimplemented code and a hint (PTY resize / streamed stdin remain unsupported:
// sandboxd has no PTY and the SDK sends stdin via SendInput).
func (s *Server) handleUnimplementedProcess(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeEnvdError(w, connectError("unimplemented", name+" is not supported"))
	}
}

// handleUnimplementedFS answers a filesystem RPC with the honest
// unimplemented code.
func (s *Server) handleUnimplementedFS(name string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		s.writeEnvdError(w, connectError("unimplemented", name+" is not supported: K8E has no watch"))
	}
}

// --- filesystem service (shell-backed) ------------------------------------

type entryInfo struct {
	Name         string `json:"name"`
	Type         string `json:"type"`
	Path         string `json:"path"`
	Size         string `json:"size"`
	Mode         string `json:"mode"`
	Permissions  string `json:"permissions"`
	Owner        string `json:"owner"`
	Group        string `json:"group"`
	ModifiedTime string `json:"modifiedTime"`
}

// fsReady resolves the sandbox for a filesystem RPC, auto-resuming a paused
// one. Returns the live sandbox ID or writes the error and returns "".
func (s *Server) fsReady(w http.ResponseWriter, r *http.Request) string {
	sandboxID := sandboxIDOf(r)
	if _, _, e2e := s.wakeForTraffic(r, sandboxID); e2e != nil {
		s.writeEnvdError(w, e2e)
		return ""
	}
	return sandboxID
}

// baseName returns the final path component.
func baseName(p string) string {
	if i := strings.LastIndex(p, "/"); i >= 0 {
		return p[i+1:]
	}
	return p
}

// modePerms renders an octal mode string as rwx triplets (display only).
func modePerms(octal string) string {
	n, err := strconv.ParseUint(octal, 8, 32)
	if err != nil {
		return ""
	}
	var b strings.Builder
	for shift := 6; shift >= 0; shift -= 3 {
		bits := (n >> shift) & 7
		b.WriteString(permTriplet(bits))
	}
	return b.String()
}

func permTriplet(bits uint64) string {
	out := []byte("---")
	if bits&4 != 0 {
		out[0] = 'r'
	}
	if bits&2 != 0 {
		out[1] = 'w'
	}
	if bits&1 != 0 {
		out[2] = 'x'
	}
	return string(out)
}

// statEntryToInfo converts a sandboxd statEntry to the E2B EntryInfo shape.
func statEntryToInfo(e *statEntry, fallbackPath string) map[string]any {
	info := map[string]any{
		"path":         e.Name,
		"size":         strconv.FormatInt(e.Size, 10),
		"mode":         e.Mode,
		"owner":        strconv.Itoa(e.UID),
		"group":        strconv.Itoa(e.GID),
		"modifiedTime": strconv.FormatInt(e.Mtime, 10),
		"name":         baseName(e.Name),
	}
	switch e.Type {
	case "file":
		info["type"] = "FILE_TYPE_FILE"
	case "dir":
		info["type"] = "FILE_TYPE_DIRECTORY"
	default:
		info["type"] = "FILE_TYPE_UNSPECIFIED"
	}
	info["permissions"] = modePerms(e.Mode)
	if e.SymlinkTarget != "" {
		info["symlinkTarget"] = e.SymlinkTarget
	}
	if info["name"] == "" {
		info["name"] = baseName(fallbackPath)
	}
	return info
}

// handleFSStat implements filesystem.Filesystem/Stat (unary).
func (s *Server) handleFSStat(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path, err := resolveSandboxPath(body.Path)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	entry, err := s.sandboxd.stat(r.Context(), sandboxID, path)
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeEnvdError(w, connectError("not_found", "path not found: "+path))
			return
		}
		s.writeEnvdError(w, connectError("internal", "stat failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entry": statEntryToInfo(entry, path)})
}

// handleFSListDir implements filesystem.Filesystem/ListDir (unary, depth 1).
func (s *Server) handleFSListDir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path  string `json:"path"`
		Depth int    `json:"depth"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	if body.Depth < 1 {
		body.Depth = 1
	}
	path, err := resolveSandboxPath(body.Path)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	// ListDir needs per-entry stats; walk via the native stat of the dir
	// itself plus a shell-free approach: stat the directory, then list its
	// immediate children. sandboxd's /files/list is recursive and returns
	// paths only, so we use stat on the dir + the native list for names.
	dirEntry, err := s.sandboxd.stat(r.Context(), sandboxID, path)
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeEnvdError(w, connectError("not_found", "path not found: "+path))
			return
		}
		s.writeEnvdError(w, connectError("internal", "list failed: "+err.Error()))
		return
	}
	if dirEntry.Type != "dir" {
		s.writeEnvdError(w, connectError("invalid_argument", "not a directory: "+path))
		return
	}
	// List files under the path (recursive walk from sandboxd), filter to
	// depth-1 children, and stat each for the full EntryInfo.
	resp, err := s.gw.ListFiles(r.Context(), &pb.ListFilesRequest{SessionId: sandboxID})
	if err != nil {
		s.writeEnvdError(w, connectError("internal", "list failed: "+err.Error()))
		return
	}
	prefix := path
	if !strings.HasSuffix(prefix, "/") {
		prefix += "/"
	}
	entries := []map[string]any{}
	seen := map[string]bool{}
	for _, f := range resp.Files {
		if !strings.HasPrefix(f.Path, prefix) {
			continue
		}
		rest := strings.TrimPrefix(f.Path, prefix)
		// Respect the requested depth: count path segments below the root
		// and skip anything deeper. Depth 1 = direct children only (the
		// SDK default).
		if depthOf(rest) > body.Depth {
			continue
		}
		if seen[f.Path] {
			continue
		}
		seen[f.Path] = true
		e, serr := s.sandboxd.stat(r.Context(), sandboxID, f.Path)
		if serr != nil {
			// A file may vanish between list and stat; skip it honestly.
			continue
		}
		entries = append(entries, statEntryToInfo(e, f.Path))
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entries": entries})
}

// handleFSMakeDir implements filesystem.Filesystem/MakeDir (unary).
func (s *Server) handleFSMakeDir(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path, err := resolveSandboxPath(body.Path)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	err = s.sandboxd.mkdir(r.Context(), sandboxID, path)
	if err != nil {
		if errors.Is(err, errAlreadyExists) {
			s.writeEnvdError(w, connectError("already_exists", "already exists: "+path))
			return
		}
		s.writeEnvdError(w, connectError("internal", "mkdir failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entry": map[string]any{
		"path": path, "name": baseName(path), "type": "FILE_TYPE_DIRECTORY",
	}})
}

// handleFSMove implements filesystem.Filesystem/Move (unary).
func (s *Server) handleFSMove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Source      string `json:"source"`
		Destination string `json:"destination"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	src, err := resolveSandboxPath(body.Source)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	dst, err := resolveSandboxPath(body.Destination)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	err = s.sandboxd.move(r.Context(), sandboxID, src, dst)
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeEnvdError(w, connectError("not_found", "source not found: "+src))
			return
		}
		s.writeEnvdError(w, connectError("internal", "move failed: "+err.Error()))
		return
	}
	entry, serr := s.sandboxd.stat(r.Context(), sandboxID, dst)
	if serr != nil {
		entry = &statEntry{Name: dst}
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"entry": statEntryToInfo(entry, dst)})
}

// handleFSRemove implements filesystem.Filesystem/Remove (unary).
func (s *Server) handleFSRemove(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path string `json:"path"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path, err := resolveSandboxPath(body.Path)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	err = s.sandboxd.remove(r.Context(), sandboxID, path)
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeEnvdError(w, connectError("not_found", "path not found: "+path))
			return
		}
		s.writeEnvdError(w, connectError("internal", "remove failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_, _ = w.Write([]byte("{}"))
}

// --- helpers --------------------------------------------------------------

// streamSubscriber writes enveloped frames to a hijacked connection.
type streamSubscriber struct {
	pid int
	w   io.Writer
}

func (ss *streamSubscriber) OnOutput(channel OutputChannel, data []byte) {
	writeEnvelopeRaw(ss.w, envelope(FlagMessage, map[string]any{
		"event": map[string]any{"data": map[string]string{string(channel): base64.StdEncoding.EncodeToString(data)}},
	}))
}

func (ss *streamSubscriber) OnEnd(end ProcessEnd) {}

// writeEnvelopeRaw writes raw bytes to a hijacked writer and flushes a
// bufio wrapper so the client sees frames live (a hijacked response is
// never auto-flushed by the server).
func writeEnvelopeRaw(w io.Writer, data []byte) {
	_, _ = w.Write(data)
	if f, ok := w.(interface{ Flush() error }); ok {
		_ = f.Flush()
	} else if f, ok := w.(interface{ Flush() }); ok {
		f.Flush()
	}
}

// writeEnvdStreamError answers a streaming RPC with an in-stream error frame
// (200 + end-stream frame), the Connect streaming convention.
func writeEnvdStreamError(w http.ResponseWriter, e *E2bError) {
	h, ok := w.(http.Hijacker)
	if !ok {
		jsonWriter(w, e.StatusCode, errorBody(e))
		return
	}
	conn, buf, err := h.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\n\r\n")
	_ = buf.Flush()
	writeEnvelopeRaw(buf, envelope(FlagEndStream, map[string]any{
		"error": map[string]any{"code": e.Code, "message": e.Message},
	}))
}

func mustReadBody(r *http.Request) []byte {
	b, err := io.ReadAll(r.Body)
	if err != nil {
		return nil
	}
	return b
}

func extractShellCommand(cmd string, args []string) string {
	if !strings.HasSuffix(cmd, "bash") {
		return ""
	}
	if len(args) == 3 && args[0] == "-l" && args[1] == "-c" {
		return args[2]
	}
	if len(args) == 2 && args[0] == "-c" {
		return args[1]
	}
	return ""
}

func stringSlice(v any) []string {
	arr, ok := v.([]any)
	if !ok {
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, item := range arr {
		if s, ok := item.(string); ok {
			out = append(out, s)
		}
	}
	return out
}

func toStringMap(v map[string]any) map[string]string {
	out := map[string]string{}
	for k, val := range v {
		if s, ok := val.(string); ok {
			out[k] = s
		}
	}
	return out
}

func exitStatus(code int) string {
	// 137 = SIGKILL (reported by sandboxd's wrapper trap); -1 = the exit
	// marker was never written (the process was SIGKILLed by the in-container
	// timeout backstop, whose SIGKILL is untrappable — sandboxd report §3/§4).
	if code == 137 || code == -1 {
		return "killed"
	}
	return "exited"
}

// attachRemote serves a Process/Connect when the local process table has no
// record (the Start stream ran on another control-plane node). It attaches
// to the sandbox-owned process via /exec/attach, which replays the buffered
// output as SSE frames; these are re-enveloped into the Connect wire
// (start + data + end), mirroring the local-stream shape the SDK expects.
func (s *Server) attachRemote(w http.ResponseWriter, r *http.Request, sandboxID string, pid int) {
	raw, err := s.sandboxd.attach(r.Context(), sandboxID, pid)
	if err != nil {
		writeEnvdStreamError(w, connectError("not_found", fmt.Sprintf("process not found: %d", pid)))
		return
	}
	hijack, ok := w.(http.Hijacker)
	if !ok {
		return
	}
	conn, buf, err := hijack.Hijack()
	if err != nil {
		return
	}
	defer conn.Close()
	_, _ = buf.WriteString("HTTP/1.1 200 OK\r\nContent-Type: application/connect+json\r\n\r\n")

	writeEnvelopeRaw(buf, envelope(FlagMessage, map[string]any{
		"event": map[string]any{"start": map[string]int{"pid": pid}},
	}))

	// Replay the sandboxd SSE frames (data: ...) as stdout data frames,
	// skipping the leading pid frame (already sent as the start envelope).
	pidSeen := false
	for _, frame := range splitSSEFrames(raw) {
		out := stripDataPrefix(frame)
		// Skip the leading pid frame and the done marker (both are sandboxd
		// protocol frames, not process output).
		if bytes.Contains(out, []byte(`"done":true`)) {
			continue
		}
		if !pidSeen {
			if parsePidFrame(out) != nil {
				pidSeen = true
				continue
			}
			pidSeen = true
		}
		if len(out) > 0 {
			writeEnvelopeRaw(buf, envelope(FlagMessage, map[string]any{
				"event": map[string]any{"data": map[string]any{"stdout": base64.StdEncoding.EncodeToString(out)}},
			}))
		}
	}

	writeEnvelopeRaw(buf, envelope(FlagEndStream, map[string]any{}))
}

// splitSSEFrames splits an SSE body into its frames on the "\n\n" delimiter.
func splitSSEFrames(body []byte) [][]byte {
	var frames [][]byte
	rest := body
	for {
		idx := indexOf(rest, "\n\n")
		if idx < 0 {
			if len(rest) > 0 {
				frames = append(frames, rest)
			}
			break
		}
		frames = append(frames, rest[:idx])
		rest = rest[idx+2:]
	}
	return frames
}

// handleStreamInputUnimplemented answers process.Process/StreamInput with a
// documented 501. Decision (KIP-18 P1 follow-up): StreamInput is the only
// client_stream RPC in the envd spec, but neither the official Python nor
// JS SDK ever calls it — send_stdin goes through the unary SendInput RPC
// (verified against e2b packages/python-sdk + js-sdk sources). Implementing
// HTTP/2 client_stream in sandboxd (Zig) would cost significant work with
// zero SDK consumers, so the honest answer is a permanent 501 with this
// rationale. Custom low-level clients that need streamed stdin are out of
// scope; they can fall back to SendInput.
func (s *Server) handleStreamInputUnimplemented(w http.ResponseWriter, r *http.Request) {
	s.writeEnvdError(w, connectError("unimplemented",
		"StreamInput is not supported: the official e2b SDKs send stdin via SendInput, not the StreamInput client stream; use SendInput"))
}

// keepaliveInterval is how often a process stream sends a keepalive event
// while idle, so long-running processes with no output are not cut by proxy
// timeouts (the official envd sends ProcessEvent.KeepAlive for the same
// reason).
const keepaliveInterval = 15 * time.Second

// keepaliveSubscriber wraps a stream subscriber and emits ProcessEvent
// keepalive frames whenever the stream has been idle for keepaliveInterval.
// OnOutput forwards and resets the idle clock; the ticker fires only when
// nothing was written.
type keepaliveSubscriber struct {
	ProcessSubscriber
	mu   sync.Mutex // serializes writes to w (ticker vs OnOutput)
	w    io.Writer
	stop chan struct{}
	last chan struct{} // reset signal
	done chan struct{}
}

// newKeepaliveSubscriber wraps sub with a keepalive loop. Call stop() when
// the stream ends to release the goroutine.
func newKeepaliveSubscriber(sub ProcessSubscriber, w io.Writer) (*keepaliveSubscriber, func()) {
	k := &keepaliveSubscriber{
		ProcessSubscriber: sub,
		w:                 w,
		stop:              make(chan struct{}),
		last:              make(chan struct{}, 1),
		done:              make(chan struct{}),
	}
	go k.loop()
	return k, func() {
		close(k.stop)
		<-k.done
	}
}

func (k *keepaliveSubscriber) OnOutput(channel OutputChannel, data []byte) {
	k.mu.Lock()
	k.ProcessSubscriber.OnOutput(channel, data)
	k.mu.Unlock()
	select {
	case k.last <- struct{}{}:
	default:
	}
}

func (k *keepaliveSubscriber) loop() {
	defer close(k.done)
	t := time.NewTicker(keepaliveInterval)
	defer t.Stop()
	for {
		select {
		case <-k.stop:
			return
		case <-k.last:
			t.Reset(keepaliveInterval)
		case <-t.C:
			k.mu.Lock()
			writeEnvelopeRaw(k.w, envelope(FlagMessage, map[string]any{
				"event": map[string]any{"keepalive": map[string]any{}},
			}))
			k.mu.Unlock()
		}
	}
}

// depthOf counts the path segments in a relative path (e.g. "a/b/c" → 3,
// "a" → 1, "" → 0). Used by ListDir's depth filter.
func depthOf(rel string) int {
	if rel == "" {
		return 0
	}
	return strings.Count(rel, "/") + 1
}

// handleFSCreateWatcher implements filesystem.Filesystem/CreateWatcher:
// start an inotify watch on a path in the sandbox, return a watcher id the
// SDK polls with GetWatcherEvents (WatchHandle semantics).
func (s *Server) handleFSCreateWatcher(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Path      string `json:"path"`
		Recursive bool   `json:"recursive"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	path, err := resolveSandboxPath(body.Path)
	if err != nil {
		s.writeEnvdError(w, connectError("invalid_argument", err.Error()))
		return
	}
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	id, err := s.sandboxd.createWatcher(r.Context(), sandboxID, path)
	if err != nil {
		if errors.Is(err, errNotFound) {
			s.writeEnvdError(w, connectError("not_found", "path not found: "+path))
			return
		}
		s.writeEnvdError(w, connectError("internal", "create watcher failed: "+err.Error()))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"watcherId": id})
}

// handleFSGetWatcherEvents implements filesystem.Filesystem/GetWatcherEvents:
// return filesystem events since the last call (WatchHandle.get_new_events).
func (s *Server) handleFSGetWatcherEvents(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WatcherID int `json:"watcherId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	events, err := s.sandboxd.getWatcherEvents(r.Context(), sandboxID, body.WatcherID)
	if err != nil {
		s.writeEnvdError(w, connectError("not_found", "watcher not found"))
		return
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"name": e.Name,
			"type": eventTypeName(e.Type),
		})
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"events": out})
}

// handleFSRemoveWatcher implements filesystem.Filesystem/RemoveWatcher:
// stop watching (WatchHandle.stop).
func (s *Server) handleFSRemoveWatcher(w http.ResponseWriter, r *http.Request) {
	var body struct {
		WatcherID int `json:"watcherId"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	sandboxID := s.fsReady(w, r)
	if sandboxID == "" {
		return
	}
	if err := s.sandboxd.removeWatcher(r.Context(), sandboxID, body.WatcherID); err != nil {
		s.writeEnvdError(w, connectError("not_found", "watcher not found"))
		return
	}
	w.WriteHeader(http.StatusOK)
}

// eventTypeName maps the sandboxd numeric event type (1..5) to the E2B
// proto enum name the SDK's map_event_type expects.
func eventTypeName(t int) string {
	switch t {
	case 1:
		return "EVENT_TYPE_CREATE"
	case 2:
		return "EVENT_TYPE_WRITE"
	case 3:
		return "EVENT_TYPE_REMOVE"
	case 4:
		return "EVENT_TYPE_RENAME"
	case 5:
		return "EVENT_TYPE_CHMOD"
	default:
		return "EVENT_TYPE_UNSPECIFIED"
	}
}
