//go:build linux
// +build linux

package config

import (
	"path/filepath"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func applyContainerdStateAndAddress(nodeConfig *config.Node) {
	nodeConfig.Containerd.State = "/run/k8e/containerd"
	nodeConfig.Containerd.Address = filepath.Join(nodeConfig.Containerd.State, "containerd.sock")
}

func applyCRIDockerdAddress(nodeConfig *config.Node) {
	nodeConfig.CRIDockerd.Address = "unix:///run/k8e/cri-dockerd/cri-dockerd.sock"
}
