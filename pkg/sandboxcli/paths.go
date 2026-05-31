package sandboxcli

import (
	"os"
	"path/filepath"
)

const dataDirName = ".k8e/sandbox"

// dataDir returns the sandbox data directory under the user's home.
func dataDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dataDirName), nil
}
