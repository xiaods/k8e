// KIP-30 M3.4: the copy-pasteable MCP config block.
//
// An API key is only half of an MCP client's configuration; the other half is
// the URL and the Authorization header wiring, which used to live only in
// documentation. `apikey create` / `apikey list` now emit a ready config block
// so an operator can paste it straight into a client (Claude Code, Codex, …):
//
//	{"url": "https://sbx.example.com/mcp", "headers": {"Authorization": "Bearer e2b_<key>"}}
//
// The URL is resolved from, in order:
//
//  1. K8E_MCP_URL — explicit operator override (also accepts http:// for
//     pre-M3.2 installs without the sandbox-e2b Secret).
//  2. The sandbox advertise hostname (KIP-22, --sandbox-advertise-hostname /
//     K8E_SANDBOX_ADVERTISED_HOSTNAME) when set.
//  3. The host part of the active profile endpoint (KIP-17).
//
// When none is available the block is omitted rather than guessed: a wrong URL
// in a copy-paste block is worse than no block.
package sandboxcli

import (
	"os"
	"strings"

	"github.com/urfave/cli"
)

// mcpURLOverrideEnv is the explicit MCP endpoint override (also the place a
// plain-HTTP install points at http://host/mcp before M3.2's mirror exists).
const mcpURLOverrideEnv = "K8E_MCP_URL"

// resolveMCPURL computes the MCP endpoint URL, or "" when it cannot be
// determined. scheme follows the override when given, else https (M3.2's TLS
// mirror makes https the default on any install with a routable address).
func resolveMCPURL(ctx *cli.Context, endpoint string) string {
	if override := strings.TrimSpace(getenvTrimmed(mcpURLOverrideEnv)); override != "" {
		return strings.TrimRight(override, "/")
	}
	host := ""
	if advertise := strings.TrimSpace(getenvTrimmed("K8E_SANDBOX_ADVERTISED_HOSTNAME")); advertise != "" {
		host = advertise
	} else if endpoint != "" {
		host = hostOfEndpoint(endpoint)
	}
	if host == "" {
		return ""
	}
	return "https://" + host + "/mcp"
}

// hostOfEndpoint strips the port from host:port, keeping IPv6 brackets.
func hostOfEndpoint(endpoint string) string {
	endpoint = strings.TrimSpace(endpoint)
	if endpoint == "" {
		return ""
	}
	if i := strings.LastIndex(endpoint, ":"); i > 0 && !strings.Contains(endpoint[i:], "]") {
		return endpoint[:i]
	}
	return endpoint
}

// mcpConfigBlock returns the JSON-serializable MCP client config for key, or
// nil when the URL is unknown.
func mcpConfigBlock(ctx *cli.Context, endpoint, key string) map[string]any {
	url := resolveMCPURL(ctx, endpoint)
	if url == "" {
		return nil
	}
	return map[string]any{
		"url": url,
		"headers": map[string]string{
			"Authorization": "Bearer e2b_" + key,
		},
	}
}

// getenvTrimmed wraps os.Getenv trimming whitespace (kept separate so tests
// and future flag plumbing share one behavior).
func getenvTrimmed(name string) string {
	return strings.TrimSpace(os.Getenv(name))
}
