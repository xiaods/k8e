package client

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const skillFileName = "SKILL.md"

const (
	dirClaude = ".claude"
	dirCodex  = ".codex"
	dirPi     = ".pi"
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

// skillsDataDir returns the staged skills directory.
// Search order: /var/lib/k8e/server/skills/ (production), binary dir/skills/, working dir/skills/ (dev).
func skillsDataDir() (string, error) {
	candidates := []string{
		"/var/lib/k8e/server/skills", // production: k8e server data dir
		filepath.Join("skills"),      // working dir (dev/go run)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(exe), "skills")}, candidates...)
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("skills/ directory not found; run 'k8e server' first or check /var/lib/k8e/server/skills/")
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
		return installSkillLocalOrGlobal(dirPi, "pi")
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

// installAllSkills copies every skill directory from dataDir/skills/ into agentSkillsDir.
// Each skill must contain a SKILL.md. Idempotent.
func installAllSkills(agentSkillsDir, label string) error {
	src, err := skillsDataDir()
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return fmt.Errorf("%s: read skills dir: %w", label, err)
	}
	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}
		skillName := entry.Name()
		skillMD := filepath.Join(src, skillName, skillFileName)
		if _, err := os.Stat(skillMD); err != nil {
			continue // skip dirs without SKILL.md
		}
		dest := filepath.Join(agentSkillsDir, skillName, skillFileName)
		if _, err := os.Stat(dest); err == nil {
			fmt.Printf("✓ %s: skill %s already exists\n", label, skillName)
			continue
		}
		if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
			return fmt.Errorf("%s: mkdir %s: %w", label, filepath.Dir(dest), err)
		}
		data, err := os.ReadFile(skillMD)
		if err != nil {
			return fmt.Errorf("%s: read %s: %w", label, skillMD, err)
		}
		if err := os.WriteFile(dest, data, 0644); err != nil {
			return fmt.Errorf("%s: write %s: %w", label, dest, err)
		}
		fmt.Printf("✓ %s: skill %s installed → %s\n", label, skillName, dest)
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
