package agent

import (
	"bufio"
	"context"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/control"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
)

func StartAgent(config *config.Agent) error {
	var err error
	if err = agent(config); err != nil {
		return err
	}
	return nil
}

func agent(config *config.Agent) error {
	var err error
	if err = prepare(config); err != nil {
		return err
	}
	if err = kubelet(config); err != nil {
		return nil
	}
	return nil
}
func Prepare(ctx context.Context, config *config.Agent) error {
	return prepare(config)
}

func prepare(config *config.Agent) error {
	os.MkdirAll(filepath.Join(config.DataDir, "cred"), 0700)
	initTLSCredPath(config)
	var err error
	err = genCerts(config)
	if err != nil {
		return err
	}
	return nil
}

func genCerts(config *config.Agent) error {
	var err error
	if err = genClientCerts(config); err != nil {
		return err
	}
	return nil
}

func genClientCerts(config *config.Agent) error {
	apiEndpoint := config.APIServerURL
	if err := control.KubeConfig(config.KubeConfigKubelet, apiEndpoint, "", "", ""); err != nil {
		return err
	}
	if err := control.KubeConfig(config.KubeConfigKubeProxy, apiEndpoint, "", "", ""); err != nil {
		return err
	}

	return nil
}

func initTLSCredPath(config *config.Agent) {
	config.KubeConfigKubelet = filepath.Join(config.DataDir, "cred", "kubelet.kubeconfig")
	config.KubeConfigKubeProxy = filepath.Join(config.DataDir, "cred", "kubeproxy.kubeconfig")
}

func Kubelet(ctx context.Context, cfg *config.Agent) error {
	return kubelet(cfg)
}

func KubeProxy(ctx context.Context, cfg *config.Agent) error {
	return kubeProxy(cfg)
}

func kubelet(cfg *config.Agent) error {
	argsMap := map[string]string{
		"healthz-bind-address":     "127.0.0.1",
		"read-only-port":           "0",
		"cluster-domain":           cfg.ClusterDomain,
		"kubeconfig":               cfg.KubeConfigKubelet,
		"eviction-hard":            "imagefs.available<5%,nodefs.available<5%",
		"eviction-minimum-reclaim": "imagefs.available=10%,nodefs.available=10%",
		"fail-swap-on":             "false",
		//"cgroup-root": "/k3s",
		"cgroup-driver":                "cgroupfs",
		"authentication-token-webhook": "false",
		"anonymous-auth":               "false",
		"authorization-mode":           modes.ModeWebhook}

	argsMap["container-runtime"] = "docker"

	if cfg.NodeName != "" {
		argsMap["hostname-override"] = cfg.NodeName
	}
	defaultIP, err := net.ChooseHostInterface()
	if err != nil || defaultIP.String() != cfg.NodeIP {
		argsMap["node-ip"] = cfg.NodeIP
	}
	root, hasCFS, hasPIDs := checkCgroups()
	if !hasCFS {
		logrus.Warn("Disabling CPU quotas due to missing cpu.cfs_period_us")
		argsMap["cpu-cfs-quota"] = "false"
	}
	if !hasPIDs {
		logrus.Warn("Disabling pod PIDs limit feature due to missing cgroup pids support")
		argsMap["cgroups-per-qos"] = "false"
		argsMap["enforce-node-allocatable"] = ""
		argsMap["feature-gates"] = addFeatureGate(argsMap["feature-gates"], "SupportPodPidsLimit=false")
	}
	if root != "" {
		argsMap["runtime-cgroups"] = root
		argsMap["kubelet-cgroups"] = root
	}
	if system.RunningInUserNS() {
		argsMap["feature-gates"] = addFeatureGate(argsMap["feature-gates"], "DevicePlugins=false")
	}

	argsMap["node-labels"] = strings.Join(cfg.NodeLabels, ",")
	if len(cfg.NodeTaints) > 0 {
		argsMap["register-with-taints"] = strings.Join(cfg.NodeTaints, ",")
	}
	if !cfg.DisableCCM {
		argsMap["cloud-provider"] = "external"
	}

	if cfg.ProtectKernelDefaults {
		argsMap["protect-kernel-defaults"] = "true"
	}

	args := config.GetArgsList(argsMap, cfg.ExtraKubeletArgs)
	logrus.Infof("Running kubelet %s", config.ArgString(args))
	return executor.Kubelet(args)
}

func kubeProxy(cfg *config.Agent) error {
	argsMap := map[string]string{
		"proxy-mode":           "iptables", // ipvs
		"healthz-bind-address": "127.0.0.1",
		"kubeconfig":           cfg.KubeConfigKubeProxy,
		"cluster-cidr":         cfg.ClusterCIDR.String(),
	}
	if cfg.NodeName != "" {
		argsMap["hostname-override"] = cfg.NodeName
	}

	args := config.GetArgsList(argsMap, cfg.ExtraKubeProxyArgs)
	logrus.Infof("Running kube-proxy %s", config.ArgString(args))
	return executor.KubeProxy(args)
}

func addFeatureGate(current, new string) string {
	if current == "" {
		return new
	}
	return current + "," + new
}

func checkCgroups() (root string, hasCFS bool, hasPIDs bool) {
	f, err := os.Open("/proc/self/cgroup")
	if err != nil {
		return "", false, false
	}
	defer f.Close()

	scan := bufio.NewScanner(f)
	for scan.Scan() {
		parts := strings.Split(scan.Text(), ":")
		if len(parts) < 3 {
			continue
		}
		systems := strings.Split(parts[1], ",")
		for _, system := range systems {
			if system == "pids" {
				hasPIDs = true
			} else if system == "cpu" {
				p := filepath.Join("/sys/fs/cgroup", parts[1], parts[2], "cpu.cfs_period_us")
				if _, err := os.Stat(p); err == nil {
					hasCFS = true
				}
			} else if system == "name=systemd" {
				last := parts[len(parts)-1]
				i := strings.LastIndex(last, ".slice")
				if i > 0 {
					root = "/systemd" + last[:i+len(".slice")]
				} else {
					root = "/systemd"
				}
			}
		}
	}
	return root, hasCFS, hasPIDs
}
