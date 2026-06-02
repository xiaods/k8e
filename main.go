//go:generate go run pkg/codegen/cleanup/main.go
//go:generate /bin/rm -rf pkg/generated
//go:generate go run pkg/codegen/main.go
//go:generate go fmt pkg/deploy/zz_generated_bindata.go
//go:generate go fmt pkg/static/zz_generated_bindata.go

package main

import (
	"context"
	"errors"
	"os"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/commands"
	"github.com/xiaods/k8e/pkg/configfilearg"
	"github.com/xiaods/k8e/pkg/sandboxcli"
)

func main() {
	app := cmds.NewApp()
	app.Commands = cmds.NewAppCommands(commands.Funcs())

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil && !errors.Is(err, context.Canceled) {
		var exitErr *sandboxcli.ExitError
		if errors.As(err, &exitErr) {
			os.Exit(exitErr.ExitCode)
		}
		logrus.Fatal(err)
	}
}
