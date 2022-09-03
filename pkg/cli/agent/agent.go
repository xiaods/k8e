package agent

import (
	"fmt"
	"os"
	"runtime"

	"github.com/erikdubbelboer/gspt"
	"github.com/rancher/wrangler/pkg/signals"
	"github.com/sirupsen/logrus"
	"github.com/urfave/cli"
	"github.com/xiaods/k8e/pkg/agent"
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/datadir"
	"github.com/xiaods/k8e/pkg/token"
	"github.com/xiaods/k8e/pkg/version"
)

func Run(ctx *cli.Context) error {
	// hide process arguments from ps output, since they may contain
	// database credentials or other secrets.
	gspt.SetProcTitle(os.Args[0] + " agent")

	// Evacuate cgroup v2 before doing anything else that may fork.
	if err := cmds.EvacuateCgroup2(); err != nil {
		return err
	}

	// Initialize logging, and subprocess reaping if necessary.
	// Log output redirection and subprocess reaping both require forking.
	if err := cmds.InitLogging(); err != nil {
		return err
	}

	if os.Getuid() != 0 && runtime.GOOS != "windows" {
		return fmt.Errorf("agent must be ran as root")
	}

	if cmds.AgentConfig.TokenFile != "" {
		token, err := token.ReadFile(cmds.AgentConfig.TokenFile)
		if err != nil {
			return err
		}
		cmds.AgentConfig.Token = token
	}

	if cmds.AgentConfig.Token == "" && cmds.AgentConfig.ClusterSecret != "" {
		logrus.Fatal("cluster-secret is deprecated. Use --token instead.")
	}

	if cmds.AgentConfig.Token == "" {
		return fmt.Errorf("--token is required")
	}

	if cmds.AgentConfig.ServerURL == "" {
		return fmt.Errorf("--server is required")
	}

	logrus.Info("Starting " + version.Program + " agent " + ctx.App.Version)

	dataDir, err := datadir.LocalHome(cmds.AgentConfig.DataDir, cmds.AgentConfig.Rootless)
	if err != nil {
		return err
	}

	cfg := cmds.AgentConfig
	cfg.Debug = ctx.GlobalBool("debug")
	cfg.DataDir = dataDir

	contextCtx := signals.SetupSignalContext()

	return agent.Run(contextCtx, cfg)
}
