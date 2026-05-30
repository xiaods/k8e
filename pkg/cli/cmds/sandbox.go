package cmds

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxcli"
)

func NewSandboxCommand() cli.Command {
	return cli.Command{
		Name:  "sandbox",
		Usage: "Manage K8E sandbox sessions — run code in isolated environments",
		Flags: []cli.Flag{
			cli.StringFlag{Name: "endpoint", EnvVar: "K8E_SANDBOX_ENDPOINT", Usage: "gRPC endpoint (default: 127.0.0.1:50051)"},
			cli.StringFlag{Name: "apikey", EnvVar: "K8E_SANDBOX_APIKEY", Usage: "API key for remote cluster authentication"},
			cli.BoolFlag{Name: "debug", EnvVar: "K8E_SANDBOX_DEBUG", Usage: "Enable debug logging"},
		},
		Subcommands: []cli.Command{
			sandboxcli.RunCommand(),
			sandboxcli.StatusCommand(),
			sandboxcli.CreateCommand(),
			sandboxcli.DestroyCommand(),
			sandboxcli.WriteCommand(),
			sandboxcli.ReadCommand(),
			sandboxcli.ListCommand(),
			sandboxcli.SubagentCommand(),
			sandboxcli.ConfirmCommand(),
			sandboxcli.ApproveCommand(),
			sandboxcli.SnapshotCommand(),
			sandboxcli.ApiKeyCommand(),
			sandboxcli.InstallSkillCommand(),
		},
	}
}
