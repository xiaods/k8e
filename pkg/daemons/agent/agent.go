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
	"k8s.io/component-base/logs"
	_ "k8s.io/component-base/metrics/prometheus/restclient" // for client metric registration
	_ "k8s.io/component-base/metrics/prometheus/version"    // for version metric registration
	kubeletconfigv1beta1 "k8s.io/kubelet/config/v1beta1"
	"k8s.io/kubernetes/pkg/kubeapiserver/authorizer/modes"
	"k8s.io/kubernetes/pkg/util/taints"
)

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
	settings := newKubeletSettings()
	commonKubeletSettings(settings, cfg)
	applyPlatformKubeletSettings(settings, cfg)

	dir, err := kubeletConfigDir(cfg)
	if err != nil {
		return err
	}
	if err := settings.writeDropIn(dir); err != nil {
		return err
	}

	args := settings.args(cfg.ExtraKubeletArgs)
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

func commonKubeletSettings(s *kubeletSettings, cfg *daemonconfig.Agent) {
	kc := s.config
	kc.HealthzBindAddress = "127.0.0.1"
	kc.ReadOnlyPort = 0
	kc.ClusterDomain = cfg.ClusterDomain
	kc.EvictionHard = map[string]string{
		"imagefs.available": "5%",
		"nodefs.available":  "5%",
	}
	kc.EvictionMinimumReclaim = map[string]string{
		"imagefs.available": "10%",
		"nodefs.available":  "10%",
	}
	kc.FailSwapOn = boolPtr(false)
	kc.Authentication.Webhook.Enabled = boolPtr(true)
	kc.Authentication.Anonymous.Enabled = boolPtr(false)
	kc.Authorization.Mode = kubeletconfigv1beta1.KubeletAuthorizationMode(modes.ModeWebhook)

	// kubeconfig is a KubeletFlags field with no KubeletConfiguration
	// equivalent, so it has to stay on the command line.
	s.setFlag("kubeconfig", cfg.KubeConfigKubelet)

	applyCommonPathSettings(s, cfg)
	applyCommonConnectivitySettings(s, cfg)
	if cfg.NodeName != "" {
		s.setFlag("hostname-override", cfg.NodeName)
	}
	s.setFlag("node-labels", strings.Join(cfg.NodeLabels, ","))
	if len(cfg.NodeTaints) > 0 {
		taints, _, err := taints.ParseTaints(cfg.NodeTaints)
		if err != nil {
			logrus.Fatalf("Failed to parse node taints %v: %v", cfg.NodeTaints, err)
		}
		kc.RegisterWithTaints = taints
	}
	if !cfg.DisableCCM {
		s.setFlag("cloud-provider", "external")
	}
	applyImageCredentialSettings(s, cfg)
	if cfg.ProtectKernelDefaults {
		kc.ProtectKernelDefaults = true
	}
}

func applyCommonPathSettings(s *kubeletSettings, cfg *daemonconfig.Agent) {
	if cfg.PodManifests != "" && s.config.StaticPodPath == "" {
		s.config.StaticPodPath = cfg.PodManifests
	}
	if err := os.MkdirAll(s.config.StaticPodPath, 0755); err != nil {
		logrus.Errorf("Failed to mkdir %s: %v", s.config.StaticPodPath, err)
	}
	if cfg.RootDir != "" {
		s.setFlag("root-dir", cfg.RootDir)
		s.setFlag("cert-dir", filepath.Join(cfg.RootDir, "pki"))
	}
}

func applyCommonConnectivitySettings(s *kubeletSettings, cfg *daemonconfig.Agent) {
	if len(cfg.ClusterDNS) > 0 {
		s.config.ClusterDNS = ipStrings(cfg.ClusterDNSs)
	}
	if cfg.ResolvConf != "" {
		s.config.ResolverConfig = stringPtr(cfg.ResolvConf)
	}
	if cfg.ListenAddress != "" {
		s.config.Address = cfg.ListenAddress
	}
	if cfg.ClientCA != "" {
		s.config.Authentication.Anonymous.Enabled = boolPtr(false)
		s.config.Authentication.X509.ClientCAFile = cfg.ClientCA
	}
	if cfg.ServingKubeletCert != "" && cfg.ServingKubeletKey != "" {
		s.config.TLSCertFile = cfg.ServingKubeletCert
		s.config.TLSPrivateKeyFile = cfg.ServingKubeletKey
	}
}

func applyImageCredentialSettings(s *kubeletSettings, cfg *daemonconfig.Agent) {
	if !ImageCredProvAvailable(cfg) {
		return
	}
	logrus.Infof("Kubelet image credential provider bin dir and configuration file found.")
	// Both paths are KubeletFlags, not KubeletConfiguration fields.
	s.setFlag("image-credential-provider-bin-dir", cfg.ImageCredProvBinDir)
	s.setFlag("image-credential-provider-config", cfg.ImageCredProvConfig)
}
