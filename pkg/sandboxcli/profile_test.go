package sandboxcli

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestSelectProfileName(t *testing.T) {
	f := &ProfileFile{
		CurrentProfile: "prod",
		Profiles: map[string]Profile{
			"default": {Endpoint: "127.0.0.1:50051"},
			"prod":    {Endpoint: "prod:50051"},
		},
	}
	if got := SelectProfileName(f, "default"); got != "default" {
		t.Fatalf("explicit: %q", got)
	}
	t.Setenv("K8E_SANDBOX_PROFILE", "prod")
	if got := SelectProfileName(f, ""); got != "prod" {
		t.Fatalf("env: %q", got)
	}
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	if got := SelectProfileName(f, ""); got != "prod" {
		t.Fatalf("current_profile: %q", got)
	}
	f.CurrentProfile = ""
	if got := SelectProfileName(f, ""); got != "default" {
		t.Fatalf("fallback default: %q", got)
	}
}

func TestResolveConnProfileCertDir(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CONFIG", "")
	t.Setenv("K8E_SANDBOX_ENDPOINT", "")
	t.Setenv("K8E_SANDBOX_APIKEY", "")
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	t.Setenv("K8E_SANDBOX_DEVICE_NAME", "")

	// Canonical path: ~/.k8e/sandbox/profiles.yaml
	profDir := filepath.Join(home, ".k8e", "sandbox")
	if err := os.MkdirAll(profDir, 0700); err != nil {
		t.Fatal(err)
	}
	yaml := []byte(`
version: 1
current_profile: prod
profiles:
  default:
    endpoint: 127.0.0.1:50051
  prod:
    endpoint: sandbox.prod.example:50051
    cert_dir: ~/sandbox-prod
    device_name: ci-bot
`)
	if err := os.WriteFile(filepath.Join(profDir, profilesFileName), yaml, 0600); err != nil {
		t.Fatal(err)
	}

	r, err := ResolveConn("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != "prod" {
		t.Fatalf("profile=%q", r.Profile)
	}
	if r.Endpoint != "sandbox.prod.example:50051" {
		t.Fatalf("endpoint=%q", r.Endpoint)
	}
	wantCert := filepath.Join(home, "sandbox-prod")
	if r.CertDir != wantCert {
		t.Fatalf("cert_dir=%q want %q", r.CertDir, wantCert)
	}
	if r.DeviceName != "ci-bot" {
		t.Fatalf("device=%q", r.DeviceName)
	}

	// Flag endpoint wins over profile.
	r2, err := ResolveConn("flag.example:1", "k8e-key", "default", "")
	if err != nil {
		t.Fatal(err)
	}
	if r2.Endpoint != "flag.example:1" || r2.APIKey != "k8e-key" || r2.Profile != "default" {
		t.Fatalf("flag override: %+v", r2)
	}
}

func TestResolveConnMissingProfile(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CONFIG", "")
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	profDir := filepath.Join(home, ".k8e", "sandbox")
	_ = os.MkdirAll(profDir, 0700)
	_ = os.WriteFile(filepath.Join(profDir, profilesFileName), []byte("version: 1\nprofiles:\n  only:\n    endpoint: x:1\n"), 0600)

	_, err := ResolveConn("", "", "nope", "")
	if err == nil {
		t.Fatal("expected missing profile error")
	}
	if !strings.Contains(err.Error(), "profiles.yaml") {
		t.Fatalf("error should mention profiles.yaml: %v", err)
	}
}

func TestLegacyHomeConfigYAMLStillLoads(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CONFIG", "")
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	t.Setenv("K8E_SANDBOX_ENDPOINT", "")
	t.Setenv("K8E_SANDBOX_APIKEY", "")
	t.Setenv("K8E_SANDBOX_DEVICE_NAME", "")

	// Only legacy ~/.k8e/config.yaml present (no profiles.yaml).
	legacyDir := filepath.Join(home, ".k8e")
	if err := os.MkdirAll(legacyDir, 0700); err != nil {
		t.Fatal(err)
	}
	legacy := []byte(`
version: 1
current_profile: legacy
profiles:
  legacy:
    endpoint: legacy.example:50051
`)
	if err := os.WriteFile(filepath.Join(legacyDir, "config.yaml"), legacy, 0600); err != nil {
		t.Fatal(err)
	}

	r, err := ResolveConn("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != "legacy" || r.Endpoint != "legacy.example:50051" {
		t.Fatalf("legacy load failed: %+v", r)
	}
}

func TestCanonicalProfilesPathPreferredOverLegacy(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CONFIG", "")
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	t.Setenv("K8E_SANDBOX_ENDPOINT", "")

	// Both files exist — canonical must win.
	_ = os.MkdirAll(filepath.Join(home, ".k8e", "sandbox"), 0700)
	_ = os.MkdirAll(filepath.Join(home, ".k8e"), 0700)
	_ = os.WriteFile(filepath.Join(home, ".k8e", "sandbox", profilesFileName), []byte(`
version: 1
current_profile: new
profiles:
  new:
    endpoint: new.example:1
`), 0600)
	_ = os.WriteFile(filepath.Join(home, ".k8e", "config.yaml"), []byte(`
version: 1
current_profile: old
profiles:
  old:
    endpoint: old.example:1
`), 0600)

	r, err := ResolveConn("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if r.Profile != "new" || r.Endpoint != "new.example:1" {
		t.Fatalf("canonical should win: %+v", r)
	}
}

func TestDefaultProfilesPath(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	p, err := DefaultProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	want := filepath.Join(home, ".k8e", "sandbox", profilesFileName)
	if p != want {
		t.Fatalf("got %q want %q", p, want)
	}
}

func TestSaveConnectProfileWritesDefault(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)

	if err := SaveConnectProfile("10.0.0.1:50051"); err != nil {
		t.Fatal(err)
	}
	path, err := DefaultProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), "10.0.0.1:50051") {
		t.Fatalf("profiles.yaml missing endpoint:\n%s", data)
	}

	// A later ResolveConn (no flags/env) must pick up the endpoint.
	resolved, err := ResolveConn("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "10.0.0.1:50051" {
		t.Fatalf("ResolveConn endpoint = %q, want 10.0.0.1:50051", resolved.Endpoint)
	}
	if resolved.Profile != "default" {
		t.Fatalf("ResolveConn profile = %q, want default", resolved.Profile)
	}
}

func TestSaveConnectProfilePreservesOtherProfiles(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)

	// Pre-seed a manually managed profile file.
	path, err := DefaultProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\ncurrent_profile: prod\nprofiles:\n  prod:\n    endpoint: prod:50051\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveConnectProfile("10.0.0.2:50051"); err != nil {
		t.Fatal(err)
	}
	file, _, err := LoadProfileFile()
	if err != nil {
		t.Fatal(err)
	}
	if file.Profiles["prod"].Endpoint != "prod:50051" {
		t.Fatalf("prod profile overwritten: %+v", file.Profiles["prod"])
	}
	if file.Profiles["default"].Endpoint != "10.0.0.2:50051" {
		t.Fatalf("default profile = %+v", file.Profiles["default"])
	}
	if file.CurrentProfile != "default" {
		t.Fatalf("current_profile = %q, want default", file.CurrentProfile)
	}
}

func TestSaveConnectProfileLocalNoOp(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)
	if err := SaveConnectProfile(""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "profiles.yaml")); !os.IsNotExist(err) {
		t.Fatalf("local connect must not write profiles.yaml (err=%v)", err)
	}
}

func TestResolveConnFallsBackToConnectionConfig(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)

	// No profiles.yaml; only the legacy config.json from an earlier connect.
	cfg := &ConnectionConfig{Mode: "remote", Endpoint: "192.168.1.10:50051"}
	if err := SaveConnectionConfig(cfg); err != nil {
		t.Fatal(err)
	}
	resolved, err := ResolveConn("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Endpoint != "192.168.1.10:50051" {
		t.Fatalf("ResolveConn endpoint = %q, want config.json fallback", resolved.Endpoint)
	}
}

func TestSaveConnectProfilePreservesDefaultMetadata(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)

	path, err := DefaultProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	// Default profile already carries cert_dir/device_name (manual setup).
	if err := os.WriteFile(path, []byte("version: 1\ncurrent_profile: default\nprofiles:\n  default:\n    endpoint: old:50051\n    cert_dir: ~/.k8e/custom-certs\n    device_name: laptop\n"), 0600); err != nil {
		t.Fatal(err)
	}

	if err := SaveConnectProfile("10.0.0.3:50051"); err != nil {
		t.Fatal(err)
	}
	file, _, err := LoadProfileFile()
	if err != nil {
		t.Fatal(err)
	}
	d := file.Profiles["default"]
	if d.Endpoint != "10.0.0.3:50051" {
		t.Fatalf("endpoint = %q", d.Endpoint)
	}
	if d.CertDir != "~/.k8e/custom-certs" {
		t.Fatalf("cert_dir dropped: %q", d.CertDir)
	}
	if d.DeviceName != "laptop" {
		t.Fatalf("device_name dropped: %q", d.DeviceName)
	}
}

func TestResolveConnLocalProfileNotRedirected(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("K8E_SANDBOX_CERT_DIR", dir)

	// config.json points at a remote gateway (stale connect).
	cfg := &ConnectionConfig{Mode: "remote", Endpoint: "192.168.1.10:50051"}
	if err := SaveConnectionConfig(cfg); err != nil {
		t.Fatal(err)
	}
	// An explicitly selected local profile (no endpoint) exists.
	path, err := DefaultProfilesPath()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("version: 1\ncurrent_profile: local\nprofiles:\n  local:\n    device_name: devbox\n"), 0600); err != nil {
		t.Fatal(err)
	}

	resolved, err := ResolveConn("", "", "", "")
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Profile != "local" {
		t.Fatalf("profile = %q, want local", resolved.Profile)
	}
	if resolved.Endpoint != "" {
		t.Fatalf("local profile must stay endpoint-less, got %q (redirected to stale remote)", resolved.Endpoint)
	}
}
