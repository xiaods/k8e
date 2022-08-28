//go:build windows
// +build windows

package cridockerd

import (
	"context"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

const socketPrefix = "npipe://"

func setupDockerCRIConfig(ctx context.Context, cfg *config.Node) error {
	return nil
}
