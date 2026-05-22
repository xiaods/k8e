package main

import (
	"context"
	"errors"
	"os"
	"path/filepath"

	"github.com/docker/docker/pkg/reexec"
	"github.com/sirupsen/logrus"
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
	"github.com/xiaods/k8e/pkg/cli/token"
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
	app.Commands = cmds.NewAppCommands(cmds.AppCommandFuncs{
		Server:  server.Run,
		Agent:   agent.Run,
		Kubectl: kubectl.Run,
		CRICTL:  crictl.Run,
		Ctr:     ctr.Run,
		Token: cmds.TokenCommandFuncs{
			Create: token.Create, Delete: token.Delete,
			Generate: token.Generate, List: token.List, Rotate: token.Rotate,
		},
		EtcdSnapshot: cmds.EtcdSnapshotCommandFuncs{
			Delete: etcdsnapshot.Delete, List: etcdsnapshot.List,
			Prune: etcdsnapshot.Prune, Save: etcdsnapshot.Save,
		},
		SecretsEncrypt: cmds.SecretsEncryptCommandFuncs{
			Status: secretsencrypt.Status, Enable: secretsencrypt.Enable,
			Disable: secretsencrypt.Disable, Prepare: secretsencrypt.Prepare,
			Rotate: secretsencrypt.Rotate, Reencrypt: secretsencrypt.Reencrypt,
			RotateKeys: secretsencrypt.RotateKeys,
		},
		Cert: cmds.CertCommandFuncs{
			Check: cert.Check, Rotate: cert.Rotate, RotateCA: cert.RotateCA,
		},
		Completion: completion.Run,
	})

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil && !errors.Is(err, context.Canceled) {
		logrus.Fatal(err)
	}
}
