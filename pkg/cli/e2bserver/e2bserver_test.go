package e2bserver

import (
	"testing"
)

// TestResolveGatewayLoginKey covers the Greptile-raised gateway login bug:
// the sandbox-apikeys Secret stores the bare token, so the login credential
// must be the normalized key — a configured "e2b_<hex>" must reach the
// gateway as "<hex>", and a legacy "k8e-…" key must be passed through
// unchanged (the Secret accepts it).
func TestResolveGatewayLoginKey(t *testing.T) {
	hex := "a5e1bd21c99d3cfa44e17ba9825b6fad1bc7f1a7f26c0f48eba10f40d1692b05"

	tests := []struct {
		name       string
		configured string
		env        string
		want       string
	}{
		{"bare hex passed through", hex, "", hex},
		{"e2b_ prefix stripped", "e2b_" + hex, "", hex},
		{"legacy k8e- key passed through", "k8e-" + hex, "", "k8e-" + hex},
		{"empty flag falls back to env", "", hex, hex},
		{"empty flag + prefixed env", "", "e2b_" + hex, hex},
		{"whitespace trimmed", "  " + hex + "  ", "", hex},
		{"all empty", "", "", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("K8E_SANDBOX_APIKEY", tt.env)
			if got := resolveGatewayLoginKey(tt.configured); got != tt.want {
				t.Errorf("resolveGatewayLoginKey(%q) = %q, want %q", tt.configured, got, tt.want)
			}
		})
	}
}
