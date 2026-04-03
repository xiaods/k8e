package cmds

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxmcp"
)

func NewSandboxMCPCommand(action func(*cli.Context) error) cli.Command {
	return cli.Command{
		Name:   "sandbox-mcp",
		Usage:  "Run the sandbox MCP skill server (stdio, JSON-RPC 2.0)",
		Action: action,
		Flags: []cli.Flag{
			cli.StringFlag{Name: "endpoint", Value: "", Usage: "gRPC endpoint override", EnvVar: "K8E_SANDBOX_ENDPOINT"},
			cli.StringFlag{Name: "tls-cert", Value: "", Usage: "TLS CA cert file", EnvVar: "K8E_SANDBOX_CERT"},
			cli.StringFlag{Name: "tls-key", Value: "", Usage: "TLS client key file (for mTLS)", EnvVar: "K8E_SANDBOX_KEY"},
			cli.StringFlag{Name: "tenant-id", Value: "", Usage: "Tenant ID for cross-process session reuse", EnvVar: "K8E_SANDBOX_TENANT"},
		},
	}
}

func NewSandboxInstallSkillCommand() cli.Command {
	return cli.Command{
		Name:      "sandbox-install-skill",
		Usage:     "Install the k8e-sandbox MCP skill into an AI agent config",
		ArgsUsage: "[claude|kiro|gemini|all]",
		Action: func(ctx *cli.Context) error {
			target := ctx.Args().First()
			if target == "" {
				target = "all"
			}
			return sandboxmcp.InstallSkill(target)
		},
	}
}

func SandboxMCP(ctx *cli.Context) error {
	if ep := ctx.String("endpoint"); ep != "" {
		os.Setenv("K8E_SANDBOX_ENDPOINT", ep)
	}
	if cert := ctx.String("tls-cert"); cert != "" {
		os.Setenv("K8E_SANDBOX_CERT", cert)
	}
	if key := ctx.String("tls-key"); key != "" {
		os.Setenv("K8E_SANDBOX_KEY", key)
	}

	client, err := sandboxmcp.NewClient()
	if err != nil {
		return err
	}
	defer client.Close()

	c, cancel := context.WithCancel(context.Background())
	defer cancel()

	sig := make(chan os.Signal, 1)
	signal.Notify(sig, syscall.SIGTERM, syscall.SIGINT)
	go func() { <-sig; cancel() }()

	return sandboxmcp.NewServerWithTenant(client, ctx.String("tenant-id")).Run(c)
}
