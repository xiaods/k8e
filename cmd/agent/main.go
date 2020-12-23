package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/agent"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/configfilearg"
)

func main() {
	app := cmds.NewApp()
	app.Commands = []cli.Command{
		cmds.NewAgentCommand(agent.Run),
	}

	err := app.Run(configfilearg.MustParse(os.Args))
	if err != nil {
		logrus.Fatal(err)
	}
}
