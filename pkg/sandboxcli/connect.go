package sandboxcli

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandbox/client"
	pb "github.com/xiaods/k8e/pkg/sandboxmatrix/grpc/pb/sandbox/v1"
)

// Connect CLI flag names (shared by flag definitions and Action lookups).
const (
	flagAgent      = "agent"
	flagSkipSkill  = "skip-skill"
	flagSkillOnly  = "skill-only"
	flagSkipVerify = "skip-verify"
	flagResetCerts = "reset-certs"
	flagSkipPath   = "skip-path"
)

// Connection flag names. They are registered BOTH on the root command
// (k8e-sandbox-cli --endpoint … connect) and on connect itself
// (k8e-sandbox-cli connect --endpoint …) — the latter is the natural
// spelling users reach for first.
const (
	flagEndpoint = "endpoint"
	flagApikey   = "apikey"
	flagProfile  = "profile"
)

// connFlag returns a connection flag from the subcommand-local flag set when
// explicitly provided, else falls back to the root-command global flag (and
// its env var binding). Needed because urfave/cli v1 keeps local and global
// flag sets separate: an unset local flag does not see the global value.
func connFlag(ctx *cli.Context, name string) string {
	if v := ctx.String(name); v != "" {
		return v
	}
	return ctx.GlobalString(name)
}

// ConnectCommand returns the "connect" subcommand: authenticate (local or remote),
// persist connection config, verify the gateway, ensure CLI is on PATH, and install
// agent skills so harnesses can invoke `/k8e-sandbox <goal>` (Claude),
// `$k8e-sandbox <goal>` (Codex), `/skill:k8e-sandbox <goal>` (Pi), or load the
// `k8e-sandbox` skill in dsh (DeepSeek Harness) via its `skill` tool.
func ConnectCommand() cli.Command {
	return cli.Command{
		Name:  "connect",
		Usage: "Connect to local or remote K8E sandbox and install /k8e-sandbox agent skills",
		Flags: []cli.Flag{
			cli.StringFlag{
				Name:   flagEndpoint,
				EnvVar: "K8E_SANDBOX_ENDPOINT",
				Usage:  "gRPC endpoint (default: 127.0.0.1:50051)",
			},
			cli.StringFlag{
				Name:   flagApikey,
				EnvVar: "K8E_SANDBOX_APIKEY",
				Usage:  "API key for remote cluster authentication",
			},
			cli.StringFlag{
				Name:   flagProfile,
				EnvVar: "K8E_SANDBOX_PROFILE",
				Usage:  "Named profile from ~/.k8e/sandbox/profiles.yaml (KIP-17)",
			},
			cli.StringFlag{
				Name:  flagAgent,
				Value: "auto",
				Usage: "Skill install target: auto|claude|codex|pi|dsh|all (default: auto-detect)",
			},
			cli.BoolFlag{
				Name:  flagSkipSkill,
				Usage: "Authenticate only; do not install agent skills",
			},
			cli.BoolFlag{
				Name:  flagSkillOnly,
				Usage: "Only (re)install agent skills; skip gateway dial/auth",
			},
			cli.BoolFlag{
				Name:  flagSkipVerify,
				Usage: "Skip gateway connectivity verification",
			},
			cli.BoolFlag{
				Name:  flagSkipPath,
				Usage: "Skip ensuring k8e-sandbox-cli is on PATH",
			},
			cli.BoolFlag{
				Name:  flagResetCerts,
				Usage: "Delete cached CA/client certs for this connection first — use after a server reinstall or CA rotation (trust is re-established from the --apikey bootstrap)",
			},
		},
		Action: connectAction,
	}
}

func connectAction(ctx *cli.Context) error {
	if ctx.Bool(flagSkillOnly) && ctx.Bool(flagSkipSkill) {
		return printErrorExit("conflicting flags: --"+flagSkillOnly+" and --"+flagSkipSkill, 1)
	}

	// Skill-only: refresh harness install without requiring a live gateway.
	if ctx.Bool(flagSkillOnly) {
		return connectSkillOnly(ctx)
	}

	resolved, err := ResolveConn(connFlag(ctx, flagEndpoint), connFlag(ctx, flagApikey), connFlag(ctx, flagProfile), "")
	if err != nil {
		return printErrorExit(err.Error(), 1)
	}
	ApplyResolvedConn(resolved)
	endpoint := resolved.Endpoint
	apikey := resolved.APIKey

	if ctx.Bool(flagResetCerts) {
		resetCerts(resolved)
	}

	mode, cfgEndpoint, err := resolveConnectMode(endpoint, apikey)
	if err != nil {
		return printErrorExit(err.Error(), 1)
	}

	c, dialErr := dialForConnect(mode, cfgEndpoint, apikey)
	if dialErr != nil {
		return printErrorExit("connect failed: "+dialErr.Error(), 2)
	}
	defer c.Close()

	if !ctx.Bool(flagSkipVerify) {
		if err := verifyGateway(c); err != nil {
			msg := "gateway verify failed: " + err.Error()
			if hint := client.ConnErrorHint(err); hint != "" {
				msg += "\n  hint: " + hint + "\n  (or re-run connect with --reset-certs)"
			}
			return printErrorExit(msg, 2)
		}
	}

	cliPath := ensurePathUnlessSkipped(ctx)
	installedAgents, installResults, skillErr := installSkillsUnlessSkipped(ctx)
	if skillErr != nil {
		return printErrorExit(skillErr.Error(), 1)
	}

	cfg := &ConnectionConfig{
		Mode:     mode,
		Endpoint: cfgEndpoint,
		Agents:   installedAgents,
	}
	if err := SaveConnectionConfig(cfg); err != nil {
		return printErrorExit("save connection config: "+err.Error(), 1)
	}
	// Persist the gateway as the active profile (KIP-17) so later CLI
	// invocations dial it without repeating --endpoint. Non-fatal: a
	// profiles.yaml write failure degrades to the config.json fallback.
	if err := SaveConnectProfile(cfgEndpoint); err != nil {
		fmt.Fprintf(os.Stderr, "⚠ save default profile: %v\n", err)
	}

	printConnectSuccess(mode, cfgEndpoint, cliPath, installedAgents, installResults)
	return nil
}

// resetCerts deletes cached trust material (CA, client cert/key, endpoint
// stamp) for this connection so the next dial re-bootstraps from the API key.
// When no explicit cert_dir was configured it falls back to the default cache
// dir (~/.k8e/sandbox) — otherwise --reset-certs would silently no-op on
// default installs while the dial keeps trusting the stale CA there.
func resetCerts(resolved *ResolvedConn) {
	dir := resolved.CertDir
	if dir == "" {
		dir, _ = dataDir()
	}
	if dir == "" {
		return
	}
	for _, f := range []string{"ca.crt", "client.crt", "client.key", "endpoint"} {
		_ = os.Remove(filepath.Join(dir, f))
	}
}

func connectSkillOnly(ctx *cli.Context) error {
	cliPath := ensurePathUnlessSkipped(ctx)
	installedAgents, installResults, skillErr := installSkillsUnlessSkipped(ctx)
	if skillErr != nil {
		return printErrorExit(skillErr.Error(), 1)
	}
	if len(installResults) == 0 {
		return printErrorExit("no skills installed (check --agent target)", 1)
	}

	// Preserve existing endpoint/mode when only refreshing skills.
	cfg, _ := LoadConnectionConfig()
	if cfg == nil {
		cfg = &ConnectionConfig{Mode: "local"}
	}
	cfg.Agents = installedAgents
	if err := SaveConnectionConfig(cfg); err != nil {
		return printErrorExit("save connection config: "+err.Error(), 1)
	}

	printConnectSuccess(cfg.Mode+" ("+flagSkillOnly+")", cfg.Endpoint, cliPath, installedAgents, installResults)
	return nil
}

func ensurePathUnlessSkipped(ctx *cli.Context) string {
	if ctx.Bool(flagSkipPath) {
		return ""
	}
	p, pathErr := EnsureCLIOnPATH()
	if pathErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ k8e-sandbox-cli not on PATH: %v\n", pathErr)
		fmt.Fprintf(os.Stderr, "  Agents need `k8e-sandbox-cli` in PATH to run sandbox commands.\n")
		return ""
	}
	return p
}

func installSkillsUnlessSkipped(ctx *cli.Context) (agents []string, results []InstallResult, err error) {
	if ctx.Bool(flagSkipSkill) {
		return nil, nil, nil
	}
	agentFlag := ctx.String(flagAgent)
	// Validate early so bogus --agent fails before any writes.
	if _, resolveErr := resolveSkillTargets(agentFlag); resolveErr != nil {
		return nil, nil, resolveErr
	}
	results, instErr := InstallSkillMulti(agentFlag, true)
	if instErr != nil {
		fmt.Fprintf(os.Stderr, "⚠ skill install: %v\n", instErr)
	}
	seen := map[string]bool{}
	for _, r := range results {
		if seen[r.Agent] {
			continue
		}
		seen[r.Agent] = true
		agents = append(agents, r.Agent)
	}
	return agents, results, nil
}

// resolveConnectMode decides local vs remote from endpoint/apikey.
func resolveConnectMode(endpoint, apikey string) (mode, cfgEndpoint string, err error) {
	endpoint = strings.TrimSpace(endpoint)
	apikey = strings.TrimSpace(apikey)

	if endpoint == "" && apikey == "" {
		return "local", "", nil
	}
	if endpoint == "" && apikey != "" {
		return "", "", fmt.Errorf("remote connect requires --endpoint (or K8E_SANDBOX_ENDPOINT) with --apikey")
	}
	if isLocalEndpoint(endpoint) && apikey == "" {
		return "local", endpoint, nil
	}
	// Remote: with apikey (fresh auth) or without (reuse cached mTLS certs).
	return "remote", endpoint, nil
}

func isLocalEndpoint(endpoint string) bool {
	switch strings.ToLower(endpointHost(endpoint)) {
	case "", "127.0.0.1", "localhost", "::1", "0.0.0.0":
		return true
	default:
		return false
	}
}

// endpointHost extracts the host from host:port, [ipv6]:port, or bare address.
func endpointHost(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	// Bracketed IPv6: [addr]:port or [addr]
	if strings.HasPrefix(endpoint, "[") {
		if end := strings.Index(endpoint, "]"); end > 0 {
			return endpoint[1:end]
		}
	}
	// host:port — only when the host part has no colons (IPv4 / hostname).
	// Bare IPv6 (e.g. ::1) keeps colons in the "host" slice and must stay whole.
	if i := strings.LastIndex(endpoint, ":"); i >= 0 {
		host := endpoint[:i]
		if !strings.Contains(host, ":") {
			return host
		}
	}
	return endpoint
}

func dialForConnect(mode, endpoint, apikey string) (*client.Client, error) {
	if mode == "remote" {
		return client.NewClientWithEndpoint(endpoint, apikey)
	}
	if endpoint != "" {
		_ = os.Setenv("K8E_SANDBOX_ENDPOINT", endpoint)
	}
	return client.NewClient()
}

func verifyGateway(c *client.Client) error {
	_, err := c.SandboxServiceClient.DestroySession(context.Background(),
		&pb.DestroySessionRequest{SessionId: "healthcheck-probe-noop"})
	if err == nil {
		return nil
	}
	msg := err.Error()
	if strings.Contains(msg, errSessionNotFound) {
		return nil
	}
	if strings.Contains(strings.ToLower(msg), "not found") {
		return nil
	}
	return err
}

func printConnectSuccess(mode, endpoint, cliPath string, agents []string, results []InstallResult) {
	ep := endpoint
	if ep == "" {
		ep = "127.0.0.1:50051 (default)"
	}
	fmt.Fprintf(os.Stderr, "✓ Connected to K8E sandbox (%s)\n", mode)
	fmt.Fprintf(os.Stderr, "  Endpoint: %s\n", ep)
	fmt.Fprintf(os.Stderr, "  Config:   ~/.k8e/sandbox/config.json (last connect)\n")
	fmt.Fprintf(os.Stderr, "  Profiles: ~/.k8e/sandbox/profiles.yaml — default profile set (later CLI calls need no --endpoint)\n")
	if mode == "remote" {
		fmt.Fprintf(os.Stderr, "  Creds:    ~/.k8e/sandbox/{ca.crt,client.crt,client.key} (or profile cert_dir)\n")
	}
	if cliPath != "" {
		fmt.Fprintf(os.Stderr, "  CLI:      %s\n", cliPath)
	}

	if len(results) > 0 {
		fmt.Fprintf(os.Stderr, "  Skills:   installed (%d location(s))\n", len(results))
		for _, r := range results {
			fmt.Fprintf(os.Stderr, "            - %s → %s\n", r.Label, r.Path)
		}
		fmt.Fprintf(os.Stderr, "\nInvoke in your agent harness:\n")
		for _, line := range InvocationHints(agents) {
			fmt.Fprintf(os.Stderr, "  %s\n", line)
		}
		fmt.Fprintf(os.Stderr, "\nExample goal:\n")
		fmt.Fprintf(os.Stderr, "  /k8e-sandbox run unit tests and fix failures\n")
		fmt.Fprintf(os.Stderr, "\nRestart or open a new agent session if the skill does not appear yet.\n")
		return
	}
	if len(agents) == 0 {
		fmt.Fprintf(os.Stderr, "  Skills:   skipped\n")
		fmt.Fprintf(os.Stderr, "  Install later: k8e-sandbox-cli connect --%s\n", flagSkillOnly)
	}
}
