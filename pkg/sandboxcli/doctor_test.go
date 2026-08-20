package sandboxcli

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// writeDshProfile creates a temp dsh profile whose package.json registers the
// given bundles (dsh.profile.bundles), pointing HOME/DSH_HOME at the temp dir.
// Returns the profile package.json path.
func writeDshProfile(t *testing.T, bundles []string) string {
	t.Helper()
	tmp := t.TempDir()
	t.Setenv("HOME", tmp)
	t.Setenv("USERPROFILE", tmp)
	t.Setenv("DSH_HOME", filepath.Join(tmp, ".dsh"))

	profilesDir := filepath.Join(tmp, ".dsh", "profiles", "web")
	if err := os.MkdirAll(profilesDir, 0755); err != nil {
		t.Fatal(err)
	}
	profilePkg := filepath.Join(profilesDir, "package.json")
	body := map[string]any{
		"dependencies": map[string]string{"@k8e-sandbox/dsh-k8e-sandbox-bundle": "^0.3.0"},
		"dsh": map[string]any{
			"profile": map[string]any{"bundles": bundles},
		},
	}
	data, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(profilePkg, data, 0644); err != nil {
		t.Fatal(err)
	}
	return profilePkg
}

// writeDshBundle simulates the installed bundle inside the profile's node_modules.
func writeDshBundle(t *testing.T, profilePkg string) {
	t.Helper()
	nm := filepath.Join(filepath.Dir(profilePkg), "node_modules", "@k8e-sandbox", "dsh-k8e-sandbox-bundle")
	if err := os.MkdirAll(nm, 0755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(nm, "package.json"),
		[]byte(`{"name":"@k8e-sandbox/dsh-k8e-sandbox-bundle","version":"0.3.0"}`), 0644)
}

func TestDoctor_DetectsMissingDshBundle(t *testing.T) {
	// A dsh profile whose dependencies contain the bundle but whose
	// dsh.profile.bundles does NOT — the exact `dsh plugin add` pnpm gap.
	profilePkg := writeDshProfile(t, []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"})
	writeDshBundle(t, profilePkg)

	if got := dshProfileBundles(profilePkg); contains(got, k8eBundleName) {
		t.Fatal("fixture must start with the bundle NOT registered")
	}
	for _, p := range dshProfilePaths() {
		if v := dshBundleInstalledVersion(p); v != "0.3.0" {
			t.Fatalf("installed version: got %q, want 0.3.0", v)
		}
	}
}

func TestDoctor_DetectsRegisteredBundle(t *testing.T) {
	profilePkg := writeDshProfile(t, []string{"@deepseek-ai/dsh-base", k8eBundleName})
	if !contains(dshProfileBundles(profilePkg), k8eBundleName) {
		t.Fatal("bundle must be detected as registered")
	}
}

func TestFixDshBundles_AppendsAndPreservesExisting(t *testing.T) {
	profilePkg := writeDshProfile(t, []string{"@deepseek-ai/dsh-base", "@deepseek-ai/dsh-web-app"})

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

func contains(items []string, want string) bool {
	for _, s := range items {
		if strings.HasPrefix(s, want) {
			return true
		}
	}
	return false
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
