package sandboxmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
)

const (
	mcpServerName = "k8e-sandbox"
	settingsFile  = "settings.json"
	skillDirName  = "k8e-sandbox-skill"
	skillFileName = "SKILL.md"
)

// mcpEntry is the JSON snippet added to agent config files.
var mcpEntry = map[string]any{
	"command": "k8e",
	"args":    []string{"sandbox-mcp"},
}

// readSkillContent reads SKILL.md from the filesystem.
// Search order: next to the k8e binary (production), then working directory (dev/go run).
func readSkillContent() ([]byte, error) {
	candidates := []string{skillFileName} // working directory first (dev)
	if exe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(exe), skillFileName)}, candidates...)
	}
	for _, path := range candidates {
		if data, err := os.ReadFile(path); err == nil {
			return data, nil
		}
	}
	return nil, fmt.Errorf("SKILL.md not found; place it next to the k8e binary or in the working directory")
}

// InstallSkill writes the k8e-sandbox MCP server entry and SKILL.md into the given agent's config.
// target: "claude", "kiro", "gemini", or "all"
func InstallSkill(target string) error {
	switch target {
	case "claude":
		return installClaude()
	case "kiro":
		return installKiro()
	case "gemini":
		return installGemini()
	case "all":
		var errs []error
		for _, fn := range []func() error{installClaude, installKiro, installGemini} {
			if err := fn(); err != nil {
				errs = append(errs, err)
			}
		}
		if len(errs) > 0 {
			return fmt.Errorf("some installs failed: %v", errs)
		}
		return nil
	default:
		return fmt.Errorf("unknown target %q — use: claude, kiro, gemini, all", target)
	}
}

func installClaude() error {
	path := filepath.Join(homeDir(), ".claude.json")
	if err := mergeJSON(path, []string{"mcpServers", mcpServerName}, mcpEntry, "claude code"); err != nil {
		return err
	}
	return installSkillDoc(filepath.Join(homeDir(), ".claude", "skills", skillDirName, skillFileName), "claude code")
}

func installKiro() error {
	local := filepath.Join(".kiro", settingsFile)
	if _, err := os.Stat(filepath.Dir(local)); err == nil {
		if err := mergeJSON(local, []string{"mcpServers", mcpServerName}, mcpEntry, "kiro-cli (workspace)"); err != nil {
			return err
		}
		return installSkillDoc(filepath.Join(".kiro", "skills", skillDirName, skillFileName), "kiro-cli (workspace)")
	}
	global := filepath.Join(homeDir(), ".kiro", settingsFile)
	if err := mergeJSON(global, []string{"mcpServers", mcpServerName}, mcpEntry, "kiro-cli (global)"); err != nil {
		return err
	}
	return installSkillDoc(filepath.Join(homeDir(), ".kiro", "skills", skillDirName, skillFileName), "kiro-cli (global)")
}

func installGemini() error {
	path := filepath.Join(homeDir(), ".gemini", settingsFile)
	if err := mergeJSON(path, []string{"mcpServers", mcpServerName}, mcpEntry, "gemini cli"); err != nil {
		return err
	}
	return installSkillDoc(filepath.Join(homeDir(), ".gemini", "skills", skillDirName, skillFileName), "gemini cli")
}

// installSkillDoc copies SKILL.md to path if not already present. Idempotent.
func installSkillDoc(path, label string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("✓ %s: skill doc already exists → %s\n", label, path)
		return nil
	}
	content, err := readSkillContent()
	if err != nil {
		return fmt.Errorf("%s: %w", label, err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("%s: mkdir %s: %w", label, filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, content, 0644); err != nil {
		return fmt.Errorf("%s: write skill doc %s: %w", label, path, err)
	}
	fmt.Printf("✓ %s: skill doc installed → %s\n", label, path)
	return nil
}

// mergeJSON reads path (creating if absent), sets obj[keys[0]][keys[1]] = value, writes back.
func mergeJSON(path string, keys []string, value any, label string) error {
	os.MkdirAll(filepath.Dir(path), 0755) //nolint:errcheck

	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &root) //nolint:errcheck
	}

	parent := root
	for _, k := range keys[:len(keys)-1] {
		if _, ok := parent[k]; !ok {
			parent[k] = map[string]any{}
		}
		parent = parent[k].(map[string]any)
	}
	leaf := keys[len(keys)-1]
	if _, exists := parent[leaf]; exists {
		fmt.Printf("✓ %s: k8e-sandbox already configured in %s\n", label, path)
		return nil
	}
	parent[leaf] = value

	data, err := json.MarshalIndent(root, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(path, append(data, '\n'), 0644); err != nil {
		return fmt.Errorf("%s: write %s: %w", label, path, err)
	}
	fmt.Printf("✓ %s: k8e-sandbox skill installed → %s\n", label, path)
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
