package agent

import (
	"context"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/sirupsen/logrus"
	"github.com/xiaods/k8e/pkg/agent/proxy"
	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/executor"
	"github.com/xiaods/k8e/pkg/util"
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/restclient" // for client metric registration
	_ "k8s.io/component-base/metrics/prometheus/version"    // for version metric registration
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
)

const podManifestPathKey = "pod-manifest-path"

func Agent(ctx context.Context, nodeConfig *daemonconfig.Node, proxy proxy.Proxy) error {
	rand.Seed(time.Now().UTC().UnixNano())

	logs.InitLogs()
	defer logs.FlushLogs()
	if err := startKubelet(ctx, &nodeConfig.AgentConfig); err != nil {
		return err
	}

	return nil
}

func startKubelet(ctx context.Context, cfg *daemonconfig.Agent) error {
	argsMap := kubeletArgs(cfg)

	args := daemonconfig.GetArgs(argsMap, cfg.ExtraKubeletArgs)
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

func commonKubeletArgs(cfg *daemonconfig.Agent) map[string]string {
	argsMap := map[string]string{
		"healthz-bind-address":         "127.0.0.1",
		"read-only-port":               "0",
		"cluster-domain":               cfg.ClusterDomain,
		"kubeconfig":                   cfg.KubeConfigKubelet,
		"eviction-hard":                "imagefs.available<5%,nodefs.available<5%",
		"eviction-minimum-reclaim":     "imagefs.available=10%,nodefs.available=10%",
		"fail-swap-on":                 "false",
		"authentication-token-webhook": "true",
		"anonymous-auth":               "false",
		"authorization-mode":           modes.ModeWebhook,
	}
	if cfg.PodManifests != "" && argsMap[podManifestPathKey] == "" {
		argsMap[podManifestPathKey] = cfg.PodManifests
	}
	if err := os.MkdirAll(argsMap[podManifestPathKey], 0755); err != nil {
		logrus.Errorf("Failed to mkdir %s: %v", argsMap[podManifestPathKey], err)
	}
	if cfg.RootDir != "" {
		argsMap["root-dir"] = cfg.RootDir
		argsMap["cert-dir"] = filepath.Join(cfg.RootDir, "pki")
	}
	if len(cfg.ClusterDNS) > 0 {
		argsMap["cluster-dns"] = util.JoinIPs(cfg.ClusterDNSs)
	}
	if cfg.ResolvConf != "" {
		argsMap["resolv-conf"] = cfg.ResolvConf
	}
	if cfg.ListenAddress != "" {
		argsMap["address"] = cfg.ListenAddress
	}
	if cfg.ClientCA != "" {
		argsMap["anonymous-auth"] = "false"
		argsMap["client-ca-file"] = cfg.ClientCA
	}
	if cfg.ServingKubeletCert != "" && cfg.ServingKubeletKey != "" {
		argsMap["tls-cert-file"] = cfg.ServingKubeletCert
		argsMap["tls-private-key-file"] = cfg.ServingKubeletKey
	}
	if cfg.NodeName != "" {
		argsMap["hostname-override"] = cfg.NodeName
	}
	argsMap["node-labels"] = strings.Join(cfg.NodeLabels, ",")
	if len(cfg.NodeTaints) > 0 {
		argsMap["register-with-taints"] = strings.Join(cfg.NodeTaints, ",")
	}
	if !cfg.DisableCCM {
		argsMap["cloud-provider"] = "external"
	}
	if ImageCredProvAvailable(cfg) {
		logrus.Infof("Kubelet image credential provider bin dir and configuration file found.")
		argsMap["feature-gates"] = util.AddFeatureGate(argsMap["feature-gates"], "KubeletCredentialProviders=true")
		argsMap["image-credential-provider-bin-dir"] = cfg.ImageCredProvBinDir
		argsMap["image-credential-provider-config"] = cfg.ImageCredProvConfig
	}
	if cfg.ProtectKernelDefaults {
		argsMap["protect-kernel-defaults"] = "true"
	}
	return argsMap
}
