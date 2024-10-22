package main

import (
	"context"
	"errors"
	"os"

	"github.com/xiaods/k8e/pkg/cli/cert"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/configfilearg"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
)

func main() {
	app := cmds.NewApp()
	app.Commands = []cli.Command{
		cmds.NewCertCommands(
			cert.Check,
			cert.Rotate,
			cert.RotateCA,
		),
	}

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil && !errors.Is(err, context.Canceled) {
		logrus.Fatal(err)
	}
}