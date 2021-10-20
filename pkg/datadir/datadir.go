package datadir

import (
	"os"
	"path/filepath"

	"github.com/pkg/errors"
	"github.com/rancher/wrangler/pkg/resolvehome"
	"github.com/xiaods/k8e/pkg/version"
)

var (
	DefaultDataDir     = "/var/lib/" + version.Program
	DefaultHomeDataDir = "${HOME}/." + version.Program
	HomeConfig         = "${HOME}/.kube/" + version.Program + ".yaml"
	GlobalConfig       = "/etc/" + version.Program + "/" + version.Program + ".yaml"
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
