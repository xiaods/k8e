package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/reexec"
	crictl2 "github.com/kubernetes-sigs/cri-tools/cmd/crictl"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/agent"
	"github.com/xiaods/k8e/pkg/cli/cert"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/completion"
	"github.com/xiaods/k8e/pkg/cli/crictl"
	"github.com/xiaods/k8e/pkg/cli/ctr"
	"github.com/xiaods/k8e/pkg/cli/etcdsnapshot"
	"github.com/xiaods/k8e/pkg/cli/kubectl"
	"github.com/xiaods/k8e/pkg/cli/secretsencrypt"
	"github.com/xiaods/k8e/pkg/cli/server"
	"github.com/xiaods/k8e/pkg/configfilearg"
	"github.com/xiaods/k8e/pkg/containerd"
	ctr2 "github.com/xiaods/k8e/pkg/ctr"
	kubectl2 "github.com/xiaods/k8e/pkg/kubectl"
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
	app.Commands = []cli.Command{
		cmds.NewServerCommand(server.Run),
		cmds.NewAgentCommand(agent.Run),
		cmds.NewKubectlCommand(kubectl.Run),
		cmds.NewCRICTL(crictl.Run),
		cmds.NewCtrCommand(ctr.Run),
		cmds.NewEtcdSnapshotCommand(etcdsnapshot.Save,
			cmds.NewEtcdSnapshotSubcommands(
				etcdsnapshot.Delete,
				etcdsnapshot.List,
				etcdsnapshot.Prune,
				etcdsnapshot.Save),
		),
		cmds.NewSecretsEncryptCommand(cli.ShowAppHelp,
			cmds.NewSecretsEncryptSubcommands(
				secretsencrypt.Status,
				secretsencrypt.Enable,
				secretsencrypt.Disable,
				secretsencrypt.Prepare,
				secretsencrypt.Rotate,
				secretsencrypt.Reencrypt),
		),
		cmds.NewCertCommand(
			cmds.NewCertSubcommands(
				cert.Run),
		),
		cmds.NewCompletionCommand(completion.Run),
	}

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil && !errors.Is(err, context.Canceled) {
		logrus.Fatal(err)
	}
}
