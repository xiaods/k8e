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

// TestE2BAPIKeyCacheFailedRefreshDropsSecretKeys is the Greptile finding:
// a deleted unexpired Secret token must not stay authorized while later
// Secret reads fail. Failed refresh fail-closes Secret-backed keys and
// keeps only the static token.
func TestE2BAPIKeyCacheFailedRefreshDropsSecretKeys(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	t0 := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	set := mustParseSecretKeys(t, map[string]apikey.Record{
		"live": {Key: hex, CreatedAt: t0}, // never-expire — revocation, not TTL
	})

	cache := &e2bAPIKeyCache{static: "static"}
	keys, apply, dropped := cache.refresh(true, set, t0)
	if !apply || dropped {
		t.Fatalf("startup snapshot: apply=%v dropped=%v", apply, dropped)
	}
	if !containsKey(keys, hex) || !containsKey(keys, "static") {
		t.Fatalf("boot keyring; got %v", keys)
	}
	if !cache.haveCache {
		t.Fatal("startup snapshot must seed haveCache so the first failed refresh can fail-close")
	}

	keys, apply, dropped = cache.refresh(false, sandboxe2b.SecretKeySet{}, t0.Add(time.Second))
	if !apply {
		t.Fatal("failed refresh after a snapshot must apply (fail-close), not skip")
	}
	if !dropped {
		t.Fatal("failed refresh must report dropped Secret keys")
	}
	if containsKey(keys, hex) {
		t.Errorf("revoked Secret token must not survive a failed read; got %v", keys)
	}
	if !containsKey(keys, "static") {
		t.Errorf("static --e2b-apikey must remain; got %v", keys)
	}
	if cache.haveCache {
		t.Fatal("fail-close must clear haveCache so later failed ticks leave the static-only keyring")
	}

	keys, apply, dropped = cache.refresh(false, sandboxe2b.SecretKeySet{}, t0.Add(2*time.Second))
	if apply || dropped {
		t.Fatalf("second failed refresh: want apply=false dropped=false, got keys=%v apply=%v dropped=%v", keys, apply, dropped)
	}
}

func TestE2BAPIKeyCacheFailedRefreshThenRecovers(t *testing.T) {
	t0 := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	first := mustParseSecretKeys(t, map[string]apikey.Record{
		"old": {Key: "deadbeef", CreatedAt: t0},
	})
	second := mustParseSecretKeys(t, map[string]apikey.Record{
		"new": {Key: "cafebabe", CreatedAt: t0},
	})
	cache := &e2bAPIKeyCache{static: "static"}
	cache.refresh(true, first, t0)
	cache.refresh(false, sandboxe2b.SecretKeySet{}, t0.Add(time.Second))
	keys, apply, dropped := cache.refresh(true, second, t0.Add(2*time.Second))
	if !apply || dropped {
		t.Fatalf("recovering read: apply=%v dropped=%v", apply, dropped)
	}
	if containsKey(keys, "deadbeef") {
		t.Errorf("recovered snapshot must not restore the revoked key; got %v", keys)
	}
	if !containsKey(keys, "cafebabe") || !containsKey(keys, "static") {
		t.Errorf("new snapshot + static; got %v", keys)
	}
}

func TestE2BAPIKeyCacheNoSnapshotKeepsCurrentKeyring(t *testing.T) {
	cache := &e2bAPIKeyCache{static: "static-hex"}
	keys, apply, dropped := cache.refresh(false, sandboxe2b.SecretKeySet{}, time.Now())
	if apply || dropped {
		t.Fatalf("no snapshot: want apply=false so NewServer static keyring is kept, got keys=%v apply=%v dropped=%v", keys, apply, dropped)
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
	keys, _, _ := cache.refresh(true, first, t0)
	if !containsKey(keys, "deadbeef") || !containsKey(keys, "static") {
		t.Fatalf("first snapshot: got %v", keys)
	}
	keys, apply, dropped := cache.refresh(true, second, t0)
	if !apply || dropped {
		t.Fatalf("successful reload must apply without dropping; apply=%v dropped=%v", apply, dropped)
	}
	if containsKey(keys, "deadbeef") {
		t.Errorf("replaced snapshot must drop revoked key; got %v", keys)
	}
	if !containsKey(keys, "cafebabe") || !containsKey(keys, "static") {
		t.Errorf("new snapshot + static; got %v", keys)
	}
}
