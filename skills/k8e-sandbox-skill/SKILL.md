---
name: k8e-sandbox-skill
description: Provides sandbox tools for safely running code, scripts, and shell commands in an isolated K8E environment — never on the host. Use this skill for ANY code execution request. This means: running Python/bash/Node.js code, data analysis, CSV/Excel processing, file manipulation, package installation, or any terminal operation. CRITICAL: Always trigger this skill when the user wants something actually executed or run — not just written or explained. Trigger for English requests like "run this", "execute", "test it", "check if this works", "install X and use it". Trigger equally for Chinese requests: "帮我跑一下", "运行这个", "试试这个", "执行一下", "跑一下", "测试这段代码", "帮我分析这个CSV", "帮我跑这段代码", "跑一下看看", "帮我试试", "能不能跑", "看看输出", "帮我算一下", "帮我处理这个文件", "安装并使用". When a user pastes code and wants results, use this skill. When in doubt, use the sandbox — it's always safer than running on the host.
---

# K8E Sandbox Skill

**Always run code and shell commands in the sandbox, never directly on the host.** Each session runs in a gVisor/Kata/Firecracker-isolated pod with kernel-level egress enforcement — mistakes can't damage the host, and network access is restricted to an explicit allowlist.

## Tool reference

| Situation | Tool |
|---|---|
| Default: run any code or command | `sandbox_run` |
| Install Python packages | `sandbox_pip_install` |
| Run command in an existing session | `sandbox_exec` |
| Run command with streaming output | `sandbox_exec_stream` |
| Write a file to workspace | `sandbox_write_file` |
| Read a file from workspace | `sandbox_read_file` |
| List recently changed files | `sandbox_list_files` |
| Check sandbox availability | `sandbox_status` |
| Parallel independent subtasks | `sandbox_run_subagent` |
| Custom runtime or egress allowlist | `sandbox_create_session` |
| Explicitly clean up a session | `sandbox_destroy_session` |
| Before irreversible actions | `sandbox_confirm_action` |

## Running code

`sandbox_run` is the default entry point — it handles session creation and reuse automatically:

```
tool: sandbox_run
args: { "code": "<shell command or script>", "language": "python|bash|node" }
```

For multi-step workflows, capture the `session_id` from the first response and pass it to subsequent `sandbox_exec` calls. All steps then share the same `/workspace` filesystem and environment.

## Common patterns

**Install a package then run code:**
```
1. sandbox_pip_install  { "packages": ["pandas", "matplotlib"] }
   → note the session_id in the response
2. sandbox_exec         { "session_id": "<id>", "command": "python script.py" }
```

**Write a file then execute it:**
```
1. sandbox_write_file  { "path": "/workspace/script.py", "content": "..." }
   → note the session_id
2. sandbox_exec        { "session_id": "<id>", "command": "python /workspace/script.py" }
```

**Long-running command with streaming output:**
```
tool: sandbox_exec_stream
args: { "session_id": "<id>", "command": "python train.py" }
```

**Custom egress allowlist** (e.g., allow github.com in addition to defaults):
```
tool: sandbox_create_session
args: { "allowed_hosts": ["pypi.org", "files.pythonhosted.org", "github.com"] }
```
Default allowed hosts: `pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `github.com`, `raw.githubusercontent.com`. Anything not on the list is blocked at the kernel level by Cilium eBPF — the connection is dropped, not just timed out.

**Parallel sub-agents sharing a workspace:**
```
tool: sandbox_run_subagent
args: { "parent_session_id": "<id>", "agent_type": "research|coding|general", "workspace_path": "/workspace/results" }
```
Sub-agents (depth=1) share the parent's `/workspace` PVC and communicate by writing files. Sub-agents cannot spawn further agents — calling `sandbox_run_subagent` from a sub-agent returns `PERMISSION_DENIED`.

**Before any irreversible action** (deleting files, deploying, sending data externally):
```
tool: sandbox_confirm_action
args: { "session_id": "<id>", "action": "describe exactly what is about to happen and why" }
```
This is an architectural safety gate, not a courtesy prompt. The call blocks until an external approver explicitly approves via a separate API call. It cannot be bypassed by prompt instructions.

## SDK — direct gRPC, no MCP overhead

Use the SDK when writing Python or TypeScript programs that call the sandbox directly — **no process spawn, no stdio handshake, ~1-5ms latency vs ~500ms for MCP stdio**.

SDK source: `sdk/python/sandbox_client.py` · `sdk/typescript/sandbox_client.ts`

### When to use SDK vs MCP tools

| Scenario | Use |
|---|---|
| Agent executing user requests interactively | MCP tools (`sandbox_run` etc.) |
| Python/TS program calling sandbox in a loop | SDK |
| Low-latency batch execution | SDK |

---

### Python SDK

**Install:**
```bash
pip install grpcio grpcio-tools protobuf
# generate stubs once:
python -m grpc_tools.protoc -I proto --python_out=. --grpc_python_out=. proto/sandbox/v1/sandbox.proto
```

**Run code (session auto-managed):**
```python
from sandbox_client import SandboxClient

with SandboxClient() as client:
    result = client.run("print('hello')", language="python")
    print(result.stdout)          # hello
    print(result.exit_code)       # 0
```

**Multi-step workflow:**
```python
with SandboxClient() as client:
    client.run("pip install pandas", "bash")   # same session reused
    result = client.run("python3 analyze.py", "bash")
```

**Explicit session with custom options:**
```python
from sandbox_client import SandboxClient, sandbox_session

with sandbox_session(runtime_class="kata", allowed_hosts=["github.com"]) as (client, sid):
    client.write_file(sid, "/workspace/main.py", code)
    result = client.exec(sid, "python3 /workspace/main.py")
```

**Streaming output:**
```python
for chunk in client.exec_stream(sid, "python3 train.py"):
    print(chunk, end="", flush=True)
```

**API:**

| Method | Description |
|---|---|
| `SandboxClient(endpoint?, tenant_id?)` | Create client, TLS auto-discovered |
| `client.run(code, language, timeout)` | Run code, session lazily created and reused |
| `client.exec(sid, command, timeout)` | Run in explicit session |
| `client.exec_stream(sid, command)` | Streaming output iterator |
| `client.create_session(runtime_class, allowed_hosts, tenant_id)` | Create session |
| `client.destroy_session(sid)` | Destroy session |
| `client.write_file(sid, path, content)` | Write to `/workspace` |
| `client.read_file(sid, path)` | Read from `/workspace` |
| `client.pip_install(sid, packages)` | Install Python packages |
| `client.close()` | Close connection + destroy default session |
| `sandbox_session(...)` | Context manager for a dedicated session |

---

### TypeScript SDK

**Install:**
```bash
npm install @grpc/grpc-js @grpc/proto-loader
```

**Run code (session auto-managed):**
```typescript
import { SandboxClient } from "./sandbox_client";

const client = new SandboxClient();
const result = await client.run("print('hello')", "python");
console.log(result.stdout);   // hello
await client.close();
```

**Multi-step workflow:**
```typescript
const client = new SandboxClient();
await client.run("pip install pandas", "bash");   // same session reused
const result = await client.run("python3 analyze.py", "bash");
await client.close();
```

**Explicit session:**
```typescript
const sid = await client.createSession({ runtimeClass: "kata", allowedHosts: ["github.com"] });
await client.writeFile(sid, "/workspace/main.py", code);
const result = await client.exec(sid, "python3 /workspace/main.py");
await client.destroySession(sid);
```

**Streaming output:**
```typescript
for await (const chunk of client.execStream(sid, "python3 train.py")) {
  process.stdout.write(chunk);
}
```

**One-shot helper:**
```typescript
import { sandboxRun } from "./sandbox_client";
const { stdout } = await sandboxRun("echo hello");
```

**API:**

| Method | Description |
|---|---|
| `new SandboxClient(endpoint?, tenantId?)` | Create client, TLS auto-discovered |
| `client.run(code, language, timeout)` | Run code, session lazily created and reused |
| `client.exec(sid, command, timeout)` | Run in explicit session |
| `client.execStream(sid, command)` | Async iterable of output chunks |
| `client.createSession(opts)` | Create session |
| `client.destroySession(sid)` | Destroy session |
| `client.writeFile(sid, path, content)` | Write to `/workspace` |
| `client.readFile(sid, path)` | Read from `/workspace` |
| `client.pipInstall(sid, packages)` | Install Python packages |
| `client.close()` | Close connection + destroy default session |
| `sandboxRun(code, language?)` | One-shot convenience function |

## Error handling

- If `sandbox_status` fails → sandbox service is unreachable; suggest `kubectl -n sandbox-matrix get pods`
- If a command exits with an error → show full stderr and diagnose before retrying
- If a package install fails → check package name spelling and Python version compatibility
- If `sandbox_run_subagent` returns `PERMISSION_DENIED` → the current session is already a sub-agent (depth=1); sub-agents cannot spawn children
- If `curl` or network calls fail inside the sandbox → the target host is likely not in `allowed_hosts`; use `sandbox_create_session` with an explicit allowlist
- Files in `/workspace/` persist for the lifetime of the session (and are shared across sub-agents); use `sandbox_list_files` to verify writes before reading back
