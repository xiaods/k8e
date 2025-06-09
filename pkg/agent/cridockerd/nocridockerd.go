//go:build no_cri_dockerd
// +build no_cri_dockerd

package cridockerd

import (
	"context"
	"errors"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func Run(ctx context.Context, cfg *config.Node) error {
	return errors.New("cri-dockerd disabled at build time")
}
