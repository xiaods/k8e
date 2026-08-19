package e2b

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/xiaods/k8e/pkg/sandbox/apikey"
)

func TestNormalizeE2BAPIKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"deadbeef", "deadbeef"},
		{"e2b_deadbeef", "deadbeef"},         // SDK prefix is stripped
		{" e2b_deadbeef ", "deadbeef"},       // surrounding whitespace trimmed
		{"E2B_deadbeef", "E2B_deadbeef"},     // prefix match is exact (lowercase only)
		{"e2b_e2b_deadbeef", "e2b_deadbeef"}, // only the first prefix is stripped
	}
	for _, tt := range tests {
		if got := NormalizeE2BAPIKey(tt.in); got != tt.want {
			t.Errorf("NormalizeE2BAPIKey(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestValidateE2BAPIKey(t *testing.T) {
	valid := []string{
		"",
		"849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9",
		"e2b_849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9",
		"deadbeef",
		"0",
	}
	for _, key := range valid {
		if err := ValidateE2BAPIKey(key); err != nil {
			t.Errorf("ValidateE2BAPIKey(%q) = %v, want nil", key, err)
		}
	}

	invalid := []string{
		"k8e-849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9",
		"e2b_k8e-849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9",
		"my-api-key",
		"test-key",
		"dead beef",
	}
	for _, key := range invalid {
		if err := ValidateE2BAPIKey(key); err == nil {
			t.Errorf("ValidateE2BAPIKey(%q) = nil, want error", key)
		} else if !strings.Contains(err.Error(), "openssl rand -hex 32") {
			t.Errorf("ValidateE2BAPIKey(%q) error should hint at key generation: %v", key, err)
		}
	}
}

// TestServerAPIKeyPrefixNormalized verifies the configured key is normalized:
// a key configured with the SDK's "e2b_" prefix must authenticate the same
// bare token the SDK presents (previously the prefixed config never matched,
// so every request got 401).
func TestServerAPIKeyPrefixNormalized(t *testing.T) {
	for _, configured := range []string{"e2b_deadbeef", "deadbeef", " e2b_deadbeef "} {
		_, ts := newServerWithKey(t, configured)
		for _, presented := range []string{"e2b_deadbeef", "deadbeef"} {
			req, _ := http.NewRequest("POST", ts.URL+"/e2b/api/sandboxes", bytes.NewReader([]byte("{}")))
			req.Header.Set("X-API-KEY", presented)
			resp, err := ts.Client().Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != 201 {
				t.Errorf("configured=%q presented=%q: want 201, got %d", configured, presented, resp.StatusCode)
			}
		}
	}
}

// TestServerAPIKeyMismatch verifies a wrong key still fails closed.
func TestServerAPIKeyMismatch(t *testing.T) {
	_, ts := newServerWithKey(t, "deadbeef")
	req, _ := http.NewRequest("POST", ts.URL+"/e2b/api/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-API-KEY", "e2b_cafebabe")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong key: want 401, got %d", resp.StatusCode)
	}
}

func newServerWithKey(t *testing.T, apiKey string) (*Server, *httptest.Server) {
	t.Helper()
	s := NewServer(Config{
		Listen:        "127.0.0.1:0",
		APIKey:        apiKey,
		NodeID:        "node-test",
		SigningSecret: "signing-secret",
	}, newFakeGateway())
	ts := httptest.NewServer(s.Handle())
	t.Cleanup(ts.Close)
	return s, ts
}

func TestSDKAPIKey(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	if got := SDKAPIKey(hex); got != "e2b_"+hex {
		t.Errorf("SDKAPIKey(%q) = %q", hex, got)
	}
	if got := SDKAPIKey("e2b_" + hex); got != "e2b_"+hex {
		t.Errorf("SDKAPIKey prefixed = %q", got)
	}
	if got := SDKAPIKey("  e2b_" + hex + "  "); got != "e2b_"+hex {
		t.Errorf("SDKAPIKey whitespace = %q", got)
	}
	if got := SDKAPIKey(""); got != "" {
		t.Errorf("SDKAPIKey empty = %q", got)
	}
}

func TestParseSecretKeysActive(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	now := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	expired := now.Add(-time.Hour)
	live := apikey.NewRecord(hex, 30, false, now)
	dead := apikey.Record{Key: "cafebabe", ExpiresAt: &expired}
	data, err := apikey.Encode(map[string]apikey.Record{
		"e2b":     live,
		"expired": dead,
		"legacy":  {Key: "e2b_deadbeef"},
	})
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseSecretKeys(data)
	if err != nil {
		t.Fatal(err)
	}
	keys := set.Active(now)
	got := map[string]bool{}
	for _, k := range keys {
		got[k] = true
	}
	if !got[hex] {
		t.Errorf("missing live hex key; keys=%v", keys)
	}
	if !got["deadbeef"] {
		t.Errorf("prefixed secret token should be stripped; keys=%v", keys)
	}
	if got["cafebabe"] {
		t.Errorf("expired key should be dropped; keys=%v", keys)
	}

	legacySet, err := ParseSecretKeys([]byte(`{"agent":"` + hex + `"}`))
	if err != nil {
		t.Fatal(err)
	}
	legacy := legacySet.Active(now)
	if len(legacy) != 1 || legacy[0] != hex {
		t.Errorf("legacy flat map: got %v", legacy)
	}
}

// TestSecretKeySetExpiryReevaluatedOnFailedRefresh is the Greptile concern:
// when a later Secret read/parse fails, the cached SecretKeySet must still
// drop keys that expired since it was parsed — the keyring must not keep an
// expired credential authenticated just because the Secret became unreadable.
func TestSecretKeySetExpiryReevaluatedOnFailedRefresh(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	t0 := time.Date(2026, 8, 19, 12, 0, 0, 0, time.UTC)
	shortExp := t0.Add(10 * time.Minute)   // key "short" dies 10 min after parse
	longExp := t0.Add(30 * 24 * time.Hour) // key "long" stays alive
	data, err := apikey.Encode(map[string]apikey.Record{
		"short": {Key: "deadbeef", CreatedAt: t0, ExpiresAt: &shortExp, TTLDays: 30},
		"long":  {Key: hex, CreatedAt: t0, ExpiresAt: &longExp, TTLDays: 30},
	})
	if err != nil {
		t.Fatal(err)
	}
	set, err := ParseSecretKeys(data)
	if err != nil {
		t.Fatal(err)
	}

	activeBefore := set.Active(t0)
	if !containsStr(activeBefore, "deadbeef") || !containsStr(activeBefore, hex) {
		t.Fatalf("both keys active at parse; got %v", activeBefore)
	}

	// Simulate a failed refresh long after the short key expired: Active(now)
	// must drop it even though no new Secret read happened.
	later := shortExp.Add(time.Hour)
	activeAfter := set.Active(later)
	if containsStr(activeAfter, "deadbeef") {
		t.Errorf("expired key must be re-filtered on a failed refresh; got %v", activeAfter)
	}
	if !containsStr(activeAfter, hex) {
		t.Errorf("long-lived key must survive re-filtering; got %v", activeAfter)
	}
}

func containsStr(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}
	return false
}

// TestServerSecretLoadedKeyAuthenticatesSDKHeader is the production failure
// mode: --e2b-apikey is unset (embedded default) but sandbox-apikeys holds a
// hex token. Official JS SDK sends X-API-KEY: e2b_<hex> and used to get 401.
func TestServerSecretLoadedKeyAuthenticatesSDKHeader(t *testing.T) {
	hex := "849a5302e66f98d1d5064ef8501703574af4053b7bf9cf5337f9533326ce2bc9"
	s, ts := newServerWithKey(t, "") // empty static key, as in default embed
	req, _ := http.NewRequest("POST", ts.URL+"/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-API-KEY", SDKAPIKey(hex)) // exact official SDK header
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty keyring: want 401, got %d", resp.StatusCode)
	}

	s.ReplaceAPIKeys([]string{hex})
	for _, presented := range []string{SDKAPIKey(hex), hex} {
		req, _ := http.NewRequest("POST", ts.URL+"/sandboxes", bytes.NewReader([]byte("{}")))
		req.Header.Set("X-API-KEY", presented)
		resp, err := ts.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != 201 {
			t.Errorf("presented=%q: want 201 after Secret load, got %d", presented, resp.StatusCode)
		}
	}

	req, _ = http.NewRequest("POST", ts.URL+"/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("Authorization", "Bearer "+SDKAPIKey(hex))
	resp, err = ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 201 {
		t.Errorf("Bearer e2b_<hex>: want 201, got %d", resp.StatusCode)
	}
}

func TestReplaceAPIKeysEmptyRejects(t *testing.T) {
	s, ts := newServerWithKey(t, "deadbeef")
	s.ReplaceAPIKeys(nil)
	req, _ := http.NewRequest("POST", ts.URL+"/sandboxes", bytes.NewReader([]byte("{}")))
	req.Header.Set("X-API-KEY", "e2b_deadbeef")
	resp, err := ts.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("empty replace: want 401, got %d", resp.StatusCode)
	}
}
