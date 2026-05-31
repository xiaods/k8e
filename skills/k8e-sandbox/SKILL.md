---
name: k8e-sandbox-skill
description: K8E sandbox skill — execute code and shell commands in gVisor/Kata/Firecracker-isolated pods. All code execution must go through the sandbox, never on the host. Trigger when user asks to run, execute, test, try, install, analyze code or data files.
user-invocable: true
---

# K8E Sandbox

All code execution goes through `k8e-sandbox-cli` — never run code directly on the host. Each session is a gVisor/Kata/Firecracker-isolated pod with kernel-level egress enforcement. Mistakes can't damage the host.

**Trigger keywords (English):** "run this", "execute", "test it", "install X", "check if this works", "analyze this data"

**Trigger keywords (中文):** "帮我跑", "运行", "试试", "执行", "测试", "安装并使用", "分析这个CSV", "处理这个文件", "看看输出", "帮我算"

## Environment

Download the platform-specific binary (~44MB) from [GitHub Releases](https://github.com/xiaods/k8e/releases).

| Platform | Binary |
|----------|--------|
| macOS Intel | `k8e-sandbox-cli-darwin-amd64` |
| macOS Apple Silicon | `k8e-sandbox-cli-darwin-arm64` |
| Linux x86_64 | `k8e-sandbox-cli-linux-amd64` |
| Linux ARM64 | `k8e-sandbox-cli-linux-arm64` |
| Windows x86_64 | `k8e-sandbox-cli-windows-amd64.exe` |

```bash
# Download and make executable
curl -sLO https://github.com/xiaods/k8e/releases/latest/download/k8e-sandbox-cli-linux-amd64
chmod +x k8e-sandbox-cli-linux-amd64

# On the K8E server node, auto-discovery works automatically
./k8e-sandbox-cli-linux-amd64 run "echo hello"

# From a remote client, set the gateway endpoint
export K8E_SANDBOX_ENDPOINT=<server-ip>:50051
./k8e-sandbox-cli-linux-amd64 run "echo hello"
```

## Command Reference

| Command | Purpose | Key flags |
|---------|---------|-----------|
| `k8e-sandbox-cli run <code>` | Execute code or shell command | `--lang python\|bash\|node\|ts`, `--session-id`, `--tenant`, `--timeout 30`, `--raw` |
| `k8e-sandbox-cli status` | Check service + current session | — |
| `k8e-sandbox-cli create` | New session (manual lifecycle) | `--runtime gvisor\|kata\|firecracker`, `--allowed-hosts`, `--manifest`, `--git-repo` |
| `k8e-sandbox-cli destroy <sid>` | Destroy session | — |
| `k8e-sandbox-cli write <sid> <path>` | Write file to /workspace | content via stdin, `--mode w\|a` |
| `k8e-sandbox-cli read <sid> <path>` | Read file from /workspace | `--raw` (plain text output) |
| `k8e-sandbox-cli list <sid>` | List workspace files | `--since <unix_ts>` |
| `k8e-sandbox-cli subagent <parent-sid>` | Spawn child sandbox (depth 1) | shares parent /workspace PVC |
| `k8e-sandbox-cli confirm <sid> <action>` | Gate destructive action on human approval | `--timeout 30`, `--no-wait` |
| `k8e-sandbox-cli approve <approval-id>` | Approve pending confirm | — |

## Run details

```
k8e-sandbox-cli run <code> [--lang python|bash|node|ts] [--session-id <id>] [--tenant <id>] [--timeout <seconds>] [--raw]
```

Code source: argument > stdin. Language default: `bash`.

| Language | Single-line | Multi-line |
|----------|-------------|------------|
| python | `python3 -c "..."` | writes `/tmp/_k8e_run.py` |
| node/js | `node -e "..."` | writes `/tmp/_k8e_run.js` |
| ts | `tsx -e "..."` | writes `/tmp/_k8e_run.ts` |
| bash | pass-through | pass-through |

### Mode table

| Mode | Behavior | Output | Exit code |
|------|----------|--------|-----------|
| Default (JSON) | Wait for completion | `{"stdout":"...","stderr":"...","exit_code":0,"session_id":"sess-xxx"}` | match command |
| `--raw` | Stream in real-time | plain text to stdout | match command |

## Session lifecycle

| Mode | How it works | State location |
|------|-------------|----------------|
| Auto (default) | `run` auto-creates session if none exists | `~/.k8e/sandbox/default/state.json` |
| Tenant | `--tenant my-project` for cross-process reuse | `~/.k8e/sandbox/{tenant}/state.json` |
| Manual | `create` → `run --session-id` → `destroy` | no state file |

Auto-sessions persist across `run` calls within the same process. Use `--tenant` to persist across process restarts. Use manual mode when you need custom runtime or egress settings.

## Egress rules

Default allowed hosts (kernel-level Cilium eBPF enforcement):
`pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `github.com`, `raw.githubusercontent.com`

Override with `--allowed-hosts` on `create`. Everything else blocked.

## Typical Scenarios

### Scenario 1: Quick code execution (most common)

```
→ k8e-sandbox-cli run "print('hello')" --lang python
→ parse JSON output, display stdout to user
```

No session management needed. CLI auto-creates and reuses.

### Scenario 2: Install package then use it

```
1. k8e-sandbox-cli run "pip install pandas" --lang bash
2. k8e-sandbox-cli run "python3 -c 'import pandas; print(pandas.__version__)'" --lang bash
```

Same auto-session reused across both calls.

### Scenario 3: Write file, execute, read results

```
1. k8e-sandbox-cli write $SID /workspace/analyze.py <<'PYEOF'
   import pandas as pd
   df = pd.read_csv('/workspace/data.csv')
   print(df.describe())
   PYEOF

2. echo "name,value\na,1\nb,2" | k8e-sandbox-cli write $SID /workspace/data.csv

3. k8e-sandbox-cli run "python3 /workspace/analyze.py" --session-id $SID

4. k8e-sandbox-cli read $SID /workspace/result.json --raw | jq .
```

### Scenario 4: Custom egress + explicit session

```
1. SID=$(k8e-sandbox-cli create --runtime gvisor --allowed-hosts pypi.org,api.example.com | jq -r .session_id)
2. k8e-sandbox-cli run "curl -s https://api.example.com/data" --session-id $SID
3. k8e-sandbox-cli destroy $SID
```

### Scenario 5: Parallel sub-agents

```
1. SUB1=$(k8e-sandbox-cli subagent $PARENT | jq -r .session_id)
2. SUB2=$(k8e-sandbox-cli subagent $PARENT | jq -r .session_id)
3. k8e-sandbox-cli run "python3 task_a.py" --session-id $SUB1 --raw &
4. k8e-sandbox-cli run "python3 task_b.py" --session-id $SUB2 --raw &
5. wait
```

Sub-agents share parent `/workspace` PVC, communicate via files. Max depth 1.

### Scenario 6: Human approval for destructive actions

```
1. k8e-sandbox-cli confirm $SID "delete /workspace/production-data"  →  blocks, outputs approval prompt to stderr
2. Show the stderr output to user — they copy the approve command
3. Command returns {"approved": true} on success
```

For non-blocking registration: add `--no-wait`, get `approval_id`, call `k8e-sandbox-cli approve $AID` later.

## Error reference

| Exit code | Cause | Action |
|-----------|-------|--------|
| 2 | Sandbox service unreachable | Check `k8e-sandbox-cli status`, ensure gateway is running |
| 1 | Command failed / session not found | Parse error message from JSON; create new session if expired |
| non-zero | Command inside sandbox failed | Check `stderr` in JSON output |

JSON error output format:
```json
{"error":"description","detail":"specific cause"}
{"ok":false,"error":"reason"}
```

## Isolated runtimes

| Runtime | Isolation | Boot time | Requirement |
|---------|-----------|-----------|-------------|
| `gvisor` (default) | Userspace kernel | ~10ms | runsc binary |
| `kata` | VM (QEMU) | ~500ms | kata-runtime |
| `firecracker` | Hardware microVM | ~125ms | /dev/kvm |

Use `--runtime firecracker` only when `/dev/kvm` is available. gVisor is the recommended default — no KVM required.

## Your role

**Do:**
- Run ALL code through `k8e-sandbox-cli run`, never on host
- Use `--lang python` for `.py` code, `--lang bash` for shell commands
- Use `--raw` for long-running output (training, streaming)
- Use `jq` to parse JSON output: `k8e-sandbox-cli run "..." | jq -r .stdout`
- Auto-manage sessions (default mode) unless custom settings needed
- Show stderr from `confirm` to user verbatim

**Don't:**
- Never run `python3`, `node`, `pip`, `npm`, `curl` directly on host
- Never use `k8e-sandbox-cli run` with `sudo`
- Don't create sessions manually when auto-mode works
