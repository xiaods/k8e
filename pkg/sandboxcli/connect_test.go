package sandboxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestResolveConnectMode_LocalDefault(t *testing.T) {
	mode, ep, err := resolveConnectMode("", "")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "local" || ep != "" {
		t.Fatalf("got mode=%q ep=%q", mode, ep)
	}
}

func TestResolveConnectMode_LocalLoopback(t *testing.T) {
	mode, ep, err := resolveConnectMode("127.0.0.1:50051", "")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "local" || ep != "127.0.0.1:50051" {
		t.Fatalf("got mode=%q ep=%q", mode, ep)
	}
}

func TestResolveConnectMode_RemoteRequiresEndpointWithKey(t *testing.T) {
	if _, _, err := resolveConnectMode("", "k8e-secret"); err == nil {
		t.Fatal("expected error when apikey without endpoint")
	}
}

func TestResolveConnectMode_RemoteWithKey(t *testing.T) {
	mode, ep, err := resolveConnectMode("sandbox.example.com:50051", "k8e-abc")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "remote" || ep != "sandbox.example.com:50051" {
		t.Fatalf("got mode=%q ep=%q", mode, ep)
	}
}

func TestResolveConnectMode_RemoteReauthWithoutKey(t *testing.T) {
	mode, ep, err := resolveConnectMode("sandbox.example.com:50051", "")
	if err != nil {
		t.Fatal(err)
	}
	if mode != "remote" || ep != "sandbox.example.com:50051" {
		t.Fatalf("got mode=%q ep=%q", mode, ep)
	}
}

func TestIsLocalEndpoint(t *testing.T) {
	cases := map[string]bool{
		"127.0.0.1:50051": true,
		"localhost:50051": true,
		"0.0.0.0:50051":   true,
		"example.com:1":   false,
		"10.0.0.1:50051":  false,
	}
	for ep, want := range cases {
		if got := isLocalEndpoint(ep); got != want {
			t.Errorf("isLocalEndpoint(%q)=%v want %v", ep, got, want)
		}
	}
}

func TestResolveSkillTargets(t *testing.T) {
	got, err := resolveSkillTargets("claude")
	if err != nil || len(got) != 1 || got[0] != "claude" {
		t.Fatalf("claude: %v %v", got, err)
	}
	got, err = resolveSkillTargets("all")
	if err != nil || len(got) != 3 {
		t.Fatalf("all: %v %v", got, err)
	}
	if _, err := resolveSkillTargets("bogus"); err == nil {
		t.Fatal("expected error for bogus agent")
	}
	got, err = resolveSkillTargets("auto")
	if err != nil || len(got) == 0 {
		t.Fatalf("auto: %v %v", got, err)
	}
}

func TestConnectionConfigRoundTrip(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	cfg := &ConnectionConfig{
		Mode:     "remote",
		Endpoint: "sandbox.example.com:50051",
		Agents:   []string{"claude", "codex"},
	}
	if err := SaveConnectionConfig(cfg); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(tmp, ".k8e", "sandbox", "config.json")
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("config not written: %v", err)
	}
	loaded, err := LoadConnectionConfig()
	if err != nil {
		t.Fatal(err)
	}
	if loaded == nil || loaded.Mode != "remote" || loaded.Endpoint != cfg.Endpoint {
		t.Fatalf("loaded: %+v", loaded)
	}
	if len(loaded.Agents) != 2 {
		t.Fatalf("agents: %v", loaded.Agents)
	}
}

func TestInstallSkillMulti_ClaudeWritesGlobalAndAgents(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	// No workspace .claude — still must install global paths.
	results, err := InstallSkillMulti("claude", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) < 2 {
		t.Fatalf("expected >=2 install locations, got %v", results)
	}
	// Claude Code slash command comes from directory name k8e-sandbox
	dest := filepath.Join(tmp, ".claude", "skills", "k8e-sandbox", "SKILL.md")
	data, err := os.ReadFile(dest)
	if err != nil {
		t.Fatalf("missing claude global skill: %v", err)
	}
	body := string(data)
	if !strings.Contains(body, "name: k8e-sandbox") {
		t.Fatalf("frontmatter name missing:\n%s", truncate(body, 200))
	}
	if !strings.Contains(body, "argument-hint:") {
		t.Fatal("missing argument-hint for slash autocomplete")
	}
	if !strings.Contains(body, "$ARGUMENTS") {
		t.Fatal("skill must use $ARGUMENTS for goal text")
	}
	if !strings.Contains(body, "k8e-sandbox-cli") {
		t.Fatal("skill must instruct k8e-sandbox-cli usage")
	}
	// Shared .agents path
	agentsDest := filepath.Join(tmp, ".agents", "skills", "k8e-sandbox", "SKILL.md")
	if _, err := os.Stat(agentsDest); err != nil {
		t.Fatalf("missing .agents skill path: %v", err)
	}
}

func TestInstallSkillMulti_CodexAndPiPaths(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	if _, err := InstallSkillMulti("codex", true); err != nil {
		t.Fatal(err)
	}
	codex := filepath.Join(tmp, ".codex", "skills", "k8e-sandbox", "SKILL.md")
	if _, err := os.Stat(codex); err != nil {
		t.Fatalf("codex skill: %v", err)
	}

	if _, err := InstallSkillMulti("pi", true); err != nil {
		t.Fatal(err)
	}
	piAgent := filepath.Join(tmp, ".pi", "agent", "skills", "k8e-sandbox", "SKILL.md")
	if _, err := os.Stat(piAgent); err != nil {
		t.Fatalf("pi agent skill: %v", err)
	}
	agents := filepath.Join(tmp, ".agents", "skills", "k8e-sandbox", "SKILL.md")
	if _, err := os.Stat(agents); err != nil {
		t.Fatalf("pi .agents skill: %v", err)
	}
}

func TestInstallSkillMulti_RemovesLegacySkillDir(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	legacy := filepath.Join(tmp, ".claude", "skills", "k8e-sandbox-skill")
	if err := os.MkdirAll(legacy, 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(legacy, "SKILL.md"), []byte("old"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := InstallSkillMulti("claude", true); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(legacy); !os.IsNotExist(err) {
		t.Fatalf("legacy skill dir should be removed, err=%v", err)
	}
}

func TestInvocationHints(t *testing.T) {
	lines := InvocationHints([]string{"claude", "codex", "pi", "claude"})
	if len(lines) != 3 {
		t.Fatalf("got %v", lines)
	}
	joined := strings.Join(lines, "\n")
	if !strings.Contains(joined, "/k8e-sandbox") {
		t.Fatal("claude slash missing")
	}
	if !strings.Contains(joined, "$k8e-sandbox") {
		t.Fatal("codex $ invoke missing")
	}
	if !strings.Contains(joined, "/skill:k8e-sandbox") {
		t.Fatal("pi skill command missing")
	}
}

func TestConnectSkillOnly_InstallsWithoutDial(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	// Run the skill-only path via connectSkillOnly-equivalent package API
	results, err := InstallSkillMulti("all", true)
	if err != nil {
		t.Fatal(err)
	}
	if len(results) == 0 {
		t.Fatal("expected skill install locations")
	}
	// skill-only must not require gateway config
	if pathExists(filepath.Join(tmp, ".claude", "skills", "k8e-sandbox", "SKILL.md")) ||
		pathExists(filepath.Join(tmp, ".codex", "skills", "k8e-sandbox", "SKILL.md")) {
		// ok — at least one primary path written
	} else {
		t.Fatalf("expected claude or codex skill, results=%v", results)
	}
}

func TestEnsureCLIOnPATH_SymlinksWhenMissing(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	// Prepend empty dir so LookPath fails for k8e-sandbox-cli even if host has it
	empty := filepath.Join(tmp, "empty-bin")
	if err := os.MkdirAll(empty, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", empty)

	link, err := EnsureCLIOnPATH()
	if err != nil {
		t.Fatal(err)
	}
	wantPrefix := filepath.Join(tmp, ".local", "bin", "k8e-sandbox-cli")
	if link != wantPrefix {
		t.Fatalf("link path %q want %q", link, wantPrefix)
	}
	fi, err := os.Lstat(link)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeSymlink == 0 {
		t.Fatal("expected symlink")
	}
}

func TestInstallAllSkills_SkipWhenUpToDate(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)

	// First install writes content.
	if _, err := InstallSkillMulti("claude", true); err != nil {
		t.Fatal(err)
	}
	dest := filepath.Join(tmp, dirClaude, "skills", skillDirName, skillFileName)
	before, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}

	// force=false with identical bytes must not rewrite (mtime stable).
	if _, err := InstallSkillMulti("claude", false); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(dest)
	if err != nil {
		t.Fatal(err)
	}
	if !before.ModTime().Equal(after.ModTime()) {
		t.Fatal("up-to-date install should not rewrite skill file")
	}
}

func TestSkillDestsForAgent(t *testing.T) {
	home := "/tmp/home-test"
	claude, err := skillDestsForAgent("claude", home)
	if err != nil || len(claude) < 2 {
		t.Fatalf("claude dests: %v %v", claude, err)
	}
	codex, err := skillDestsForAgent("codex", home)
	if err != nil || len(codex) < 2 {
		t.Fatalf("codex dests: %v %v", codex, err)
	}
	pi, err := skillDestsForAgent("pi", home)
	if err != nil || len(pi) < 2 {
		t.Fatalf("pi dests: %v %v", pi, err)
	}
	if _, err := skillDestsForAgent("bogus", home); err == nil {
		t.Fatal("expected error for unknown agent")
	}
}

func TestIsLocalEndpoint_IPv6AndBracketed(t *testing.T) {
	if !isLocalEndpoint("[::1]:50051") {
		t.Fatal("bracketed ::1 should be local")
	}
	if !isLocalEndpoint("::1") {
		t.Fatal("::1 without port should be local")
	}
	if isLocalEndpoint("[2001:db8::1]:50051") {
		t.Fatal("public IPv6 should not be local")
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}
