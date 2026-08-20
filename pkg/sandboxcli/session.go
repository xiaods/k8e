package sandboxcli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

type SessionState struct {
	SessionID string `json:"session_id"`
	Phase     string `json:"phase"` // "active" or "creating"
	TenantID  string `json:"tenant_id"`
	CreatedAt string `json:"created_at"`
	LockedAt  string `json:"locked_at,omitempty"`
	PID       int    `json:"pid,omitempty"`
}

func stateDir(tenant string) string {
	dir, _ := dataDir()
	if tenant == "" {
		tenant = "default"
	}
	return filepath.Join(dir, tenant)
}

func statePath(tenant string) string {
	return filepath.Join(stateDir(tenant), "state.json")
}

func lockPath(tenant string) string {
	return filepath.Join(stateDir(tenant), "state.lock")
}

func loadState(tenant string) (*SessionState, error) {
	if tenant == "" {
		tenant = "default"
	}
	data, err := os.ReadFile(statePath(tenant))
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, err
	}
	var s SessionState
	if err := json.Unmarshal(data, &s); err != nil {
		return nil, err
	}
	return &s, nil
}

func saveState(tenant string, state *SessionState) error {
	if tenant == "" {
		tenant = "default"
	}
	dir := stateDir(tenant)
	if err := os.MkdirAll(dir, 0755); err != nil {
		return err
	}
	// atomic write: temp file + rename
	data, err := json.Marshal(state)
	if err != nil {
		return err
	}
	tmp := statePath(tenant) + ".tmp"
	if err := os.WriteFile(tmp, data, 0644); err != nil {
		return err
	}
	return os.Rename(tmp, statePath(tenant))
}

func clearState(tenant string) error {
	if tenant == "" {
		tenant = "default"
	}
	return os.Remove(statePath(tenant))
}

// create deadline / stale-creating recovery bounds. The CLI historically used
// context.Background() (no deadline) for session RPCs, so a hung gateway left
// a "creating" placeholder behind when the process died; waiters then wedged
// forever. These constants bound both sides: a placeholder older than
// creatingStaleAfter (or whose creator process is gone) is reclaimed.
const (
	creatingStaleAfter   = 2 * time.Minute
	creatingPollInterval = 200 * time.Millisecond
	sessionCreateTimeout = 45 * time.Second
)

// lockedAt parses the placeholder's RFC3339 stamp; zero time for a missing one.
func lockedAt(raw string) time.Time {
	t, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		return time.Time{}
	}
	return t
}

// pidAlive reports whether the process is running (signal-0 probe). EPERM means
// the process exists but belongs to another user — treat it as alive.
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	p, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	if err := p.Signal(syscall.Signal(0)); err != nil {
		return errors.Is(err, syscall.EPERM)
	}
	return true
}

// creatingPlaceholderStale reports whether a "creating" placeholder may be
// reclaimed: its creator process is gone (crash / Ctrl-C / OOM), or it outlived
// creatingStaleAfter (creator hung without an RPC deadline). A live, fresh
// creator is left alone so concurrent runners still share one session.
func creatingPlaceholderStale(state *SessionState) bool {
	if state.PID > 0 {
		if pidAlive(state.PID) {
			// Creator still running, but it may be hung on an RPC with no
			// deadline — reclaim once the stamp is old enough.
			return time.Since(lockedAt(state.LockedAt)) > creatingStaleAfter
		}
		return true // creator process is gone — the placeholder can never finalize
	}
	// No PID recorded (older placeholder): give it the same age grace.
	return time.Since(lockedAt(state.LockedAt)) > creatingStaleAfter
}

// resolveSession returns an active session ID using the three-tier strategy plus flock locking.
func resolveSession(ctx context.Context, tenant string, sessionIDOverride string) (string, error) {
	if sessionIDOverride != "" {
		return sessionIDOverride, nil
	}
	if sid := os.Getenv("K8E_SANDBOX_SESSION_ID"); sid != "" {
		return sid, nil
	}

	if tenant == "" {
		tenant = "default"
	}
	os.MkdirAll(stateDir(tenant), 0755) //nolint:errcheck

	lf, err := os.Create(lockPath(tenant))
	if err != nil {
		return "", fmt.Errorf("create lock file: %w", err)
	}
	defer lf.Close()
	defer unlockFile(lf)

	for {
		if sid, done := tryClaimSession(lf, tenant); done {
			return sid, nil
		}

		state, _ := loadState(tenant)
		if state != nil && state.Phase == "creating" && !creatingPlaceholderStale(state) {
			// A live creator is finalizing; wait, but bounded — a creator hung
			// on an RPC without a deadline must not wedge every later caller.
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(creatingPollInterval):
			}
			continue
		}

		// No valid session, or the "creating" placeholder is stale (its creator
		// crashed or hung): claim the creator role and return empty (caller creates).
		sid, claimed := claimCreatorRole(lf, tenant)
		if claimed {
			return sid, nil
		}
	}
}

// tryClaimSession checks under lock whether an active session exists and returns it.
func tryClaimSession(lf *os.File, tenant string) (sid string, found bool) {
	if err := lockFile(lf); err != nil {
		return "", false
	}
	state, _ := loadState(tenant)
	unlockFile(lf)
	if state != nil && state.Phase == "active" && state.SessionID != "" {
		return state.SessionID, true
	}
	return "", false
}

// claimCreatorRole writes a "creating" placeholder under lock.
// Returns the existing session ID if another process finished creating first, or ("", true) to signal caller to create.
func claimCreatorRole(lf *os.File, tenant string) (sid string, claimed bool) {
	if err := lockFile(lf); err != nil {
		return "", false
	}
	defer unlockFile(lf)

	// Re-read after lock to avoid TOCTOU
	state, _ := loadState(tenant)
	if state != nil && state.Phase == "active" && state.SessionID != "" {
		return state.SessionID, true
	}

	placeholder := &SessionState{
		Phase:    "creating",
		TenantID: tenant,
		LockedAt: time.Now().UTC().Format(time.RFC3339),
		PID:      os.Getpid(),
	}
	if err := saveState(tenant, placeholder); err != nil {
		return "", false
	}
	return "", true
}

// finalizeState writes the active state after session creation.
func finalizeState(tenant, sessionID string) error {
	if tenant == "" {
		tenant = "default"
	}
	lf, err := os.Create(lockPath(tenant))
	if err != nil {
		return err
	}
	defer lf.Close()
	if err := lockFile(lf); err != nil {
		return err
	}
	defer unlockFile(lf)

	state := &SessionState{
		SessionID: sessionID,
		Phase:     "active",
		TenantID:  tenant,
		CreatedAt: time.Now().UTC().Format(time.RFC3339),
	}
	return saveState(tenant, state)
}
