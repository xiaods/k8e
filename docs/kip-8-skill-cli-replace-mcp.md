# KIP-8: CLI + Skill 与 MCP 双入口

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-09-08 | Revised design; MCP reimplementation pending |

## 2026-09-08 修订：本地 CLI 与远程 MCP 并存

本节替代下方历史版本中“删除 MCP、仅保留 CLI”的产品决策。2026-06-03 的 CLI 迁移已经实现；本次 MCP 重建尚未实现，不能据本文宣称远程入口已可用。保留历史正文用于解释迁移背景，不把旧命令示例、时延估计和会话设计当作当前规范。

### 目标与架构

面向本地编码、CI 和脚本保留 CLI + skill；面向远程、多客户端服务增加 MCP 2026-07-28 Streamable HTTP 入口。gRPC Gateway、sandboxd 和 Kubernetes 会话继续作为执行后端。

```text
本地编码 / CI       -> CLI + skill ------+
远程 MCP 客户端     -> MCP HTTP adapter -+-> gRPC Gateway -> sandboxd
自有宿主            -> 原生工具插件 ------+
```

MCP adapter 不启动 CLI 子进程，不调用依赖 cli.Context、stdout 或进程环境修改的 CLI handler。复用显式配置的 gRPC client；需要共享的语言包装、操作结果与会话初始化逻辑在独立接口中实现。连接池按后端及认证身份隔离。禁止在并发请求中调用 `ApplyResolvedConn` 修改全局环境。

旧 MCP 进程退出时丢失默认会话属于旧实现的状态设计问题，不是 MCP 协议限制。CLI 每次启动进程并建立连接，常驻适配器有连接复用优势；两条路径的实际延迟和 token 成本以相同工作负载测量，不沿用历史 localhost 握手估计作为结论。

### 协议范围

以 MCP **2026-07-28** 为唯一首期协议版本，不默默接受旧版初始化请求。首期采用单一 `/mcp` POST 端点和普通 JSON 响应；请求级 SSE 按需求后续增加。

- 实现 `server/discover`、`tools/list`、`tools/call`，只声明已经实现的能力。
- 按规范处理每请求版本、client capabilities、HTTP metadata 与 body 一致性校验；发现方法遵循其专门规则。
- 普通结果包含 `resultType: complete`；工具列表包含 `ttlMs`、`cacheScope`，稳定排序。鉴权相关的列表使用 private 缓存范围，不能跨身份共享。
- 不建立协议级 session，不使用 `Mcp-Session-Id`，不实现旧 `initialize` 握手或 GET SSE 端点。沙箱 `session_id` 是业务 handle，生命周期独立于 HTTP 连接。
- 不声明未实现的 subscriptions、resources、prompts、Tasks、MRTR 或 Skills 扩展。Tasks 后续可以适配现有后台任务，但必须完成扩展协商及持久化语义，不能只改结果字段。
- JSON-RPC 参数错误、协议错误、认证失败与沙箱程序非零退出分别表达。程序失败保留结构化 `exit_code`，不误报成传输失败。

首期工具覆盖：创建/查询/销毁沙箱、提交后台命令、轮询结果、读取/写入/列出文件。所有操作显式携带沙箱或运行 handle；不使用跨用户共享的 default session。长任务通过 `run_id + poll` 返回，PTY、快照和服务暴露保留现有 CLI/gRPC 路径，后续按客户端需求扩展。

### 认证与所有权

当前 gRPC mTLS interceptor 验证客户端身份与证书吊销；这不等于逐资源授权。公开 MCP 入口前必须建立并测试下列边界：

1. 每个请求由服务端认证取得稳定 principal，不能信任模型传入的 tenant、owner、profile、endpoint 或任意转发身份头。
2. 沙箱创建时持久化 owner；查询、执行、文件访问、销毁及运行轮询均校验 owner。运行 handle 必须可追溯到所属沙箱与 principal，列表也按 owner 过滤。
3. 首期仅允许访问通过该入口创建且具有可信 owner 记录的沙箱；旧的无 owner 沙箱不自动授予远程用户。管理迁移需独立授权流程。
4. 后端连接身份不能悄悄退化为全局管理员权限。若由受信服务账号访问 gRPC，则入口必须强制逐资源授权，后端仅允许受信入口访问，并明确这一部署信任边界。
5. 对标准远程客户端的授权机制按官方 HTTP authorization 规范实现；现有 K8E API key/mTLS 是后端认证，不能冒充已经实现 MCP OAuth。任何受限认证试验必须标注兼容性，不能宣称通用客户端正式可用。
6. 默认关闭入口；显式配置启用。远程传输使用 HTTPS，验证 Origin，限制请求大小和操作超时，不记录凭证。反向代理负责哪些校验必须在部署文档中明确。

### 幂等、持久化与断线

新版 Streamable HTTP 不支持 SSE Last-Event-ID 恢复。网络失败后新的 JSON-RPC request ID 不能保证业务去重。首期将创建沙箱和执行提交视为必须去重的操作：

- 客户端提交稳定 `operation_id`，服务端按 `(principal, operation_kind, operation_id)` 原子登记请求摘要和状态；相同键不同参数明确拒绝。
- 重复请求返回已有 handle 或可查询状态，不重复创建沙箱或执行命令。记录由多副本共享并持久化，不能仅保存在 MCP 进程内存。
- 写入已受理记录后、后端执行完成前的进程崩溃必须有明确恢复策略。提交结果不确定时返回可查询的 unknown/reconciling 状态，禁止盲目重新执行；不宣称跨后端 exactly-once。
- 业务 `session_id`、`run_id` 与 operation 记录一起跨 adapter 重启恢复。后台任务与其轮询 HTTP 请求取消分离；取消一次轮询不会杀死后台进程。
- 如果后续增加前台 SSE，关闭响应流应按规范传递取消，但取消不是回滚。必须验证 context 到后端进程的实际传播，不能只关闭 HTTP socket。

### 实施顺序与验收

| 阶段 | 交付物 | 必须验证 |
|------|--------|----------|
| A | 显式客户端配置、principal/owner 与 operation 存储接口 | 并发身份不串扰；持久化原子性；相同键不同参数拒绝 |
| B | MCP HTTP 协议处理与工具适配 | discover/list/call；版本/头部校验；错误与结构化结果；请求限制 |
| C | 认证、CLI/服务启用配置与部署文档 | 匿名拒绝；伪造身份拒绝；不同用户不可读取、执行、销毁或轮询对方资源 |
| D | 后端集成与恢复验证 | 跨副本重复提交只产生一次业务提交；崩溃后的不确定状态可查询；后台结果恢复 |
| E | 回归、客户端互操作与性能比较 | CLI+skill 原行为保持；声明支持的 MCP 客户端实测；相同工作负载对比 |

验收测试使用 fake backend 覆盖协议与拒绝路径、真实持久化接口覆盖竞争/重启；运行集群验收前不能把单元测试通过等同于生产就绪。部署启用、发布和生产变更不包含在代码实现授权内。

### 依据

- [MCP 2026-07-28 变更说明](https://modelcontextprotocol.io/specification/2026-07-28/changelog)
- [Streamable HTTP](https://modelcontextprotocol.io/specification/2026-07-28/basic/transports/streamable-http)
- [Tasks 扩展](https://modelcontextprotocol.io/extensions/tasks/overview)
- [Skills Over MCP 工作组](https://modelcontextprotocol.io/community/working-groups/skills-over-mcp)：属于独立扩展工作，不作为首期前置依赖。
- 本仓库：`proto/sandbox/v1/sandbox.proto`、`pkg/sandboxcli/commands.go`、`pkg/sandboxcli/profile.go`、`pkg/sandboxmatrix/grpc/server.go`、`plugins/deepseek-harness/packages/dsh-k8e-sandbox-client/src/grpc.ts`。

---

## 历史版本：2026-06-03 CLI 迁移（已实现）

截至 2026-08-24，CLI 迁移已完成，命令集扩展至 snapshot、poll、log、events、ps、catalog、expose、allow-hosts、profiles 及 PTY 相关功能。该历史验收不代表本次重建的 MCP 已通过验收。

## Summary

删除 `pkg/sandboxmcp/`（MCP JSON-RPC stdio/SSE server），替换为纯 CLI 命令组 `k8e sandbox`。AI Agent（codex/claude/pi，面向 openclaw 等 SKILL 驱动平台）读取 SKILL.md 学习可用命令，直接通过 shell 执行 `k8e sandbox <subcommand>`，跳过 MCP 协议翻译层。gRPC 网关、sandboxd、CRD 控制器、Python/TypeScript SDK 全部保留不改。

## Motivation

KIP-4 实现的 MCP server 存在结构性问题：

1. **进程模型开销**：常驻子进程 `k8e sandbox-mcp`，Agent 重启即丢失 session 状态
2. **协议翻译开销**：每次调用 JSON ↔ protobuf 两次序列化
3. **OpenClaw 集成摩擦**：SKILL.md 描述工具、MCP 实际执行——同一件事定义两次
4. **状态管理脆弱**：session ID 存进程内存，跨进程复用需要 tenant 配置

CLI 模式：Agent 执行一次 shell 命令 → 直接 gRPC → 结果。无持久进程、无 JSON-RPC 协议、无 SSE deployment。

## Design

### 整体架构

```
AI Agent (codex/claude/pi)
  │
  ├── 读取 SKILL.md → 了解可用命令
  │
  ├── k8e sandbox run "print(42)" --lang python    ← 直接 CLI
  ├── echo "import x" | k8e sandbox write $SID /workspace/x.py
  └── k8e sandbox read $SID /workspace/result.txt --raw
        │
        └── gRPC (TLS auto-discover) → sandbox-grpc-gateway :50051
                                         │
                                         └── HTTP :2024 → sandboxd (pod)
```

### 命令清单（9 个子命令 + 1 个管理命令）

```
k8e sandbox run <code>
  [--lang python|bash|node]   # 默认 bash
  [--session-id <id>]         # 显式指定，不读不写 state 文件
  [--tenant <id>]             # 影响 state 文件目录
  [--timeout 30]              # 超时秒数
  [--raw]                     # 流式裸输出 + ExecStream

k8e sandbox status

k8e sandbox create
  [--runtime gvisor|kata|firecracker]  # 默认 gvisor
  [--tenant <id>]
  [--allowed-hosts pypi.org,...]
  [--session-id <id>]

k8e sandbox destroy <session-id>

k8e sandbox write <session-id> <path>
  [--mode w|a]               # 默认 w
  内容来自 stdin

k8e sandbox read <session-id> <path>
  [--raw]                    # 裸输出文件内容

k8e sandbox list <session-id>
  [--since <unix-timestamp>]

k8e sandbox subagent <parent-session-id>

k8e sandbox confirm <session-id> <action>
  [--timeout 30]             # 默认 30s
  [--no-wait]                # 仅注册，立即返回

k8e sandbox approve <approval-id>
  [--reject]
  [--reason "..."]
```

### 代码来源

`run` 的代码取法：`arg 非空 → arg；arg 为空 + stdin 是 pipe → 读 stdin；stdin 是终端 → 报错 "code required"`。

```bash
# arg
k8e sandbox run "print('hello')" --lang python

# stdin (heredoc)
k8e sandbox run --lang python <<'PYEOF'
for i in range(10):
    print(i)
PYEOF

# stdin (pipe)
echo "print(42)" | k8e sandbox run --lang python
```

### `--lang` 行为

| lang | 单行 | 多行 |
|------|------|------|
| `bash`（默认）| 原样 `sh -c "<code>"` | 同左 |
| `python` | `python3 -c "<code>"` | WriteFile → Exec（两次 gRPC，零 shell 转义）|
| `node` | `node -e "<code>"` | WriteFile → Exec |

多行检测：`strings.Contains(code, "\n")`。

### 输出格式

**原则**：默认 JSON（Agent 解析），`--raw` 裸输出（人类/管道）。错误始终 JSON。exit code 0/1/2 表达成败类型。

| exit | 语义 |
|------|------|
| 0 | 正常（JSON 中 `exit_code` 字段表达业务成败） |
| 1 | 业务预期失败（超时、拒绝、session not found） |
| 2 | 基础设施错误（gRPC 连接断开、TLS 错误） |

**逐命令输出**：

| 命令 | 默认 (JSON) | `--raw` | note |
|------|------------|---------|------|
| `run` | `{"stdout":"...","stderr":"...","exit_code":0,"session_id":"sess-xxx"}` | 流式 ExecStream，裸输出，exit code 表达成败 | --raw 走 ExecStream |
| `status` | `{"available":true,"session_id":"sess-abc","tenant_id":"my-project"}` | — | 始终 JSON |
| `create` | `{"session_id":"sess-xxx","pod_ip":"10.42.1.5"}` | — | 始终 JSON |
| `destroy` | `{"ok":true}` | — | 始终 JSON |
| `write` | `{"ok":true,"path":"/workspace/x.py"}` | — | 始终 JSON |
| `read` | `{"content":"...","path":"/workspace/x.py"}` | 裸文件内容 | |
| `list` | `{"files":[{"path":"...","modified":123}]}` | — | 始终 JSON |
| `subagent` | `{"session_id":"sess-parent-sub-xxx"}` | — | 始终 JSON |
| `confirm` | `{"approved":true/false,"approval_id":"..."}` | — | 始终 JSON，stderr 输出审批提示 |
| `approve` | `{"ok":true}` | — | 始终 JSON |

**错误输出**：`{"error":"...","detail":"..."}` + exit 1（业务）/ exit 2（基础设施）。

```bash
# 基础设施错误
$ k8e sandbox run "echo hello"
{"error":"sandbox not reachable","detail":"connection refused"}
exit code 2

# 业务错误
$ k8e sandbox destroy no-exist
{"ok":false,"error":"session not found"}
exit code 1
```

### 会话持久化

按 tenant 分目录的 state 文件：`~/.k8e/sandbox/{tenant}/state.json`

```
~/.k8e/sandbox/
  default/state.json          ← 无 --tenant 时
  my-project/state.json       ← --tenant my-project
```

**state.json 格式**：

```json
{
  "session_id": "sess-abc123",
  "phase": "active",
  "tenant_id": "my-project",
  "created_at": "2026-05-30T10:00:00Z"
}
```

**并发控制**（方案 B：轻量 flock）：

```
resolveSession(tenant):
  dir = ~/.k8e/sandbox/{tenant}/

  阶段 1：读取（持锁 ~100μs）
    flock(state.lock, LOCK_EX)
    state = read(state.json)
    if state.phase == "active" → unlock; return
    if state.phase == "creating" && now - state.locked_at < 30s → unlock; sleep(200ms); goto 阶段 1
    write(state.json, {phase:"creating", locked_at: now, pid: $PID})
    unlock

  阶段 2：创建（不持锁，~500ms-2s）
    resp = CreateSession(...)

  阶段 3：回写（持锁 ~100μs）
    flock(state.lock, LOCK_EX)
    write(state.json, {phase:"active", session_id: resp.SessionId, ...})
    unlock
```

**优先级**：

```
K8E_SANDBOX_SESSION_ID 环境变量    ← 最高
  ↓ (未设置)
~/.k8e/sandbox/{tenant}/state.json
  ↓ (未找到)
--tenant 的 FindActiveSession (CRD 查询)
  ↓ (未设置或未找到)
新建 session
```

**规则**：

- `run`（无 `--session-id`）+ `create` → 写 state
- `destroy` → 如果 session_id 匹配 state，清空 state
- `--session-id` 显式指定 → 不读不写 state。session 过期 → 返回错误让 Agent 决定

### confirm / approve 审批流

```
Agent CLI 进程                     gRPC Gateway                     Admin CLI 进程
─────────────────                  ────────────                    ────────────────
k8e sandbox confirm sess "action"
  [stderr] ⚠ Approval required     ← 立即输出提示（stderr）
  [stderr] approve xxx-12345       ← 立即输出审批命令（stderr）
  [阻塞 gRPC ConfirmAction]        ConfirmAction()
                                     ├── 注册 approval_id
                                     └── select {channel} ← 阻塞
                                                                    k8e sandbox approve xxx-12345
                                                                      → ApproveAction()
                                                                        ├── channel <- true
                                                                        └── {"ok":true}
                                     ← channel 收到 true
  [stdout] {"approved":true,...}    ← 返回结果
```

`confirm` stderr 输出（立即，不阻塞）：
```
[k8e-sandbox] ⚠ Approval required: delete /workspace/secret.txt
[k8e-sandbox]    To approve: k8e sandbox approve approval-abc-12345
[k8e-sandbox]    Timeout: 30s
```

### Proto 变更

`proto/sandbox/v1/sandbox.proto` 新增：

```protobuf
rpc ApproveAction(ApproveActionRequest) returns (ApproveActionResponse);

message ApproveActionRequest {
  string approval_id = 1;
  bool   approved    = 2;
  string reason      = 3;
}

message ApproveActionResponse {
  bool ok = 1;
}
```

gRPC 网关 `orchestrator.go` 新增 `ApproveAction` 方法（复用已有 `Approve()` 内部方法）。

## 文件变更

### 新增

```
pkg/sandboxcli/commands.go         # 9 个命令 handler
pkg/sandboxcli/session.go          # 状态文件读写 + tenant 查询 + flock 锁
pkg/sandboxcli/session_unix.go     # Unix flock 实现
pkg/sandboxcli/output.go           # JSON/raw 输出格式化
pkg/cli/cmds/sandbox.go            # sandbox 子命令组注册
```

### 删除

```
pkg/sandboxmcp/server.go           # MCP stdio + SSE server
pkg/sandboxmcp/tools.go             # 12 MCP tool schemas + handlers
pkg/cli/cmds/sandbox_mcp.go         # sandbox-mcp CLI 注册 + sandbox-install-skill
manifests/sandbox-matrix/mcp-sse-server.yaml  # SSE Deployment
```

### 修改

```
proto/sandbox/v1/sandbox.proto                          # 新增 ApproveAction RPC
pkg/sandboxmatrix/grpc/orchestrator.go                   # 新增 ApproveAction 实现
pkg/sandboxmatrix/grpc/server.go                         # 注册 ApproveAction
pkg/sandboxmcp/install.go                                # 简化为仅 installAllSkills
pkg/sandboxmcp/client.go                                 # 保留，被 sandboxcli 复用
cmd/server/main.go                                       # 注册 sandbox 命令组 + 删 sandbox-mcp
pkg/sandboxcli/skills/k8e-sandbox/SKILL.md               # 重写为 CLI 格式
```

## 实现任务

### Task 1 — Proto + ApproveAction

- `proto/sandbox/v1/sandbox.proto` 新增 `ApproveAction` RPC + message
- `protoc` 生成 Go 代码
- `pkg/sandboxmatrix/grpc/orchestrator.go` 新增 `ApproveAction` 方法
- `pkg/sandboxmatrix/grpc/server.go` 注册方法

### Task 2 — 会话持久化（`pkg/sandboxcli/session.go`）

- state 文件按 tenant 分目录：`~/.k8e/sandbox/{tenant}/state.json`
- `loadState(tenant) → *SessionState` — 读 state.json
- `saveState(tenant, state)` — 写 state.json
- `clearState(tenant)` — 清空 state
- `resolveSession(tenant, client)` — 完整三级策略 + flock 锁
- 复用 `pkg/sandboxmcp/client.go` 的 `FindActiveSession()` + `NewClient()`
- `session_unix.go` — `syscall.Flock` 实现（仅 Unix）

### Task 3 — CLI 命令实现（`pkg/sandboxcli/commands.go` + `output.go`）

10 个命令 handler，每个：解析参数 → 连接 gRPC → 调用 RPC → 格式化输出。

关键逻辑：
- `run`：代码来源 arg/stdin，--lang 包装，--raw 走 ExecStream
- `run`（python/node 多行）：WriteFile + Exec 两步
- `confirm`：stderr 立即输出审批提示，stdout 阻塞等结果
- `create` + `run`：写 state；`destroy`：命中则清空 state
- `--session-id` 显式：不读不写 state，session 过期返回错误
- 所有错误 JSON `{"error":"..."}` + exit 1/2

### Task 4 — 命令注册（`pkg/cli/cmds/sandbox.go`）

- `cmd/server/main.go` 注册 `k8e sandbox` 命令组
- 删除 `sandbox-mcp` 和 `sandbox-install-skill` 注册

### Task 5 — 删除 MCP 文件

- 删除 `pkg/sandboxmcp/server.go` `tools.go`
- 删除 `pkg/cli/cmds/sandbox_mcp.go`
- 删除 `manifests/sandbox-matrix/mcp-sse-server.yaml`
- 简化 `pkg/sandboxmcp/install.go`：只保留 `installAllSkills`，删 `mergeJSON`/`mcpEntryFor`/`readSandboxMCPAddr`

### Task 6 — SKILL.md 重写

`pkg/sandboxcli/skills/k8e-sandbox/SKILL.md` 改为 CLI 格式：
- 命令表格 + 使用示例
- stdin/heredoc 文件写入
- session 持久化（state 文件 + --tenant）
- 审批流（confirm + approve）

### Task 7 — 测试

- 单元测试：state 文件 CRUD、flock 并发、命令参数解析
- Mock 测试：gRPC client mock 验证 handler 行为
- 端到端：`k8e sandbox run` → `write` → `read` → `destroy` 完整流程

## 迁移指南

直接删除 `k8e sandbox-mcp`，无过渡期。开源项目，不考虑向后兼容。

Agent 配置变更：
- openclaw：`~/.openclaw/openclaw.json` 删除 `mcp.servers.k8e-sandbox` 配置，`k8e sandbox-install-skill openclaw` 仅安装 SKILL.md
- codex/claude/pi：通过 SKILL.md 学习 CLI 命令

| 旧 MCP Tool | 新 CLI 命令 |
|-------------|-----------|
| `sandbox_run` | `k8e sandbox run <code>` |
| `sandbox_status` | `k8e sandbox status` |
| `sandbox_create_session` | `k8e sandbox create` |
| `sandbox_destroy_session` | `k8e sandbox destroy <sid>` |
| `sandbox_exec` | `k8e sandbox run "cmd" --session-id <sid>` |
| `sandbox_exec_stream` | `k8e sandbox run "cmd" --session-id <sid> --raw` |
| `sandbox_write_file` | `echo "content" \| k8e sandbox write <sid> <path>` |
| `sandbox_read_file` | `k8e sandbox read <sid> <path>` |
| `sandbox_list_files` | `k8e sandbox list <sid>` |
| `sandbox_pip_install` | `k8e sandbox run "pip install pkg" --session-id <sid>` |
| `sandbox_run_subagent` | `k8e sandbox subagent <parent-sid>` |
| `sandbox_confirm_action` | `k8e sandbox confirm <sid> <action>` |

## 风险评估

| 风险 | 缓解 |
|------|------|
| CLI 参数注入 | 所有参数经 flag 解析，不接受原始 JSON |
| 状态文件并发写 | flock 锁 + 30s 超时恢复 + TTL GC 兜底 |
| 每次 CLI 调用新建 gRPC 连接 | TLS 握手 ~1ms localhost，Agent 场景不可感知 |
| confirm approval 纯内存，gateway 重启丢失 | 已有问题，非本 KIP 引入 |

## 相关 KIP

- [KIP-3](./kip-3-agentic-ai-sandbox-matrix.md) — Sandbox Matrix 核心设计
- [KIP-4](./kip-4-sandbox-mcp-skill.md) — 当前 MCP Skill 实现（本 KIP 替代对象）
- [KIP-5](./kip-5-openclaw-sandbox-management.md) — OpenClaw 集成（本 KIP 简化其配置）
