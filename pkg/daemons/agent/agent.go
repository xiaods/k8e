package agent

import (
	"bufio"
	"context"
	"io/ioutil"
	"os"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/daemons/agent/containerd"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/control"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/datadir"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
)

const (
	dockershimSock = "unix:///var/run/dockershim.sock"
	containerdSock = "unix:///run/k8e/containerd/containerd.sock"
)

// func StartAgent(config *config.Agent) error {
// 	var err error
// 	if err = agent(config); err != nil {
// 		return err
// 	}
// 	return nil
// }

// func agent(config *config.Agent) error {
// 	var err error
// 	if err = prepare(config); err != nil {
// 		return err
// 	}
// 	if err = kubelet(config); err != nil {
// 		return nil
// 	}
// 	return nil
// }

// setupCriCtlConfig creates the crictl config file and populates it
// with the given data from config.
func setupCriCtlConfig(nodeConfig *config.Node) error {
	cre := nodeConfig.ContainerRuntimeEndpoint
	if cre == "" {
		cre = containerdSock
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

func Prepare(ctx context.Context, config *config.Node) error {
	return prepare(config)
}

func Containerd(ctx context.Context, config *config.Node) error {
	return containerd.Run(ctx, config)
}

func prepare(config *config.Node) error {
	os.MkdirAll(filepath.Join(config.AgentConfig.DataDir, "cred"), 0700)
	initTLSCredPath(config)
	var err error
	err = setupCriCtlConfig(config)
	if err != nil {
		return err
	}
	err = genCerts(&config.AgentConfig)
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

func initTLSCredPath(nodeConfig *config.Node) {
	dataDir := nodeConfig.AgentConfig.DataDir
	nodeConfig.AgentConfig.KubeConfigKubelet = filepath.Join(dataDir, "cred", "kubelet.kubeconfig")
	nodeConfig.AgentConfig.KubeConfigKubeProxy = filepath.Join(dataDir, "cred", "kubeproxy.kubeconfig")
	nodeConfig.Containerd.Config = filepath.Join(dataDir, "etc/containerd/config.toml")
	nodeConfig.Containerd.Root = filepath.Join(dataDir, "containerd")
	nodeConfig.Containerd.Opt = filepath.Join(dataDir, "containerd")
	nodeConfig.Containerd.State = "/run/k3s/containerd"
	nodeConfig.Containerd.Address = filepath.Join(nodeConfig.Containerd.State, "containerd.sock")
	nodeConfig.Containerd.Template = filepath.Join(dataDir, "etc/containerd/config.toml.tmpl")
}

func Kubelet(ctx context.Context, cfg *config.Node) error {
	return kubelet(&cfg.AgentConfig)
}

func KubeProxy(ctx context.Context, cfg *config.Node) error {
	return kubeProxy(&cfg.AgentConfig)
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

	//	argsMap["container-runtime"] = "docker"
	if cfg.RuntimeSocket != "" {
		argsMap["container-runtime"] = "remote"
		argsMap["container-runtime-endpoint"] = cfg.RuntimeSocket
		argsMap["containerd"] = cfg.RuntimeSocket
		argsMap["serialize-image-pulls"] = "false"
	}
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
	//--cloud-provider=external 的 kubelet 将被添加一个 node.cloudprovider.kubernetes.io/uninitialized 的污点，导致其在初始化过程中不可调度（NoSchedule）
	if !cfg.DisableCCM {
		argsMap["cloud-provider"] = "external"
	}
	//设置 kubelet 的默认内核调整行为。如果已设置该参数，当任何内核可调参数与 kubelet 默认值不同时，kubelet 都会出错
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
