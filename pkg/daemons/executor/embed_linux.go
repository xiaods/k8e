//go:build linux && !no_embedded_executor
// +build linux,!no_embedded_executor

package executor

import (
	daemonconfig "github.com/xiaods/k8e/pkg/daemons/config"

	// registering k8e cloud provider
	_ "github.com/xiaods/k8e/pkg/cloudprovider"
)

func platformKubeProxyArgs(nodeConfig *daemonconfig.Node) map[string]string {
	argsMap := map[string]string{}
	return argsMap
}