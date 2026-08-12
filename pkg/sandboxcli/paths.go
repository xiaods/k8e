package sandboxcli

import (
	"os"
	"path/filepath"
	"strings"
)

const dataDirName = ".k8e/sandbox"

// dataDir returns the sandbox data directory for certs + connection config.
// Priority matches pkg/sandbox/client: K8E_SANDBOX_CERT_DIR → ~/.k8e/sandbox.
func dataDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("K8E_SANDBOX_CERT_DIR")); dir != "" {
		return filepath.Clean(dir), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dataDirName), nil
}
