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
)

// mcpEntry is the JSON snippet added to agent config files.
var mcpEntry = map[string]any{
	"command": "k8e",
	"args":    []string{"sandbox-mcp"},
}

// InstallSkill writes the k8e-sandbox MCP server entry into the given agent's config.
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

// installClaude runs `claude mcp add` if available, otherwise writes ~/.claude.json.
func installClaude() error {
	// claude code stores MCP servers in ~/.claude.json under mcpServers
	path := filepath.Join(homeDir(), ".claude.json")
	return mergeJSON(path, []string{"mcpServers", mcpServerName}, mcpEntry, "claude code")
}

func installKiro() error {
	// kiro-cli: workspace .kiro/settings.json takes precedence; fall back to global
	local := filepath.Join(".kiro", settingsFile)
	if _, err := os.Stat(filepath.Dir(local)); err == nil {
		return mergeJSON(local, []string{"mcpServers", mcpServerName}, mcpEntry, "kiro-cli (workspace)")
	}
	global := filepath.Join(homeDir(), ".kiro", settingsFile)
	return mergeJSON(global, []string{"mcpServers", mcpServerName}, mcpEntry, "kiro-cli (global)")
}

func installGemini() error {
	path := filepath.Join(homeDir(), ".gemini", settingsFile)
	return mergeJSON(path, []string{"mcpServers", mcpServerName}, mcpEntry, "gemini cli")
}

// mergeJSON reads path (creating if absent), sets obj[keys[0]][keys[1]] = value, writes back.
func mergeJSON(path string, keys []string, value any, label string) error {
	os.MkdirAll(filepath.Dir(path), 0755)

	root := map[string]any{}
	if data, err := os.ReadFile(path); err == nil {
		json.Unmarshal(data, &root) //nolint:errcheck — best effort, start fresh on corrupt
	}

	// navigate/create nested key
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
