package sandboxcli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"
)

const stateDirName = ".k8e/sandbox"

type SessionState struct {
	SessionID string `json:"session_id"`
	Phase     string `json:"phase"` // "active" or "creating"
	TenantID  string `json:"tenant_id"`
	CreatedAt string `json:"created_at"`
	LockedAt  string `json:"locked_at,omitempty"`
	PID       int    `json:"pid,omitempty"`
}

func stateDir(tenant string) string {
	home, _ := os.UserHomeDir()
	if tenant == "" {
		tenant = "default"
	}
	return filepath.Join(home, stateDirName, tenant)
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
// It takes a context for cancellation support.
// sessionIDOverride: if non-empty, skips state management entirely (caller manages lifecycle).
func resolveSession(ctx context.Context, tenant string, sessionIDOverride string) (string, error) {
	// Tier 0: explicit session ID — no state file, no auto-create
	if sessionIDOverride != "" {
		return sessionIDOverride, nil
	}

	// Tier 0.5: K8E_SANDBOX_SESSION_ID env var
	if sid := os.Getenv("K8E_SANDBOX_SESSION_ID"); sid != "" {
		return sid, nil
	}

	if tenant == "" {
		tenant = "default"
	}

	dir := stateDir(tenant)
	os.MkdirAll(dir, 0755) //nolint:errcheck

	// Open lock file for the state directory
	lf, err := os.Create(lockPath(tenant))
	if err != nil {
		return "", fmt.Errorf("create lock file: %w", err)
	}
	defer lf.Close()
	defer unlockFile(lf)

	for {
		if err := lockFile(lf); err != nil {
			return "", fmt.Errorf("lock: %w", err)
		}

		state, _ := loadState(tenant)
		unlockFile(lf)

		if state != nil && state.Phase == "active" && state.SessionID != "" {
			return state.SessionID, nil
		}

		if state != nil && state.Phase == "creating" {
			// another process is creating — wait and retry
			select {
			case <-ctx.Done():
				return "", ctx.Err()
			case <-time.After(200 * time.Millisecond):
			}
			continue
		}

		// no valid session — claim creator role
		if err := lockFile(lf); err != nil {
			return "", err
		}
		// Re-read after lock to avoid TOCTOU
		state, _ = loadState(tenant)
		if state != nil && state.Phase == "active" {
			unlockFile(lf)
			return state.SessionID, nil
		}

		placeholder := &SessionState{
			Phase:    "creating",
			TenantID: tenant,
			LockedAt: time.Now().UTC().Format(time.RFC3339),
			PID:      os.Getpid(),
		}
		if err := saveState(tenant, placeholder); err != nil {
			unlockFile(lf)
			return "", fmt.Errorf("save placeholder state: %w", err)
		}
		unlockFile(lf)

		// Phase 2: create session (no lock held)
		// This is called from commands.go which has the gRPC client.
		// For now, we return an empty string and let the caller handle creation.
		return "", nil
	}
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
