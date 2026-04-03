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

// skillContent is the SKILL.md content embedded into agent steering directories.
// Agents read this to understand when and how to use the sandbox tools.
const skillContent = `# K8E Sandbox Skill

**Always use the sandbox to run code, scripts, or shell commands** instead of executing directly on the host.

## How to Run Code

Use ` + "`sandbox_run`" + ` for all code execution — session is created and reused automatically:

` + "```" + `
tool: sandbox_run
args: { "code": "<your code>", "language": "python|bash|node" }
` + "```" + `

## Common Patterns

**Install packages then run:**
1. ` + "`sandbox_pip_install`" + ` — install packages into the session
2. ` + "`sandbox_exec`" + ` — run commands in the same session

**Write and execute a file:**
1. ` + "`sandbox_write_file`" + ` — write to ` + "`/workspace/<file>`" + `
2. ` + "`sandbox_exec`" + ` — run it

**Before irreversible actions** (delete, send, deploy):
` + "```" + `
tool: sandbox_confirm_action
args: { "session_id": "<id>", "action": "describe what you are about to do" }
` + "```" + `

## All Available Tools

| Tool | When to use |
|---|---|
| ` + "`sandbox_run`" + ` | ✅ Default — run any code or command |
| ` + "`sandbox_status`" + ` | Check if sandbox service is reachable |
| ` + "`sandbox_write_file`" + ` | Write a file to ` + "`/workspace`" + ` |
| ` + "`sandbox_read_file`" + ` | Read a file from ` + "`/workspace`" + ` |
| ` + "`sandbox_list_files`" + ` | List recently changed files |
| ` + "`sandbox_pip_install`" + ` | Install Python packages |
| ` + "`sandbox_exec`" + ` | Run command in an explicit session |
| ` + "`sandbox_exec_stream`" + ` | Run command with streaming output |
| ` + "`sandbox_create_session`" + ` | Create session with custom options (runtime, egress) |
| ` + "`sandbox_destroy_session`" + ` | Explicitly destroy a session |
| ` + "`sandbox_run_subagent`" + ` | Spawn a child sandbox for parallel tasks (depth ≤ 1) |
| ` + "`sandbox_confirm_action`" + ` | Require human approval before irreversible operations |
`

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

// installClaude runs `claude mcp add` if available, otherwise writes ~/.claude.json.
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
		// skill goes to workspace .kiro/skills/ so it's project-scoped
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

// installSkillDoc writes SKILL.md to path if not already present. Idempotent.
func installSkillDoc(path, label string) error {
	if _, err := os.Stat(path); err == nil {
		fmt.Printf("✓ %s: skill doc already exists → %s\n", label, path)
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return fmt.Errorf("%s: mkdir %s: %w", label, filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(skillContent), 0644); err != nil {
		return fmt.Errorf("%s: write skill doc %s: %w", label, path, err)
	}
	fmt.Printf("✓ %s: skill doc installed → %s\n", label, path)
	return nil
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
