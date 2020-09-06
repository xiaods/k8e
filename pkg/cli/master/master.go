package master

import (
	"context"
	"log"
	"runtime"

	"github.com/xiaods/k8e/pkg/etcd"
)

var ctx = context.Background()

//Run start master
func Run() {
	runtime.GOMAXPROCS(runtime.NumCPU())
	log.SetFlags(log.Lshortfile | log.LstdFlags)
	log.Println("start master")
	runEtcd()

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
