# KIP-12: Sandbox 计算层定位 — Ports/Preview 与 Env/Secrets 注入

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-24 | Implemented — env (`--env`) + secrets (`--secret`, CRD `SecretRef`, resolved at exec time) shipped; **ports/preview delivered by [KIP-24](./kip-24-sandbox-service-exposure.md)** as a k8e API Gateway reverse proxy (`/k8e/expose/<session>/<port>/`), not the Service+Ingress HMAC preview URL sketched in Part A below. Positioning ADR (harness vs compute) still stands. |

## Summary

明确 K8E sandbox 的产品定位为**面向 shell-first agent harness 的安全计算层（compute provider）**，并据此补齐两个能力缺口：

1. **Ports/Preview**：`k8e sandbox expose` 通过 K8s Service + Ingress 暴露 session 内的服务端口，返回**签名、限时、可撤销**的预览 URL。
2. **Env/Secrets 注入**：`CreateSession` 支持 `env`（非敏感）与 `secret_refs`（引用 K8s Secret），由网关在 **exec 时**解析并注入，保留 warm pool 延迟、敏感值不落本地 state 文件。

本 KIP 同时作为定位决策记录（ADR），界定哪些 OpenAI Agent Sandboxes 的能力**不**属于 K8E 范围。参考 [OpenAI Agent Sandboxes](https://developers.openai.com/api/docs/guides/agents/sandboxes) 设计对比。

## Motivation

### 层级区分：harness vs compute

OpenAI Agent Sandboxes 是一个 **harness**（控制面：agent loop、模型调用、handoff、审批、tracing、RunState），其沙箱执行层是**可插拔的 compute provider**（Docker / Unix-local / 托管商 E2B、Modal、Daytona、Cloudflare、Vercel…）。OpenAI 不运行沙箱，它**编排**沙箱。

K8E 恰好相反：它**就是** compute provider（K8s + gVisor/Kata/Firecracker + eBPF egress + warm pool），通过 gRPC/CLI 暴露，被 harness（Claude Code / codex / pi，见 KIP-8）**消费**。它没有模型循环、没有 agent 定义、没有 handoff —— 这是设计取舍，不是缺陷。

**结论**：真正的同侪是 OpenAI 的 provider 层（E2B/Modal/Daytona），而非整个 SDK。OpenAI 那些 K8E 没有的"功能"（SandboxAgent、memory、compaction、handoff）属于 **harness 职责**；K8E 的优势（统一强隔离、内核级 egress、warm pool、K8s 原生 mounts/ports 潜力）属于 **compute 职责**。

### 优劣势对比（K8E vs OpenAI provider 层）

| 维度 | K8E | OpenAI |
|------|-----|--------|
| 隔离强度 | gVisor/Kata/Firecracker，**默认统一强隔离** | 取决于 provider；Unix-local 无隔离、Docker 弱 |
| 网络管控 | 内核级 eBPF egress，默认拒绝 | provider-specific / 网关中介 |
| 冷启动 | warm pool ~10ms claim，内存感知准入 | provider 而定，常见冷启动 |
| 部署 | 自托管单二进制，数据不出域 | 第三方托管 SaaS |
| 接入 | 通用 CLI，任何能跑 shell 的 agent | 需实现 `SandboxClient` SDK 契约 |
| 审批 | **compute 侧**强制 (`confirm`/`approve`) | harness 侧（模型可绕过） |
| SDK provider 生态 | ❌ 不接入（CLI-only，见决策） | ✅ 原生 provider 表 |
| 外部存储 mounts | ❌ 不做（见决策） | ✅ S3/GCS/R2/Azure/Box |
| Ports/Preview | ❌ → **本 KIP 补齐** | ✅ |
| Env/Secrets | ❌ → **本 KIP 补齐** | ✅ manifest.environment + provider secret |
| Memory / 模型循环 / handoff | ❌ 不做（harness 职责） | ✅ |

### 为什么是这两个能力

在 shell-first coding agent 的真实负载下，分流后只有两项高价值且贴合 K8E 用户：

- **Ports/Preview**："把我的 app 跑起来给我看" —— coding agent 的高频诉求，K8E 的 K8s 原生 Service/Ingress 是结构性优势。
- **Env/Secrets**：被测代码需要 API key、registry token 等运行期配置 —— 当前 `CreateSession` 无 env 字段，是真实空缺。

外部存储 mounts、能力裁剪（capability scoping）、memory、模型循环**明确不在本 KIP 范围**（理由见决策记录）。

## Design

### Part A — Ports/Preview

#### 命令

```bash
# 暴露 session 内监听在 <port> 的服务，返回预览 URL
k8e sandbox expose <session-id> <port> [--ttl 3600]
#   → {"url":"https://preview.k8e.example/p/sess-abc/8080/<token>/","expires_at":"..."}

# 撤销（也在 destroy / GC 时自动清理）
k8e sandbox unexpose <session-id> <port>
```

#### 架构

```
浏览器 / agent
  │  GET https://preview.<domain>/p/<sid>/<port>/<signed-token>/...
  ▼
Ingress (nginx)  ──auth_request──▶  gateway /preview/verify
  │                                   校验 HMAC token + 未过期 + session active → 200/403
  ▼ (200)
Service (selector: k8e.sandbox/session=<sid>)
  ▼
session pod :<port>
```

- **路由**：K8s 原生 Service（按 `k8e.sandbox/session=<sid>` label 选中**唯一** session pod）+ Ingress 按 `/p/<sid>/<port>/` 前缀转发。
- **鉴权**：签名限时 token。Ingress 用标准 `external-auth`/`auth_request` 注解回调网关 `/preview/verify`；网关校验 HMAC（载荷含 `sid`、`port`、`exp`，用 server key 签名）+ session 仍 Active。**token 本身即凭证，浏览器无需 API key**，符合预览分享的本意。
- **生命周期**：Service/Ingress 带 session label，`DestroySession` 与 session GC 时一并删除；token TTL 默认随 session TTL。

#### 与 warm pool 的关系

**无冲突**。Service/Ingress 在 `expose` 时（claim 之后）创建，selector 指向已运行的 pod。前提：warm pod 在被 claim 时即打上 `k8e.sandbox/session=<sid>` label（使 Service 精确选中单一 pod）。

#### 新增 gRPC

```protobuf
rpc ExposePort(ExposePortRequest)     returns (ExposePortResponse);
rpc UnexposePort(UnexposePortRequest) returns (UnexposePortResponse);

message ExposePortRequest  { string session_id = 1; int32 port = 2; int32 ttl_seconds = 3; }
message ExposePortResponse { string url = 1; int64 expires_at = 2; }
message UnexposePortRequest  { string session_id = 1; int32 port = 2; }
message UnexposePortResponse { bool ok = 1; }
```

### Part B — Env/Secrets 注入

#### CreateSession 扩展

```protobuf
message CreateSessionRequest {
  string          session_id    = 1;
  string          tenant_id     = 2;
  repeated string allowed_hosts = 3;
  string          runtime_class = 4;
  map<string,string> env        = 5;  // 非敏感，明文存于 SandboxSession CRD
  repeated SecretRef secret_refs = 6; // 仅存引用，值在 exec 时解析
}

message SecretRef {
  string secret_name = 1;  // 同 namespace 下已存在的 K8s Secret
  string key         = 2;  // Secret.data 中的键
  string env_var     = 3;  // 注入到进程的环境变量名
}
```

CLI：

```bash
k8e sandbox create \
  --env LOG_LEVEL=debug --env PYTHONPATH=/workspace/lib \
  --secret OPENAI_API_KEY=my-llm-secret:api_key      # env_var=Secret:key
```

#### 注入路径（exec 时，warm pool 兼容）

```
create:  env + secret_refs 写入 SandboxSession CRD（secret_refs 仅存引用，不存值）
              │
exec:    gateway 读取 session 的 env + secret_refs
              │  对每个 secret_ref：以网关 SA 的 RBAC 读取 K8s Secret → 取值
              ▼
         合并 {inline env + 解析后的 secret env}，放入 /exec 请求体的 env 字段
              │  （走既有 gateway→sandboxd :2024 内部 TLS 通道）
              ▼
         sandboxd 将 env 设入子进程环境后执行命令
```

- **为何在 exec 注入而非 pod spec**：运行中的 pod 无法追加 env/secretKeyRef/volume；warm pod 是通用预热的。exec 时注入使 warm pod 仍可复用，保住 ~10ms claim 延迟。
- **sandboxd `/exec` body** 新增 `env: map<string,string>`；`exec.zig` 在 fork 后、exec 前将其写入子进程环境。
- **网关 RBAC**：gateway ServiceAccount 需 `get` 同 namespace 的 Secrets，以及 `create`/`delete` Services 与 Ingresses（供 Part A）。

#### 敏感值卫生

- 解析后的 secret **值**只在单次 exec 的请求体与 pod 内存中存在，**不写入 CRD、不写入** `~/.k8e/sandbox/{tenant}/state.json`。
- CLI state 文件仅持久化 `session_id`（reconnect 所需），env/secret_refs 由服务端 session 对象保存并在每次 exec 重放。
- ⚠️ **inline `env` 明文存于 CRD**，凡有 RBAC 读 SandboxSession 者可见 —— 真正敏感的值**必须**用 `--secret`（CRD 只存引用），不可用 `--env`。SKILL.md 需写明这条红线。

### 决策记录（ADR）

| 决策 | 选择 | 理由 |
|------|------|------|
| **定位** | 安全计算层 provider，面向 shell-first harness | K8E 全部差异化都在 compute 层；与 harness 竞争是必输之战 |
| **分发** | 仅 CLI/skill（不做 SDK adapter、不做 MCP server） | 延续 KIP-8；代价：OpenAI-SDK agent 不会把 K8E 作为托管 provider，可达市场=能跑 shell 的 agent |
| **resume 机制** | 以 `snapshot`（KIP-10）为主，不做可序列化 session-state | CLI-only 无 harness RunState 可对接 |
| **本期能力** | Ports/Preview + Env/Secrets | 最贴合 shell-first coding agent |
| **不做** | 外部存储 mounts、capability scoping、memory、模型循环/handoff | mounts 对 coding agent 价值低；memory/loop 是 harness 职责 |
| **实现风格** | **优先标准 K8s 原语，网关保持薄** | 贯穿全程的设计偏好：CLI 优于 SDK adapter、Ingress 优于自研代理、Secret 引用优于自研密钥库 |

### 边界处理

| 场景 | 行为 |
|------|------|
| `expose` 的 session 已销毁/非 Active | 报错 "session not active" |
| 预览 token 过期 | Ingress external-auth 返回 403 |
| 同一 (sid, port) 重复 expose | 复用现有 Service/Ingress，签发新 token |
| `secret_refs` 指向不存在的 Secret/key | `CreateSession` 不校验值，首次 exec 解析失败 → 报错并将命令置为失败 |
| 集群无 Ingress controller | `expose` 报错并提示需部署 ingress；其余命令不受影响 |
| warm pod 未打 session label | Service 选不中 → `expose` 报错（claim 时打 label 为前置条件） |
| inline `env` 误放敏感值 | 文档红线 + （可选）对疑似密钥名告警到 stderr |

### 文件变更

| 文件 | 变更 |
|------|------|
| `proto/sandbox/v1/sandbox.proto` | 新增 `ExposePort`/`UnexposePort` RPC 与消息；`CreateSessionRequest` 加 `env`、`secret_refs`；`SecretRef` 消息 |
| `pkg/sandboxmatrix/grpc/server.go` | `ExposePort`/`UnexposePort` handler；`Exec`/`ExecStream` 中解析 secret_refs + 合并 env 注入 `/exec` body |
| `pkg/sandboxmatrix/grpc/orchestrator.go` | 创建/删除 Service+Ingress；claim 时打 session label；session 上存取 env/secret_refs |
| `pkg/sandboxmatrix/api/v1alpha1/` | `SandboxSession` 增加 `Env`、`SecretRefs`、`ExposedPorts` 字段 |
| `sandboxd/src/exec.zig` | `/exec` 支持 `env` map，设入子进程环境 |
| `pkg/sandboxcli/commands.go` | 新增 `expose`/`unexpose` 命令；`create` 增 `--env`、`--secret` flag |
| `pkg/cli/cmds/sandbox.go` | 注册 `expose`/`unexpose` 子命令 |
| 部署清单 / RBAC | gateway SA：`get` Secrets、`create`/`delete` Services 与 Ingresses |
| `pkg/sandboxcli/skills/k8e-sandbox/SKILL.md` | 文档化 expose 预览与 env/secret 用法 + 敏感值红线 |

## 相关 KIP

- [KIP-3](./kip-3-agentic-ai-sandbox-matrix.md) — Sandbox Matrix 总体设计
- [KIP-8](./kip-8-skill-cli-replace-mcp.md) — CLI 替换 MCP（本 KIP 的定位前置：shell-first / CLI-only）
- [KIP-9](./kip-9-sandbox-workspace-manifest.md) — Workspace manifest（env 的 manifest 形态可作为后续）
- [KIP-10](./kip-10-sandbox-snapshot.md) — Snapshot（CLI-only 下的主 resume 机制）
- [KIP-11](./kip-11-background-sandbox-execution.md) — 后台执行（与 env 注入共享 gateway→sandboxd exec 路径）
