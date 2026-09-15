package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

func TestApplyImageCredentialArgsOmitsRemovedFeatureGate(t *testing.T) {
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.Mkdir(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(configPath, []byte("kind: CredentialProviderConfig\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	argsMap := map[string]string{}
	applyImageCredentialArgs(argsMap, &config.Agent{
		ImageCredProvBinDir: binDir,
		ImageCredProvConfig: configPath,
	})

	if got := argsMap["image-credential-provider-bin-dir"]; got != binDir {
		t.Fatalf("image-credential-provider-bin-dir = %q, want %q", got, binDir)
	}
	if got := argsMap["image-credential-provider-config"]; got != configPath {
		t.Fatalf("image-credential-provider-config = %q, want %q", got, configPath)
	}
	if fg := argsMap["feature-gates"]; strings.Contains(fg, "KubeletCredentialProviders") {
		t.Fatalf("feature-gates %q still sets removed KubeletCredentialProviders gate", fg)
	}
}

func TestApplyImageCredentialArgsSkipsMissingProvider(t *testing.T) {
	argsMap := map[string]string{}
	applyImageCredentialArgs(argsMap, &config.Agent{
		ImageCredProvBinDir: filepath.Join(t.TempDir(), "missing-bin"),
		ImageCredProvConfig: filepath.Join(t.TempDir(), "missing-config.yaml"),
	})
	if len(argsMap) != 0 {
		t.Fatalf("expected no kubelet args when provider files are missing, got %#v", argsMap)
	}
}
