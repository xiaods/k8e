package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/configfilearg"
)

func main() {
	app := cmds.NewApp()

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil {
		logrus.Fatal(err)
	}
}
