package agent

import (
	"bufio"
	"context"
	"io/ioutil"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/opencontainers/runc/libcontainer/system"
	"github.com/pkg/errors"
	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/clientaccess"
	"github.com/xiaods/k8e/pkg/daemons/agent/containerd"
	"github.com/xiaods/k8e/pkg/daemons/agent/flannel"
	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/control"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/daemons/syssetup"
	"github.com/xiaods/k8e/pkg/datadir"
	"k8s.io/apimachinery/pkg/util/net"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/clientcmd"
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
	return prepare(ctx, config)
}

func Containerd(ctx context.Context, config *config.Node) error {
	return containerd.Run(ctx, config)
}

func NetWorkCNI(ctx context.Context, config *config.Node) error {
	return networkCNI(ctx, config)
}

func Kubelet(ctx context.Context, cfg *config.Node) error {
	return kubelet(&cfg.AgentConfig)
}

func KubeProxy(ctx context.Context, cfg *config.Node) error {
	return kubeProxy(&cfg.AgentConfig)
}

func networkCNI(ctx context.Context, config *config.Node) error {
	logrus.Info("start networkcni")
	coreClient, err := coreClient(config.AgentConfig.KubeConfigKubelet)
	if err != nil {
		return err
	}
	if err := flannel.Run(ctx, config, coreClient.CoreV1().Nodes()); err != nil {
		return err
	}
	return nil
}

func coreClient(cfg string) (kubernetes.Interface, error) {
	restConfig, err := clientcmd.BuildConfigFromFlags("", cfg)
	if err != nil {
		return nil, err
	}

	return kubernetes.NewForConfig(restConfig)
}

func prepare(ctx context.Context, config *config.Node) error {
	syssetup.Configure()
	os.MkdirAll(filepath.Join(config.AgentConfig.DataDir, "cred"), 0700)
	var err error
	err = initTLSCredPath(config)
	if err != nil {
		return err
	}
	err = setupCriCtlConfig(config)
	if err != nil {
		return err
	}
	err = genCerts(&config.AgentConfig)
	if err != nil {
		return err
	}
	config.FlannelBackend = "vxlan"
	if err := flannel.Prepare(ctx, config); err != nil {
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

	nodeName, nodeIP, err := getHostnameAndIP(config)
	if err != nil {
		return err
	}

	info, err := clientaccess.ParseAndValidateToken(config.ServerURL, "")
	clientCAFile := filepath.Join(config.DataDir, "client-ca.crt")
	if err = getHostFile(clientCAFile, "", info); err != nil {
		return err
	}

	serverCAFile := filepath.Join(config.DataDir, "server-ca.crt")
	if err = getHostFile(serverCAFile, "", info); err != nil {
		return err
	}

	clientKubeletCert := filepath.Join(config.DataDir, "client-kubelet.crt")
	clientKubeletKey := filepath.Join(config.DataDir, "client-kubelet.key")
	if err = getNodeNamedHostFile(clientKubeletCert, clientKubeletKey, nodeName, nodeIP, "", info); err != nil {
		return err
	}
	apiEndpoint := config.APIServerURL
	if err = control.KubeConfig(config.KubeConfigKubelet, apiEndpoint, "", "", ""); err != nil {
		return err
	}
	if err = control.KubeConfig(config.KubeConfigKubeProxy, apiEndpoint, "", "", ""); err != nil {
		return err
	}

	return nil
}

func initTLSCredPath(nodeConfig *config.Node) error {
	dataDir := nodeConfig.AgentConfig.DataDir
	nodeConfig.AgentConfig.KubeConfigKubelet = filepath.Join(dataDir, "cred", "kubelet.kubeconfig")
	nodeConfig.AgentConfig.KubeConfigKubeProxy = filepath.Join(dataDir, "cred", "kubeproxy.kubeconfig")
	nodeConfig.Containerd.Config = filepath.Join(dataDir, "etc/containerd/config.toml")
	nodeConfig.Containerd.Root = filepath.Join(dataDir, "containerd")
	nodeConfig.Containerd.Opt = filepath.Join(dataDir, "containerd")
	nodeConfig.Containerd.State = "/run/k8e/containerd"
	nodeConfig.Containerd.Address = filepath.Join(nodeConfig.Containerd.State, "containerd.sock")
	nodeConfig.Containerd.Template = filepath.Join(dataDir, "etc/containerd/config.toml.tmpl")
	nodeConfig.AgentConfig.RuntimeSocket = containerdSock
	nodeName, nodeIP, err := getHostnameAndIP(&nodeConfig.AgentConfig)
	if err != nil {
		return err
	}
	// if envInfo.WithNodeID {
	// 	nodeID, err := ensureNodeID(filepath.Join(nodeConfigPath, "id"))
	// 	if err != nil {
	// 		return nil, err
	// 	}
	// 	nodeName += "-" + nodeID
	// }
	nodeConfig.AgentConfig.NodeName = nodeName
	nodeConfig.AgentConfig.NodeIP = nodeIP
	os.Setenv("NODE_NAME", nodeConfig.AgentConfig.NodeName)
	nodeConfig.NoFlannel = false
	if !nodeConfig.NoFlannel {
		hostLocal, err := exec.LookPath("host-local")
		if err != nil {
			return errors.Wrapf(err, "failed to find host-local")
		}

		//if envInfo.FlannelConf == "" {
		nodeConfig.FlannelConf = filepath.Join(dataDir, "etc/flannel/net-conf.json")
		// } else {
		// 	nodeConfig.FlannelConf = envInfo.FlannelConf
		// 	nodeConfig.FlannelConfOverride = true
		// }
		nodeConfig.AgentConfig.CNIBinDir = filepath.Dir(hostLocal)
		nodeConfig.AgentConfig.CNIConfDir = filepath.Join(dataDir, "etc/cni/net.d")
	}
	return nil
}

func getHostnameAndIP(info *config.Agent) (string, string, error) {
	ip := info.NodeIP
	if ip == "" {
		hostIP, err := net.ChooseHostInterface()
		if err != nil {
			return "", "", err
		}
		ip = hostIP.String()
	}

	name := info.NodeName
	if name == "" {
		hostname, err := os.Hostname()
		if err != nil {
			return "", "", err
		}
		name = hostname
	}

	// Use lower case hostname to comply with kubernetes constraint:
	// https://github.com/kubernetes/kubernetes/issues/71140
	name = strings.ToLower(name)

	return name, ip, nil
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
	argsMap["network-plugin"] = "cni"
	logrus.Info("RuntimeSocket", cfg.RuntimeSocket)
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
