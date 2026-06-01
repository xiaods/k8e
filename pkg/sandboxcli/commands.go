package sandboxcli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
	"github.com/xiaods/k8e/pkg/sandbox/client"
)

const errSessionNotFound = "not found"

// ErrSessionGone is returned when a sandbox session has expired or been destroyed.
var ErrSessionGone = errors.New("sandbox session expired or destroyed")

// newClientFromCtx creates a gRPC client using endpoint/apikey from global flags.
func newClientFromCtx(ctx *cli.Context) *client.Client {
	endpoint := ctx.GlobalString("endpoint")
	apikey := ctx.GlobalString("apikey")

	var c *client.Client
	var err error
	if endpoint != "" {
		c, err = client.NewClientWithEndpoint(endpoint, apikey)
	} else {
		c, err = client.NewClient()
	}
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

// ensureSession resolves session ID, auto-creating one with manifest if needed.
// Returns (sessionID, needsFinalize, error).
func ensureSession(client *client.Client, ctx *cli.Context) (string, bool, error) {
	sid, err := resolveSession(context.Background(), ctx.String("tenant"), ctx.String("session-id"))
	if err != nil {
		return "", false, err
	}
	if sid != "" {
		return sid, false, nil
	}

	resp, err := client.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
		TenantId: ctx.String("tenant"), RuntimeClass: "gvisor",
	})
	if err != nil {
		return "", false, fmt.Errorf("create session: %w", err)
	}
	sid = resp.SessionId

	manifest, mErr := resolveManifest(ctx)
	if mErr != nil {
		client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
		return "", false, fmt.Errorf("manifest: %w", mErr)
	}
	if manifest != nil {
		if err := materializeManifest(client, sid, manifest); err != nil {
			client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
			return "", false, fmt.Errorf("manifest materialization: %w", err)
		}
	}
	return sid, true, nil
}

// isInterpretedLang returns true for languages that need shell wrapping.
func isInterpretedLang(lang string) bool {
	switch strings.ToLower(lang) {
	case "python", "python3", "py", "node", "nodejs", "js", "javascript", "ts", "typescript":
		return true
	}
	return false
}

// buildCommand wraps code for a given language.
// bash: pass through as-is. python/node/ts: single-line uses -c/-e, multi-line uses temp file.
func buildCommand(lang, code string) string {
	switch strings.ToLower(lang) {
	case "python", "python3", "py":
		if isMultiLine(code) {
			return "python3 /workspace/_k8e_run.py"
		}
		return fmt.Sprintf("python3 -c %q", code)
	case "node", "nodejs", "js", "javascript":
		if isMultiLine(code) {
			return "node /workspace/_k8e_run.js"
		}
		return fmt.Sprintf("node -e %q", code)
	case "ts", "typescript":
		// tsx always creates /tmp/tsx-0. Redirect via TMPDIR.
		return "TMPDIR=/workspace tsx /workspace/_k8e_run.ts"
	default: // bash / sh
		return code
	}
}

// writeCodeFile writes multi-line code to the sandbox workspace via WriteFile RPC.
func writeCodeFile(client *client.Client, sid, lang, code string) error {
	path := "/workspace/_k8e_run.py"
	switch strings.ToLower(lang) {
	case "node", "nodejs", "js", "javascript":
		path = "/workspace/_k8e_run.js"
	case "ts", "typescript":
		path = "/workspace/_k8e_run.ts"
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
			cli.StringFlag{Name: "lang", Value: "bash", Usage: "Language hint: python, bash (default), node, ts"},
			cli.IntFlag{Name: "timeout", Value: 30, Usage: "Timeout in seconds"},
			cli.StringFlag{Name: "session-id", EnvVar: "K8E_SANDBOX_SESSION_ID", Usage: "Explicit session ID"},
			cli.StringFlag{Name: "tenant", EnvVar: "K8E_SANDBOX_TENANT", Usage: "Tenant for cross-process session reuse"},
			cli.BoolFlag{Name: "raw", Usage: "Stream raw output (no JSON wrapper)"},
			cli.StringFlag{Name: "manifest", Usage: "Path to workspace manifest (only when auto-creating session)"},
			cli.StringFlag{Name: "git-repo", Usage: "Git repo to clone (only when auto-creating session)"},
			cli.StringFlag{Name: "git-ref", Value: "main", Usage: "Git ref for --git-repo"},
			cli.StringFlag{Name: "git-path", Value: "repo", Usage: "Destination path for --git-repo"},
		},
		Action: runAction,
	}
}

func runAction(ctx *cli.Context) error {
	code, err := readCode(ctx)
	if err != nil {
		printErrorExit(err.Error(), 1)
		return nil
	}
	lang := ctx.String("lang")
	raw := ctx.Bool("raw")

	cli := newClientFromCtx(ctx)
	defer cli.Close()

	sid, needsFinalize, err := ensureSession(cli, ctx)
	if err != nil {
		printErrorExit(err.Error(), 2)
		return nil
	}

	if isInterpretedLang(lang) && (isMultiLine(code) || strings.HasPrefix(lang, "ts")) {
		if err := writeCodeFile(cli, sid, lang, code); err != nil {
			printErrorExit("write code: "+err.Error(), 1)
			return nil
		}
	}

	cmd := buildCommand(lang, code)
	req := &pb.ExecRequest{SessionId: sid, Command: cmd, Timeout: int32(ctx.Int("timeout")), Workdir: "/workspace"}

	retry := func() (int, error) {
		clearState(ctx.String("tenant"))
		s, f, e := ensureSession(cli, ctx)
		if e != nil {
			return 1, e
		}
		req.SessionId = s
		if raw {
			return runStream(cli, req, s, f, ctx.String("tenant"))
		}
		err := runJSON(cli, req, s, f, ctx.String("tenant"))
		return 0, err
	}

	if raw {
		exitCode, err := runStream(cli, req, sid, needsFinalize, ctx.String("tenant"))
		if isSessionExpired(err) {
			exitCode, err = retry()
		}
		if err != nil {
			return err
		}
		os.Exit(exitCode)
		return nil
	}
	if err := runJSON(cli, req, sid, needsFinalize, ctx.String("tenant")); isSessionExpired(err) {
		_, err = retry()
		if err != nil {
			return err
		}
	}
	return err
}

func runStream(client *client.Client, req *pb.ExecRequest, sid string, needsFinalize bool, tenant string) (exitCode int, err error) {
	stream, err := client.SandboxServiceClient.ExecStream(context.Background(), req)
	if err != nil {
		if isSessionExpired(err) {
			return 1, fmt.Errorf("%w: session %s", ErrSessionGone, req.SessionId)
		}
		logrus.Errorf("exec stream: %v", err)
		return 1, err
	}

	var streamErr error
	for {
		chunk, recvErr := stream.Recv()
		if recvErr != nil {
			if recvErr.Error() == "EOF" || strings.Contains(recvErr.Error(), "EOF") {
				break
			}
			streamErr = recvErr
			break
		}
		os.Stdout.WriteString(chunk.Chunk)
	}

	if needsFinalize {
		_ = finalizeState(tenant, sid)
	}

	if streamErr != nil {
		fmt.Fprintf(os.Stderr, "stream error: %v\n", streamErr)
		return 1, nil
	}
	return 0, nil
}

func isSessionExpired(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, ErrSessionGone) {
		return true
	}
	st, ok := status.FromError(err)
	if !ok {
		return false
	}
	switch st.Code() {
	case codes.NotFound:
		return true
	case codes.FailedPrecondition:
		return true
	}
	return false
}

func runJSON(client *client.Client, req *pb.ExecRequest, sid string, needsFinalize bool, tenant string) error {
	resp, err := client.SandboxServiceClient.Exec(context.Background(), req)
	if err != nil {
		if isSessionExpired(err) {
			return fmt.Errorf("%w: session %s", ErrSessionGone, sid)
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
			client := newClientFromCtx(ctx)
			defer client.Close()

			// lightweight probe
		_, err := client.SandboxServiceClient.DestroySession(context.Background(),
			&pb.DestroySessionRequest{SessionId: "healthcheck-probe-noop"})
		available := err == nil || strings.Contains(err.Error(), errSessionNotFound)
		errMsg := ""
		if !available && err != nil {
			errMsg = err.Error()
		}

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

			printJSON(map[string]any{"available": available, "session_id": sid, "tenant_id": tid, "error": errMsg})
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
			cli.StringFlag{Name: "manifest", Usage: "Path to workspace manifest YAML file"},
			cli.StringFlag{Name: "git-repo", Usage: "Git repository URL to clone (shortcut)"},
			cli.StringFlag{Name: "git-ref", Value: "main", Usage: "Git ref for --git-repo"},
			cli.StringFlag{Name: "git-path", Value: "repo", Usage: "Destination path for --git-repo"},
		},
		Action: func(ctx *cli.Context) error {
			client := newClientFromCtx(ctx)
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

			sid := resp.SessionId

			// materialize manifest if provided
			manifest, mErr := resolveManifest(ctx)
			if mErr != nil {
				client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
				printErrorExit("manifest: "+mErr.Error(), 1)
				return nil
			}
			if manifest != nil {
				if err := materializeManifest(client, sid, manifest); err != nil {
					client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
					printErrorExit("manifest materialization failed: "+err.Error(), 1)
					return nil
				}
			}

			// always write state file (unless explicit session-id)
			if ctx.String("session-id") == "" {
				_ = finalizeState(ctx.String("tenant"), resp.SessionId)
			}

			count := 0
			if manifest != nil {
				count = len(manifest.Entries)
			}
			printJSON(map[string]any{"session_id": resp.SessionId, "pod_ip": resp.PodIp, "entries_materialized": count})
			return nil
		},
	}
}

// resolveManifest builds a Manifest from --manifest, --git-repo flags, or returns nil.
func resolveManifest(ctx *cli.Context) (*Manifest, error) {
	if path := ctx.String("manifest"); path != "" {
		return parseManifest(path)
	}
	if repo := ctx.String("git-repo"); repo != "" {
		return &Manifest{Entries: []ManifestEntry{
			{GitRepo: &GitRepoEntry{
				Path: ctx.String("git-path"),
				Repo: repo,
				Ref:  ctx.String("git-ref"),
			}},
		}}, nil
	}
	return nil, nil
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
			client := newClientFromCtx(ctx)
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
				printErrorExit("usage: k8e-sandbox-cli write <session-id> <path>", 1)
				return nil
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				printErrorExit("read stdin: "+err.Error(), 1)
				return nil
			}

			client := newClientFromCtx(ctx)
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
				printErrorExit("usage: k8e-sandbox-cli read <session-id> <path>", 1)
				return nil
			}

			client := newClientFromCtx(ctx)
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

			client := newClientFromCtx(ctx)
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

			client := newClientFromCtx(ctx)
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
				printErrorExit("usage: k8e-sandbox-cli confirm <session-id> <action>", 1)
				return nil
			}

			client := newClientFromCtx(ctx)
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
			fmt.Fprintf(os.Stderr, "[k8e-sandbox]    To approve: k8e-sandbox-cli approve %s\n", resp.ApprovalId)
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

			client := newClientFromCtx(ctx)
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

// ── InstallSkillCommand ────────────────────────────────────────────────────

func InstallSkillCommand() cli.Command {
	return cli.Command{
		Name:      "install-skill",
		Usage:     "Install K8E sandbox skill into AI agent config",
		ArgsUsage: "[claude|codex|pi|all]",
		Action: func(ctx *cli.Context) error {
			target := ctx.Args().First()
			if target == "" {
				target = "all"
			}
			if err := InstallSkill(target); err != nil {
				printErrorExit(err.Error(), 1)
			}
			return nil
		},
	}
}
