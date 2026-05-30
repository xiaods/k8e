# K8E Sandbox CLI Quickstart

> **Note**: `k8e sandbox-mcp` has been replaced by `k8e sandbox` CLI commands as of KIP-8.
> See [KIP-8: SKILL + CLI 替换 MCP 协议层](./kip-8-skill-cli-replace-mcp.md) for migration details.

## Quick Start

```bash
# Run code (auto-creates session)
k8e sandbox run "print('hello')" --lang python

# Check status
k8e sandbox status

# Run shell commands
k8e sandbox run "ls -la /workspace"
```

## Install the Skill

```bash
# Install skill files for all agents
k8e sandbox-install-skill all
```

## Core Commands

| Command | Description |
|---------|-------------|
| `k8e sandbox run <code>` | Run code or shell command (auto-manages session) |
| `k8e sandbox status` | Check sandbox service and current session |
| `k8e sandbox create` | Create a new session |
| `k8e sandbox destroy <sid>` | Destroy a session |
| `k8e sandbox write <sid> <path>` | Write file to /workspace (stdin) |
| `k8e sandbox read <sid> <path>` | Read file from /workspace |
| `k8e sandbox list <sid>` | List files in /workspace |
| `k8e sandbox subagent <parent-sid>` | Spawn child sandbox |
| `k8e sandbox confirm <sid> <action>` | Gate irreversible action |
| `k8e sandbox snapshot save <sid> <name>` | Save workspace snapshot |
| `k8e sandbox snapshot restore <name>` | Restore from snapshot |

## Examples

```bash
# Multi-line code via stdin
k8e sandbox run --lang python <<'EOF'
for i in range(10):
    print(i)
EOF

# Write a script then execute
echo "import pandas as pd" | k8e sandbox write $SID /workspace/script.py
k8e sandbox run "python3 /workspace/script.py" --session-id $SID

# Create session with manifest
k8e sandbox create --manifest workspace.yaml

# Save and restore snapshots
k8e sandbox snapshot save $SID my-checkpoint
k8e sandbox snapshot restore my-checkpoint

# Stream long-running output
k8e sandbox run "python3 train.py" --session-id $SID --raw
```

## Configuration

```bash
# Remote cluster
K8E_SANDBOX_ENDPOINT=10.0.0.1:50051 k8e sandbox run "echo hello"

# Custom TLS cert
K8E_SANDBOX_CERT=/path/to/ca.crt k8e sandbox run "echo hello"

# Tenant-based session persistence
k8e sandbox run "echo hello" --tenant my-project
```

## Verify

```bash
k8e sandbox status
# {"available":true,"session_id":"sess-abc","tenant_id":"default"}
```

## Session Persistence

Sessions are automatically persisted via state files at `~/.k8e/sandbox/{tenant}/state.json`. Use `--tenant` for cross-process reuse.

```bash
export K8E_SANDBOX_TENANT=my-project
k8e sandbox run "echo hello"   # creates/uses project session
k8e sandbox run "echo world"   # reuses same session
```

## Migration from MCP

| Old (MCP) | New (CLI) |
|-----------|-----------|
| `k8e sandbox-mcp` | `k8e sandbox` |
| `sandbox_run` | `k8e sandbox run` |
| `sandbox_create_session` | `k8e sandbox create` |
| `sandbox_write_file` | `k8e sandbox write` |
| `sandbox_pip_install` | `k8e sandbox run "pip install ..."` |

Full migration guide: [KIP-8](./kip-8-skill-cli-replace-mcp.md)
