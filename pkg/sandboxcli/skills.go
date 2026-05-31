package sandboxcli

import (
	"path/filepath"

	"github.com/xiaods/k8e/pkg/sandbox/client"
)

// StageSkills extracts embedded skill files for the install-skill command.
// Stages to ~/.k8e/sandbox/skills/ (user-private, not world-writable).
func StageSkills() error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	dest := filepath.Join(dir, "skills")
	return client.StageSkills(dest)
}
