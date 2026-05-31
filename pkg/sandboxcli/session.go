package sandboxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
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
		if state != nil && state.Phase == "creating" {
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		// no valid session — claim creator role and return empty (caller creates)
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
