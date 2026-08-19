package server

import (
	"testing"

	"github.com/xiaods/k8e/pkg/daemons/config"
)

// TestSandboxConfigCarriesDisableE2B pins the KIP-18 final architecture
// wiring: DisableE2B (default false = embedded e2b ON) + E2BListen live on
// SandboxConfig, which pkg/server reads when starting the embedded e2b
// surface. The Gateway API fronts both :50051 and :3676; the e2b listen
// defaults to 0.0.0.0 so the cluster's headless e2b-server Service/Endpoints
// can reach it.
func TestSandboxConfigCarriesDisableE2B(t *testing.T) {
	cfg := config.SandboxConfig{DisableE2B: false, E2BListen: "0.0.0.0:3676", GRPCPort: 50051}
	if cfg.DisableE2B {
		t.Fatal("DisableE2B should default false (embedded e2b on)")
	}
	if cfg.E2BListen == "" {
		t.Fatal("E2BListen empty")
	}
}

// TestRunEmbeddedE2BListenDefault verifies the default listen address is the
// cluster-reachable 0.0.0.0 form (loopback would make the headless Service
// unreachable from the Gateway).
func TestRunEmbeddedE2BListenDefault(t *testing.T) {
	cfg := config.SandboxConfig{}
	if cfg.E2BListen == "" {
		cfg.E2BListen = "0.0.0.0:3676"
	}
	if cfg.E2BListen != "0.0.0.0:3676" {
		t.Fatalf("default E2BListen = %q, want 0.0.0.0:3676 (cluster-reachable)", cfg.E2BListen)
	}
}

func TestResolveEmbeddedAPIKey(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	t.Run("configured wins", func(t *testing.T) {
		t.Setenv("K8E_SANDBOX_APIKEY", "from-env")
		if got := resolveEmbeddedAPIKey(hex); got != hex {
			t.Fatalf("got %q want configured", got)
		}
	})
	t.Run("falls back to K8E_SANDBOX_APIKEY", func(t *testing.T) {
		t.Setenv("K8E_SANDBOX_APIKEY", hex)
		if got := resolveEmbeddedAPIKey(""); got != hex {
			t.Fatalf("got %q want env", got)
		}
	})
	t.Run("trims whitespace", func(t *testing.T) {
		t.Setenv("K8E_SANDBOX_APIKEY", "")
		if got := resolveEmbeddedAPIKey("  " + hex + "  "); got != hex {
			t.Fatalf("got %q", got)
		}
	})
	t.Run("all empty", func(t *testing.T) {
		t.Setenv("K8E_SANDBOX_APIKEY", "")
		if got := resolveEmbeddedAPIKey(""); got != "" {
			t.Fatalf("got %q want empty", got)
		}
	})
}
