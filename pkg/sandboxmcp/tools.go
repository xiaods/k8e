package sandboxmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// Tool defines a single MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, s *Server, args map[string]any) (string, error)
}

const (
	descSessionID = "Sandbox session ID"
	descTimeout   = "Timeout in seconds (default 30)"
)

// newCreateReq constructs a CreateSessionRequest without copying the proto value (avoids Mutex copy).
func newCreateReq(tenantID string) *pb.CreateSessionRequest {
	return &pb.CreateSessionRequest{RuntimeClass: "gvisor", TenantId: tenantID}
}

func destroyReq(id string) *pb.DestroySessionRequest {
	return &pb.DestroySessionRequest{SessionId: id}
}

var allTools = []Tool{
	// ── High-level: agent uses these without managing sessions ──────────────
	{
		Name: "sandbox_run",
		Description: "Run code or a shell command in an isolated K8E sandbox. " +
			"Automatically manages the session — no need to call sandbox_create_session first. " +
			"Reuses the same sandbox for the entire conversation. " +
			"Supports language hints: python, bash, node (default: bash).",
		InputSchema: schema(props{
			"code":     {"type": "string", "description": "Code or shell command to execute"},
			"language": {"type": "string", "description": "Language hint: python, bash (default), node"},
			"timeout":  {"type": "integer", "description": descTimeout},
		}, []string{"code"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			sid, err := s.defaultSession(ctx)
			if err != nil {
				return "", err
			}
			cmd := buildCommand(str(args, "code"), str(args, "language"))
			execReq := &pb.ExecRequest{
				SessionId: sid,
				Command:   cmd,
				Timeout:   int32val(args, "timeout", 30),
				Workdir:   "/workspace",
			}
			resp, err := s.client.SandboxServiceClient.Exec(ctx, execReq)
			if err != nil {
				// session expired or destroyed — clear and retry once
				if containsStr(err.Error(), "not found") || containsStr(err.Error(), "no pod IP") {
					s.mu.Lock()
					s.defaultSessID = ""
					s.mu.Unlock()
					if sid, err = s.defaultSession(ctx); err != nil {
						return "", err
					}
					execReq.SessionId = sid
					// pod may still be starting — retry with backoff
					for i := 0; i < 12; i++ {
						resp, err = s.client.SandboxServiceClient.Exec(ctx, execReq)
						if err == nil || !containsStr(err.Error(), "no pod IP") {
							break
						}
						select {
						case <-ctx.Done():
							return "", ctx.Err()
						case <-waitTick():
						}
					}
				}
				if err != nil {
					return "", err
				}
			}
			return formatExecResult(resp.Stdout, resp.Stderr, resp.ExitCode, sid), nil
		},
	},
	{
		Name:        "sandbox_status",
		Description: "Check whether the K8E sandbox service is available and return the active session ID if one exists.",
		InputSchema: schema(props{}, nil),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			// lightweight probe: DestroySession with a nonexistent ID
			// "not found" means the service is up; connection errors mean it's down
			_, err := s.client.SandboxServiceClient.DestroySession(ctx, &pb.DestroySessionRequest{SessionId: "healthcheck-probe-noop"})
			available := err == nil || containsStr(err.Error(), "not found")

			s.mu.Lock()
			sid := s.defaultSessID
			s.mu.Unlock()

			return jsonStr(map[string]any{
				"available":  available,
				"session_id": sid,
			}), nil
		},
	},

	// ── Low-level: full lifecycle control ───────────────────────────────────
	{
		Name:        "sandbox_create_session",
		Description: "Create a new isolated sandbox session. Returns session_id. Use sandbox_run instead for simple code execution.",
		InputSchema: schema(props{
			"session_id":    {"type": "string", "description": "Optional custom ID; auto-generated if omitted"},
			"tenant_id":     {"type": "string", "description": "Tenant identifier"},
			"runtime_class": {"type": "string", "description": "gvisor (default), kata, firecracker"},
			"allowed_hosts": {"type": "array", "items": map[string]any{"type": "string"}, "description": "Egress FQDN allowlist"},
		}, nil),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.CreateSession(ctx, &pb.CreateSessionRequest{
				SessionId:    str(args, "session_id"),
				TenantId:     str(args, "tenant_id"),
				RuntimeClass: str(args, "runtime_class"),
				AllowedHosts: strs(args, "allowed_hosts"),
			})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"session_id": resp.SessionId, "pod_ip": resp.PodIp}), nil
		},
	},
	{
		Name:        "sandbox_destroy_session",
		Description: "Destroy a sandbox session and clean up all resources.",
		InputSchema: schema(props{"session_id": {"type": "string", "description": "Session ID to destroy"}}, []string{"session_id"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			sid := str(args, "session_id")
			resp, err := s.client.SandboxServiceClient.DestroySession(ctx, destroyReq(sid))
			if err != nil {
				return "", err
			}
			// clear default session if it was destroyed
			s.mu.Lock()
			if s.defaultSessID == sid {
				s.defaultSessID = ""
			}
			s.mu.Unlock()
			return jsonStr(map[string]any{"ok": resp.Ok}), nil
		},
	},
	{
		Name:        "sandbox_exec",
		Description: "Execute a shell command in a specific sandbox session. Returns stdout, stderr, exit_code.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": descSessionID},
			"command":    {"type": "string", "description": "Shell command to execute"},
			"timeout":    {"type": "integer", "description": descTimeout},
			"workdir":    {"type": "string", "description": "Working directory (default /workspace)"},
		}, []string{"session_id", "command"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.Exec(ctx, &pb.ExecRequest{
				SessionId: str(args, "session_id"),
				Command:   str(args, "command"),
				Timeout:   int32val(args, "timeout", 30),
				Workdir:   str(args, "workdir"),
			})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"stdout": resp.Stdout, "stderr": resp.Stderr, "exit_code": resp.ExitCode}), nil
		},
	},
	{
		Name:        "sandbox_exec_stream",
		Description: "Execute a command and return accumulated streaming output.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": descSessionID},
			"command":    {"type": "string", "description": "Shell command to execute"},
			"timeout":    {"type": "integer", "description": descTimeout},
			"workdir":    {"type": "string", "description": "Working directory (default /workspace)"},
		}, []string{"session_id", "command"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			stream, err := s.client.SandboxServiceClient.ExecStream(ctx, &pb.ExecRequest{
				SessionId: str(args, "session_id"),
				Command:   str(args, "command"),
				Timeout:   int32val(args, "timeout", 30),
				Workdir:   str(args, "workdir"),
			})
			if err != nil {
				return "", err
			}
			var sb strings.Builder
			for {
				chunk, err := stream.Recv()
				if err != nil {
					if err.Error() == "EOF" || containsStr(err.Error(), "EOF") {
						break
					}
					return sb.String(), fmt.Errorf("stream error: %w", err)
				}
				sb.WriteString(chunk.Chunk)
			}
			return sb.String(), nil
		},
	},
	{
		Name:        "sandbox_write_file",
		Description: "Write a file into the sandbox /workspace.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": descSessionID},
			"path":       {"type": "string", "description": "File path inside sandbox"},
			"content":    {"type": "string", "description": "File content"},
			"mode":       {"type": "string", "description": "w (overwrite, default) or a (append)"},
		}, []string{"session_id", "path", "content"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.WriteFile(ctx, &pb.WriteFileRequest{
				SessionId: str(args, "session_id"),
				Path:      str(args, "path"),
				Content:   str(args, "content"),
				Mode:      str(args, "mode"),
			})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"ok": resp.Ok}), nil
		},
	},
	{
		Name:        "sandbox_read_file",
		Description: "Read a file from the sandbox /workspace.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": descSessionID},
			"path":       {"type": "string", "description": "File path inside sandbox"},
		}, []string{"session_id", "path"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.ReadFile(ctx, &pb.ReadFileRequest{
				SessionId: str(args, "session_id"),
				Path:      str(args, "path"),
			})
			if err != nil {
				return "", err
			}
			return resp.Content, nil
		},
	},
	{
		Name:        "sandbox_list_files",
		Description: "List files in the sandbox /workspace modified since a Unix timestamp.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": descSessionID},
			"since":      {"type": "integer", "description": "Unix timestamp; 0 = all files"},
		}, []string{"session_id"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.ListFiles(ctx, &pb.ListFilesRequest{
				SessionId: str(args, "session_id"),
				Since:     int64val(args, "since"),
			})
			if err != nil {
				return "", err
			}
			files := make([]map[string]any, len(resp.Files))
			for i, f := range resp.Files {
				files[i] = map[string]any{"path": f.Path, "modified": f.Modified}
			}
			return jsonStr(map[string]any{"files": files}), nil
		},
	},
	{
		Name:        "sandbox_pip_install",
		Description: "Install Python packages inside the sandbox using pip.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": descSessionID},
			"packages":   {"type": "array", "items": map[string]any{"type": "string"}, "description": "Package names"},
		}, []string{"session_id", "packages"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.PipInstall(ctx, &pb.PipInstallRequest{
				SessionId: str(args, "session_id"),
				Packages:  strs(args, "packages"),
			})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"output": resp.Output, "exit_code": resp.ExitCode}), nil
		},
	},
	{
		Name:        "sandbox_run_subagent",
		Description: "Spawn a child sandbox under a parent session (max depth 1).",
		InputSchema: schema(props{
			"parent_session_id": {"type": "string", "description": "Parent session ID"},
			"agent_type":        {"type": "string", "description": "Agent type: research, coding, general"},
			"workspace_path":    {"type": "string", "description": "Shared sub-path under /workspace"},
		}, []string{"parent_session_id"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.RunSubAgent(ctx, &pb.RunSubAgentRequest{
				ParentSessionId: str(args, "parent_session_id"),
				AgentType:       str(args, "agent_type"),
				WorkspacePath:   str(args, "workspace_path"),
			})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"session_id": resp.SessionId}), nil
		},
	},
	{
		Name:        "sandbox_confirm_action",
		Description: "Gate an irreversible action on user approval. First call: provide session_id + action to register (returns approval_id). Second call: provide approval_id to poll for result.",
		InputSchema: schema(props{
			"session_id":  {"type": "string", "description": descSessionID},
			"action":      {"type": "string", "description": "Description of the action requiring approval (first call only)"},
			"approval_id": {"type": "string", "description": "Approval ID returned by first call (poll call only)"},
		}, []string{"session_id"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			resp, err := s.client.SandboxServiceClient.ConfirmAction(ctx, &pb.ConfirmActionRequest{
				SessionId:  str(args, "session_id"),
				Action:     str(args, "action"),
				ApprovalId: str(args, "approval_id"),
			})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"approval_id": resp.ApprovalId, "approved": resp.Approved}), nil
		},
	},
}

// waitTick returns a channel that fires after 5 seconds (pod startup backoff).
func waitTick() <-chan time.Time { return time.After(5 * time.Second) }

// buildCommand wraps code in the appropriate interpreter based on language hint.
// Multi-line code is written to a temp file and executed to avoid shell quoting issues.
// Code longer than 4KB is always written to a file to avoid ARG_MAX limits.
func buildCommand(code, lang string) string {
	multiline := strings.Contains(code, "\n") || len(code) > 4096
	switch strings.ToLower(lang) {
	case "python", "python3", "py":
		if multiline {
			escaped := strings.ReplaceAll(code, "'", "'\\''")
			return fmt.Sprintf("printf '%%s' '%s' > /tmp/_k8e_run.py && python3 /tmp/_k8e_run.py", escaped)
		}
		return fmt.Sprintf("python3 -c %q", code)
	case "node", "nodejs", "js", "javascript":
		if multiline {
			escaped := strings.ReplaceAll(code, "'", "'\\''")
			return fmt.Sprintf("printf '%%s' '%s' > /tmp/_k8e_run.js && node /tmp/_k8e_run.js", escaped)
		}
		return fmt.Sprintf("node -e %q", code)
	default: // bash / sh
		if multiline {
			escaped := strings.ReplaceAll(code, "'", "'\\''")
			return fmt.Sprintf("printf '%%s' '%s' | bash", escaped)
		}
		return code
	}
}

// formatExecResult returns a human-readable execution result, always including session_id.
func formatExecResult(stdout, stderr string, exitCode int32, sessionID string) string {
	if exitCode == 0 && stderr == "" {
		// clean success: return stdout + session_id for follow-up tool calls
		return jsonStr(map[string]any{"stdout": stdout, "exit_code": 0, "session_id": sessionID})
	}
	return jsonStr(map[string]any{"stdout": stdout, "stderr": stderr, "exit_code": exitCode, "session_id": sessionID})
}

type props = map[string]map[string]any

func schema(p props, required []string) map[string]any {
	properties := make(map[string]any, len(p))
	for k, v := range p {
		properties[k] = v
	}
	s := map[string]any{"type": "object", "properties": properties}
	if len(required) > 0 {
		s["required"] = required
	}
	return s
}

func jsonStr(v any) string {
	b, err := json.Marshal(v)
	if err != nil {
		return fmt.Sprintf("%v", v)
	}
	return string(b)
}
