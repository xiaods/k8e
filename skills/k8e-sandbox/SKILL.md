---
name: k8e-sandbox-skill
description: "Manage K8E Sandbox — run code in isolated environments via CLI. Supports codex, claude, pi, and openclaw."
metadata:
  openclaw:
    emoji: "🏗️"
---

# K8E Sandbox Skill

Always run code and shell commands in the sandbox, never directly on the host. Each session runs in a gVisor/Kata/Firecracker-isolated pod with kernel-level egress enforcement — mistakes can't damage the host, and network access is restricted to an explicit allowlist.

Use this skill for **any code execution, testing, or isolated environment needs**. Trigger for English requests like "run this", "execute", "test it", "check if this works", "install X and use it". Trigger equally for Chinese requests: "帮我跑一下", "运行这个", "试试这个", "执行一下", "跑一下", "测试这段代码", "帮我分析这个CSV", "帮我跑这段代码", "跑一下看看", "帮我试试", "能不能跑", "看看输出", "帮我算一下", "帮我处理这个文件", "安装并使用".

## Quick Start

```bash
# Run code (auto-creates session)
k8e sandbox run "print('hello')" --lang python

# Run shell command
k8e sandbox run "ls -la /workspace"

# Check status
k8e sandbox status
```

## All Commands

| Command | Description |
|---------|-------------|
| `k8e sandbox run <code>` | Run code or shell command (auto-manages session, auto-creates if none) |
| `k8e sandbox status` | Check sandbox service availability and current session |
| `k8e sandbox create` | Create a new session (custom runtime, egress allowlist) |
| `k8e sandbox destroy <sid>` | Destroy a session and free resources |
| `k8e sandbox write <sid> <path>` | Write file to `/workspace` (content via stdin) |
| `k8e sandbox read <sid> <path>` | Read file from `/workspace` |
| `k8e sandbox list <sid>` | List files in `/workspace` (filter by --since timestamp) |
| `k8e sandbox subagent <parent-sid>` | Spawn child sandbox under parent session (max depth 1) |
| `k8e sandbox confirm <sid> <action>` | Gate irreversible action on human approval |

## `k8e sandbox run` — Detailed Usage

```bash
k8e sandbox run <code>
  [--lang python|bash|node|ts]   # default: bash
  [--session-id <id>]         # specify session explicitly (skips auto-creation)
  [--tenant <id>]             # tenant for cross-process session reuse
  [--timeout 30]              # timeout in seconds
  [--raw]                     # streaming raw output (no JSON wrapper)
```

**Code source priority**: argument > stdin (pipe/heredoc).

### Single-line code

```bash
k8e sandbox run "print('hello')" --lang python

k8e sandbox run "node -e 'console.log(42)'"

k8e sandbox run "ls -la /workspace"              # default --lang bash
```

### Multi-line code via stdin

```bash
k8e sandbox run --lang python <<'PYEOF'
import json
data = {"key": "value"}
print(json.dumps(data))
PYEOF
```

### Streaming output (long-running tasks)

```bash
k8e sandbox run "python3 train.py" --session-id sess-abc --raw
# Real-time output, exit code = command exit code
```

### Specify session explicitly

```bash
k8e sandbox run "pip install pandas" --session-id sess-abc
```

When `--session-id` is set, the CLI does NOT use state files. If the session is expired, it returns an error — the Agent should create a new session.

## Output Format

### Default: JSON (parse with jq)

```bash
$ k8e sandbox run "print('hello')" --lang python
{"stdout":"hello\n","stderr":"","exit_code":0,"session_id":"sess-abc123"}

$ k8e sandbox run "print('hello')" --lang python | jq -r .stdout
hello
```

### --raw mode: plain text

```bash
$ k8e sandbox run "print('hello')" --lang python --raw
hello

$ k8e sandbox read sess-abc /workspace/data.json --raw | jq .key
42
```

### Error format

```bash
# Service unreachable → exit 2
$ k8e sandbox run "echo hello"
{"error":"sandbox not reachable","detail":"connection refused"}
exit 2

# Session not found → exit 1
$ k8e sandbox destroy no-exist
{"ok":false,"error":"session not found"}
exit 1
```

## Session Management

### Auto-managed session (default)

```bash
k8e sandbox run "echo hello"
# Creates a session automatically if none exists
# Saves session ID to ~/.k8e/sandbox/default/state.json
# Subsequent calls reuse the same session
```

### Tenant-based persistence

```bash
k8e sandbox run "echo hello" --tenant my-project
# Session saved to ~/.k8e/sandbox/my-project/state.json
# All calls with same tenant reuse the session across process restarts
```

### Manual lifecycle

```bash
SID=$(k8e sandbox create --runtime gvisor --allowed-hosts pypi.org,github.com | jq -r .session_id)
k8e sandbox run "python3 analyze.py" --session-id $SID
k8e sandbox destroy $SID
```

## Common Patterns

### Write a script then execute

```bash
k8e sandbox write $SESSION_ID /workspace/analyze.py <<'EOF'
import pandas as pd
df = pd.read_csv('/workspace/data.csv')
print(df.describe())
EOF
k8e sandbox run "python3 /workspace/analyze.py" --session-id $SESSION_ID
```

### Write data file then analyze

```bash
echo "name,value\na,1\nb,2" | k8e sandbox write $SID /workspace/data.csv
k8e sandbox run "python3 /workspace/analyze.py" --session-id $SID
k8e sandbox read $SID /workspace/result.json --raw | jq .
```

### Custom egress allowlist

```bash
SID=$(k8e sandbox create --runtime gvisor --allowed-hosts pypi.org,github.com | jq -r .session_id)
k8e sandbox run "curl -s https://api.github.com/repos/kubernetes/kubernetes" --session-id $SID
k8e sandbox destroy $SID
```

Default allowed hosts: `pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `github.com`, `raw.githubusercontent.com`. Anything not on the list is blocked at the kernel level by Cilium eBPF.

### Parallel sub-agents sharing a workspace

```bash
SUB1=$(k8e sandbox subagent sess-parent | jq -r .session_id)
SUB2=$(k8e sandbox subagent sess-parent | jq -r .session_id)

k8e sandbox run "python3 /workspace/code.py" --session-id $SUB1 --raw &
k8e sandbox run "python3 /workspace/test.py" --session-id $SUB2 --raw &
wait
```

Sub-agents (depth=1) share the parent's `/workspace` PVC and communicate by writing files. Sub-agents cannot spawn further agents — calling `subagent` from a sub-agent returns an error.

## Human Approval (ConfirmAction)

Before any irreversible action (deleting files, deploying, sending data externally), call `k8e sandbox confirm`:

```bash
k8e sandbox confirm $SID "delete /workspace/secret.txt"
```

This command:
1. Immediately prints the approval request and ID to stderr
2. Blocks until a human approves via `k8e sandbox approve <id>` in another terminal
3. Returns `{"approved":true}` on stdout when approved
4. Times out after 30s (use `--timeout <seconds>` to extend)

The output on stderr looks like:
```
[k8e-sandbox] ⚠ Approval required: delete /workspace/secret.txt
[k8e-sandbox]    To approve: k8e sandbox approve approval-xxx-12345
[k8e-sandbox]    Timeout: 30s
```

**Show this output to the user** when using confirm. The user copies the approve command to another terminal.

### Register only (non-blocking)

```bash
AID=$(k8e sandbox confirm $SID "delete file" --no-wait | jq -r .approval_id)
# ... later ...
k8e sandbox approve $AID
```

## Tips

- `k8e sandbox run` creates a session automatically and saves it to `~/.k8e/sandbox/{tenant}/state.json`
- Use `--tenant` for cross-process session persistence across Agent restarts
- All commands output JSON by default; pipe to `jq` for field extraction
- Session timeout is controlled by `SandboxMatrix` CRD, not the CLI
- Use `--raw` on `run` for streaming output of long-running commands
- Use `--raw` on `read` to pipe file contents directly to `jq` or other tools
- For pip install, just use `k8e sandbox run "pip install pkg" --session-id $SID`
