package master

import (
	"context"
	"log"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons/master"
	"github.com/xiaods/k8e/pkg/signals"
)

//var ctx = context.Background()

//Run start master
func Run(cmd *cobra.Command, args []string) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	log.Println("start master")
	run(&cmds.Master)
}

func run(cfg *cmds.MasterConfig) {
	ctx := signals.SetupSignalHandler(context.Background())
	//log.Println(cfg.HTTPSPort)
	master.StartMaster(ctx)
	<-ctx.Done()
}
