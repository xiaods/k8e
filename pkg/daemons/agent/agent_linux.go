//go:build linux
// +build linux

package agent

import (
	"net"
	"strings"

	cadvisorcontainerd "github.com/google/cadvisor/lib/container/containerd"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/cgroups"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/util"
	"golang.org/x/sys/unix"
	"k8s.io/kubernetes/pkg/features"
	utilsnet "k8s.io/utils/net"
)

const socketPrefix = "unix://"

func createRootlessConfig(s *kubeletSettings, controllers map[string]bool) {
	// Referenced through the typed gate constant: if upstream removes the gate
	// the build breaks here, instead of the node going down at startup.
	s.addFeatureGate(string(features.KubeletInUserNamespace), true)
	// "/sys/fs/cgroup" is namespaced
	cgroupfsWritable := unix.Access("/sys/fs/cgroup", unix.W_OK) == nil
	if controllers["cpu"] && controllers["pids"] && cgroupfsWritable {
		logrus.Info("cgroup v2 controllers are delegated for rootless.")
		return
	}
	logrus.Fatal("delegated cgroup v2 controllers are required for rootless.")
}

// applyRuntimeSocketArgs points kubelet and cadvisor at the container runtime and
// image service sockets.
func applyRuntimeSocketArgs(s *kubeletSettings, cfg *config.Agent) {
	if cfg.RuntimeSocket == "" {
		return
	}
	s.config.SerializeImagePulls = boolPtr(false)
	if strings.Contains(cfg.RuntimeSocket, "containerd") {
		// cadvisor needs the containerd endpoint to collect container stats. The
		// kubelet used to expose this as a --containerd flag, but stopped
		// registering it in v1.37, so the flag now makes kubelet fail to parse
		// its own arguments ("unknown flag: --containerd") and the agent never
		// comes up. Set the cadvisor flag value directly instead.
		*cadvisorcontainerd.ArgContainerdEndpoint = strings.TrimPrefix(cfg.RuntimeSocket, socketPrefix)
	}
	// cadvisor wants the containerd CRI socket without the prefix, but kubelet wants it with the prefix
	if strings.HasPrefix(cfg.RuntimeSocket, socketPrefix) {
		s.config.ContainerRuntimeEndpoint = cfg.RuntimeSocket
	} else {
		s.config.ContainerRuntimeEndpoint = socketPrefix + cfg.RuntimeSocket
	}
	if cfg.ImageServiceSocket != "" {
		if strings.HasPrefix(cfg.ImageServiceSocket, socketPrefix) {
			s.config.ImageServiceEndpoint = cfg.ImageServiceSocket
		} else {
			s.config.ImageServiceEndpoint = socketPrefix + cfg.ImageServiceSocket
		}
	}
}

func applyCgroupArgs(s *kubeletSettings, controllers map[string]bool, kubeletRoot, runtimeRoot string) {
	if !controllers["cpu"] {
		logrus.Warn("Disabling CPU quotas due to missing cpu controller or cpu.cfs_period_us")
		s.config.CPUCFSQuota = boolPtr(false)
	}
	if !controllers["pids"] {
		logrus.Fatal("pids cgroup controller not found")
	}
	if kubeletRoot != "" {
		s.config.KubeletCgroups = kubeletRoot
	}
	if runtimeRoot != "" {
		// runtime-cgroups has no KubeletConfiguration field.
		s.setFlag("runtime-cgroups", runtimeRoot)
	}
}

func computeBindAddress(cfg *config.Agent) string {
	if utilsnet.IsIPv6(net.ParseIP([]string{cfg.NodeIP}[0])) {
		return "::1"
	}
	return "127.0.0.1"
}

func applyPlatformKubeletSettings(s *kubeletSettings, cfg *config.Agent) {
	s.config.HealthzBindAddress = computeBindAddress(cfg)
	s.config.CgroupDriver = "cgroupfs"
	applyRuntimeSocketArgs(s, cfg)
	if util.JoinIPs(cfg.NodeIPs) != "" {
		dualStack, err := utilsnet.IsDualStackIPs(cfg.NodeIPs)
		if err == nil && !dualStack {
			s.setFlag("node-ip", cfg.NodeIP)
		}
	}
	kubeletRoot, runtimeRoot, controllers := cgroups.CheckCgroups()
	applyCgroupArgs(s, controllers, kubeletRoot, runtimeRoot)
	if cfg.Rootless {
		createRootlessConfig(s, controllers)
	}
	if cfg.Systemd {
		s.config.CgroupDriver = "systemd"
	}
}
