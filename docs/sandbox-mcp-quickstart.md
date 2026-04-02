# K8E Sandbox MCP Quickstart

Install the K8E sandbox skill into your AI agent in one step.

## Prerequisites

- K8E cluster running (`systemctl status k8e`)
- `k8e` binary in `$PATH`

## Agent Setup

### claude code

```bash
claude mcp add k8e-sandbox -- k8e sandbox-mcp
```

### kiro-cli

Add to `.kiro/settings.json` in your project (or `~/.kiro/settings.json` globally):

```json
{
  "mcpServers": {
    "k8e-sandbox": {
      "command": "k8e",
      "args": ["sandbox-mcp"]
    }
  }
}
```

### gemini cli

Add to `~/.gemini/settings.json`:

```json
{
  "mcpServers": {
    "k8e-sandbox": {
      "command": "k8e",
      "args": ["sandbox-mcp"]
    }
  }
}
```

## Verify

```bash
# Check the MCP server starts and lists tools
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1.0"},"capabilities":{}}}' \
  | k8e sandbox-mcp
```

Expected: JSON response with `capabilities.tools`.

## Available Tools

| Tool | Description |
|---|---|
| `sandbox_create_session` | Create an isolated sandbox pod |
| `sandbox_destroy_session` | Destroy session and clean up |
| `sandbox_exec` | Run a command, get stdout/stderr/exit_code |
| `sandbox_exec_stream` | Run a command, get accumulated streaming output |
| `sandbox_write_file` | Write a file into `/workspace` |
| `sandbox_read_file` | Read a file from `/workspace` |
| `sandbox_list_files` | List files modified since a timestamp |
| `sandbox_pip_install` | Install Python packages via pip |
| `sandbox_run_subagent` | Spawn a child sandbox (depth ≤ 1) |
| `sandbox_confirm_action` | Gate irreversible actions on user approval |

## Configuration Overrides

```bash
# Use a remote cluster
K8E_SANDBOX_ENDPOINT=10.0.0.1:50051 k8e sandbox-mcp

# Override TLS cert
K8E_SANDBOX_CERT=/path/to/ca.crt k8e sandbox-mcp

# Via CLI flags
k8e sandbox-mcp --endpoint 10.0.0.1:50051 --tls-cert /path/to/ca.crt
```

## Example Agent Interaction

Once installed, ask your agent:

> "Run this Python snippet in a sandbox and show me the output"

The agent will automatically:
1. Call `sandbox_create_session` → get `session_id`
2. Call `sandbox_exec` with your code
3. Return stdout/stderr
4. Call `sandbox_destroy_session` to clean up
