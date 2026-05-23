package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/reexec"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/commands"
	"github.com/xiaods/k8e/pkg/configfilearg"
	"github.com/xiaods/k8e/pkg/containerd"
	ctr2 "github.com/xiaods/k8e/pkg/ctr"
	kubectl2 "github.com/xiaods/k8e/pkg/kubectl"
	crictl2 "sigs.k8s.io/cri-tools/cmd/crictl"
)

func init() {
	reexec.Register("containerd", containerd.Main)
	reexec.Register("kubectl", kubectl2.Main)
	reexec.Register("crictl", crictl2.Main)
	reexec.Register("ctr", ctr2.Main)
}

func main() {
	cmd := os.Args[0]
	os.Args[0] = filepath.Base(os.Args[0])
	if reexec.Init() {
		return
	}
	os.Args[0] = cmd

	app := cmds.NewApp()
	app.Commands = cmds.NewAppCommands(commands.Funcs())

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil && !errors.Is(err, context.Canceled) {
		logrus.Fatal(err)
	}
}
