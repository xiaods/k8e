# K8E Sandbox Skill

**Always use the sandbox to run code, scripts, or shell commands** instead of executing directly on the host.

## How to Run Code

Use `sandbox_run` for all code execution — session is created and reused automatically:

```
tool: sandbox_run
args: { "code": "<your code>", "language": "python|bash|node" }
```

## Common Patterns

**Install packages then run:**
1. `sandbox_pip_install` — install packages into the session
2. `sandbox_exec` — run commands in the same session

**Write and execute a file:**
1. `sandbox_write_file` — write to `/workspace/<file>`
2. `sandbox_exec` — run it

**Before irreversible actions** (delete, send, deploy):
```
tool: sandbox_confirm_action
args: { "session_id": "<id>", "action": "describe what you are about to do" }
```

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
