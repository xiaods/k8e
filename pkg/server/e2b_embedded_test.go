package server

import (
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/daemons/config"
	"github.com/xiaods/k8e/pkg/sandbox/apikey"
	sandboxe2b "github.com/xiaods/k8e/pkg/sandbox/e2b"
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

func containsKey(keys []string, want string) bool {
	for _, k := range keys {
		if k == want {
			return true
		}
	}
	return false
}

func mustParseSecretKeys(t *testing.T, records map[string]apikey.Record) sandboxe2b.SecretKeySet {
	t.Helper()
	data, err := apikey.Encode(records)
	if err != nil {
		t.Fatal(err)
	}
	set, err := sandboxe2b.ParseSecretKeys(data)
	if err != nil {
		t.Fatal(err)
	}
	return set
}

// TestE2BAPIKeyCacheStartupSnapshotUsedOnFailedRefresh is the Greptile
// finding: reload must inherit the startup Secret snapshot. A failed first
// refresh after boot still re-filters expiry, so a key that expired after
// applyE2BAPIKeys cannot stay authorized until a later successful read.
func TestE2BAPIKeyCacheStartupSnapshotUsedOnFailedRefresh(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	t0 := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	shortExp := t0.Add(10 * time.Minute)
	longExp := t0.Add(30 * 24 * time.Hour)
	set := mustParseSecretKeys(t, map[string]apikey.Record{
		"short": {Key: "deadbeef", CreatedAt: t0, ExpiresAt: &shortExp, TTLDays: 1},
		"long":  {Key: hex, CreatedAt: t0, ExpiresAt: &longExp, TTLDays: 30},
	})

	cache := &e2bAPIKeyCache{}
	keys, apply := cache.refresh(true, set, t0) // startup Secret read succeeds
	if !apply {
		t.Fatal("startup snapshot must install")
	}
	if !containsKey(keys, "deadbeef") || !containsKey(keys, hex) {
		t.Fatalf("both keys live at boot; got %v", keys)
	}
	if !cache.haveCache {
		t.Fatal("startup snapshot must seed haveCache for the reload loop")
	}

	later := shortExp.Add(time.Hour)
	keys, apply = cache.refresh(false, sandboxe2b.SecretKeySet{}, later) // first reload fails
	if !apply {
		t.Fatal("failed first refresh must still apply using the startup snapshot")
	}
	if containsKey(keys, "deadbeef") {
		t.Errorf("expired boot-time key must be dropped on failed refresh; got %v", keys)
	}
	if !containsKey(keys, hex) {
		t.Errorf("unexpired boot-time key must survive; got %v", keys)
	}
}

func TestE2BAPIKeyCacheNoSnapshotKeepsCurrentKeyring(t *testing.T) {
	cache := &e2bAPIKeyCache{static: "static-hex"}
	keys, apply := cache.refresh(false, sandboxe2b.SecretKeySet{}, time.Now())
	if apply {
		t.Fatalf("no snapshot: want apply=false so NewServer static keyring is kept, got keys=%v", keys)
	}
}

func TestE2BAPIKeyCacheSuccessfulReloadReplacesSnapshot(t *testing.T) {
	t0 := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	first := mustParseSecretKeys(t, map[string]apikey.Record{
		"old": {Key: "deadbeef", CreatedAt: t0},
	})
	second := mustParseSecretKeys(t, map[string]apikey.Record{
		"new": {Key: "cafebabe", CreatedAt: t0},
	})
	cache := &e2bAPIKeyCache{static: "static"}
	keys, _ := cache.refresh(true, first, t0)
	if !containsKey(keys, "deadbeef") || !containsKey(keys, "static") {
		t.Fatalf("first snapshot: got %v", keys)
	}
	keys, apply := cache.refresh(true, second, t0)
	if !apply {
		t.Fatal("successful reload must apply")
	}
	if containsKey(keys, "deadbeef") {
		t.Errorf("replaced snapshot must drop revoked key; got %v", keys)
	}
	if !containsKey(keys, "cafebabe") || !containsKey(keys, "static") {
		t.Errorf("new snapshot + static; got %v", keys)
	}
}
