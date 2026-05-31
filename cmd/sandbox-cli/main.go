package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/sandboxcli"
	"github.com/xiaods/k8e/pkg/version"
)

func main() {
	app := cmds.NewApp()
	app.Name = version.Program
	app.Usage = "K8E sandbox CLI — connect to any K8E cluster"
	app.Version = version.Version
	app.HideVersion = false

	// Stage embedded skill files for install-skill command
	if err := sandboxcli.StageSkills(); err != nil {
		logrus.Warnf("skills: %v", err)
	}

	sandboxCmd := cmds.NewSandboxCommand()
	sandboxCmd.Subcommands = append(sandboxCmd.Subcommands, sandboxcli.InstallSkillCommand())

	app.Commands = []cli.Command{
		sandboxCmd,
	}
	app.Flags = nil // no data-dir flag needed for sandbox CLI

	if err := app.Run(os.Args); err != nil {
		logrus.Fatal(err)
	}
}
