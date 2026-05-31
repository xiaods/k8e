package sandboxcli

import (
	"os"
	"path/filepath"

	"github.com/xiaods/k8e/pkg/sandbox/client"
)

// StageSkills extracts embedded skill files for the install-skill command.
// Stages to ~/.k8e/sandbox/skills/ (user-private, not world-writable).
func StageSkills() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	dest := filepath.Join(home, ".k8e", "sandbox", "skills")
	return client.StageSkills(dest)
}
