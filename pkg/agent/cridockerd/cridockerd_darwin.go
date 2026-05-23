//go:build darwin
// +build darwin

package cridockerd

import (
	"context"
	"fmt"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func Run(ctx context.Context, cfg *config.Node) error {
	return fmt.Errorf("cri-dockerd is not supported on darwin")
}

func setupDockerCRIConfig(ctx context.Context, cfg *config.Node) error {
	return nil
}
