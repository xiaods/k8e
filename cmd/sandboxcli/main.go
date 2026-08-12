package main

import (
	"errors"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/sandboxcli"
	"github.com/xiaods/k8e/pkg/version"
)

func main() {
	// Stage embedded skill files for connect (agent harness install).
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
		cli.StringFlag{Name: "profile", EnvVar: "K8E_SANDBOX_PROFILE", Usage: "Named profile from ~/.k8e/sandbox/profiles.yaml (KIP-17; not server /etc/k8e/config.yaml)"},
	}
	commands := []cli.Command{
		sandboxcli.ConnectCommand(),
		sandboxcli.LoginCommand(),
		sandboxcli.RunCommand(),
		sandboxcli.StatusCommand(),
		sandboxcli.CreateCommand(),
		sandboxcli.GetCommand(),
		sandboxcli.SessionsCommand(),
		sandboxcli.DestroyCommand(),
		sandboxcli.WriteCommand(),
		sandboxcli.ReadCommand(),
		sandboxcli.ListCommand(),
		sandboxcli.SubagentCommand(),
		sandboxcli.ConfirmCommand(),
		sandboxcli.ApproveCommand(),
		sandboxcli.SnapshotCommand(),
		sandboxcli.PollCommand(),
		sandboxcli.LogCommand(),
		sandboxcli.EventsCommand(),
		sandboxcli.PsCommand(),
		sandboxcli.BenchmarkCommand(),
	}
	// M9 catalog: emits the full command surface (single source for SDK stubs).
	commands = append(commands, sandboxcli.CatalogCommand(commands))
	app.Commands = commands

	if err := app.Run(os.Args); err != nil {
		var exitErr *sandboxcli.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode)
		}
		logrus.Fatal(err)
	}
}
