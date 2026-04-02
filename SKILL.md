# K8E Sandbox Skill

This project provides an isolated sandbox for safe code execution via the `k8e-sandbox` MCP skill.

**Always use the sandbox to run code, scripts, or shell commands** instead of executing directly on the host.

## Setup (one-time)

```bash
k8e sandbox-install-skill all
```

This registers the `k8e-sandbox` MCP server in your agent config automatically.

## How to Run Code

Use `sandbox_run` for all code execution tasks:

```
tool: sandbox_run
args: { "code": "<your code>", "language": "python|bash|node" }
```

- Session is created automatically and reused for the entire conversation
- No need to call `sandbox_create_session` manually
- On conversation end, the session is cleaned up automatically

## Common Patterns

**Run Python:**
```json
{ "code": "import pandas as pd; print(pd.__version__)", "language": "python" }
```

**Run shell:**
```json
{ "code": "curl -s https://api.github.com/zen" }
```

**Install packages then run:**
```
1. sandbox_pip_install  { "session_id": "<id>", "packages": ["requests"] }
2. sandbox_exec         { "session_id": "<id>", "command": "python3 -c 'import requests; print(requests.get(\"https://httpbin.org/get\").status_code)'" }
```

**Write a file and execute it:**
```
1. sandbox_write_file  { "session_id": "<id>", "path": "/workspace/main.py", "content": "print('hello')" }
2. sandbox_exec        { "session_id": "<id>", "command": "python3 /workspace/main.py" }
```

**Before irreversible actions** (delete, send, deploy), always call:
```
tool: sandbox_confirm_action
args: { "session_id": "<id>", "action": "describe what you are about to do" }
```

## Check Availability

```
tool: sandbox_status
args: {}
```

Returns `{ "available": true, "session_id": "..." }`.

## All Available Tools

| Tool | When to use |
|---|---|
| `sandbox_run` | ✅ Default — run any code or command |
| `sandbox_status` | Check if sandbox service is reachable |
| `sandbox_write_file` | Write a file to `/workspace` |
| `sandbox_read_file` | Read a file from `/workspace` |
| `sandbox_list_files` | List recently changed files |
| `sandbox_pip_install` | Install Python packages |
| `sandbox_exec` | Run command in an explicit session |
| `sandbox_exec_stream` | Run command with streaming output |
| `sandbox_create_session` | Create session with custom options (runtime, egress) |
| `sandbox_destroy_session` | Explicitly destroy a session |
| `sandbox_run_subagent` | Spawn a child sandbox for parallel tasks (depth ≤ 1) |
| `sandbox_confirm_action` | Require human approval before irreversible operations |
