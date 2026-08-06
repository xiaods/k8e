package grpc

import (
	"fmt"
	"unicode/utf8"
)

// Session env limits (KIP-12 / #483 hardening). Applied at CreateSession so
// oversized maps cannot fill etcd or explode per-exec sandboxd envp builds.
const (
	maxSessionEnvKeys       = 64
	maxSessionEnvKeyBytes   = 256
	maxSessionEnvValueBytes = 4 * 1024  // 4 KiB per value
	maxSessionEnvTotalBytes = 32 * 1024 // 32 KiB keys+values combined
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
		if k == "" {
			return fmt.Errorf("empty key")
		}
		if !utf8.ValidString(k) || !utf8.ValidString(v) {
			return fmt.Errorf("key %q: non-utf8 key or value", k)
		}
		if containsNUL(k) || containsNUL(v) {
			return fmt.Errorf("key %q: NUL byte not allowed in key or value", k)
		}
		if containsByte(k, '=') {
			return fmt.Errorf("key %q: '=' not allowed in env key", k)
		}
		kb := len(k)
		vb := len(v)
		if kb > maxSessionEnvKeyBytes {
			return fmt.Errorf("key %q: too long (%d bytes, max %d)", k, kb, maxSessionEnvKeyBytes)
		}
		if vb > maxSessionEnvValueBytes {
			return fmt.Errorf("key %q: value too long (%d bytes, max %d)", k, vb, maxSessionEnvValueBytes)
		}
		total += kb + vb
		if total > maxSessionEnvTotalBytes {
			return fmt.Errorf("total size exceeds %d bytes", maxSessionEnvTotalBytes)
		}
	}
	return nil
}

func containsNUL(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == 0 {
			return true
		}
	}
	return false
}

func containsByte(s string, b byte) bool {
	for i := 0; i < len(s); i++ {
		if s[i] == b {
			return true
		}
	}
	return false
}

// sandboxdExecBody builds the JSON body for sandboxd /exec and /exec/stream.
// env is omitted when empty so older sandboxd builds stay compatible.
func sandboxdExecBody(command string, timeout int32, workdir string, env map[string]string) map[string]any {
	body := map[string]any{
		"command": command,
		"timeout": timeout,
		"workdir": workdir,
	}
	if len(env) > 0 {
		body["env"] = env
	}
	return body
}

// sandboxdBackgroundBody builds the JSON body for sandboxd /exec/background.
func sandboxdBackgroundBody(command, runID string, timeout int32, workdir string, env map[string]string) map[string]any {
	body := map[string]any{
		"command": command,
		"run_id":  runID,
		"timeout": timeout,
		"workdir": workdir,
	}
	if len(env) > 0 {
		body["env"] = env
	}
	return body
}
