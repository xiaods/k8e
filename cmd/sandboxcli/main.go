package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxcli"
	"github.com/xiaods/k8e/pkg/version"
)

func main() {
	// Stage embedded skill files for install-skill command
	if err := sandboxcli.StageSkills(); err != nil {
		logrus.Fatalf("Failed to stage skill files: %v", err)
	}

	app := cli.NewApp()
	app.Name = version.Program
	app.Usage = "K8E sandbox CLI — connect to any K8E cluster"
	app.Version = version.Version
	app.HideVersion = false
	app.Flags = []cli.Flag{
		cli.StringFlag{Name: "endpoint", EnvVar: "K8E_SANDBOX_ENDPOINT", Usage: "gRPC endpoint (default: 127.0.0.1:50051)"},
		cli.StringFlag{Name: "apikey", EnvVar: "K8E_SANDBOX_APIKEY", Usage: "API key for remote cluster authentication"},
		cli.BoolFlag{Name: "debug", EnvVar: "K8E_SANDBOX_DEBUG", Usage: "Enable debug logging"},
	}
	app.Commands = []cli.Command{
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
		sandboxcli.InstallSkillCommand(),
	}

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
