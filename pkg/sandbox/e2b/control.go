package e2b

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/mux"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// createBody mirrors what the v2 SDK sends. .loose() tolerance is implicit:
// unknown fields are simply not decoded, which is what "two URLs and it
// works" requires; acting on them is tracked feature by feature.
type createBody struct {
	TemplateID string            `json:"templateID"`
	Timeout    *int              `json:"timeout"`
	Metadata   map[string]string `json:"metadata"`
	EnvVars    map[string]string `json:"envVars"`
	AutoPause  bool              `json:"autoPause"`
}

type timeoutBody struct {
	Timeout int `json:"timeout"`
}

type connectBody struct {
	Timeout *int `json:"timeout"`
}

// parseSandboxTimeout validates the E2B timeout knob shared by create and
// connect. Returns the effective seconds, whether the sandbox should never
// time out, and a protocol error message on invalid values.
func parseSandboxTimeout(t *int) (timeoutSeconds int, neverTimeout bool, errMsg string) {
	timeoutSeconds = DefaultTimeoutSeconds
	if t == nil {
		return timeoutSeconds, false, ""
	}
	if *t == NeverTimeout {
		return timeoutSeconds, true, ""
	}
	if *t <= 0 || *t > maxTimeoutSeconds {
		return 0, false, "invalid timeout"
	}
	return *t, false, ""
}

// resolveRuntimeClass maps a templateID to a runtime class: a known runtime
// wins, 'base' and absence mean the default runtime, anything else is not
// found — the SDK default is the literal string 'base'.
func resolveRuntimeClass(runtimes map[string]struct{}, templateID string) (runtimeClass string, ok bool) {
	if templateID == "" {
		return "", true
	}
	if _, found := runtimes[templateID]; found {
		return templateID, true
	}
	if templateID != "base" {
		return "", false
	}
	return "", true
}

// reacquireByKey serves the Dormice idempotent-create extension: a live
// sandbox under the same metadata.name key is reused (deadline extended like
// a connect). A protocol-dead one is destroyed so the caller builds fresh
// under the same key. Returns true when the response was fully written.
func (s *Server) reacquireByKey(w http.ResponseWriter, r *http.Request, key string, timeoutSeconds int) bool {
	if key == "" {
		return false
	}
	id, ok := s.registry.byKeyName(key)
	if !ok {
		return false
	}
	sess, err := s.gw.GetSession(r.Context(), &pb.GetSessionRequest{SessionId: id})
	if err == nil && sessionState(sess.Phase) == stateRunning {
		s.registry.extendDeadline(id, timeoutSeconds)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(s.sessionView(sess))
		return true
	}
	// Protocol-dead but not yet reaped: E2B semantics say it is gone, so
	// finish the job and build fresh under the same key.
	_, _ = s.gw.DestroySession(r.Context(), &pb.DestroySessionRequest{SessionId: id})
	s.registry.del(id)
	return false
}

// handleCreate implements POST /e2b/api/sandboxes.
func (s *Server) handleCreate(w http.ResponseWriter, r *http.Request) {
	var body createBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		s.writeControlError(w, apiError(400, "invalid request body: "+err.Error()))
		return
	}
	timeoutSeconds, neverTimeout, errMsg := parseSandboxTimeout(body.Timeout)
	if errMsg != "" {
		s.writeControlError(w, apiError(400, errMsg))
		return
	}
	meta := sanitizeMetadata(body.Metadata)
	requestedKey := meta["name"]

	// templateID resolution: a known runtime class wins; 'base' and absence
	// mean the default runtime; anything else is 404 — the SDK default is the
	// literal string 'base'.
	runtimeClass, ok := resolveRuntimeClass(s.runtimes, body.TemplateID)
	if !ok {
		s.writeControlError(w, apiError(404, "template '"+body.TemplateID+"' not found"))
		return
	}

	// The Dormice extension: metadata.name makes create idempotent — same
	// key, same sandbox (an acquire in E2B clothes).
	if s.reacquireByKey(w, r, requestedKey, timeoutSeconds) {
		return
	}

	sessionID := requestedKey
	if sessionID == "" {
		sessionID = "e2b-" + randomID()
	}
	resp, err := s.gw.CreateSession(r.Context(), &pb.CreateSessionRequest{
		SessionId:    sessionID,
		RuntimeClass: runtimeClass,
		Env:          normalizeEnvVars(body.EnvVars),
	})
	if err != nil {
		s.writeControlError(w, gwErrorToE2B(err, "create sandbox failed"))
		return
	}

	onDeadline := "kill"
	if body.AutoPause {
		onDeadline = "pause"
	}
	entry := &registryEntry{
		sandboxID:   sessionID,
		onDeadline:  onDeadline,
		createdAt:   time.Now(),
		metadata:    meta,
		runtimeName: runtimeClass,
	}
	if !neverTimeout {
		entry.deadlineAt = time.Now().Add(time.Duration(timeoutSeconds) * time.Second)
	}
	s.registry.put(entry)

	sess := &pb.GetSessionResponse{
		SessionId:    sessionID,
		RuntimeClass: runtimeClass,
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(s.sessionView(sess))
	_ = resp
}

// handleConnect implements POST /e2b/api/sandboxes/:id/connect.
func (s *Server) handleConnect(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var body connectBody
	if r.ContentLength != 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}
	timeoutSeconds, neverTimeout, errMsg := parseSandboxTimeout(body.Timeout)
	if errMsg != "" {
		s.writeControlError(w, apiError(400, errMsg))
		return
	}

	sess, err := s.gw.GetSession(r.Context(), &pb.GetSessionRequest{SessionId: id})
	if err != nil {
		s.writeControlError(w, gwErrorToE2B(err, "sandbox \""+id+"\" not found"))
		return
	}
	if sessionState(sess.GetPhase()) == stateDead {
		s.writeControlError(w, apiError(404, "sandbox \""+id+"\" not found"))
		return
	}
	// A paused sandbox resumes on connect (E2B semantics: connect returns a
	// live sandbox, period). 201 = this connect resumed it.
	status := http.StatusOK
	if sessionState(sess.GetPhase()) == statePaused {
		if _, rerr := s.gw.ResumeSession(r.Context(), &pb.ResumeSessionRequest{SessionId: id}); rerr != nil {
			s.writeControlError(w, gwErrorToE2B(rerr, "resume failed"))
			return
		}
		s.registry.markPaused(id, false)
		s.sandboxd.invalidatePodIP(id)
		status = http.StatusCreated
	}
	// connect extends the TTL — but only for E2B-created sandboxes (they
	// always have a deadline entry); natively-created ones stay immortal.
	if _, ok := s.registry.get(id); ok {
		if neverTimeout {
			s.registry.clearDeadline(id)
		} else {
			s.registry.extendDeadline(id, timeoutSeconds)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(s.sessionView(sess))
}

// handleGet implements GET /e2b/api/sandboxes/:id.
func (s *Server) handleGet(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	sess, state, ok := s.findLive(r, id)
	if !ok {
		s.writeControlError(w, apiError(404, "sandbox \""+id+"\" not found"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(s.infoView(sess, state))
}

// handleKill implements DELETE /e2b/api/sandboxes/:id. kill = destroy, for
// real: pod, workspace and session gone. Persistence on the E2B surface is
// "don't kill" (autoPause / metadata.name), never a kill that secretly keeps
// data.
func (s *Server) handleKill(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if _, _, ok := s.findLive(r, id); !ok {
		// A sandbox mid-lifecycle answers 503 + Retry-After (CubeSandbox);
		// a genuinely gone one is 404 (the SDK's kill()===false key).
		s.goneOrNotFound(w, s.lastSessionErr(id), id)
		return
	}
	if _, err := s.gw.DestroySession(r.Context(), &pb.DestroySessionRequest{SessionId: id}); err != nil {
		e := gwErrorToE2B(err, "destroy sandbox failed")
		if e.StatusCode == http.StatusConflict {
			s.writeConflict(w, e, 2)
			return
		}
		s.writeControlError(w, e)
		return
	}
	s.registry.del(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleTimeout implements POST /e2b/api/sandboxes/:id/timeout. setTimeout
// overwrites in both directions, measured from now — but only for
// E2B-created sandboxes; native ones stay immortal.
func (s *Server) handleTimeout(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	var body timeoutBody
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil || !validTimeout(body.Timeout) {
		s.writeControlError(w, apiError(400, "invalid timeout"))
		return
	}
	if _, _, ok := s.findLive(r, id); !ok {
		s.writeControlError(w, apiError(404, "sandbox \""+id+"\" not found"))
		return
	}
	if _, ok := s.registry.get(id); ok {
		if body.Timeout == NeverTimeout {
			s.registry.clearDeadline(id)
		} else {
			s.registry.extendDeadline(id, body.Timeout)
		}
	}
	w.WriteHeader(http.StatusNoContent)
}

// validTimeout accepts a positive TTL or the NEVER_TIMEOUT sentinel (-1).
func validTimeout(seconds int) bool {
	return seconds == NeverTimeout || (seconds > 0 && seconds <= maxTimeoutSeconds)
}

// handlePause implements POST /sandboxes/:id/pause. Backed by the gateway's
// PauseSession RPC: the pod (CPU/memory) is released, the workspace PVC and
// session survive. An ephemeral (EmptyDir) sandbox cannot pause without
// losing its files — the gateway refuses with FailedPrecondition, surfaced
// here as 409 (CubeSandbox's "sandbox cannot be paused").
func (s *Server) handlePause(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if _, _, ok := s.findLive(r, id); !ok {
		s.goneOrNotFound(w, s.lastSessionErr(id), id)
		return
	}
	if _, err := s.gw.PauseSession(r.Context(), &pb.PauseSessionRequest{SessionId: id}); err != nil {
		e := gwErrorToE2B(err, "pause failed")
		s.writeControlError(w, e)
		return
	}
	s.registry.markPaused(id, true)
	s.sandboxd.invalidatePodIP(id)
	w.WriteHeader(http.StatusNoContent)
}

// handleResume implements POST /sandboxes/:id/resume. Backed by the
// gateway's ResumeSession RPC: the pod is re-created with its PVC.
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	sess, _, ok := s.findLive(r, id)
	if !ok {
		s.goneOrNotFound(w, s.lastSessionErr(id), id)
		return
	}
	if _, err := s.gw.ResumeSession(r.Context(), &pb.ResumeSessionRequest{SessionId: id}); err != nil {
		e := gwErrorToE2B(err, "resume failed")
		s.writeControlError(w, e)
		return
	}
	s.registry.markPaused(id, false)
	s.sandboxd.invalidatePodIP(id)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusCreated)
	_ = json.NewEncoder(w).Encode(s.sessionView(sess))
}

// handleMetrics implements GET /e2b/api/sandboxes/:id/metrics. K8E has no
// metrics pipeline yet — honest empty.
func (s *Server) handleMetrics(w http.ResponseWriter, r *http.Request) {
	id := mux.Vars(r)["id"]
	if _, _, ok := s.findLive(r, id); !ok {
		s.writeControlError(w, apiError(404, "sandbox \""+id+"\" not found"))
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte("[]"))
}

// listQuery carries the parsed /v2/sandboxes query parameters.
type listQuery struct {
	states     map[string]bool
	limit      int
	offset     int
	nameFilter string
}

// parseListQuery decodes the E2B list query: a state filter (default running
// + paused), pagination limit/nextToken, and the metadata name filter.
func parseListQuery(query url.Values) listQuery {
	q := listQuery{limit: 100}
	if state := query.Get("state"); state != "" {
		q.states = map[string]bool{}
		for _, st := range strings.Split(state, ",") {
			q.states[strings.TrimSpace(st)] = true
		}
	} else {
		q.states = map[string]bool{"running": true, "paused": true}
	}
	if l := query.Get("limit"); l != "" {
		if n, err := strconv.Atoi(l); err == nil && n > 0 && n <= 1000 {
			q.limit = n
		}
	}
	if tok := query.Get("nextToken"); tok != "" {
		if n, err := strconv.Atoi(tok); err == nil && n > 0 {
			q.offset = n
		}
	}
	// metadata filter (name key only — the one metadata we actually store).
	if meta := query.Get("metadata"); meta != "" {
		for _, pair := range strings.Split(meta, "&") {
			if strings.HasPrefix(pair, "name=") {
				q.nameFilter = strings.TrimPrefix(pair, "name=")
			}
		}
	}
	return q
}

// listLiveSessions collects the non-dead sessions matching the query's state
// and name filters, sorted by id.
func (s *Server) listLiveSessions(ctx context.Context, q listQuery) ([]*pb.GetSessionResponse, error) {
	resp, err := s.gw.ListSessions(ctx, &pb.ListSessionsRequest{Phase: "all"})
	if err != nil {
		return nil, err
	}
	var all []*pb.GetSessionResponse
	for _, sess := range resp.Sessions {
		st := sessionState(sess.Phase)
		if st == stateDead {
			continue
		}
		if len(q.states) > 0 && !q.states[string(st)] {
			continue
		}
		if q.nameFilter != "" {
			e, ok := s.registry.get(sess.SessionId)
			if !ok || e.metadata["name"] != q.nameFilter {
				continue
			}
		}
		all = append(all, sess)
	}
	sort.Slice(all, func(i, j int) bool { return all[i].SessionId < all[j].SessionId })
	return all, nil
}

// handleList implements GET /e2b/api/v2/sandboxes.
func (s *Server) handleList(w http.ResponseWriter, r *http.Request) {
	q := parseListQuery(r.URL.Query())
	all, err := s.listLiveSessions(r.Context(), q)
	if err != nil {
		s.writeControlError(w, apiError(500, "list sandboxes failed: "+err.Error()))
		return
	}
	end := q.offset + q.limit
	if q.offset > len(all) {
		q.offset = len(all)
	}
	if end > len(all) {
		end = len(all)
	}
	page := all[q.offset:end]
	if end < len(all) {
		w.Header().Set("x-next-token", strconv.Itoa(end))
	}
	out := make([]map[string]any, 0, len(page))
	for _, sess := range page {
		out = append(out, s.infoView(sess, sessionState(sess.Phase)))
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_ = json.NewEncoder(w).Encode(out)
}

// findLive looks a sandbox up as the protocol sees it: a session that is not
// logically running is protocol-dead — 404, exactly what the SDK expects of
// a timed-out sandbox. The last gateway error is remembered so callers can
// distinguish a genuinely gone sandbox from one mid-lifecycle.
func (s *Server) findLive(r *http.Request, id string) (*pb.GetSessionResponse, sandboxState, bool) {
	sess, err := s.gw.GetSession(r.Context(), &pb.GetSessionRequest{SessionId: id})
	if err != nil {
		s.mu.Lock()
		s.lastErr[id] = err
		s.mu.Unlock()
		return nil, stateDead, false
	}
	s.mu.Lock()
	delete(s.lastErr, id)
	s.mu.Unlock()
	st := sessionState(sess.Phase)
	if st == stateDead {
		return nil, st, false
	}
	// A kill deadline that has passed is protocol-dead even while the
	// physical teardown is a sweep away.
	if e, ok := s.registry.get(id); ok && !e.deadlineAt.IsZero() && time.Now().After(e.deadlineAt) {
		if e.onDeadline != "pause" {
			return nil, stateDead, false
		}
		st = statePaused
	}
	return sess, st, true
}

// lastSessionErr returns the most recent GetSession error for a sandbox, if
// any (used to decide gone vs mid-lifecycle on 404 paths).
func (s *Server) lastSessionErr(id string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastErr[id]
}
