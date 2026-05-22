//go:build darwin
// +build darwin

package containerd

import (
	containerd "github.com/containerd/containerd/v2/client"
	"github.com/pkg/errors"
	"github.com/xiaods/k8e/pkg/agent/templates"
	"github.com/xiaods/k8e/pkg/daemons/config"
	util3 "github.com/xiaods/k8e/pkg/util"
)

func getContainerdArgs(cfg *config.Node) []string {
	return nil
}

func SetupContainerdConfig(cfg *config.Node) error {
	containerdConfig := templates.ContainerdConfig{
		NodeConfig: cfg,
	}
	return writeContainerdConfig(cfg, containerdConfig)
}

func Client(address string) (*containerd.Client, error) {
	return nil, errors.Wrapf(util3.ErrUnsupportedPlatform, "containerd client is not supported on darwin")
}

func OverlaySupported(root string) error {
	return errors.Wrapf(util3.ErrUnsupportedPlatform, "overlayfs is not supported on darwin")
}

func FuseoverlayfsSupported(root string) error {
	return errors.Wrapf(util3.ErrUnsupportedPlatform, "fuse-overlayfs is not supported on darwin")
}

func StargzSupported(root string) error {
	return errors.Wrapf(util3.ErrUnsupportedPlatform, "stargz is not supported on darwin")
}
