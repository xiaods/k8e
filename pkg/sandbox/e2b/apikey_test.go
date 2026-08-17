package e2b

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeE2BAPIKey(t *testing.T) {
	tests := []struct {
		in   string
		want string
	}{
		{"", ""},
		{"deadbeef", "deadbeef"},
		{"e2b_deadbeef", "deadbeef"},        // SDK prefix is stripped
		{" e2b_deadbeef ", "deadbeef"},      // surrounding whitespace trimmed
		{"E2B_deadbeef", "E2B_deadbeef"},    // prefix match is exact (lowercase only)
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
