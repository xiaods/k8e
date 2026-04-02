package sandboxmcp

import (
	"context"
	"encoding/json"
	"fmt"

	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// Tool defines a single MCP tool.
type Tool struct {
	Name        string
	Description string
	InputSchema map[string]any
	Handler     func(ctx context.Context, c *Client, args map[string]any) (string, error)
}

var allTools = []Tool{
	{
		Name:        "sandbox_create_session",
		Description: "Create a new isolated sandbox session in K8E. Returns a session_id for subsequent calls.",
		InputSchema: schema(props{
			"session_id":    {"type": "string", "description": "Optional custom session ID; auto-generated if omitted"},
			"tenant_id":     {"type": "string", "description": "Tenant identifier for multi-tenant isolation"},
			"runtime_class": {"type": "string", "description": "Isolation backend: gvisor (default), kata, firecracker"},
			"allowed_hosts": {"type": "array", "items": map[string]any{"type": "string"}, "description": "Egress allowlist (FQDN)"},
		}, nil),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.CreateSession(ctx, &pb.CreateSessionRequest{
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
		Description: "Destroy a sandbox session and clean up all associated resources.",
		InputSchema: schema(props{"session_id": {"type": "string", "description": "Session ID to destroy"}}, []string{"session_id"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.DestroySession(ctx, &pb.DestroySessionRequest{SessionId: str(args, "session_id")})
			if err != nil {
				return "", err
			}
			return jsonStr(map[string]any{"ok": resp.Ok}), nil
		},
	},
	{
		Name:        "sandbox_exec",
		Description: "Execute a shell command inside an isolated K8E sandbox pod. Returns stdout, stderr, and exit code.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"command":    {"type": "string", "description": "Shell command to execute"},
			"timeout":    {"type": "integer", "description": "Timeout in seconds (default 30)"},
			"workdir":    {"type": "string", "description": "Working directory (default /workspace)"},
		}, []string{"session_id", "command"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.Exec(ctx, &pb.ExecRequest{
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
		Description: "Execute a command and stream output. Returns accumulated stdout chunks.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"command":    {"type": "string", "description": "Shell command to execute"},
			"timeout":    {"type": "integer", "description": "Timeout in seconds (default 30)"},
			"workdir":    {"type": "string", "description": "Working directory (default /workspace)"},
		}, []string{"session_id", "command"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			stream, err := c.SandboxServiceClient.ExecStream(ctx, &pb.ExecRequest{
				SessionId: str(args, "session_id"),
				Command:   str(args, "command"),
				Timeout:   int32val(args, "timeout", 30),
				Workdir:   str(args, "workdir"),
			})
			if err != nil {
				return "", err
			}
			var out string
			for {
				chunk, err := stream.Recv()
				if err != nil {
					break
				}
				out += chunk.Chunk
			}
			return out, nil
		},
	},
	{
		Name:        "sandbox_write_file",
		Description: "Write a file into the sandbox workspace.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"path":       {"type": "string", "description": "File path inside sandbox"},
			"content":    {"type": "string", "description": "File content"},
			"mode":       {"type": "string", "description": "Write mode: w (overwrite, default) or a (append)"},
		}, []string{"session_id", "path", "content"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.WriteFile(ctx, &pb.WriteFileRequest{
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
		Description: "Read a file from the sandbox workspace.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"path":       {"type": "string", "description": "File path inside sandbox"},
		}, []string{"session_id", "path"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.ReadFile(ctx, &pb.ReadFileRequest{
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
		Description: "List files in the sandbox workspace modified since a Unix timestamp.",
		InputSchema: schema(props{
			"session_id": {"type": "string", "description": "Sandbox session ID"},
			"since":      {"type": "integer", "description": "Unix timestamp; 0 lists all files"},
		}, []string{"session_id"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.ListFiles(ctx, &pb.ListFilesRequest{
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
			"packages":   {"type": "array", "items": map[string]any{"type": "string"}, "description": "Package names to install"},
		}, []string{"session_id", "packages"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.PipInstall(ctx, &pb.PipInstallRequest{
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
		Description: "Spawn a child sandbox session under a parent session (max depth 1).",
		InputSchema: schema(props{
			"parent_session_id": {"type": "string", "description": "Parent session ID"},
			"agent_type":        {"type": "string", "description": "Agent type identifier"},
			"workspace_path":    {"type": "string", "description": "Workspace path for the sub-agent"},
		}, []string{"parent_session_id"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.RunSubAgent(ctx, &pb.RunSubAgentRequest{
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
		Description: "Gate an irreversible action on user approval. Call without approval_id to register; call with approval_id to poll result.",
		InputSchema: schema(props{
			"session_id":  {"type": "string", "description": "Sandbox session ID"},
			"action":      {"type": "string", "description": "Description of the action requiring approval"},
			"approval_id": {"type": "string", "description": "Approval ID from a previous call (to poll result)"},
		}, []string{"session_id"}),
		Handler: func(ctx context.Context, c *Client, args map[string]any) (string, error) {
			resp, err := c.SandboxServiceClient.ConfirmAction(ctx, &pb.ConfirmActionRequest{
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
