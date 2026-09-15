//go:build windows
// +build windows

package agent

import (
	"strings"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/util"
	"k8s.io/apimachinery/pkg/util/net"
)

const socketPrefix = "npipe://"

func applyPlatformKubeletSettings(s *kubeletSettings, cfg *config.Agent) {
	bindAddress := "127.0.0.1"
	_, IPv6only, _ := util.GetFirstString([]string{cfg.NodeIP})
	if IPv6only {
		bindAddress = "::1"
	}
	s.config.HealthzBindAddress = bindAddress
	if cfg.RuntimeSocket != "" {
		s.config.SerializeImagePulls = boolPtr(false)
		if strings.HasPrefix(cfg.RuntimeSocket, socketPrefix) {
			s.config.ContainerRuntimeEndpoint = cfg.RuntimeSocket
		} else {
			s.config.ContainerRuntimeEndpoint = socketPrefix + cfg.RuntimeSocket
		}
	}
	defaultIP, err := net.ChooseHostInterface()
	if err != nil || defaultIP.String() != cfg.NodeIP {
		s.setFlag("node-ip", cfg.NodeIP)
	}
}
