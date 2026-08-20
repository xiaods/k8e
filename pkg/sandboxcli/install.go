package sandboxcli

import (
	"bytes"
	_ "embed" // required for //go:embed directive
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
)

//go:embed skills/k8e-sandbox/SKILL.md
var embeddedSkill []byte

const skillFileName = "SKILL.md"

// skillDirName is the directory name AND the slash-command name for Claude Code
// (`/k8e-sandbox`). Codex uses `$k8e-sandbox`; Pi uses `/skill:k8e-sandbox`.
const skillDirName = "k8e-sandbox"

// Harness config / discovery directory names (also used as path segments).
const (
	dirClaude = ".claude"
	dirCodex  = ".codex"
	dirAgents = ".agents"
	dirPi     = ".pi"
	dirDsh    = ".dsh"
)

// skillDest is one filesystem location where the skill should be written.
type skillDest struct {
	label string
	dir   string
}

// InstallResult records one install destination for user-facing summary.
type InstallResult struct {
	Agent string // claude | codex | pi | dsh
	Path  string
	Label string
}

// InstallSkill installs skill files into the given agent (refresh if content differs).
// target: "claude", "codex", "pi", "dsh", or "all"
func InstallSkill(target string) error {
	_, err := InstallSkillMulti(target, false)
	return err
}

// InstallSkillForce installs or updates skill files, always writing embedded content.
func InstallSkillForce(target string) error {
	_, err := InstallSkillMulti(target, true)
	return err
}

// InstallSkillMulti installs the skill into every discovery path used by the
// target harness(es). Returns destinations written (or already up to date).
func InstallSkillMulti(target string, force bool) ([]InstallResult, error) {
	targets, err := resolveSkillTargets(target)
	if err != nil {
		return nil, err
	}

	var results []InstallResult
	var errs []error
	for _, t := range targets {
		rs, e := installAgentPaths(t, force)
		results = append(results, rs...)
		if e != nil {
			errs = append(errs, e)
		}
	}
	if len(errs) > 0 {
		return results, fmt.Errorf("some installs failed: %v", errs)
	}
	return results, nil
}

// installAgentPaths writes the skill to every path a harness actually scans.
func installAgentPaths(agent string, force bool) ([]InstallResult, error) {
	dests, err := skillDestsForAgent(agent, homeDir())
	if err != nil {
		return nil, err
	}

	var results []InstallResult
	var errs []error
	seen := map[string]bool{}
	for _, d := range dests {
		abs, absErr := filepath.Abs(d.dir)
		if absErr != nil {
			abs = d.dir
		}
		if seen[abs] {
			continue
		}
		seen[abs] = true

		// Remove legacy directory name if present (old skill was k8e-sandbox-skill)
		legacy := filepath.Join(d.dir, "k8e-sandbox-skill")
		if pathExists(legacy) {
			_ = os.RemoveAll(legacy)
		}

		if err := installAllSkills(d.dir, d.label, force); err != nil {
			errs = append(errs, err)
			continue
		}
		results = append(results, InstallResult{
			Agent: agent,
			Path:  filepath.Join(d.dir, skillDirName, skillFileName),
			Label: d.label,
		})
	}
	if len(errs) > 0 {
		return results, fmt.Errorf("%s: %v", agent, errs)
	}
	return results, nil
}

// skillDestsForAgent returns the discovery paths for one harness.
func skillDestsForAgent(agent, home string) ([]skillDest, error) {
	switch agent {
	case "claude":
		return claudeSkillDests(home), nil
	case "codex":
		return codexSkillDests(home), nil
	case "pi":
		return piSkillDests(home), nil
	case "dsh":
		return dshSkillDests(home), nil
	default:
		return nil, fmt.Errorf("unknown target %q (want claude|codex|pi|dsh)", agent)
	}
}

// claudeSkillDests: directory name under skills/ becomes /k8e-sandbox.
// Personal skills override project when names collide — install both.
func claudeSkillDests(home string) []skillDest {
	dests := []skillDest{
		{"claude (global)", filepath.Join(home, dirClaude, "skills")},
		{"claude (.agents global)", filepath.Join(home, dirAgents, "skills")},
	}
	if pathExists(dirClaude) {
		dests = append(dests, skillDest{"claude (workspace)", filepath.Join(dirClaude, "skills")})
	}
	return dests
}

// codexSkillDests: ~/.codex/skills and project .codex/skills; $k8e-sandbox invoke.
// Also ~/.agents/skills for cross-harness discovery.
func codexSkillDests(home string) []skillDest {
	dests := []skillDest{
		{"codex (global)", filepath.Join(home, dirCodex, "skills")},
		{"codex (.agents global)", filepath.Join(home, dirAgents, "skills")},
	}
	if pathExists(dirCodex) {
		dests = append(dests, skillDest{"codex (workspace)", filepath.Join(dirCodex, "skills")})
	}
	if pathExists(dirAgents) {
		dests = append(dests, skillDest{"codex (.agents workspace)", filepath.Join(dirAgents, "skills")})
	}
	return dests
}

// piSkillDests: ~/.pi/agent/skills, .pi/skills, ~/.agents/skills (and project .agents).
func piSkillDests(home string) []skillDest {
	dests := []skillDest{
		{"pi (.agents global)", filepath.Join(home, dirAgents, "skills")},
		{"pi (global agent)", filepath.Join(home, dirPi, "agent", "skills")},
	}
	if pathExists(dirPi) {
		dests = append(dests, skillDest{"pi (workspace)", filepath.Join(dirPi, "skills")})
	}
	if pathExists(dirAgents) {
		dests = append(dests, skillDest{"pi (.agents workspace)", filepath.Join(dirAgents, "skills")})
	}
	return dests
}

// dshHome returns the DeepSeek Harness config root: $DSH_HOME or ~/.dsh
// (mirrors @deepseek-ai/dsh-home-paths resolveDshHome).
func dshHome() string {
	if h := strings.TrimSpace(os.Getenv("DSH_HOME")); h != "" {
		return h
	}
	return filepath.Join(homeDir(), dirDsh)
}

// dshSkillDests: $DSH_HOME/skills (default ~/.dsh/skills — the user-dsh root
// dsh scans at a higher rank than ~/.agents/skills) plus ~/.agents/skills for
// cross-harness discovery. The model loads the skill via the `skill` tool.
func dshSkillDests(home string) []skillDest {
	dests := []skillDest{
		{"dsh (global)", filepath.Join(dshHome(), "skills")},
		{"dsh (.agents global)", filepath.Join(home, dirAgents, "skills")},
	}
	if pathExists(dirDsh) {
		dests = append(dests, skillDest{"dsh (workspace)", filepath.Join(dirDsh, "skills")})
	}
	if pathExists(dirAgents) {
		dests = append(dests, skillDest{"dsh (.agents workspace)", filepath.Join(dirAgents, "skills")})
	}
	return dests
}

// resolveSkillTargets expands "auto" / named targets into concrete agent names.
func resolveSkillTargets(agentFlag string) ([]string, error) {
	agentFlag = strings.TrimSpace(strings.ToLower(agentFlag))
	if agentFlag == "" {
		agentFlag = "auto"
	}
	switch agentFlag {
	case "claude", "codex", "pi", "dsh":
		return []string{agentFlag}, nil
	case "all":
		return []string{"claude", "codex", "pi", "dsh"}, nil
	case "auto":
		return detectAvailableAgents(), nil
	default:
		return nil, fmt.Errorf("unknown --agent %q (want auto|claude|codex|pi|dsh|all)", agentFlag)
	}
}

// detectAvailableAgents returns harnesses that appear installed; if none, all three.
func detectAvailableAgents() []string {
	var found []string
	home := homeDir()
	if pathExists(dirClaude) || pathExists(filepath.Join(home, dirClaude)) {
		found = append(found, "claude")
	}
	if pathExists(dirCodex) || pathExists(filepath.Join(home, dirCodex)) {
		found = append(found, "codex")
	}
	// Pi: ~/.pi, .pi, or shared ~/.agents (common install root)
	if pathExists(dirPi) || pathExists(filepath.Join(home, dirPi)) ||
		pathExists(filepath.Join(home, dirAgents)) {
		found = append(found, "pi")
	}
	// dsh: ~/.dsh (DeepSeek Harness config root)
	if pathExists(dirDsh) || pathExists(filepath.Join(home, dirDsh)) ||
		pathExists(filepath.Join(home, dirAgents)) {
		found = append(found, "dsh")
	}
	if len(found) == 0 {
		return []string{"claude", "codex", "pi"}
	}
	return uniqueStrings(found)
}

func uniqueStrings(in []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(in))
	for _, s := range in {
		if seen[s] {
			continue
		}
		seen[s] = true
		out = append(out, s)
	}
	return out
}

func pathExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// installAllSkills copies the embedded SKILL.md to agentSkillsDir/k8e-sandbox/.
func installAllSkills(agentSkillsDir, label string, force bool) error {
	dest := filepath.Join(agentSkillsDir, skillDirName, skillFileName)
	if !force {
		if existing, err := os.ReadFile(dest); err == nil {
			if bytes.Equal(existing, embeddedSkill) {
				fmt.Printf("✓ %s: skill %s already up to date → %s\n", label, skillDirName, dest)
				return nil
			}
		}
	}
	if err := os.MkdirAll(filepath.Dir(dest), 0755); err != nil {
		return fmt.Errorf("%s: mkdir %s: %w", label, filepath.Dir(dest), err)
	}
	if err := os.WriteFile(dest, embeddedSkill, 0644); err != nil {
		return fmt.Errorf("%s: write %s: %w", label, dest, err)
	}
	fmt.Printf("✓ %s: skill %s installed → %s\n", label, skillDirName, dest)
	return nil
}

// StageSkills extracts the embedded skill file to the staging directory.
func StageSkills() error {
	dir, err := dataDir()
	if err != nil {
		return err
	}
	skillDir := filepath.Join(dir, "skills", skillDirName)
	if err := os.MkdirAll(skillDir, 0755); err != nil {
		return fmt.Errorf("stage skills: %w", err)
	}
	dest := filepath.Join(skillDir, skillFileName)
	if err := os.WriteFile(dest, embeddedSkill, 0644); err != nil {
		return fmt.Errorf("stage skills: %w", err)
	}
	return nil
}

// EnsureCLIOnPATH makes sure `k8e-sandbox-cli` is resolvable for agent shells.
// If missing from PATH, symlinks the current executable into ~/.local/bin.
func EnsureCLIOnPATH() (string, error) {
	if p, err := exec.LookPath("k8e-sandbox-cli"); err == nil {
		return p, nil
	}
	// Some installs expose the binary as `k8e` with sandbox subcommands only;
	// the standalone CLI name is required by the skill.
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("cannot resolve this binary path: %w", err)
	}
	if resolved, evalErr := filepath.EvalSymlinks(exe); evalErr == nil {
		exe = resolved
	}

	binDir := filepath.Join(homeDir(), ".local", "bin")
	if err := os.MkdirAll(binDir, 0755); err != nil {
		return "", fmt.Errorf("mkdir %s: %w", binDir, err)
	}
	link := filepath.Join(binDir, "k8e-sandbox-cli")
	// Replace stale symlink/file
	_ = os.Remove(link)
	if err := os.Symlink(exe, link); err != nil {
		return "", fmt.Errorf("symlink %s → %s: %w (add this binary to PATH as k8e-sandbox-cli)", link, exe, err)
	}
	return link, nil
}

func homeDir() string {
	if runtime.GOOS == "windows" {
		if h := os.Getenv("USERPROFILE"); h != "" {
			return h
		}
	}
	if h := os.Getenv("HOME"); h != "" {
		return h
	}
	return "/root"
}

// InvocationHints returns how to invoke the skill in each installed harness.
func InvocationHints(agents []string) []string {
	var lines []string
	for _, a := range uniqueStrings(agents) {
		switch a {
		case "claude":
			lines = append(lines, "Claude Code:  /k8e-sandbox <goal>")
		case "codex":
			lines = append(lines, "Codex:        $k8e-sandbox <goal>   (or /skills → k8e-sandbox)")
		case "pi":
			lines = append(lines, "Pi:           /skill:k8e-sandbox <goal>   (or /k8e-sandbox if skill commands enabled)")
		case "dsh":
			lines = append(lines, "dsh:          skill tool — the model loads k8e-sandbox by name, or the user names it directly in chat")
		}
	}
	return lines
}
