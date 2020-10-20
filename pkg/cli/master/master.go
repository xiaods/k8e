package master

import (
	"context"
	"fmt"
	"runtime"

	net2 "net"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/agent"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons"
	"github.com/xiaods/k8e/pkg/daemons/master"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/signals"
	"k8s.io/apimachinery/pkg/util/net"
)

//var ctx = context.Background()

//Run start master
func Run(cmd *cobra.Command, args []string) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	logrus.Info("start master")
	run(&cmds.Master)
}

func run(cfg *cmds.MasterConfig) error {
	var err error
	datadir, _ := datadir.LocalHome(cfg.DataDir, true)
	masterConfig := master.Config{}
	masterConfig.ControlConfig.DataDir = datadir
	masterConfig.ControlConfig.JoinURL = cfg.ServerURL
	masterConfig.ControlConfig.SANs = knownIPs(cfg.TLSSan)
	_, masterConfig.ControlConfig.ClusterIPRange, err = net2.ParseCIDR(cfg.ClusterCIDR)
	if err != nil {
		return err
	}
	ctx := signals.SetupSignalHandler(context.Background())
	//daemon := &daemons.Daemon{}
	if err = daemons.D.StartMaster(ctx, &masterConfig.ControlConfig); err != nil {
		return err
	}
	if cfg.DisableAgent {
		<-ctx.Done()
		return nil
	}
	ip := masterConfig.ControlConfig.BindAddress
	if ip == "" {
		ip = "127.0.0.1"
	}
	url := fmt.Sprintf("http://%s:%d", ip, 8080)
	agentConfig := cmds.AgentConfig
	agentConfig.ServerURL = url
	agentConfig.DataDir = datadir
	agentConfig.ClusterCIDR = cfg.ClusterCIDR
	return agent.InternlRun(ctx, &agentConfig)
}

func knownIPs(ips []string) []string {
	ips = append(ips, "127.0.0.1")
	ip, err := net.ChooseHostInterface()
	if err == nil {
		ips = append(ips, ip.String())
	}
	return ips
}
