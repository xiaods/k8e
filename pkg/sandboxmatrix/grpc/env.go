package grpc

import (
	"fmt"
	"strings"
	"unicode/utf8"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
)

// Session env limits (KIP-12 / #483 hardening). Applied at CreateSession so
// oversized maps cannot fill etcd or explode per-exec sandboxd envp builds.
const (
	maxSessionEnvKeys       = 64
	maxSessionEnvKeyBytes   = 256
	maxSessionEnvValueBytes = 4 * 1024  // 4 KiB per value
	maxSessionEnvTotalBytes = 32 * 1024 // 32 KiB keys+values combined
	maxSessionSecretRefs    = 32
	// maxExecOutputBytes is the per-stream capture cap for agent-facing truncation (~1 MiB).
	maxExecOutputBytes = 1 * 1024 * 1024
)

// validateSessionEnv checks a CreateSession env map against size and key rules.
// nil/empty maps are valid (no env configured).
func validateSessionEnv(env map[string]string) error {
	if len(env) == 0 {
		return nil
	}
	if len(env) > maxSessionEnvKeys {
		return fmt.Errorf("too many entries: %d (max %d)", len(env), maxSessionEnvKeys)
	}
	total := 0
	for k, v := range env {
		if err := validateSessionEnvEntry(k, v); err != nil {
			return err
		}
		total += len(k) + len(v)
		if total > maxSessionEnvTotalBytes {
			return fmt.Errorf("total size exceeds %d bytes", maxSessionEnvTotalBytes)
		}
	}
	return nil
}

func validateSessionEnvEntry(k, v string) error {
	if k == "" {
		return fmt.Errorf("empty key")
	}
	if !utf8.ValidString(k) || !utf8.ValidString(v) {
		return fmt.Errorf("key %q: non-utf8 key or value", k)
	}
	if strings.IndexByte(k, 0) >= 0 || strings.IndexByte(v, 0) >= 0 {
		return fmt.Errorf("key %q: NUL byte not allowed in key or value", k)
	}
	if strings.Contains(k, "=") {
		return fmt.Errorf("key %q: '=' not allowed in env key", k)
	}
	if len(k) > maxSessionEnvKeyBytes {
		return fmt.Errorf("key %q: too long (%d bytes, max %d)", k, len(k), maxSessionEnvKeyBytes)
	}
	if len(v) > maxSessionEnvValueBytes {
		return fmt.Errorf("key %q: value too long (%d bytes, max %d)", k, len(v), maxSessionEnvValueBytes)
	}
	return nil
}

// sandboxdExecBody builds the JSON body for sandboxd /exec and /exec/stream.
// env is omitted when empty so older sandboxd builds stay compatible.
func sandboxdExecBody(command string, timeout int32, workdir string, env map[string]string) map[string]any {
	return sandboxdRequestBody(command, "", timeout, workdir, env)
}

// sandboxdBackgroundBody builds the JSON body for sandboxd /exec/background.
func sandboxdBackgroundBody(command, runID string, timeout int32, workdir string, env map[string]string) map[string]any {
	return sandboxdRequestBody(command, runID, timeout, workdir, env)
}

func sandboxdRequestBody(command, runID string, timeout int32, workdir string, env map[string]string) map[string]any {
	body := map[string]any{
		"command": command,
		"timeout": timeout,
		"workdir": workdir,
	}
	if runID != "" {
		body["run_id"] = runID
	}
	if len(env) > 0 {
		body["env"] = env
	}
	return body
}

func validateSecretRefs(refs []*pb.SecretRef) error {
	if len(refs) == 0 {
		return nil
	}
	if len(refs) > maxSessionSecretRefs {
		return fmt.Errorf("too many secret_refs: %d (max %d)", len(refs), maxSessionSecretRefs)
	}
	seen := make(map[string]struct{}, len(refs))
	for _, r := range refs {
		if r == nil {
			return fmt.Errorf("nil secret_ref")
		}
		if r.SecretName == "" || r.Key == "" || r.EnvVar == "" {
			return fmt.Errorf("secret_name, key, and env_var are required")
		}
		if err := validateSessionEnvEntry(r.EnvVar, "x"); err != nil {
			// reuse key rules for env_var name (value placeholder)
			return fmt.Errorf("env_var %q: %w", r.EnvVar, err)
		}
		if _, ok := seen[r.EnvVar]; ok {
			return fmt.Errorf("duplicate env_var %q", r.EnvVar)
		}
		seen[r.EnvVar] = struct{}{}
	}
	return nil
}

func pbSecretRefsToAPI(refs []*pb.SecretRef) []sandboxv1.SecretRef {
	if len(refs) == 0 {
		return nil
	}
	out := make([]sandboxv1.SecretRef, 0, len(refs))
	for _, r := range refs {
		if r == nil {
			continue
		}
		out = append(out, sandboxv1.SecretRef{
			SecretName: r.SecretName,
			Key:        r.Key,
			EnvVar:     r.EnvVar,
		})
	}
	return out
}

// execStatusCompleted is returned when the process finished (any exit code).
const (
	execStatusStarted   = "started"
	execStatusRunning   = "running"
	execStatusCompleted = "completed"
	execStatusTimedOut  = "timed_out"
	execStatusFailed    = "failed"
)

// classifyExecStatus maps a finished execution to a neutral status string.
// Non-zero exit_code remains completed (runtime error); only timeout is timed_out.
func classifyExecStatus(timedOut bool, hadTransportError bool) string {
	if hadTransportError {
		return execStatusFailed
	}
	if timedOut {
		return execStatusTimedOut
	}
	return execStatusCompleted
}

// sessionToProtoView builds a public-safe GetSessionResponse (no secret values).
func sessionToProtoView(s *sandboxv1.SandboxSession, bgRuns int32) *pb.GetSessionResponse {
	if s == nil {
		return nil
	}
	view := &pb.GetSessionResponse{
		SessionId:       s.Name,
		Phase:           string(s.Status.Phase),
		RuntimeClass:    s.Spec.RuntimeClass,
		PodIp:           s.Status.PodIP,
		TenantId:        s.Spec.TenantID,
		BackgroundRuns:  bgRuns,
	}
	if s.Status.ExpiresAt != nil {
		view.ExpiresAt = s.Status.ExpiresAt.Unix()
	}
	if len(s.Spec.Env) > 0 {
		keys := make([]string, 0, len(s.Spec.Env))
		for k := range s.Spec.Env {
			keys = append(keys, k)
		}
		view.EnvKeys = keys
	}
	if len(s.Spec.SecretRefs) > 0 {
		vars := make([]string, 0, len(s.Spec.SecretRefs))
		for _, r := range s.Spec.SecretRefs {
			if r.EnvVar != "" {
				vars = append(vars, r.EnvVar)
			}
		}
		view.SecretEnvVars = vars
	}
	return view
}
