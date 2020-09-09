package master

import (
	"context"
	"log"
	"runtime"

	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/etcd"
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
	log.Println(cfg.HTTPSPort)
	<-ctx.Done()
}

//运行etcd
func runEtcd() {
	e := etcd.New()
	e.Start()
}

func runApiserver() {

}

func runControlManager() {

}
