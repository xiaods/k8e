package grpc

import (
	"fmt"
	"strings"
	"unicode/utf8"

	sandboxv1 "github.com/xiaods/k8e/pkg/sandboxmatrix/api/v1alpha1"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
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

// deniedSessionEnvKeys are environment variables a session may not set.
//
// These are the runtime loader and path-override variables: a value here is
// interpreted by the language runtime or the dynamic loader rather than by
// the program, so an agent that injects LD_PRELOAD or PYTHONPATH can make
// the sandbox load code it chose - an escape from the intended environment,
// and a way to subvert a policy that only looks at the program's own
// behaviour. The list is case-insensitive (the lookups upper-case the key)
// because the loader variables are read case-sensitively on Linux but the
// same name in a different case is still an attempt.
//
// The deny-list is the same shape as the one CubeSandbox enforces, and it is
// applied at CreateSession so the refusal is explicit and names the
// offending variable instead of failing later inside the sandbox.
var deniedSessionEnvKeys = map[string]string{
	"LD_PRELOAD":            "runtime loader override",
	"LD_LIBRARY_PATH":       "runtime loader path override",
	"LD_AUDIT":              "runtime loader override",
	"LD_ORIGIN_PATH":        "runtime loader path override",
	"DYLD_INSERT_LIBRARIES": "dynamic loader override",
	"DYLD_LIBRARY_PATH":     "dynamic loader path override",
	"PYTHONPATH":            "module search path override",
	"PYTHONHOME":            "interpreter path override",
	"PYTHONSTARTUP":         "interpreter startup override",
	"NODE_PATH":             "module search path override",
	"NODE_OPTIONS":          "runtime option override",
	"PERL5LIB":              "module search path override",
	"PERLLIB":               "module search path override",
	"RUBYLIB":               "module search path override",
	"RUBYOPT":               "runtime option override",
	"GEM_PATH":              "gem search path override",
	"GEM_HOME":              "gem search path override",
	"JAVA_TOOL_OPTIONS":     "runtime option override",
	"_JAVA_OPTIONS":         "runtime option override",
	"CLASSPATH":             "class search path override",
	"IFS":                   "shell field separator override",
	"PATH":                  "command search path override",
	"ENV":                   "interpreter environment override",
	"BASH_ENV":              "shell startup file override",
	"GCONV_PATH":            "locale data path override",
}

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

// validateSessionEnvEntry checks one key/value pair.
func validateSessionEnvEntry(k, v string) error {
	if k == "" {
		return fmt.Errorf("empty key")
	}
	if denied, ok := deniedSessionEnvKeys[strings.ToUpper(k)]; ok {
		return fmt.Errorf("key %q is not allowed: %s", k, denied)
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

// mergeExecEnv merges a per-call env map over the session's own env. The
// per-call keys win: the E2B SDK's commands.run(envs={...}) is a per-call
// override, not a replacement of the session's environment.
func mergeExecEnv(sessionEnv, callEnv map[string]string) map[string]string {
	if len(callEnv) == 0 {
		return sessionEnv
	}
	merged := make(map[string]string, len(sessionEnv)+len(callEnv))
	for k, v := range sessionEnv {
		merged[k] = v
	}
	for k, v := range callEnv {
		merged[k] = v
	}
	return merged
}

// sandboxdExecBody builds the JSON body for sandboxd /exec and /exec/stream.
// env is omitted when empty so older sandboxd builds stay compatible.
func sandboxdExecBody(sessionID, command string, timeout int32, workdir string, env map[string]string) map[string]any {
	return sandboxdRequestBody(sessionID, command, "", timeout, workdir, env)
}

// sandboxdBackgroundBody builds the JSON body for sandboxd /exec/background.
func sandboxdBackgroundBody(sessionID, command, runID string, timeout int32, workdir string, env map[string]string) map[string]any {
	return sandboxdRequestBody(sessionID, command, runID, timeout, workdir, env)
}

func sandboxdRequestBody(sessionID, command, runID string, timeout int32, workdir string, env map[string]string) map[string]any {
	body := map[string]any{
		"command":    command,
		"timeout":    timeout,
		"workdir":    workdir,
		"session_id": sessionID,
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
		SessionId:      s.Name,
		Phase:          string(s.Status.Phase),
		RuntimeClass:   s.Spec.RuntimeClass,
		PodIp:          s.Status.PodIP,
		TenantId:       s.Spec.TenantID,
		BackgroundRuns: bgRuns,
		AllowedHosts:   s.Spec.AllowedHosts,
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
