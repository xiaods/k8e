//go:generate go run pkg/codegen/cleanup/main.go
//go:generate /bin/rm -rf pkg/generated
//go:generate go run pkg/codegen/main.go
//go:generate go fmt pkg/deploy/zz_generated_bindata.go
//go:generate go fmt pkg/static/zz_generated_bindata.go

package main

import (
	"os"

	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/cli/agent"
	"github.com/xiaods/k8e/pkg/cli/cert"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/cli/crictl"
	"github.com/xiaods/k8e/pkg/cli/etcdsnapshot"
	"github.com/xiaods/k8e/pkg/cli/kubectl"
	"github.com/xiaods/k8e/pkg/cli/server"
	"github.com/xiaods/k8e/pkg/configfilearg"
)

func main() {
	app := cmds.NewApp()
	app.Commands = []cli.Command{
		cmds.NewServerCommand(server.Run),
		cmds.NewAgentCommand(agent.Run),
		cmds.NewKubectlCommand(kubectl.Run),
		cmds.NewCRICTL(crictl.Run),
		cmds.NewEtcdSnapshotCommand(etcdsnapshot.Run,
			cmds.NewEtcdSnapshotSubcommands(
				etcdsnapshot.Delete,
				etcdsnapshot.List,
				etcdsnapshot.Prune,
				etcdsnapshot.Run),
		),
		cmds.NewCertCommand(
			cmds.NewCertSubcommands(
				cert.Run),
		),
	}

	if err := app.Run(configfilearg.MustParse(os.Args)); err != nil {
		logrus.Fatal(err)
	}
}
