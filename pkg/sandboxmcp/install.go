package sandboxmcp

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v2"
)

const (
	mcpServerName = "k8e-sandbox"
	settingsFile  = "settings.json"
	skillFileName = "SKILL.md"
)

// readSandboxMCPAddr resolves the SSE server address.
// Priority: K8E_SANDBOX_MCP_ADDR env → /etc/k8e/sandbox-mcp.yaml → default :8811.
func readSandboxMCPAddr() string {
	if addr := os.Getenv("K8E_SANDBOX_MCP_ADDR"); addr != "" {
		return addr
	}
	data, err := os.ReadFile("/etc/k8e/sandbox-mcp.yaml")
	if err != nil {
		return ":8811"
	}
	var cfg struct {
		Addr string `yaml:"addr"`
	}
	yaml.Unmarshal(data, &cfg) //nolint:errcheck
	if cfg.Addr != "" {
		return cfg.Addr
	}
	return ":8811"
}

// mcpEntryFor returns the JSON snippet added to agent config files.
// Always uses HTTP/SSE transport (url-based); address resolved from config.
func mcpEntryFor() map[string]any {
	addr := readSandboxMCPAddr()
	url := addr
	if len(url) > 0 && url[0] == ':' {
		url = "http://127.0.0.1" + url
	}
	return map[string]any{"url": url + "/mcp"}
}

// skillsDataDir returns the staged skills directory.
// Search order: dataDir/skills/ (production), binary dir/skills/, working dir/skills/ (dev).
func skillsDataDir() (string, error) {
	candidates := []string{
		filepath.Join("skills"), // working dir (dev/go run)
	}
	if exe, err := os.Executable(); err == nil {
		candidates = append([]string{filepath.Join(filepath.Dir(exe), "skills")}, candidates...)
	}
	for _, dir := range candidates {
		if info, err := os.Stat(dir); err == nil && info.IsDir() {
			return dir, nil
		}
	}
	return "", fmt.Errorf("skills/ directory not found; run 'k8e server' first")
}

// InstallSkill installs MCP config and all skills from dataDir/skills/ into the given agent.
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
	if err := mergeJSON(filepath.Join(homeDir(), ".claude.json"), []string{"mcpServers", mcpServerName}, mcpEntryFor(), "claude code"); err != nil {
		return err
	}
	return installAllSkills(filepath.Join(homeDir(), ".claude", "skills"), "claude code")
}

func installKiro() error {
	local := filepath.Join(".kiro", settingsFile)
	if _, err := os.Stat(filepath.Dir(local)); err == nil {
		if err := mergeJSON(local, []string{"mcpServers", mcpServerName}, mcpEntryFor(), "kiro-cli (workspace)"); err != nil {
			return err
		}
		return installAllSkills(filepath.Join(".kiro", "skills"), "kiro-cli (workspace)")
	}
	if err := mergeJSON(filepath.Join(homeDir(), ".kiro", settingsFile), []string{"mcpServers", mcpServerName}, mcpEntryFor(), "kiro-cli (global)"); err != nil {
		return err
	}
	return installAllSkills(filepath.Join(homeDir(), ".kiro", "skills"), "kiro-cli (global)")
}

func installGemini() error {
	if err := mergeJSON(filepath.Join(homeDir(), ".gemini", settingsFile), []string{"mcpServers", mcpServerName}, mcpEntryFor(), "gemini cli"); err != nil {
		return err
	}
	return installAllSkills(filepath.Join(homeDir(), ".gemini", "skills"), "gemini cli")
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
