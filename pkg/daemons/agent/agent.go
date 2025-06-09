package agent

import (
	"context"
	"math/rand"
	"os"
	"time"

	pkgerrors "github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/agent/proxy"
	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/util"
	"k8s.io/component-base/logs"
	logsapi "k8s.io/component-base/logs/api/v1"
	_ "k8s.io/component-base/metrics/prometheus/restclient" // for client metric registration
	_ "k8s.io/component-base/metrics/prometheus/version"    // for version metric registration
)

func Agent(ctx context.Context, nodeConfig *daemonconfig.Node, proxy proxy.Proxy) error {
	rand.Seed(time.Now().UTC().UnixNano())
	logsapi.ReapplyHandling = logsapi.ReapplyHandlingIgnoreUnchanged
	logs.InitLogs()
	defer logs.FlushLogs()

	if err := startKubelet(ctx, &nodeConfig.AgentConfig); err != nil {
		return pkgerrors.WithMessage(err, "failed to start kubelet")
	}

	return nil
}

func startKubelet(ctx context.Context, cfg *daemonconfig.Agent) error {
	argsMap := kubeletArgs(cfg)

	args := util.GetArgs(argsMap, cfg.ExtraKubeletArgs)
	logrus.Infof("Running kubelet %s", daemonconfig.ArgString(args))

	return executor.Kubelet(ctx, args)
}

// ImageCredProvAvailable checks to see if the kubelet image credential provider bin dir and config
// files exist and are of the correct types. This is exported so that it may be used by downstream projects.
func ImageCredProvAvailable(cfg *daemonconfig.Agent) bool {
	if info, err := os.Stat(cfg.ImageCredProvBinDir); err != nil || !info.IsDir() {
		logrus.Debugf("Kubelet image credential provider bin directory check failed: %v", err)
		return false
	}
	if info, err := os.Stat(cfg.ImageCredProvConfig); err != nil || info.IsDir() {
		logrus.Debugf("Kubelet image credential provider config file check failed: %v", err)
		return false
	}
	return true
}
