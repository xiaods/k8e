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

func kubeletArgs(cfg *config.Agent) map[string]string {
	argsMap := commonKubeletArgs(cfg)
	bindAddress := "127.0.0.1"
	_, IPv6only, _ := util.GetFirstString([]string{cfg.NodeIP})
	if IPv6only {
		bindAddress = "::1"
	}
	argsMap["healthz-bind-address"] = bindAddress
	if cfg.RuntimeSocket != "" {
		argsMap["serialize-image-pulls"] = "false"
		if strings.HasPrefix(cfg.RuntimeSocket, socketPrefix) {
			argsMap["container-runtime-endpoint"] = cfg.RuntimeSocket
		} else {
			argsMap["container-runtime-endpoint"] = socketPrefix + cfg.RuntimeSocket
		}
	}
	defaultIP, err := net.ChooseHostInterface()
	if err != nil || defaultIP.String() != cfg.NodeIP {
		argsMap["node-ip"] = cfg.NodeIP
	}
	return argsMap
}
