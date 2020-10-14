package agent

import (
	"os"
	"path/filepath"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/executor"
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

	return nil
}

func prepare(config *config.Agent) {
	os.MkdirAll(filepath.Join(config.DataDir, "cred"), 0700)
	initTLSCredPath(config)
}

func genCerts(config *config.Agent) error {
	return nil
}

func genClientCerts(config *config.Agent) error {
	return nil
}

func initTLSCredPath(config *config.Agent) {
	config.KubeConfigKubelet = filepath.Join(config.DataDir, "cred", "kubelet.kubeconfig")
	config.KubeConfigKubeProxy = filepath.Join(config.DataDir, "cred", "kubeproxy.kubeconfig")
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

	args := config.GetArgsList(argsMap, cfg.ExtraKubeletArgs)
	return executor.Kubelet(args)
}
