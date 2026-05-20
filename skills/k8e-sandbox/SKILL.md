---
name: k8e-sandbox-skill
description: "Manage K8E Sandbox Matrix clusters — create/destroy sessions, run code in isolated sandboxes, manage runtime policies and warm pools."
metadata:
  {
    "openclaw":
      {
        "emoji": "🏗️",
        "requires":
          {
            "config": ["mcpServers.k8e-sandbox"],
          },
      },
  }
---

# K8E Sandbox Skill

Use this skill for **any code execution, testing, or isolated environment needs**. It runs code in K8E gVisor/Kata/Firecracker sandboxes — never on the host.

> **Running code in the sandbox**
>
> The easiest entry point is `sandbox_run` — it auto-creates a session, runs your code, and reuses the session for the whole conversation:
>
> ```json
> { "name": "k8e-sandbox-sandbox_run", "arguments": { "code": "python3 -c \"print('hello')\"", "language": "python" } }
> ```
>
> Capture the `session_id` from the response for multi-step workflows:
>
> ```json
> { "name": "k8e-sandbox-sandbox_exec", "arguments": { "session_id": "<id>", "command": "pip install pandas" } }
> ```

## Available tools

| Tool | Description |
|------|-------------|
| `sandbox_run` | Run code or a shell command (auto-manages session) |
| `sandbox_status` | Check sandbox service availability |
| `sandbox_create_session` | Create a new session (custom runtime, egress allowlist) |
| `sandbox_destroy_session` | Tear down a session and free resources |
| `sandbox_exec` | Execute a command in a specific session |
| `sandbox_exec_stream` | Execute with streaming output |
| `sandbox_write_file` | Write a file to `/workspace` |
| `sandbox_read_file` | Read a file from `/workspace` |
| `sandbox_list_files` | List recently changed files |
| `sandbox_pip_install` | Install Python packages |
| `sandbox_run_subagent` | Spawn a child sandbox (max depth 1) |
| `sandbox_confirm_action` | Gate irreversible actions on approval |

## Quick start

**Run code:**
```json
{ "name": "k8e-sandbox-sandbox_run", "arguments": { "code": "print('hello from sandbox')", "language": "python" } }
```

**Install a package then run:**
```json
{ "name": "k8e-sandbox-sandbox_pip_install", "arguments": { "packages": ["pandas", "matplotlib"] } }
```
(Note the `session_id` from the response, then pass it to subsequent calls.)

**Write a file and execute it:**
```json
{ "name": "k8e-sandbox-sandbox_write_file", "arguments": { "path": "/workspace/script.py", "content": "import pandas as pd\nprint(pd.__version__)" } }
```

**Custom egress allowlist:**
```json
{ "name": "k8e-sandbox-sandbox_create_session", "arguments": { "allowed_hosts": ["github.com", "pypi.org", "api.openclaw.ai"] } }
```

**Parallel sub-agents sharing a workspace:**
```json
{ "name": "k8e-sandbox-sandbox_run_subagent", "arguments": { "parent_session_id": "<id>", "agent_type": "coding", "workspace_path": "/workspace/results" } }
```

## Safety

- Network egress is restricted to a default allowlist (`pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `github.com`, `raw.githubusercontent.com`). Anything else is blocked at the kernel level by Cilium eBPF.
- Before any irreversible action, use `sandbox_confirm_action` — it blocks until an external approver explicitly approves.
- Sub-agents can't spawn further sub-agents (max depth = 1).
- Sessions auto-expire after TTL and are garbage-collected every 30 seconds.