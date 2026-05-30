package cmds

import (
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxcli"
)

func NewSandboxCommand() cli.Command {
	return cli.Command{
		Name:  "sandbox",
		Usage: "Manage K8E sandbox sessions — run code in isolated environments",
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
		},
	}
}
