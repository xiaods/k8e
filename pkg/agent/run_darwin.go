//go:build darwin
// +build darwin

package agent

import (
	"github.com/xiaods/k8e/pkg/cli/cmds"
	"github.com/xiaods/k8e/pkg/daemons/config"
)

func setupCriCtlConfig(cfg cmds.Agent, nodeConfig *config.Node) error {
	return nil
}
