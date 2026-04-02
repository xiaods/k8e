# K8E Sandbox MCP Quickstart

Install the K8E sandbox skill into your AI agent in one step.

## Prerequisites

- K8E cluster running (`systemctl status k8e`)
- `k8e` binary in `$PATH`

## One-Command Install

```bash
# Install into all supported agents at once
k8e sandbox-install-skill all

# Or install into a specific agent
k8e sandbox-install-skill claude
k8e sandbox-install-skill kiro
k8e sandbox-install-skill gemini
```

This automatically writes the MCP server entry into each agent's config file,
merging with any existing configuration.

| Agent | Config file modified |
|---|---|
| claude code | `~/.claude.json` |
| kiro-cli | `.kiro/settings.json` (workspace) or `~/.kiro/settings.json` (global) |
| gemini cli | `~/.gemini/settings.json` |

## Manual Setup (alternative)

If you prefer to configure manually:

**claude code:**
```bash
claude mcp add k8e-sandbox -- k8e sandbox-mcp
```

**kiro-cli / gemini cli** — add to settings JSON:
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

## Usage

Once installed, just ask your agent naturally:

> "Run this Python snippet in a sandbox"
> "Execute this shell script safely"
> "Test this code without affecting my machine"

The agent will use `sandbox_run` automatically — no session management needed.

## Available Tools

### High-level (recommended)

| Tool | Description |
|---|---|
| `sandbox_run` | Run code/commands — auto-manages session lifecycle |
| `sandbox_status` | Check if sandbox service is available |

### Low-level (full control)

| Tool | Description |
|---|---|
| `sandbox_create_session` | Create an isolated sandbox pod |
| `sandbox_destroy_session` | Destroy session and clean up |
| `sandbox_exec` | Run a command in a specific session |
| `sandbox_exec_stream` | Run a command, get streaming output |
| `sandbox_write_file` | Write a file into `/workspace` |
| `sandbox_read_file` | Read a file from `/workspace` |
| `sandbox_list_files` | List files modified since a timestamp |
| `sandbox_pip_install` | Install Python packages via pip |
| `sandbox_run_subagent` | Spawn a child sandbox (depth ≤ 1) |
| `sandbox_confirm_action` | Gate irreversible actions on user approval |

## Configuration Overrides

```bash
# Remote cluster
K8E_SANDBOX_ENDPOINT=10.0.0.1:50051 k8e sandbox-mcp

# Custom TLS cert
K8E_SANDBOX_CERT=/path/to/ca.crt k8e sandbox-mcp

# Via flags
k8e sandbox-mcp --endpoint 10.0.0.1:50051 --tls-cert /path/to/ca.crt
```

## Verify

```bash
echo '{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05","clientInfo":{"name":"test","version":"1.0"},"capabilities":{}}}' \
  | k8e sandbox-mcp
```
