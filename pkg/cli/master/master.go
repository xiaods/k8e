package master

import (
	"context"
	"runtime"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons/master"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/signals"
)

//var ctx = context.Background()

//Run start master
func Run(cmd *cobra.Command, args []string) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	logrus.Info("start master")
	run(&cmds.Master)
}

func run(cfg *cmds.MasterConfig) {
	datadir, _ := datadir.LocalHome(cfg.DataDir, true)
	masterConfig := master.Config{}
	masterConfig.ControlConfig.DataDir = datadir
	masterConfig.ControlConfig.JoinURL = cfg.ServerURL
	ctx := signals.SetupSignalHandler(context.Background())
	//log.Println(cfg.HTTPSPort)
	master.StartMaster(ctx, &masterConfig.ControlConfig)
	<-ctx.Done()
}
