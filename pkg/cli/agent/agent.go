package agent

import (
	"context"
	"io/ioutil"
	"os"

	net2 "net"

	"github.com/sirupsen/logrus"
	"github.com/spf13/cobra"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/signals"
)

const (
	dockershimSock = "unix:///var/run/dockershim.sock"
	containerdSock = "unix:///run/k3s/containerd/containerd.sock"
)

func Run(cmd *cobra.Command, args []string) {
	logrus.Info("start agent")
	ctx := signals.SetupSignalHandler(context.Background())
	InternlRun(ctx, &cmds.Agent)
}

func InternlRun(ctx context.Context, cfg *cmds.AgentConfig) error {
	var err error
	nodeConfig := &config.Node{}
	nodeConfig.Docker = cfg.Docker
	nodeConfig.ContainerRuntimeEndpoint = cfg.ContainerRuntimeEndpoint
	datadir, _ := datadir.LocalHome(cfg.DataDir, true)
	nodeConfig.AgentConfig.DataDir = datadir
	nodeConfig.AgentConfig.APIServerURL = cfg.ServerURL
	nodeConfig.AgentConfig.DisableCCM = cfg.DisableCCM
	_, nodeConfig.AgentConfig.ClusterCIDR, err = net2.ParseCIDR(cfg.ClusterCIDR)
	if err = setupCriCtlConfig(cfg); err != nil {
		return err
	}
	err = daemons.D.StartAgent(ctx, nodeConfig)
	if err != nil {
		return err
	}
	<-ctx.Done()
	return nil
}

func setupCriCtlConfig(cfg *cmds.AgentConfig) error {
	cre := cfg.ContainerRuntimeEndpoint
	if cre == "" {
		switch {
		case cfg.Docker:
			cre = dockershimSock
		default:
			cre = containerdSock
		}
	}

	agentConfDir := datadir.DefaultDataDir + "/agent/etc"
	if _, err := os.Stat(agentConfDir); os.IsNotExist(err) {
		if err := os.MkdirAll(agentConfDir, 0755); err != nil {
			return err
		}
	}

	crp := "runtime-endpoint: " + cre + "\n"
	return ioutil.WriteFile(agentConfDir+"/crictl.yaml", []byte(crp), 0600)
}
