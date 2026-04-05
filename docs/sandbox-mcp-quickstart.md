# K8E Sandbox MCP Quickstart

Install the K8E sandbox skill into your AI agent in one step.

## Prerequisites

- `runsc` (gVisor) installed **before** starting K8E:
  ```bash
  curl -fsSL https://gvisor.dev/archive.key | gpg --dearmor -o /usr/share/keyrings/gvisor-archive-keyring.gpg
  echo "deb [arch=$(dpkg --print-architecture) signed-by=/usr/share/keyrings/gvisor-archive-keyring.gpg] \
    https://storage.googleapis.com/gvisor/releases release main" \
    > /etc/apt/sources.list.d/gvisor.list
  apt-get update && apt-get install -y runsc
  ```
  > Do **not** run `runsc install`. K8E detects `runsc` at startup and auto-injects the gVisor stanza into its own containerd config (`/var/lib/k8e/agent/etc/containerd/config.toml`).
- K8E cluster running (`systemctl status k8e`)
- `k8e` binary in `$PATH`

## One-Command Install

`sandbox-install-skill` does two things in one command:
1. Writes the `k8e-sandbox` MCP server entry into the agent's config file
2. Copies the sandbox skill files from `/var/lib/k8e/server/skills/` into the agent's skills directory

> **Prerequisite:** K8E server must have started at least once. On first boot it stages the skill files to `/var/lib/k8e/server/skills/`. If that directory is missing, start K8E first: `systemctl start k8e`

```bash
# Install into all supported agents at once
k8e sandbox-install-skill all

# Or install into a specific agent
k8e sandbox-install-skill kiro      # MCP config → .kiro/settings.json (workspace)
                                    # Skills     → .kiro/skills/k8e-sandbox-skill/
k8e sandbox-install-skill claude    # MCP config → ~/.claude.json
                                    # Skills     → ~/.claude/skills/k8e-sandbox-skill/
k8e sandbox-install-skill gemini    # MCP config → ~/.gemini/settings.json
                                    # Skills     → ~/.gemini/skills/k8e-sandbox-skill/
```

| Agent | MCP config | Skills directory |
|---|---|---|
| claude code | `~/.claude.json` | `~/.claude/skills/k8e-sandbox-skill/` |
| kiro-cli | `.kiro/settings.json` (workspace) or `~/.kiro/settings.json` (global) | `.kiro/skills/k8e-sandbox-skill/` |
| gemini cli | `~/.gemini/settings.json` | `~/.gemini/skills/k8e-sandbox-skill/` |

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

## Python SDK Usage

Direct gRPC — no MCP overhead (~1–5 ms vs ~500 ms for MCP stdio).

### Install

```bash
python3 -m pip install grpcio grpcio-tools protobuf
```

### Generate stubs (once)

```bash
python3 -m grpc_tools.protoc -I proto \
  --python_out=sdk/python \
  --grpc_python_out=sdk/python \
  proto/sandbox/v1/sandbox.proto

touch sdk/python/sandbox/__init__.py sdk/python/sandbox/v1/__init__.py
```

### Run code

```python
from sandbox_client import SandboxClient

with SandboxClient() as client:
    result = client.run("print('hello')", language="python")
    print(result.stdout)   # hello
```

### Multi-step workflow

```python
with SandboxClient() as client:
    client.run("pip install pandas", "bash")
    result = client.run("python3 analyze.py", "bash")
```

### Custom session (runtime / egress)

```python
from sandbox_client import sandbox_session

with sandbox_session(runtime_class="kata", allowed_hosts=["github.com"]) as (client, sid):
    client.write_file(sid, "/workspace/main.py", code)
    result = client.exec(sid, "python3 /workspace/main.py")
```

> SDK source: `sdk/python/sandbox_client.py`

---

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
