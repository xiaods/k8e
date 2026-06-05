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
func newClientFromCtx(ctx *cli.Context) (*client.Client, *ExitError) {
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
		return nil, printErrorExit("sandbox not reachable: "+err.Error(), 2)
	}
	return c, nil
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
			cli.BoolFlag{Name: "background", Usage: "Submit asynchronously, return run_id immediately"},
		cli.BoolFlag{Name: "raw", Usage: "Stream raw output (no JSON wrapper)"},
			cli.StringFlag{Name: "manifest", Usage: "Path to workspace manifest (only when auto-creating session)"},
			cli.StringFlag{Name: "git-repo", Usage: "Git repo to clone (only when auto-creating session)"},
			cli.StringFlag{Name: "git-ref", Value: "main", Usage: "Git ref for --git-repo"},
			cli.StringFlag{Name: "git-path", Value: "repo", Usage: "Destination path for --git-repo"},
		},
		Action: runAction,
	}
}

func runBackground(cli *client.Client, ctx *cli.Context, code, lang string) error {
	sid, _, err := ensureSession(cli, ctx)
	if err != nil {
		return printErrorExit(err.Error(), 2)
	}
	cmd := buildCommand(lang, code)
	resp, err := cli.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
		SessionId: sid, Command: cmd, Timeout: int32(ctx.Int("timeout")), Workdir: "/workspace", Background: true,
	})
	if err != nil {
		return printErrorExit("background submit: "+err.Error(), 2)
	}
	printJSON(map[string]any{"run_id": resp.RunId, "status": resp.Status, "session_id": sid})
	return nil
}

func runExec(cli *client.Client, ctx *cli.Context, req *pb.ExecRequest, sid string, needsFinalize, raw bool) error {
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
		return &ExitError{ExitCode: exitCode}
	}
	if err := runJSON(cli, req, sid, needsFinalize, ctx.String("tenant")); err != nil {
		if isSessionExpired(err) {
			_, retryErr := retry()
			return retryErr
		}
		return err
	}
	return nil
}

func runAction(ctx *cli.Context) error {
	code, err := readCode(ctx)
	if err != nil {
		return printErrorExit(err.Error(), 1)
	}
	lang := ctx.String("lang")
	raw := ctx.Bool("raw")

	cli, exitErr := newClientFromCtx(ctx)
	if exitErr != nil {
		return exitErr
	}
	defer cli.Close()

	// Background mode: submit async, return run_id immediately
	if ctx.Bool("background") {
		return runBackground(cli, ctx, code, lang)
	}

	sid, needsFinalize, err := ensureSession(cli, ctx)
	if err != nil {
		return printErrorExit(err.Error(), 2)
	}

	if isInterpretedLang(lang) && (isMultiLine(code) || strings.HasPrefix(lang, "ts")) {
		if err := writeCodeFile(cli, sid, lang, code); err != nil {
			return printErrorExit("write code: "+err.Error(), 1)
		}
	}

	cmd := buildCommand(lang, code)
	req := &pb.ExecRequest{SessionId: sid, Command: cmd, Timeout: int32(ctx.Int("timeout")), Workdir: "/workspace"}
	return runExec(cli, ctx, req, sid, needsFinalize, raw)
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
		return printErrorExit("exec: "+err.Error(), 2)
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
			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
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
			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
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
				return printErrorExit("create session: "+err.Error(), 2)
			}

			sid := resp.SessionId

			// materialize manifest if provided
			manifest, mErr := resolveManifest(ctx)
			if mErr != nil {
				client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
				return printErrorExit("manifest: "+mErr.Error(), 1)
			}
			if manifest != nil {
				if err := materializeManifest(client, sid, manifest); err != nil {
					client.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
					return printErrorExit("manifest materialization failed: "+err.Error(), 1)
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
				return printErrorExit("session-id required", 1)
			}
			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.DestroySession(context.Background(),
				&pb.DestroySessionRequest{SessionId: sid})
			if err != nil {
				return printErrorExit("destroy: "+err.Error(), 1)
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
				return printErrorExit("usage: k8e-sandbox-cli write <session-id> <path>", 1)
			}
			data, err := io.ReadAll(os.Stdin)
			if err != nil {
				return printErrorExit("read stdin: "+err.Error(), 1)
			}

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.WriteFile(context.Background(), &pb.WriteFileRequest{
				SessionId: sid, Path: path, Content: string(data), Mode: ctx.String("mode"),
			})
			if err != nil {
				return printErrorExit("write: "+err.Error(), 1)
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
				return printErrorExit("usage: k8e-sandbox-cli read <session-id> <path>", 1)
			}

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.ReadFile(context.Background(), &pb.ReadFileRequest{
				SessionId: sid, Path: path,
			})
			if err != nil {
				return printErrorExit("read: "+err.Error(), 1)
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
		Action: func(ctx *cli.Context) error {
			sid := ctx.Args().First()
			if sid == "" {
				return printErrorExit("session-id required", 1)
			}

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.ListFiles(context.Background(), &pb.ListFilesRequest{
				SessionId: sid,
			})
			if err != nil {
				return printErrorExit("list: "+err.Error(), 1)
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
				return printErrorExit("parent-session-id required", 1)
			}

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.RunSubAgent(context.Background(), &pb.RunSubAgentRequest{
				ParentSessionId: parentSid,
			})
			if err != nil {
				return printErrorExit("subagent: "+err.Error(), 1)
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
				return printErrorExit("usage: k8e-sandbox-cli confirm <session-id> <action>", 1)
			}

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			// Phase 1: register
			resp, err := client.SandboxServiceClient.ConfirmAction(context.Background(), &pb.ConfirmActionRequest{
				SessionId: sid, Action: action,
			})
			if err != nil {
				return printErrorExit("confirm register: "+err.Error(), 2)
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
				return printErrorExit("confirm timeout: "+err.Error(), 1)
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
				return printErrorExit("approval-id required", 1)
			}
			approved := !ctx.Bool("reject")

			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.ApproveAction(context.Background(), &pb.ApproveActionRequest{
				ApprovalId: aid, Approved: approved, Reason: ctx.String("reason"),
			})
			if err != nil {
				return printErrorExit("approve: "+err.Error(), 1)
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

// ── BenchmarkCommand ──────────────────────────────────────────────────────

func BenchmarkCommand() cli.Command {
	return cli.Command{
		Name:  "benchmark",
		Usage: "Measure warm pool performance: cold start, warm claim, reset latency",
		Flags: []cli.Flag{
			cli.IntFlag{Name: "pool-size", Value: 3, Usage: "Number of warm pods to pre-create"},
			cli.IntFlag{Name: "iterations", Value: 3, Usage: "Number of iterations per metric"},
		},
		Action: runBenchmark,
	}
}

func runBenchmark(ctx *cli.Context) error {
	poolSize := ctx.Int("pool-size")
	iters := ctx.Int("iterations")

	cli, exitErr := newClientFromCtx(ctx)
	if exitErr != nil {
		return exitErr
	}
	defer cli.Close()

	tenant := ctx.String("tenant")
	if tenant == "" {
		tenant = "bench-" + fmt.Sprint(time.Now().Unix())
	}

	// Phase 1: Cold start
	fmt.Fprintf(os.Stderr, "\n[bench] Phase 1/3: Cold start (%d iterations)\n", iters)
	results := benchColdStart(cli, tenant, iters)

	// Phase 2: Pre-create warm pool and run warm claim benchmarks
	cleanup, err := benchPreCreateWarmPool(cli, tenant, poolSize)
	if err != nil {
		return err
	}
	defer cleanup()

	fmt.Fprintf(os.Stderr, "[bench] Phase 2/3: Warm claim (%d iterations)\n", iters)
	mergeResults(results, benchWarmClaim(cli, tenant, iters))

	// Phase 3: Full lifecycle
	fmt.Fprintf(os.Stderr, "[bench] Phase 3/3: Full lifecycle (%d iterations)\n", iters)
	mergeResults(results, benchLifecycle(cli, tenant, iters))

	printBenchmarkResults(results)
	return nil
}

// benchPreCreateWarmPool creates poolSize sessions and returns a cleanup function.
func benchPreCreateWarmPool(c *client.Client, tenant string, poolSize int) (func(), error) {
	sids := make([]string, 0, poolSize)
	for i := 0; i < poolSize; i++ {
		resp, err := c.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
			TenantId: tenant, RuntimeClass: "gvisor",
		})
		if err != nil {
			for _, sid := range sids {
				c.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
			}
			return nil, printErrorExit("warm pool create: "+err.Error(), 2)
		}
		sids = append(sids, resp.SessionId)
	}
	return func() {
		for _, sid := range sids {
			c.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: sid})
		}
	}, nil
}

// mergeResults copies all entries from src into dst.
func mergeResults(dst, src map[string][]time.Duration) {
	for k, v := range src {
		dst[k] = v
	}
}

func benchColdStart(c *client.Client, tenant string, n int) map[string][]time.Duration {
	result := map[string][]time.Duration{"cold_total": {}, "cold_create": {}, "cold_exec": {}}
	for i := 0; i < n; i++ {
		t0 := time.Now()
		resp, err := c.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
			TenantId: tenant, RuntimeClass: "gvisor",
		})
		tCreate := time.Since(t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  cold start %d: create failed: %v\n", i+1, err)
			continue
		}
		result["cold_create"] = append(result["cold_create"], tCreate)

		_, execErr := c.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
			SessionId: resp.SessionId, Command: "true", Timeout: 30,
		})
		tExec := time.Since(t0)
		result["cold_exec"] = append(result["cold_exec"], tExec)
		result["cold_total"] = append(result["cold_total"], tExec)
		fmt.Fprintf(os.Stderr, "  cold %d/%d: create=%v total=%v\n", i+1, n, tCreate.Round(time.Millisecond), tExec.Round(time.Millisecond))

		c.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: resp.SessionId})
		if execErr != nil {
			fmt.Fprintf(os.Stderr, "  cold start %d: exec failed: %v\n", i+1, execErr)
		}
		time.Sleep(200 * time.Millisecond) // let PVC cleanup
	}
	return result
}

func benchWarmClaim(c *client.Client, tenant string, n int) map[string][]time.Duration {
	result := map[string][]time.Duration{"warm_total": {}, "warm_create": {}, "warm_exec": {}}
	for i := 0; i < n; i++ {
		t0 := time.Now()
		resp, err := c.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
			TenantId: tenant, RuntimeClass: "gvisor",
		})
		tCreate := time.Since(t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  warm claim %d: create failed: %v\n", i+1, err)
			continue
		}
		result["warm_create"] = append(result["warm_create"], tCreate)

		_, execErr := c.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
			SessionId: resp.SessionId, Command: "true", Timeout: 30,
		})
		tExec := time.Since(t0)
		result["warm_exec"] = append(result["warm_exec"], tExec)
		result["warm_total"] = append(result["warm_total"], tExec)
		fmt.Fprintf(os.Stderr, "  warm %d/%d: claim=%v total=%v\n", i+1, n, tCreate.Round(time.Millisecond), tExec.Round(time.Millisecond))

		c.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: resp.SessionId})
		if execErr != nil {
			fmt.Fprintf(os.Stderr, "  warm claim %d: exec failed: %v\n", i+1, execErr)
		}
		// after destroy, reconciler will create a new warm pod — give it time
		time.Sleep(5 * time.Second)
	}
	return result
}

func benchLifecycle(c *client.Client, tenant string, n int) map[string][]time.Duration {
	result := map[string][]time.Duration{"lifecycle_create": {}, "lifecycle_exec": {}, "lifecycle_destroy": {}}
	for i := 0; i < n; i++ {
		t0 := time.Now()
		resp, err := c.SandboxServiceClient.CreateSession(context.Background(), &pb.CreateSessionRequest{
			TenantId: tenant, RuntimeClass: "gvisor",
		})
		tCreate := time.Since(t0)
		if err != nil {
			fmt.Fprintf(os.Stderr, "  lifecycle %d: create failed: %v\n", i+1, err)
			continue
		}
		result["lifecycle_create"] = append(result["lifecycle_create"], tCreate)

		_, execErr := c.SandboxServiceClient.Exec(context.Background(), &pb.ExecRequest{
			SessionId: resp.SessionId, Command: "sleep 0.5", Timeout: 30,
		})
		tExec := time.Since(t0)
		result["lifecycle_exec"] = append(result["lifecycle_exec"], tExec)

		_, destroyErr := c.SandboxServiceClient.DestroySession(context.Background(), &pb.DestroySessionRequest{SessionId: resp.SessionId})
		tDestroy := time.Since(t0)
		result["lifecycle_destroy"] = append(result["lifecycle_destroy"], tDestroy)
		fmt.Fprintf(os.Stderr, "  lifecycle %d/%d: create=%v exec=%v destroy=%v\n", i+1, n,
			tCreate.Round(time.Millisecond), tExec.Round(time.Millisecond), tDestroy.Round(time.Millisecond))

		if execErr != nil {
			fmt.Fprintf(os.Stderr, "  lifecycle %d: exec failed: %v\n", i+1, execErr)
		}
		if destroyErr != nil {
			fmt.Fprintf(os.Stderr, "  lifecycle %d: destroy failed: %v\n", i+1, destroyErr)
		}
	}
	return result
}

func avg(ds []time.Duration) time.Duration {
	if len(ds) == 0 {
		return 0
	}
	var sum time.Duration
	for _, d := range ds {
		sum += d
	}
	return sum / time.Duration(len(ds))
}

func printBenchmarkResults(results map[string][]time.Duration) {
	fmt.Fprintln(os.Stderr, "")
	fmt.Fprintln(os.Stderr, "═══ Benchmark Results ═══")

	sections := []struct {
		title  string
		keys   []string
	}{
		{"Cold Start (no warm pool)", []string{"cold_create", "cold_exec", "cold_total"}},
		{"Warm Claim (from pool)", []string{"warm_create", "warm_exec", "warm_total"}},
		{"Full Lifecycle", []string{"lifecycle_create", "lifecycle_exec", "lifecycle_destroy"}},
	}

	for _, sec := range sections {
		fmt.Fprintf(os.Stderr, "\n  %s\n", sec.title)
		fmt.Fprintln(os.Stderr, "  ─────────────────────────────────────────────")
		for _, key := range sec.keys {
			ds := results[key]
			if len(ds) == 0 {
				fmt.Fprintf(os.Stderr, "  %-25s  (no data)\n", key)
				continue
			}
			fmt.Fprintf(os.Stderr, "  %-25s  avg=%8v  samples=%d\n", key, avg(ds).Round(time.Microsecond), len(ds))
		}
	}
	fmt.Fprintln(os.Stderr, "")
}

// ── PollCommand ─────────────────────────────────────────────────────────────

func PollCommand() cli.Command {
	return cli.Command{
		Name:      "poll",
		Usage:     "Check status of a background sandbox execution",
		ArgsUsage: "<run-id>",
		Action: func(ctx *cli.Context) error {
			runID := ctx.Args().First()
			if runID == "" {
				return printErrorExit("usage: k8e-sandbox-cli poll <run-id>", 1)
			}
			client, exitErr := newClientFromCtx(ctx)
			if exitErr != nil {
				return exitErr
			}
			defer client.Close()

			resp, err := client.SandboxServiceClient.PollRun(context.Background(), &pb.PollRunRequest{RunId: runID})
			if err != nil {
				return printErrorExit("poll: "+err.Error(), 1)
			}
			printJSON(map[string]any{
				"run_id":    resp.RunId,
				"status":    resp.Status,
				"stdout":    resp.Stdout,
				"stderr":    resp.Stderr,
				"exit_code": resp.ExitCode,
			})
			return nil
		},
	}
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
				return printErrorExit(err.Error(), 1)
			}
			return nil
		},
	}
}
