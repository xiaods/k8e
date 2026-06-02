# KIP-11: Background Sandbox Execution

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-06-02 | Draft |

## Summary

Add background (non-blocking) execution to `k8e sandbox run`, enabling AI agents to submit long-running code tasks without being blocked. Agents submit a command, receive a `run_id`, and poll for results asynchronously. Background tasks run in dedicated pods, isolated from the warm pool for fast-turnaround interactive sessions.

Referencing Perplexity's Agent API `/v1/agent` sandbox tool `background: true` pattern.

## Motivation

Current `k8e sandbox run` is synchronous — the CLI blocks until the command completes or times out. AI agents (pi, claude code, codex) cannot proceed with other work while waiting for long-running tasks such as model training, batch data processing, or multi-step build workflows.

Perplexity's Agent API solves this with `background: true`: the agent submits a sandbox task, receives a response ID immediately, and polls for results. This enables multi-turn agent workflows where the agent interleaves planing, web search, and computation without blocking.

K8E needs the same pattern — but integrated with its warm pool architecture and pod reuse model (KIP-3 design).

## Design

### Architecture

```
Agent (CLI)
  │
  ├─ k8e sandbox run --background "python3 train.py"
  │    → gRPC Exec(background=true) → gateway → sandboxd POST /exec/background
  │    → 创建 session → claim background pod → 返回 run_id
  │
  ├─ k8e sandbox poll <run-id>
  │    → gRPC PollRun(run_id) → gateway → sandboxd GET /exec/background/<run-id>
  │    → 返回 status + stdout/stderr/exit_code
  │
  └─ k8e sandbox destroy <session-id>
       → 释放 pod → reset workspace → 回池
```

### Pod Lifecycle

Background pods use a dedicated pool, separate from the warm pool for interactive sessions:

```
[created] → background-running → background-completed → [agent poll] → resetting → idle-ttl → 回收
                                                        ↑ Pod 存活 + PVC 保留结果文件
                                                        │ 结果保存在 /workspace/.k8e_bg/<run_id>/
```

| State | Label | Behavior |
|-------|-------|----------|
| `background-running` | `sandbox.k8e.io/state=background-running` | Task executing |
| `background-completed` | `sandbox.k8e.io/state=background-completed` | Task done, results available, waiting for agent poll |
| 回收 | agent calls `destroy`, pod goes through resetting→warm | |

Pod is NOT automatically destroyed. Agent must explicitly destroy via `k8e sandbox destroy`.

### CLI Changes

**New flag on `run`:**

```
k8e sandbox run --background "python3 train.py"
  → {"run_id": "sess-xxx-bg-1", "status": "started", "session_id": "sess-xxx"}
```

**New subcommand `poll`:**

```
k8e sandbox poll <run-id>
  → {"run_id": "...", "status": "completed", "stdout": "...", "stderr": "...", "exit_code": 0}
```

Status values: `started`, `running`, `completed`, `failed`, `timed_out`.

### Proto Changes

```protobuf
message ExecRequest {
  string session_id = 1;
  string command    = 2;
  int32  timeout    = 3;
  string workdir    = 4;
  bool   background = 5;  // NEW: submit async, return immediately
}

message ExecResponse {
  string stdout    = 1;
  string stderr    = 2;
  int32  exit_code = 3;
  string session_id = 4;
  string run_id    = 5;  // NEW: set when background=true
  string status    = 6;  // NEW: "started" when background=true
}

message PollRunRequest {
  string run_id = 1;
}

message PollRunResponse {
  string run_id    = 1;
  string status    = 2;  // started | running | completed | failed | timed_out
  string stdout    = 3;
  string stderr    = 4;
  int32  exit_code = 5;
}

service SandboxService {
  // ... existing RPCs ...
  rpc PollRun(PollRunRequest) returns (PollRunResponse);
}
```

### sandboxd Changes

New endpoints:

```
POST /exec/background
  Body: {"command": "...", "run_id": "...", "timeout": 300, "workdir": "/workspace"}
  Behavior: fork child process, detach, write PID + results to /workspace/.k8e_bg/<run_id>/
  Response: {"status": "started", "run_id": "..."}

GET /exec/background/<run_id>
  Behavior: read /workspace/.k8e_bg/<run_id>/stdout, stderr, exit_code
  Response: {"run_id": "...", "status": "running|completed|failed|timed_out", "stdout": "...", "stderr": "...", "exit_code": 0}
```

Result file structure in workspace:
```
/workspace/.k8e_bg/<run_id>/
├── pid          # child process PID
├── started_at   # RFC3339 timestamp
├── stdout       # accumulated stdout
├── stderr       # accumulated stderr
└── exit_code    # written on completion; presence signals "completed"
```

### Gateway Changes

Orchestrator holds a `run_registry` mapping `run_id → session_id`:

```go
type Orchestrator struct {
    // ...
    runRegistry map[string]string  // run_id → session_id
}

func (o *Orchestrator) ExecBackground(ctx, req *ExecRequest) (string, error) {
    // 1. Create session (or use existing)
    // 2. Claim pod from background pool
    // 3. sandboxd POST /exec/background
    // 4. Register run_id → session_id
    // 5. Return run_id
}

func (o *Orchestrator) PollRun(ctx, runID string) (*PollRunResponse, error) {
    // 1. Lookup session_id from run_registry
    // 2. Get pod IP
    // 3. sandboxd GET /exec/background/<run_id>
    // 4. Return status + output
}
```

### Background Pool Sizing

Background pool is separate from warm pool. Configured via `SandboxMatrix` CRD:

```yaml
apiVersion: k8e.sh/v1alpha1
kind: SandboxMatrix
spec:
  warmPoolSize: 5          # interactive sessions
  backgroundPoolSize: 2    # background tasks (NEW)
  runtimeClass: gvisor
```

Background pool pods use the same pod spec but:
- Different label: `sandbox.k8e.io/pool=background`
- Do NOT participate in warm pool claim/release cycle
- Reconciler maintains `backgroundPoolSize` pods independently

### Workspace Reset After Background

When agent calls `k8e sandbox destroy`:
1. `sandboxd POST /workspace/reset` clears `/workspace` including `.k8e_bg/`
2. Pod transitions to `resetting` → `warm`
3. PVC survives (same as interactive session release)

## Limits and Edge Cases

| Scenario | Behavior |
|----------|----------|
| Agent submits background, never polls | Pod stays alive until destroy or external timeout |
| Agent polls, fetches result, never destroys | Pod stays alive until external cleanup |
| Background pod crashes during execution | sandboxd restarts, `.k8e_bg/<run_id>/exit_code` may not exist → poll returns `failed` |
| Multiple background tasks on same session | `run_id` includes sequence: `sess-xxx-bg-1`, `sess-xxx-bg-2` |
| Concurrent background tasks | Separate runs; each has its own `run_id`, shared workspace |
| Timeout expired during background execution | sandboxd kills child process, writes exit_code, poll returns `timed_out` |

## Implementation Plan

| Phase | Component | Changes |
|-------|-----------|---------|
| 1 | `sandboxd` | `POST /exec/background`, `GET /exec/background/<run_id>` |
| 2 | `sandbox.proto` | `ExecRequest.background`, `PollRun` RPC |
| 3 | `orchestrator` | `run_registry`, `ExecBackground`, `PollRun` |
| 4 | `controller` | `backgroundPoolSize` reconciler |
| 5 | CLI `commands.go` | `run --background`, `poll` subcommand |
| 6 | `types.go` | `SandboxPhaseBackgroundRunning`, `BackgroundCompleted` |
