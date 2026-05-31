package sandboxcli

import (
	"os"
	"path/filepath"

	"github.com/xiaods/k8e/pkg/sandbox/client"
)

// StageSkills extracts embedded skill files for the install-skill command.
// Stages to /tmp/k8e-skills which skillsDataDir checks.
func StageSkills() error {
	dest := filepath.Join(os.TempDir(), "k8e-skills")
	return client.StageSkills(dest)
}
