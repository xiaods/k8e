package datadir

import (
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	"github.com/xiaods/k8e/pkg/version"
	"github.com/rancher/wrangler/pkg/resolvehome"
)

var (
	DefaultDataDir     = "/var/lib/k8e/" + version.Program
	DefaultHomeDataDir = "${HOME}/.k8e/" + version.Program
	HomeConfig         = "${HOME}/.kube/" + version.Program + ".yaml"
	GlobalConfig       = "/etc/k8e/" + version.Program + "/" + version.Program + ".yaml"
)

func Resolve(dataDir string) (string, error) {
	return LocalHome(dataDir, false)
}

func LocalHome(dataDir string, forceLocal bool) (string, error) {
	if dataDir == "" {
		if os.Getuid() == 0 && !forceLocal {
			dataDir = DefaultDataDir
		} else {
			dataDir = DefaultHomeDataDir
		}
	}

	dataDir, err := resolvehome.Resolve(dataDir)
	if err != nil {
		return "", errors.Wrapf(err, "resolving %s", dataDir)
	}

	return filepath.Abs(dataDir)
}
