package templates

import (
	"github.com/rancher/wharfie/pkg/registries"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

type ContainerdRuntimeConfig struct {
	RuntimeType string
	BinaryName  string
}

type ContainerdConfig struct {
	NodeConfig            *config.Node
	DisableCgroup         bool
	SystemdCgroup         bool
	IsRunningInUserNS     bool
	EnableUnprivileged    bool
	PrivateRegistryConfig *registries.Registry
	ExtraRuntimes         map[string]ContainerdRuntimeConfig
	Program               string
}
