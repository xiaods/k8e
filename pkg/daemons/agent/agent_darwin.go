//go:build darwin
// +build darwin

package agent

import (
	"github.com/xiaods/k8e/pkg/daemons/config"
)

func kubeletArgs(cfg *config.Agent) map[string]string {
	argsMap := commonKubeletArgs(cfg)
	if cfg.NodeIP != "" {
		argsMap["node-ip"] = cfg.NodeIP
	}
	return argsMap
}
