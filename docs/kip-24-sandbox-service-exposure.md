# KIP-24: Sandbox Service Exposure — k8e API Gateway 反代

| Author | Updated | Status |
|--------|---------|--------|
| @pi-agent | 2026-08-24 | Implemented (M1–M3: gRPC `ExposeService`/`UnexposeService`/`ListExposed`/`UpdateAllowedHosts`, e2b `/k8e/expose/*` reverse proxy, CLI `expose`/`unexpose`/`exposed`/`allow-hosts`, dsh plugin tools). M4 live-cluster e2e still pending. |

> Promoted from draft filename `sandbox-expose-tunnel.md`. Code, proto, CLI, and SKILL.md already used the KIP-24 name.

> 目标（2026-08-22 用户需求）：在沙箱内创建的服务（如网页服务）需要暴露给网关/外界访问。
> 采用 **k8e API Gateway 反代**：Cilium Gateway API（:80/:443）→ 内嵌 e2b HTTP server →
> 反向代理到 `http://<podIP>:<port>`（用户明确要求「暴露的端口通过 k8e API Gateway 暴露」，
> 非 CF tunnel）。同时支持 `k8e-sandbox-cli` 与 dsh 插件两条使用路径。

## 1. 背景与现状

- 沙箱 = 一个隔离 pod（RuntimeClass gvisor/kata/firecracker），服务只监听 pod 内部，
  外部/网关无法直接访问。
- 已有执行链：`k8e-sandbox-cli` → gRPC gateway（`pkg/sandboxmatrix/grpc`）→
  sandboxd（pod 内 Zig 守护，:2024 HTTP 面：`/exec`、`/exec/background`、`/exec/signal`、
  `/pty/*`、`/files/*`、`/ready`）。
- gateway 已有 `Exec` / `ExecBackground` / `ExecSignal` RPC 与 orchestrator 实现
  （`pkg/sandboxmatrix/grpc/orchestrator.go` §714 ExecBackground）。
- 现有 `pkg/daemons/control/tunnel.go` 是 K8s remotedialer（node↔server 控制面），
  **不是**服务暴露通道，本次不依赖它。

## 2. 方案：k8e API Gateway 反向代理

```
                  k8e API Gateway（Cilium Gateway API）      沙盒 pod
公网/网关  ──►  :80/:443 HTTPRoute ──► 内嵌 e2b HTTP server ──► http://<podIP>:<port>
                       ▲                     │ reverse proxy   （服务如 nginx:8080）
                       │  :50051 TCPRoute ────┘ gRPC gateway（同进程）
                 k8e-server（sandbox-matrix + e2b 内嵌）
```

- 暴露 URL：`http(s)://<gateway>/k8e/expose/<session>/<port>/`（Gateway API 的 :80/:443 入口）。
- **一键配置（LB = 主机私有 IP）**：Gateway `spec.addresses` 固定为 `%{ADVERTISE_IP}%`（stage 时解析为
  `--advertise-address` → apiserver bind → 默认路由网卡，永不为 loopback），Cilium 会把该 IP 直接
  赋给生成的 LoadBalancer Service —— 裸机/单机部署无需 MetalLB，无 `<pending>` 等待。expose URL base
  兜底链：`--sandbox-expose-base-url` → `--sandbox-advertise-hostname` → `http://<主机私有IP>` →
  `http://localhost`，默认零 flag 即返回可用 URL。
- **e2b HTTP server**（Gateway-API 前置的唯一 HTTP 面，`pkg/sandbox/e2b`）新增反代路由
  `/k8e/expose/{session}/{port}/*`：以 gateway 的 expose 注册表（`ListExposed` RPC）鉴权 →
  解析 pod IP（`GetSession`）→ `httputil.ReverseProxy` 到 `http://<podIP>:<port>`，
  路径去前缀、保留原始 Host。
- **gRPC gateway**（`pkg/sandboxmatrix/grpc`）维护 expose 注册表：`ExposeService` 校验会话+端口 →
  注册 → **重放 CNP**（给 gateway/e2b-server 加暴露端口 ingress 规则）→ 返回 URL；
  `UnexposeService` 注销 + 重放 CNP。
- 无需 cloudflared / 无需 pod 出站到 CF；暴露不改变 CNP 出站方向，只加网关入站规则。

### 2.1 CNP 变化（关键）

默认 CNP ingress 只放行 `:2024`（host/gateway/e2b-server）。暴露端口必须在 CNP 里加
gateway + e2b-server 两条 ingress 规则（`buildSessionCNPExposed`），否则反代被 Cilium 拦截。
`expose`/`unexpose`/`allow-hosts` 统一走 `applySessionCNP`（以注册表为源），互不覆盖。

## 3. gRPC 协议扩展（proto/sandbox/v1/sandbox.proto）

```proto
message ExposeServiceRequest {
  string session_id = 1;
  int32  port       = 2;   // 沙箱内服务端口（必填）
  string host       = 3;   // 监听地址，默认 127.0.0.1
}
message ExposeServiceResponse { string url = 1; }

message UnexposeServiceRequest { string session_id = 1; int32 port = 2; }
message UnexposeServiceResponse { bool ok = 1; }

message ExposedService { int32 port = 1; string url = 2; string host = 3; int64 started_at = 4; }
message ListExposedRequest { string session_id = 1; }
message ListExposedResponse { repeated ExposedService services = 1; }

rpc ExposeService(ExposeServiceRequest)   returns (ExposeServiceResponse);
rpc UnexposeService(UnexposeServiceRequest) returns (UnexposeServiceResponse);
rpc ListExposed(ListExposedRequest)       returns (ListExposedResponse);
```

- orchestrator 实现要点：`ExposeService` 幂等（同端口已暴露 → 返回既有 URL）；
  解析 URL 超时（默认 30s，可配）后失败返回，不泄漏僵尸进程
  （失败时对 run 发 `/exec/signal` 终止）；`ListExposed` 过滤已死 run。

## 4. k8e-sandbox-cli 命令（pkg/sandboxcli/commands.go）

```
k8e sandbox expose <port> [--host <addr>] [--json]
    # 注册端口到网关反代，返回网关 URL（即时生效）
    # {"url":"http://<gateway>/k8e/expose/<sid>/<port>/","port":8080}
k8e sandbox unexpose <port>            # 终止暴露
k8e sandbox exposed [--json]           # 列出当前会话已暴露服务
k8e sandbox allow-hosts --add a.com,b.com [--remove c.com] [--json]
    # 自由增删当前会话的出网白名单（动态生效，见 §4.3）
k8e sandbox get <sid>                  # 现有命令，增加输出 allowedHosts 当前值
```

- 复用现有 `session.go` 的 `FindActiveSession()`（跨进程会话复用）；
- 错误处理：会话不存在（ErrSessionGone）、端口未监听（URL 解析超时）、
  egress 白名单未放行（提示加 allowedHosts）。

### 4.3 allowedHosts 自由配置（动态生效）

现状：`k8e sandbox create --allowed-hosts a.com,b.com` 创建会话时已可传白名单；
**但会话创建后无法增删**（需改 CRD/删会话重建）。

新增动态能力（复用 orchestrator 已有的 `updateSession` + `applyCNP`）：

```proto
message UpdateAllowedHostsRequest { string session_id = 1; repeated string hosts = 2; }
message UpdateAllowedHostsResponse { repeated string hosts = 1; }
rpc UpdateAllowedHosts(UpdateAllowedHostsRequest) returns (UpdateAllowedHostsResponse);
```

- orchestrator：`spec.allowedHosts = hosts` → `updateSession`（持久化）→
  `applyCNP`（重放 CNP，FQDN 模式即时收紧/放开；默认 world 模式也更新声明面）。
- 语义：**整包替换**（增删都通过一次调用），返回新列表；空列表 = 清空（回落 defaultAllowedHosts）。
- 幂等：重复调用同值无副作用。

## 5. dsh 插件支持（plugins/deepseek-harness）

| 包 | 改动 |
|---|---|
| `dsh-k8e-sandbox-client` | `GrpcK8eClient` 增加 `exposeService(port, host?)` / `unexposeService(port)` / `listExposed()` / `updateAllowedHosts(hosts)`；`CliK8eClient` 同接口（shell out `k8e-sandbox-cli expose|allow-hosts`，Phase 1 本地无 endpoint 时兜底） |
| `dsh-k8e-sandbox-tool` | 新工具 `k8e_sandbox_expose {port, host?}` → 返回 URL；`k8e_sandbox_unexpose {port}`；`k8e_sandbox_allow_hosts {hosts[]}`（自由配置出网白名单，动态生效） |
| `dsh-k8e-sandbox`（owner） | `getExposed()` / `getAllowedHosts()` 状态查询（读 client 层）；Config.allowedHosts 保持部署级默认，运行时以工具/API 覆盖 |
| `dsh-k8e-sandbox-client-ui`（可选 Phase 2） | 设置页「出网白名单」自由编辑（接 allow-hosts API，动态生效） |
| 测试 | fake-ctx 单测（工具映射 + URL 透传）、grpc client 单测 |

典型 agent 流程：`k8e_sandbox_exec "python3 -m http.server 8080 --bind 127.0.0.1"`（后台）
→ `k8e_sandbox_expose {port:8080}` → 拿到 `http://<gateway>/k8e/expose/<sid>/8080/` 交付用户；
需要访问其它域名时 `k8e_sandbox_allow_hosts {hosts:["internal.example.com"]}` 自由放行。

## 6. 落地步骤

1. **M1** ✅ proto + pb 生成 + orchestrator `ExposeService/UnexposeService/ListExposed` + `UpdateAllowedHosts`
   （注册表 + CNP 重放 `applySessionCNP`/`buildSessionCNPExposed`）+ 单测
2. **M2** ✅ `k8e sandbox expose/unexpose/exposed/allow-hosts` CLI（结构化输出，get 输出 allowedHosts）+ 单测
3. **M2.5** ✅ e2b HTTP server 反代路由 `/k8e/expose/{session}/{port}/*`（ListExposed 鉴权 →
   pod IP → ReverseProxy，路径去前缀保留 Host）+ 单测（转发/404/400）
4. **M3** ✅ dsh 插件：`SandboxTransport` 接口 + `CliK8eClient`/`GrpcK8eClient` 实现
   `exposeService/unexposeService/listExposed/updateAllowedHosts`；模型工具
   `k8e_sandbox_expose` / `k8e_sandbox_unexpose` / `k8e_sandbox_allow_hosts`
   （经 `ctx.k8eSandbox` 共享会话）；CLI 4 命令补 `--session-id`/`--clear`；
   tool fake-ctx 测试扩展（注册 + 参数透传 + 输出形状）—— `npm test` 7/7、
   dsh typecheck 全绿
5. **M4** e2e（真实 pod 起 nginx → expose → curl `http(s)://<gateway>/k8e/expose/<sid>/<port>/` →
   unexpose → 404；allow-hosts 动态放行验证）

## 7. 风险与决策点

- **暴露语义 = 网关可达 + 公网（经 Gateway API）**：URL 走 k8e API Gateway（Cilium
  :80/:443），访问者需能到达 gateway 地址（`--sandbox-advertise-hostname` 用于远端）；
  路由无独立鉴权（expose 注册即公开）——与 CF quick tunnel 同语义，文档需说明。
- **CNP 入站**：暴露端口必须写入 CNP ingress（gateway+e2b-server），否则反代 502/超时；
  已由 `applySessionCNP` 统一处理，`expose/unexpose/allow-hosts` 互不覆盖。
- **Host 头**：反代保留原始 Host，pod 内服务可按 Host 路由；需要重写时后续加选项。
- **WebSocket/流式**：当前反代走标准 HTTP；WebSocket upgrade 由 httputil.ReverseProxy
  支持（1.12+），未专门测试——M4 e2e 覆盖。
- **allowedHosts 动态生效依赖 FQDN 模式**：默认 world 模式下改白名单不改变实际流量
  路径（仍是全放行 443），但更新声明面 + 为 FQDN 收紧模式做好准备；文档需说明。
- **gateway 可达性**：`exposeBaseURL` 默认 localhost（本地 loopback 部署），远端集群
  需配 `--sandbox-advertise-hostname`（KIP-22）才有可用 URL。
- **多节点**：e2b server 与 gRPC gateway 同进程内嵌（KIP-18 架构），注册表为进程内存态；
  gateway 重启后暴露注册丢失 —— 后续可落到 SandboxSession annotation（如 background-runs）。
