package executor

import (
	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"
)

// Embedded is the embedded executor implementation that runs all Kubernetes
// components in-process. It is registered via init() in embed.go.
type Embedded struct {
	nodeConfig *daemonconfig.Node
}
