package sandboxcli

import (
	_ "embed" // required for //go:embed directive
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

//go:embed skills/k8e-sandbox/SKILL.md
var embeddedSkill []byte

const skillFileName = "SKILL.md"

const (
	dirClaude = ".claude"
	dirCodex  = ".codex"
)

// installSkillLocalOrGlobal installs skills into the agent's local workspace first,
// falling back to the global home directory.
func installSkillLocalOrGlobal(dir, label string) error {
	local := filepath.Join(dir, "skills")
	if _, err := os.Stat(dir); err == nil {
		return installAllSkills(local, label+" (workspace)")
	}
	return installAllSkills(filepath.Join(homeDir(), dir, "skills"), label+" (global)")
}

// InstallSkill installs skill files into the given agent.
// target: "claude", "codex", "pi", or "all"
func InstallSkill(target string) error {
	switch target {
	case "claude":
		return installAllSkills(filepath.Join(homeDir(), dirClaude, "skills"), "claude code")
	case "codex":
		return installSkillLocalOrGlobal(dirCodex, "codex")
	case "pi":
		// project-local: .pi/skills/, global: ~/.agents/skills/
		if _, err := os.Stat(filepath.Join(".pi", "skills")); err == nil {
			return installAllSkills(filepath.Join(".pi", "skills"), "pi (workspace)")
		}
		return installAllSkills(filepath.Join(homeDir(), ".agents", "skills"), "pi (global)")
	case "all":
		var errs []error
		for _, fn := range []func() error{
			func() error { return InstallSkill("claude") },
			func() error { return InstallSkill("codex") },
			func() error { return InstallSkill("pi") },
		} {
			if err := fn(); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("some installs failed: %v", errs)
		}
		return nil
	default:
		return fmt.Errorf("unknown target %q", target)
	}
}

// installAllSkills copies the embedded SKILL.md to agentSkillsDir. Idempotent.
func installAllSkills(agentSkillsDir, label string) error {
	skillName := "k8e-sandbox"
	dest := filepath.Join(agentSkillsDir, skillName, skillFileName)
	if _, err := os.Stat(dest); err == nil {
		fmt.Printf("✓ %s: skill %s already exists\n", label, skillName)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("%s: mkdir %s: %w", label, filepath.Dir(dest), err)
	}
	if err := os.WriteFile(dest, embeddedSkill, 0644); err != nil {
		return fmt.Errorf("%s: write %s: %w", label, dest, err)
	}
	fmt.Printf("✓ %s: skill %s installed → %s\n", label, skillName, dest)
	return nil
}

// StageSkills extracts the embedded skill file to the staging directory.
func StageSkills() error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	skillDir := filepath.Join(dir, "skills", "k8e-sandbox")
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("stage skills: %w", err)
	}
	dest := filepath.Join(skillDir, skillFileName)
	if err := os.WriteFile(dest, embeddedSkill, 0644); err != nil {
		return fmt.Errorf("stage skills: %w", err)
	}
	return nil
}

func homeDir() string {
	if runtime.GOOS == "windows" {
		return os.Getenv("USERPROFILE")
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}
