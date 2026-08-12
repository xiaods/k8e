package sandboxcli

import (
	"os"
	"path/filepath"
	"strings"
)

const dataDirName = ".k8e/sandbox"

// dataDir returns the sandbox data directory for certs + connection config.
// Priority matches pkg/sandbox/client: K8E_SANDBOX_CERT_DIR →
// $XDG_CONFIG_HOME/k8e/sandbox → ~/.k8e/sandbox.
func dataDir() (string, error) {
	if dir := strings.TrimSpace(os.Getenv("K8E_SANDBOX_CERT_DIR")); dir != "" {
		return filepath.Clean(dir), nil
	}
	if xdg := strings.TrimSpace(os.Getenv("XDG_CONFIG_HOME")); xdg != "" {
		return filepath.Join(xdg, "k8e", "sandbox"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, dataDirName), nil
}
