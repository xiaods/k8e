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

	s := newKubeletSettings()
	applyImageCredentialSettings(s, &config.Agent{
		ImageCredProvBinDir: binDir,
		ImageCredProvConfig: configPath,
	})

	if got := s.flags["image-credential-provider-bin-dir"]; got != binDir {
		t.Fatalf("image-credential-provider-bin-dir = %q, want %q", got, binDir)
	}
	if got := s.flags["image-credential-provider-config"]; got != configPath {
		t.Fatalf("image-credential-provider-config = %q, want %q", got, configPath)
	}
	for name := range s.gates {
		if strings.Contains(name, "KubeletCredentialProviders") {
			t.Fatalf("kubelet still requests the removed KubeletCredentialProviders gate: %#v", s.gates)
		}
	}
}

func TestApplyImageCredentialArgsSkipsMissingProvider(t *testing.T) {
	s := newKubeletSettings()
	applyImageCredentialSettings(s, &config.Agent{
		ImageCredProvBinDir: filepath.Join(t.TempDir(), "missing-bin"),
		ImageCredProvConfig: filepath.Join(t.TempDir(), "missing-config.yaml"),
	})
	if len(s.flags) != 0 {
		t.Fatalf("expected no kubelet args when provider files are missing, got %#v", s.flags)
	}
}

// The bug this guards against: every kubelet setting used to be a CLI flag, and
// a flag upstream deletes makes kubelet exit with "unknown flag" before it reads
// any configuration. Settings with a KubeletConfiguration field must therefore
// be rendered into the drop-in file, never onto the command line.
func TestKubeletFlagsOnlyCarrySettingsWithoutAConfigField(t *testing.T) {
	s := newKubeletSettings()
	commonKubeletSettings(s, &config.Agent{})

	migrated := []string{
		"healthz-bind-address",
		"read-only-port",
		"cluster-domain",
		"eviction-hard",
		"eviction-minimum-reclaim",
		"fail-swap-on",
		"authentication-token-webhook",
		"anonymous-auth",
		"authorization-mode",
		"pod-manifest-path",
		"protect-kernel-defaults",
		"cluster-dns",
		"resolv-conf",
		"address",
		"client-ca-file",
		"tls-cert-file",
		"tls-private-key-file",
		"register-with-taints",
	}
	for _, flag := range migrated {
		if _, ok := s.flags[flag]; ok {
			t.Errorf("flag %q has a KubeletConfiguration field and must not be passed on the command line", flag)
		}
	}
}

func TestKubeletConfigDirIsRequired(t *testing.T) {
	if _, err := kubeletConfigDir(&config.Agent{}); err == nil {
		t.Fatal("expected an error when the kubelet configuration directory is unset")
	}
	dir, err := kubeletConfigDir(&config.Agent{KubeletConfigDir: "/tmp/kubelet.conf.d"})
	if err != nil {
		t.Fatal(err)
	}
	if dir != "/tmp/kubelet.conf.d" {
		t.Fatalf("kubeletConfigDir = %q, want /tmp/kubelet.conf.d", dir)
	}
}
