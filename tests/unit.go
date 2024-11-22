package tests

import (
	"net"
	"os"
	"path/filepath"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/daemons/control/deps"
)

// GenerateDataDir creates a temporary directory at "/tmp/k8e/<RANDOM_STRING>/".
// The latest directory created with this function is soft linked to "/tmp/k8e/latest/".
// This allows tests to replicate the "/var/lib/k8e" directory structure.
func GenerateDataDir(cnf *config.Control) error {
	if err := os.MkdirAll(cnf.DataDir, 0700); err != nil {
		return err
	}
	testDir, err := os.MkdirTemp(cnf.DataDir, "*")
	if err != nil {
		return err
	}
	// Remove old symlink and add new one
	os.Remove(filepath.Join(cnf.DataDir, "latest"))
	if err = os.Symlink(testDir, filepath.Join(cnf.DataDir, "latest")); err != nil {
		return err
	}
	cnf.DataDir = testDir
	cnf.DataDir, err = filepath.Abs(cnf.DataDir)
	if err != nil {
		return err
	}
	return nil
}

// CleanupDataDir removes the associated "/tmp/k8e/<RANDOM_STRING>"
// directory along with the 'latest' symlink that points at it.
func CleanupDataDir(cnf *config.Control) {
	os.Remove(filepath.Join(cnf.DataDir, "..", "latest"))
	os.RemoveAll(cnf.DataDir)
}

// GenerateRuntime creates a temporary data dir and configures
// config.ControlRuntime with all the appropriate certificate keys.
func GenerateRuntime(cnf *config.Control) error {
	cnf.Runtime = config.NewRuntime(nil)
	if err := GenerateDataDir(cnf); err != nil {
		return err
	}

	os.MkdirAll(filepath.Join(cnf.DataDir, "etc"), 0700)
	os.MkdirAll(filepath.Join(cnf.DataDir, "tls"), 0700)
	os.MkdirAll(filepath.Join(cnf.DataDir, "cred"), 0700)

	deps.CreateRuntimeCertFiles(cnf)

	cnf.Datastore.ServerTLSConfig.CAFile = cnf.Runtime.ETCDServerCA
	cnf.Datastore.ServerTLSConfig.CertFile = cnf.Runtime.ServerETCDCert
	cnf.Datastore.ServerTLSConfig.KeyFile = cnf.Runtime.ServerETCDKey

	return deps.GenServerDeps(cnf)
}

func ClusterIPNet() *net.IPNet {
	_, clusterIPNet, _ := net.ParseCIDR("10.42.0.0/16")
	return clusterIPNet
}

func ServiceIPNet() *net.IPNet {
	_, serviceIPNet, _ := net.ParseCIDR("10.43.0.0/16")
	return serviceIPNet
}
