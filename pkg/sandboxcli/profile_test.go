package sandboxcli

import (
	"os"
	"path/filepath"
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
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("K8E_SANDBOX_ENDPOINT", "")
	t.Setenv("K8E_SANDBOX_APIKEY", "")
	t.Setenv("K8E_SANDBOX_CERT_DIR", "")
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	t.Setenv("K8E_SANDBOX_DEVICE_NAME", "")

	cfgDir := filepath.Join(home, ".k8e")
	if err := os.MkdirAll(cfgDir, 0700); err != nil {
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
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), yaml, 0600); err != nil {
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
	t.Setenv("XDG_CONFIG_HOME", "")
	t.Setenv("K8E_SANDBOX_PROFILE", "")
	cfgDir := filepath.Join(home, ".k8e")
	_ = os.MkdirAll(cfgDir, 0700)
	_ = os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte("version: 1\nprofiles:\n  only:\n    endpoint: x:1\n"), 0600)

	_, err := ResolveConn("", "", "nope", "")
	if err == nil {
		t.Fatal("expected missing profile error")
	}
}
