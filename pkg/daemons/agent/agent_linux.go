//go:build linux
// +build linux

package agent

import (
	"net"
	"strings"

	"github.com/moby/sys/userns"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/cgroups"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/util"
	"golang.org/x/sys/unix"
	utilsnet "k8s.io/utils/net"
)

const socketPrefix = "unix://"

func createRootlessConfig(argsMap map[string]string, controllers map[string]bool) {
	argsMap["feature-gates=KubeletInUserNamespace"] = "true"
	// "/sys/fs/cgroup" is namespaced
	cgroupfsWritable := unix.Access("/sys/fs/cgroup", unix.W_OK) == nil
	if controllers["cpu"] && controllers["pids"] && cgroupfsWritable {
		logrus.Info("cgroup v2 controllers are delegated for rootless.")
		return
	}
	logrus.Fatal("delegated cgroup v2 controllers are required for rootless.")
}

func applyRuntimeSocketArgs(argsMap map[string]string, cfg *config.Agent) {
	if cfg.RuntimeSocket == "" {
		return
	}
	argsMap["serialize-image-pulls"] = "false"
	if strings.Contains(cfg.RuntimeSocket, "containerd") {
		argsMap["containerd"] = cfg.RuntimeSocket
	}
	// cadvisor wants the containerd CRI socket without the prefix, but kubelet wants it with the prefix
	if strings.HasPrefix(cfg.RuntimeSocket, socketPrefix) {
		argsMap["container-runtime-endpoint"] = cfg.RuntimeSocket
	} else {
		argsMap["container-runtime-endpoint"] = socketPrefix + cfg.RuntimeSocket
	}
	if cfg.ImageServiceSocket != "" {
		if strings.HasPrefix(cfg.ImageServiceSocket, socketPrefix) {
			argsMap["image-service-endpoint"] = cfg.ImageServiceSocket
		} else {
			argsMap["image-service-endpoint"] = socketPrefix + cfg.ImageServiceSocket
		}
	}
}

func applyCgroupArgs(argsMap map[string]string, cfg *config.Agent, controllers map[string]bool, kubeletRoot, runtimeRoot string) {
	if !controllers["cpu"] {
		logrus.Warn("Disabling CPU quotas due to missing cpu controller or cpu.cfs_period_us")
		argsMap["cpu-cfs-quota"] = "false"
	}
	if !controllers["pids"] {
		logrus.Fatal("pids cgroup controller not found")
	}
	if kubeletRoot != "" {
		argsMap["kubelet-cgroups"] = kubeletRoot
	}
	if runtimeRoot != "" {
		argsMap["runtime-cgroups"] = runtimeRoot
	}
	if userns.RunningInUserNS() {
		argsMap["feature-gates"] = util.AddFeatureGate(argsMap["feature-gates"], "DevicePlugins=false")
	}
}

func computeBindAddress(cfg *config.Agent) string {
	if utilsnet.IsIPv6(net.ParseIP([]string{cfg.NodeIP}[0])) {
		return "::1"
	}
	return "127.0.0.1"
}

func kubeletArgs(cfg *config.Agent) map[string]string {
	argsMap := commonKubeletArgs(cfg)
	argsMap["healthz-bind-address"] = computeBindAddress(cfg)
	argsMap["cgroup-driver"] = "cgroupfs"
	applyRuntimeSocketArgs(argsMap, cfg)
	if util.JoinIPs(cfg.NodeIPs) != "" {
		dualStack, err := utilsnet.IsDualStackIPs(cfg.NodeIPs)
		if err == nil && !dualStack {
			argsMap["node-ip"] = cfg.NodeIP
		}
	}
	kubeletRoot, runtimeRoot, controllers := cgroups.CheckCgroups()
	applyCgroupArgs(argsMap, cfg, controllers, kubeletRoot, runtimeRoot)
	if cfg.Rootless {
		createRootlessConfig(argsMap, controllers)
	}
	if cfg.Systemd {
		argsMap["cgroup-driver"] = "systemd"
	}
	return argsMap
}
