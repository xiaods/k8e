//go:build windows
// +build windows

package config

import (
	"path/filepath"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func applyContainerdStateAndAddress(nodeConfig *config.Node) {
	nodeConfig.Containerd.State = filepath.Join(nodeConfig.Containerd.Root, "state")
	nodeConfig.Containerd.Address = "npipe:////./pipe/containerd-containerd"
}

func applyCRIDockerdAddress(nodeConfig *config.Node) {
	nodeConfig.CRIDockerd.Address = "npipe:////.pipe/cri-dockerd"
}
