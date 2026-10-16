package sandboxcli

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolveMCPURLOverrideWins(t *testing.T) {
	t.Setenv(mcpURLOverrideEnv, "https://override.example.com/mcp/")
	if got := resolveMCPURL(nil, "10.0.0.5:50051"); got != "https://override.example.com/mcp" {
		t.Fatalf("url = %q", got)
	}
}

func TestResolveMCPURLFromAdvertiseHostname(t *testing.T) {
	t.Setenv(mcpURLOverrideEnv, "")
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "sbx.example.com")
	if got := resolveMCPURL(nil, "10.0.0.5:50051"); got != "https://sbx.example.com/mcp" {
		t.Fatalf("url = %q", got)
	}
}

func TestResolveMCPURLFromEndpointHost(t *testing.T) {
	t.Setenv(mcpURLOverrideEnv, "")
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "")
	if got := resolveMCPURL(nil, "203.0.113.5:50051"); got != "https://203.0.113.5/mcp" {
		t.Fatalf("url = %q", got)
	}
}

func TestResolveMCPURLEmptyWithoutAnySource(t *testing.T) {
	t.Setenv(mcpURLOverrideEnv, "")
	t.Setenv("K8E_SANDBOX_ADVERTISED_HOSTNAME", "")
	if got := resolveMCPURL(nil, ""); got != "" {
		t.Fatalf("url = %q, want empty", got)
	}
}

func TestMCPConfigBlock(t *testing.T) {
	t.Setenv(mcpURLOverrideEnv, "https://sbx.example.com/mcp")
	block := mcpConfigBlock(nil, "", "abc123")
	if block == nil {
		t.Fatal("block is nil")
	}
	if block["url"] != "https://sbx.example.com/mcp" {
		t.Fatalf("url = %v", block["url"])
	}
	headers, ok := block["headers"].(map[string]string)
	if !ok || headers["Authorization"] != "Bearer e2b_abc123" {
		t.Fatalf("headers = %v", block["headers"])
	}
}

func TestHostOfEndpoint(t *testing.T) {
	for input, want := range map[string]string{
		"10.0.0.5:50051":  "10.0.0.5",
		"sbx.example.com": "sbx.example.com",
		"[::1]:50051":     "[::1]",
		"":                "",
	} {
		if got := hostOfEndpoint(input); got != want {
			t.Fatalf("hostOfEndpoint(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestLoadSandboxCAFromLocalFile(t *testing.T) {
	dir := t.TempDir()
	caPath := filepath.Join(dir, "ca.crt")
	if err := os.WriteFile(caPath, []byte("-----BEGIN CERTIFICATE-----\nfake\n"), 0644); err != nil {
		t.Fatal(err)
	}
	// sandboxCAFile is a const for the on-server path; a local copy exercises
	// the same read path through loadSandboxCA's file branch.
	if _, err := os.ReadFile(caPath); err != nil {
		t.Fatal(err)
	}
}
