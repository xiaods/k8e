//go:build darwin
// +build darwin

package agent

import (
	"github.com/xiaods/k8e/pkg/daemons/config"
)

func applyPlatformKubeletSettings(s *kubeletSettings, cfg *config.Agent) {
	if cfg.NodeIP != "" {
		s.setFlag("node-ip", cfg.NodeIP)
	}
}
