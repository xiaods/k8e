package sandboxmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// Tool defines a single MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, s *Server, args map[string]any) (string, error)
}

// sentinel values shared with server.go
var createReqDefault = pb.CreateSessionRequest{RuntimeClass: "gvisor"}

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
			"timeout":  {"type": "integer", "description": "Timeout in seconds (default 30)"},
		}, []string{"code"}),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			sid, err := s.defaultSession(ctx)
			if err != nil {
				return "", err
			}
			cmd := buildCommand(str(args, "code"), str(args, "language"))
			resp, err := s.client.SandboxServiceClient.Exec(ctx, &pb.ExecRequest{
				SessionId: sid,
				Command:   cmd,
				Timeout:   int32val(args, "timeout", 30),
				Workdir:   "/workspace",
			})
			if err != nil {
				return "", err
			}
			return formatExecResult(resp.Stdout, resp.Stderr, resp.ExitCode), nil
		},
	},
	{
		Name:        "sandbox_status",
		Description: "Check whether the K8E sandbox service is available and return the active session ID if one exists.",
		InputSchema: schema(props{}, nil),
		Handler: func(ctx context.Context, s *Server, args map[string]any) (string, error) {
			// probe by attempting a lightweight CreateSession with a dry-run session id
			_, err := s.client.SandboxServiceClient.CreateSession(ctx, &pb.CreateSessionRequest{
				SessionId:    "healthcheck-probe",
				RuntimeClass: "gvisor",
			})
			// destroy probe session regardless of error (best-effort)
			s.client.SandboxServiceClient.DestroySession(ctx, destroyReq("healthcheck-probe")) //nolint:errcheck

			s.mu.Lock()
			sid := s.defaultSessID
			s.mu.Unlock()

			if err != nil {
				return jsonStr(map[string]any{
					"available":  false,
					"session_id": sid,
					"error":      friendlyError(err),
				}), nil
			}
			return jsonStr(map[string]any{
				"available":  true,
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
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"command":    {"type": "string", "description": "Shell command to execute"},
			"timeout":    {"type": "integer", "description": "Timeout in seconds (default 30)"},
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
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"command":    {"type": "string", "description": "Shell command to execute"},
			"timeout":    {"type": "integer", "description": "Timeout in seconds (default 30)"},
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
					break
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
			"session_id": {"type": "string", "description": "Sandbox session ID"},
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
			"session_id": {"type": "string", "description": "Sandbox session ID"},
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
			"session_id": {"type": "string", "description": "Sandbox session ID"},
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
			"session_id": {"type": "string", "description": "Sandbox session ID"},
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
		Description: "Gate an irreversible action on user approval. Omit approval_id to register; provide it to poll result.",
		InputSchema: schema(props{
			"session_id":  {"type": "string", "description": "Sandbox session ID"},
			"action":      {"type": "string", "description": "Description of the action requiring approval"},
			"approval_id": {"type": "string", "description": "Approval ID from a previous call (to poll)"},
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

// buildCommand wraps code in the appropriate interpreter based on language hint.
func buildCommand(code, lang string) string {
	switch strings.ToLower(lang) {
	case "python", "python3", "py":
		return fmt.Sprintf("python3 -c %q", code)
	case "node", "nodejs", "js", "javascript":
		return fmt.Sprintf("node -e %q", code)
	default:
		return code // bash / sh
	}
}

// formatExecResult returns a human-readable execution result.
func formatExecResult(stdout, stderr string, exitCode int32) string {
	if exitCode == 0 && stderr == "" {
		return stdout
	}
	return jsonStr(map[string]any{"stdout": stdout, "stderr": stderr, "exit_code": exitCode})
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
