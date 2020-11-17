package server

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
	"github.com/xiaods/k8e/pkg/daemons/server"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/signals"
	"k8s.io/apimachinery/pkg/util/net"
)

//Run start server
func Run(cmd *cobra.Command, args []string) {
	runtime.GOMAXPROCS(runtime.NumCPU())
	logrus.Info("start server")
	run(&cmds.Server)
}

func run(cfg *cmds.ServerConfig) error {
	var err error
	datadir, _ := datadir.LocalHome(cfg.DataDir, true)
	serverConfig := server.Config{}
	serverConfig.ControlConfig.DataDir = datadir
	serverConfig.ControlConfig.JoinURL = cfg.ServerURL
	serverConfig.ControlConfig.SANs = knownIPs(cfg.TLSSan)
	serverConfig.ControlConfig.DisableCCM = cfg.DisableCCM
	serverConfig.ControlConfig.AdvertisePort = cfg.HTTPSPort
	serverConfig.ControlConfig.AdvertiseIP = cfg.AdvertiseIP
	serverConfig.ControlConfig.DisableAgent = cfg.DisableAgent
	if cfg.APIServerBindAddress == "" {
		ip, err := net.ChooseHostInterface()
		if err != nil {
			return err
		}
		serverConfig.ControlConfig.APIServerBindAddress = ip.String()
	} else {
		serverConfig.ControlConfig.APIServerBindAddress = cfg.APIServerBindAddress
	}

	_, serverConfig.ControlConfig.ClusterIPRange, err = net2.ParseCIDR(cfg.ClusterCIDR)
	if err != nil {
		return err
	}
	ctx := signals.SetupSignalHandler(context.Background())
	if err = daemons.D.StartServer(ctx, &serverConfig.ControlConfig); err != nil {
		return err
	}
	if cfg.DisableAgent {
		<-ctx.Done()
		return nil
	}
	ip := serverConfig.ControlConfig.APIServerBindAddress
	if ip == "" {
		ip = "127.0.0.1"
	}
	url := fmt.Sprintf("https://%s:%d", ip, serverConfig.ControlConfig.APIServerPort)
	agentConfig := cmds.Agent
	agentConfig.ServerURL = url
	agentConfig.DataDir = datadir
	agentConfig.ClusterCIDR = cfg.ClusterCIDR
	agentConfig.DisableCCM = cfg.DisableCCM
	agentConfig.Internal = true
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
