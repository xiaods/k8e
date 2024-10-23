package main

import (
	"context"
	"errors"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/secretsencrypt"
	"github.com/xiaods/k8e/pkg/configfilearg"
)

func main() {
	app := cmds.NewApp()
	app.Commands = []cli.Command{
		cmds.NewSecretsEncryptCommands(
			secretsencrypt.Status,
			secretsencrypt.Enable,
			secretsencrypt.Disable,
			secretsencrypt.Prepare,
			secretsencrypt.Rotate,
			secretsencrypt.Reencrypt,
			secretsencrypt.RotateKeys,
		),
	}

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil && !errors.Is(err, context.Canceled) {
		logrus.Fatal(err)
	}
}
