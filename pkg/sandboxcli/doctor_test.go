package sandboxcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDoctor_DetectsMissingDshBundle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("DSH_HOME", filepath.Join(tmp, ".dsh"))

	// A dsh profile whose dependencies contain the bundle but whose
	// dsh.profile.bundles does NOT — the exact `dsh plugin add` pnpm gap.
	profilesDir := filepath.Join(tmp, ".dsh", "profiles", "web")
	if err := os.MkdirAll(profilesDir, 0755); err != nil {
		t.Fatal(err)
	}
	profilePkg := filepath.Join(profilesDir, "package.json")
	writeJSON := map[string]any{
		"dependencies": map[string]string{
			"@k8e-sandbox/dsh-k8e-sandbox-bundle": "^0.3.0",
		},
		"dsh": map[string]any{
			"profile": map[string]any{
				"bundles": []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"},
			},
		},
	}
	data, _ := json.Marshal(writeJSON)
	if err := os.WriteFile(profilePkg, data, 0644); err != nil {
		t.Fatal(err)
	}

	// Simulate the installed bundle in node_modules so the "installed" check passes.
	nm := filepath.Join(profilesDir, "node_modules", "@k8e-sandbox", "dsh-k8e-sandbox-bundle")
	if err := os.MkdirAll(nm, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(nm, "package.json"), []byte(`{"name":"@k8e-sandbox/dsh-k8e-sandbox-bundle","version":"0.3.0"}`), 0644)

	// Set PATH to an empty dir so the binary check fails too (or keep it — just
	// assert the bundle-registration check verdict specifically).
	bundles := dshProfileBundles(profilePkg)
	found := false
	for _, b := range bundles {
		if b == k8eBundleName {
			found = true
		}
	}
	if found {
		t.Fatal("fixture must start with the bundle NOT registered")
	}

	got := ""
	for _, p := range dshProfilePaths() {
		for _, b := range dshProfileBundles(p) {
			if b == k8eBundleName {
				got = b
			}
		}
		if v := dshBundleInstalledVersion(p); v != "0.3.0" {
			t.Fatalf("installed version: got %q, want 0.3.0", v)
		}
	}
	if got != "" {
		t.Fatalf("bundle should be missing from bundles, got %q", got)
	}
}

func TestDoctor_DetectsRegisteredBundle(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("DSH_HOME", filepath.Join(tmp, ".dsh"))

	profilesDir := filepath.Join(tmp, ".dsh", "profiles", "web")
	if err := os.MkdirAll(profilesDir, 0755); err != nil {
		t.Fatal(err)
	}
	profilePkg := filepath.Join(profilesDir, "package.json")
	writeJSON := map[string]any{
		"dependencies": map[string]string{
			"@k8e-sandbox/dsh-k8e-sandbox-bundle": "^0.3.0",
		},
		"dsh": map[string]any{
			"profile": map[string]any{
				"bundles": []string{"@deepseek-ai/dsh-base", k8eBundleName},
			},
		},
	}
	data, _ := json.Marshal(writeJSON)
	if err := os.WriteFile(profilePkg, data, 0644); err != nil {
		t.Fatal(err)
	}

	found := false
	for _, p := range dshProfilePaths() {
		for _, b := range dshProfileBundles(p) {
			if strings.HasPrefix(b, k8eBundleName) {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("bundle must be detected as registered")
	}
}

func TestFixDshBundles_AppendsAndPreservesExisting(t *testing.T) {
	tmp := t.TempDir()
	profilePkg := filepath.Join(tmp, "package.json")
	orig := map[string]any{
		"dependencies": map[string]string{"@k8e-sandbox/dsh-k8e-sandbox-bundle": "^0.3.0"},
		"dsh": map[string]any{
			"profile": map[string]any{
				"bundles": []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"},
			},
		},
	}
	data, _ := json.Marshal(orig)
	if err := os.WriteFile(profilePkg, data, 0644); err != nil {
		t.Fatal(err)
	}

	names, err := fixDshBundles(profilePkg)
	if err != nil {
		t.Fatal(err)
	}
	// bundle appended after the existing entries
	if len(names) != 3 || names[2] != k8eBundleName {
		t.Fatalf("expected bundle appended, got %v", names)
	}
	// file re-parses and stays valid
	if !bundleRegistered(dshProfileBundles(profilePkg)) {
		t.Fatal("bundle must be registered after fix")
	}

	// idempotent: a second fix is a no-op
	names2, err := fixDshBundles(profilePkg)
	if err != nil {
		t.Fatal(err)
	}
	if len(names2) != 3 {
		t.Fatalf("second fix must not duplicate the bundle, got %v", names2)
	}
}

func TestClientCertDetail_MissingMaterial(t *testing.T) {
	dir := t.TempDir()
	ok, detail := clientCertDetail(dir)
	if ok {
		t.Fatal("expected failure for empty cert dir")
	}
	if !strings.Contains(detail, "missing") {
		t.Fatalf("unexpected detail: %s", detail)
	}
}
