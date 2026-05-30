package sandboxcli

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"github.com/xiaods/k8e/pkg/sandboxmcp"
)

// newClient wraps sandboxmcp.NewClient with fatal error handling.
// On connection failure, prints JSON error and exits with code 2.
func newClient() *sandboxmcp.Client {
	c, err := sandboxmcp.NewClient()
	if err != nil {
		printErrorExit("sandbox not reachable: "+err.Error(), 2)
	}
	return c
}

// readCode returns code from arg (priority) or stdin pipe.
func readCode(ctx *cli.Context) (string, error) {
	code := ctx.Args().First()
	if code != "" {
		return code, nil
	}
	stat, _ := os.Stdin.Stat()
	if (stat.Mode() & os.ModeCharDevice) != 0 {
		return "", fmt.Errorf("code required (provide as argument or via stdin)")
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin: %w", err)
	}
	return string(data), nil
}

// isMultiLine returns true if code contains a newline.
func isMultiLine(code string) bool {
	return strings.Contains(code, "\n")
}

// buildCommand wraps code for a given language.
// bash: pass through as-is. python/node: single-line uses -c, multi-line uses temp file.
func buildCommand(lang, code string) string {
	switch strings.ToLower(lang) {
	case "python", "python3", "py":
		if isMultiLine(code) {
			return "python3 /tmp/_k8e_run.py"
		}
		return fmt.Sprintf("python3 -c %q", code)
	case "node", "nodejs", "js", "javascript":
		if isMultiLine(code) {
			return "node /tmp/_k8e_run.js"
		}
		return fmt.Sprintf("node -e %q", code)
	default: // bash / sh
		return code
	}
}

// writeCodeFile writes multi-line code to the sandbox workspace via WriteFile RPC.
func writeCodeFile(client *sandboxmcp.Client, sid, lang, code string) error {
	path := "/tmp/_k8e_run.py"
	switch strings.ToLower(lang) {
	case "node", "nodejs", "js", "javascript":
		path = "/tmp/_k8e_run.js"
	}
	_, err := client.SandboxServiceClient.WriteFile(context.Background(), &pb.WriteFileRequest{
		SessionId: sid, Path: path, Content: code, Mode: "w",
	})
	return err
}

// ── RunCommand ──────────────────────────────────────────────────────────────

func RunCommand() cli.Command {
	return cli.Command{
		Name:      "run",
		Usage:     "Run code or a shell command in a sandbox. Auto-creates session if needed.",
		ArgsUsage: "<code>",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "lang", Value: "bash", Usage: "Language hint: python, bash (default), node"},
			cli.IntFlag{Name: "timeout", Value: 30, Usage: "Timeout in seconds"},
			cli.StringFlag{Name: "session-id", EnvVar: "K8E_SANDBOX_SESSION_ID", Usage: "Explicit session ID"},
			cli.StringFlag{Name: "tenant", EnvVar: "K8E_SANDBOX_TENANT", Usage: "Tenant for cross-process session reuse"},
			cli.BoolFlag{Name: "raw", Usage: "Stream raw output (no JSON wrapper)"},
		},
		Action: func(ctx *cli.Context) error {
			code, err := readCode(ctx)
			if err != nil {
				printErrorExit(err.Error(), 1)
				return nil
			}
			lang := ctx.String("lang")
			raw := ctx.Bool("raw")

			client := newClient()
			defer client.Close()

			sid, err := resolveSession(context.Background(), ctx.String("tenant"), ctx.String("session-id"))
			if err != nil {
				printErrorExit(err.Error(), 2)
				return nil
			}

			// auto-create session if none exists
			needsFinalize := false
			if sid == "" {
				resp, err := client.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
					TenantId:     ctx.String("tenant"),
					RuntimeClass: "gvisor",
				})
				if err != nil {
					printErrorExit("create session: "+err.Error(), 2)
					return nil
				}
				sid = resp.SessionId
				needsFinalize = true
			}

			// python/node multi-line: write file first, then execute
			lowLang := strings.ToLower(lang)
			if (lowLang == "python" || lowLang == "python3" || lowLang == "py" ||
				lowLang == "node" || lowLang == "nodejs" || lowLang == "js" || lowLang == "javascript") &&
				isMultiLine(code) {
				if err := writeCodeFile(client, sid, lang, code); err != nil {
					printErrorExit("write code: "+err.Error(), 1)
					return nil
				}
			}

			cmd := buildCommand(lang, code)
			execReq := &pb.ExecRequest{
				SessionId: sid, Command: cmd,
				Timeout: int32(ctx.Int("timeout")), Workdir: "/workspace",
			}

			if raw {
				return runStream(client, execReq)
			}
			return runJSON(client, execReq, sid, needsFinalize, ctx.String("tenant"))
		},
	}
}

func runStream(client *sandboxmcp.Client, req *pb.ExecRequest) error {
	stream, err := client.SandboxServiceClient.ExecStream(context.Background(), req)
	if err != nil {
		printErrorExit("exec: "+err.Error(), 1)
		return nil
	}
	for {
		chunk, err := stream.Recv()
		if err != nil {
			if err.Error() == "EOF" || strings.Contains(err.Error(), "EOF") {
				break
			}
			logrus.Debugf("stream error: %v", err)
			break
		}
		os.Stdout.WriteString(chunk.Chunk)
	}
	os.Exit(0)
	return nil
}

func runJSON(client *sandboxmcp.Client, req *pb.ExecRequest, sid string, needsFinalize bool, tenant string) error {
	resp, err := client.SandboxServiceClient.Exec(context.Background(), req)
	if err != nil {
		if strings.Contains(err.Error(), "not found") || strings.Contains(err.Error(), "no pod IP") {
			printErrorExit("session not found: "+sid+" (expired or destroyed)", 1)
			return nil
		}
		printErrorExit("exec: "+err.Error(), 2)
		return nil
	}
	if needsFinalize {
		_ = finalizeState(tenant, sid)
	}
	printJSON(map[string]any{"stdout": resp.Stdout, "stderr": resp.Stderr, "exit_code": resp.ExitCode, "session_id": sid})
	return nil
}

// ── StatusCommand ───────────────────────────────────────────────────────────

func StatusCommand() cli.Command {
	return cli.Command{
		Name:  "status",
		Usage: "Check sandbox service availability and current session",
		Action: func(ctx *cli.Context) error {
			client := newClient()
			defer client.Close()

			// lightweight probe
			_, err := client.SandboxServiceClient.DestroySession(context.Background(),
				&pb.DestroySessionRequest{SessionId: "healthcheck-probe-noop"})
			available := err == nil || strings.Contains(err.Error(), "not found")

			sid, tid := "", ""
			if state, _ := loadState("default"); state != nil && state.SessionID != "" {
				sid = state.SessionID
				tid = state.TenantID
			}
			if envSid := os.Getenv("K8E_SANDBOX_SESSION_ID"); envSid != "" && sid == "" {
				sid = envSid
			}
			if t := os.Getenv("K8E_SANDBOX_TENANT"); t != "" && tid == "" {
				tid = t
			}

			printJSON(map[string]any{"available": available, "session_id": sid, "tenant_id": tid})
			return nil
		},
	}
}

// ── CreateCommand ───────────────────────────────────────────────────────────

func CreateCommand() cli.Command {
	return cli.Command{
		Name:  "create",
		Usage: "Create a new sandbox session",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "runtime", Value: "gvisor", Usage: "Runtime class: gvisor, kata, firecracker"},
			cli.StringFlag{Name: "tenant", EnvVar: "K8E_SANDBOX_TENANT", Usage: "Tenant identifier"},
			cli.StringFlag{Name: "allowed-hosts", Usage: "Comma-separated FQDN egress allowlist"},
			cli.StringFlag{Name: "session-id", Usage: "Custom session ID"},
		},
		Action: func(ctx *cli.Context) error {
			client := newClient()
			defer client.Close()

			var hosts []string
			if raw := ctx.String("allowed-hosts"); raw != "" {
				hosts = strings.Split(raw, ",")
			}

			resp, err := client.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
				SessionId:    ctx.String("session-id"),
				TenantId:     ctx.String("tenant"),
				RuntimeClass: ctx.String("runtime"),
				AllowedHosts: hosts,
			})
			if err != nil {
				printErrorExit("create session: "+err.Error(), 2)
				return nil
			}

			// always write state file (unless explicit session-id)
			if ctx.String("session-id") == "" {
				_ = finalizeState(ctx.String("tenant"), resp.SessionId)
			}

			printJSON(map[string]any{"session_id": resp.SessionId, "pod_ip": resp.PodIp})
			return nil
		},
	}
}

// ── DestroyCommand ──────────────────────────────────────────────────────────

func DestroyCommand() cli.Command {
	return cli.Command{
		Name:      "destroy",
		Usage:     "Destroy a sandbox session and free resources",
		ArgsUsage: "<session-id>",
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().First()
			if sid == "" {
				printErrorExit("session-id required", 1)
				return nil
			}
			client := newClient()
			defer client.Close()

			resp, err := client.SandboxServiceClient.DestroySession(context.Background(),
				&pb.DestroySessionRequest{SessionId: sid})
			if err != nil {
				printErrorExit("destroy: "+err.Error(), 1)
				return nil
			}

			// clear state file if it matches this session
			for _, tenant := range []string{"default", os.Getenv("K8E_SANDBOX_TENANT")} {
				if tenant == "" {
					continue
				}
				if state, _ := loadState(tenant); state != nil && state.SessionID == sid {
					_ = clearState(tenant)
					break
				}
			}
			printJSON(map[string]any{"ok": resp.Ok})
			return nil
		},
	}
}

// ── WriteCommand ────────────────────────────────────────────────────────────

func WriteCommand() cli.Command {
	return cli.Command{
		Name:      "write",
		Usage:     "Write a file into the sandbox /workspace (content from stdin)",
		ArgsUsage: "<session-id> <path>",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "mode", Value: "w", Usage: "Write mode: w (overwrite), a (append)"},
		},
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().Get(0)
			path := ctx.Args().Get(1)
			if sid == "" || path == "" {
				printErrorExit("usage: k8e sandbox write <session-id> <path>", 1)
				return nil
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				printErrorExit("read stdin: "+err.Error(), 1)
				return nil
			}

			client := newClient()
			defer client.Close()

			resp, err := client.SandboxServiceClient.WriteFile(context.Background(), &pb.WriteFileRequest{
				SessionId: sid, Path: path, Content: string(data), Mode: ctx.String("mode"),
			})
			if err != nil {
				printErrorExit("write: "+err.Error(), 1)
				return nil
			}
			printJSON(map[string]any{"ok": resp.Ok, "path": path})
			return nil
		},
	}
}

// ── ReadCommand ─────────────────────────────────────────────────────────────

func ReadCommand() cli.Command {
	return cli.Command{
		Name:      "read",
		Usage:     "Read a file from the sandbox /workspace",
		ArgsUsage: "<session-id> <path>",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "raw", Usage: "Output raw file content (no JSON wrapper)"},
		},
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().Get(0)
			path := ctx.Args().Get(1)
			if sid == "" || path == "" {
				printErrorExit("usage: k8e sandbox read <session-id> <path>", 1)
				return nil
			}

			client := newClient()
			defer client.Close()

			resp, err := client.SandboxServiceClient.ReadFile(context.Background(), &pb.ReadFileRequest{
				SessionId: sid, Path: path,
			})
			if err != nil {
				printErrorExit("read: "+err.Error(), 1)
				return nil
			}
			if ctx.Bool("raw") {
				fmt.Print(resp.Content)
			} else {
				printJSON(map[string]any{"content": resp.Content, "path": path})
			}
			return nil
		},
	}
}

// ── ListCommand ─────────────────────────────────────────────────────────────

func ListCommand() cli.Command {
	return cli.Command{
		Name:      "list",
		Usage:     "List files in the sandbox /workspace",
		ArgsUsage: "<session-id>",
		Flags: []cli.Flag{
			cli.Int64Flag{Name: "since", Usage: "Unix timestamp; only show files modified since"},
		},
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().First()
			if sid == "" {
				printErrorExit("session-id required", 1)
				return nil
			}

			client := newClient()
			defer client.Close()

			resp, err := client.SandboxServiceClient.ListFiles(context.Background(), &pb.ListFilesRequest{
				SessionId: sid, Since: ctx.Int64("since"),
			})
			if err != nil {
				printErrorExit("list: "+err.Error(), 1)
				return nil
			}
			files := make([]map[string]any, len(resp.Files))
			for i, f := range resp.Files {
				files[i] = map[string]any{"path": f.Path, "modified": f.Modified}
			}
			printJSON(map[string]any{"files": files})
			return nil
		},
	}
}

// ── SubagentCommand ─────────────────────────────────────────────────────────

func SubagentCommand() cli.Command {
	return cli.Command{
		Name:      "subagent",
		Usage:     "Spawn a child sandbox under a parent session (max depth 1)",
		ArgsUsage: "<parent-session-id>",
		Action: func(ctx *cli.Context) error {
			parentSid := ctx.Args().First()
			if parentSid == "" {
				printErrorExit("parent-session-id required", 1)
				return nil
			}

			client := newClient()
			defer client.Close()

			resp, err := client.SandboxServiceClient.RunSubAgent(context.Background(), &pb.RunSubAgentRequest{
				ParentSessionId: parentSid,
			})
			if err != nil {
				printErrorExit("subagent: "+err.Error(), 1)
				return nil
			}
			printJSON(map[string]any{"session_id": resp.SessionId})
			return nil
		},
	}
}

// ── ConfirmCommand ──────────────────────────────────────────────────────────

func ConfirmCommand() cli.Command {
	return cli.Command{
		Name:      "confirm",
		Usage:     "Register an irreversible action and wait for human approval",
		ArgsUsage: "<session-id> <action>",
		Flags: []cli.Flag{
			cli.IntFlag{Name: "timeout", Value: 30, Usage: "Approval timeout in seconds"},
			cli.BoolFlag{Name: "no-wait", Usage: "Register only, return approval_id immediately"},
		},
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().Get(0)
			action := ctx.Args().Get(1)
			if sid == "" || action == "" {
				printErrorExit("usage: k8e sandbox confirm <session-id> <action>", 1)
				return nil
			}

			client := newClient()
			defer client.Close()

			// Phase 1: register
			resp, err := client.SandboxServiceClient.ConfirmAction(context.Background(), &pb.ConfirmActionRequest{
				SessionId: sid, Action: action,
			})
			if err != nil {
				printErrorExit("confirm register: "+err.Error(), 2)
				return nil
			}

			if ctx.Bool("no-wait") {
				printJSON(map[string]any{"approved": resp.Approved, "approval_id": resp.ApprovalId})
				return nil
			}

			// Print approval prompt to stderr immediately
			fmt.Fprintf(os.Stderr, "[k8e-sandbox] ⚠  Approval required: %s\n", action)
			fmt.Fprintf(os.Stderr, "[k8e-sandbox]    To approve: k8e sandbox approve %s\n", resp.ApprovalId)
			fmt.Fprintf(os.Stderr, "[k8e-sandbox]    Timeout: %ds\n", ctx.Int("timeout"))

			// Phase 2: poll for approval (blocks)
			cctx, cancel := context.WithTimeout(context.Background(), timeDuration(ctx.Int("timeout")))
			defer cancel()

			pollResp, err := client.SandboxServiceClient.ConfirmAction(cctx, &pb.ConfirmActionRequest{
				ApprovalId: resp.ApprovalId,
			})
			if err != nil {
				printErrorExit("confirm timeout: "+err.Error(), 1)
				return nil
			}
			printJSON(map[string]any{"approved": pollResp.Approved, "approval_id": resp.ApprovalId})
			return nil
		},
	}
}

// ── ApproveCommand ──────────────────────────────────────────────────────────

func ApproveCommand() cli.Command {
	return cli.Command{
		Name:      "approve",
		Usage:     "Approve or reject a pending sandbox action",
		ArgsUsage: "<approval-id>",
		Flags: []cli.Flag{
			cli.BoolFlag{Name: "reject", Usage: "Reject the pending action"},
			cli.StringFlag{Name: "reason", Usage: "Reason for rejection"},
		},
		Action: func(ctx *cli.Context) error {
			aid := ctx.Args().First()
			if aid == "" {
				printErrorExit("approval-id required", 1)
				return nil
			}
			approved := !ctx.Bool("reject")

			client := newClient()
			defer client.Close()

			resp, err := client.SandboxServiceClient.ApproveAction(context.Background(), &pb.ApproveActionRequest{
				ApprovalId: aid, Approved: approved, Reason: ctx.String("reason"),
			})
			if err != nil {
				printErrorExit("approve: "+err.Error(), 1)
				return nil
			}
			printJSON(map[string]any{"ok": resp.Ok})
			return nil
		},
	}
}

// timeDuration converts seconds to time.Duration.
func timeDuration(seconds int) time.Duration {
	return time.Duration(seconds) * time.Second
}
