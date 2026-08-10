# KIP-16: Sandbox Architecture Lessons from Ephemeral-Sandbox

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods (agent-assisted) | 2026-08-10 | Draft → Wave 1 implemented (R3/R4) |

## Summary

对 [Ephemeral-AI-Lab/ephemeral-sandbox](https://github.com/Ephemeral-AI-Lab/ephemeral-sandbox)（Rust workspace，~105k LOC / 20 crates，面向 coding agent 的隔离执行沙箱）做架构深潜，提取可移植的设计教训，对照 K8E `SandboxService`（gRPC + CLI + sandboxd）现状产出 lessons/gap 矩阵，为 K8E 沙箱子系统的下一波演进（transcripts、snapshots、observability、协议加固）提供依据。

**一句话结论：** e-sandbox 的机制（workspace-session 覆盖层、content-addressed layerstack、file-backed PTY transcript 窗口、disk-only observability、in-band 认证与 readiness 握手）与 K8E 的待补短板高度互补；但其**交付形态**（Rust crates、多可执行文件、Docker-provider-only、NDJSON-over-TCP）与 K8E 非协商约束（单二进制 <100MB、gRPC-first、K8s-native + RuntimeClass 加固隔离）冲突，只取机制、不取形态。

## Sources

| Source | Role |
|--------|------|
| Ephemeral-sandbox source probe at `/tmp/ephemeral-sandbox-probe`（3-agent workflow：2 parallel surveys + synthesis，全部 claims 已对源核验） | 参考实现（lessons 来源） |
| K8E repo survey（`pkg/sandboxmatrix`、`sandboxd/`、`pkg/sandboxcli`、`proto/sandbox/v1`） | K8E 现状 |
| [KIP-15](./kip-15-sandbox-api-perplexity-alignment.md) | 既有完备性评审（本 KIP 的 gap 对齐基线） |
| [KIP-10](./kip-10-sandbox-snapshot.md) / [KIP-11](./kip-11-background-sandbox-execution.md) | 既有实现（snapshot / background） |

## Motivation

K8E 与 e-sandbox 解决同一个 job-to-be-done：**为 coding agent 提供隔离的确定性执行环境**。但 e-sandbox 在四个 K8E 明确缺失的领域（transcript/PTY 回放、内容寻址快照、observability、协议/认证硬化）有成熟、可核验的工程实践，且其源码结构（contract/catalog/client 分层）与 K8E 的「gRPC proto 为唯一契约、CLI/skill 为 adapter」主张同构。与其从零发明，不如系统性地借力 —— 同时守住 K8E 的非协商约束：

1. **单二进制 <100MB**（Zig build）→ 不引入第二套语言运行时或新服务进程。
2. **gRPC-first**（KIP-15）→ 不新增 NDJSON-over-TCP 第二线协议。
3. **K8s-native + RuntimeClass 加固隔离**（gVisor/Kata/Firecracker）→ 不降级为「容器内 namespace」信任边界。
4. **Warm pool** 是 K8E 差异化能力 → 任何新机制必须与 warm/claim/reset 生命周期兼容。

## 1. What ephemeral-sandbox does well (lessons)

每条 lesson = 具体机制 + K8E 对应物（已有或缺失）。

### L1. Workspace sessions over one shared sandbox base
- **机制：** 一个 sandbox（daemon + layer stack）服务 N 个 `WorkspaceSession`；每个 session 在**自己的 mount namespace** 里把独立 overlay（`scratch_root/<sid>/{upper,work}`，lowerdirs = 共享只读 layer）挂载到**同一个** `workspace_root`，宿主侧零冲突（crates/sandbox-runtime/workspace，`WorkspaceManager` / `spawn_ns_holder` / `setns_runner.rs::mount_overlay`）。复用单元是 **workspace session**，不是 sandbox。
- **K8E 对应：** 1:1 session→pod；warm pod 复用必须走完整 reset（destroy → `POST /workspace/reset` → resetting → warm）；PVC session 永远冷启动。**缺失**第二级复用单元。

### L2. Content-addressed layerstack + OCC publish + lease GC
- **机制：** `Manifest{version, layers}`（schema v1）+ SHA-256 layer digest；CAS 存储（immutable `layers/` + `staging/` + `.layer-metadata/` sidecars，tmp+fsync+rename）；发布是**乐观并发**（`publish_layer` 对 base revision 校验，mismatch → `ManifestConflict{expected,found}`）；`plan_publish` 三路合并（line-level Myers，8MB 上限）；`delta_layer_refs` 生成 **zstd tar 增量流**（逻辑 whiteout `.wh.<name>`，仅 mode+mtime）；快照访问由 **lease** 门禁，GC 是 lease-release 驱动（release 只删无引用层）；`squash_at_n_layers: 100` 自动扁平化（单次 syncfs 提交）。
- **K8E 对应：** KIP-10 snapshot 是**客户端 tar.gz 全量打包**（非内容寻址、无增量、无服务端制品注册表），且大 workspace 直接失败（sandboxd 单读 ~64KB WriteFile 上限 / 10MB 读上限 / gRPC 默认 4MB）。`ListFiles` mtime 恒为 0，无 since/diff。

### L3. Operation contract / catalog / client layering
- **机制：** `sandbox-operations` 分组：**contract**（纯静态 `OperationSpec{name,family,args}`）、**catalog**（"single semantic operation catalog and route manifest"，Cargo features `manager/runtime/observability` 门控 domain 模块，`Routing::SystemOrSandbox` 编译期展开为 2 条 route）、**client**（`build_request_from_values` 统一校验 unknown args / scope-vs-policy / defaults）。语义定义一次，adapter 投影（clap / JSON-Schema）只消费 catalog。
- **K8E 对应：** gRPC proto 是唯一契约（✓ 同构于 contract）；但 CLI flag 与 RPC 无机器可读映射（手工 `commands.go`），无 catalog 文档/校验，SDK 生成无单一源。**部分缺失**。

### L4. File-backed PTY transcript windowing + replay
- **机制：** PTY master 在 daemon 进程；全局 `OUTPUT_REACTOR` 线程非阻塞 8KB 分块把每个 master FD 排入 per-reader sink；`CommandTranscriptRow{offset,stream,text}` 按行落盘（file sink 带 `[ISO8601]` 前缀）；读取是 `transcript_window(path, offset, limit, max_window_bytes)` 的**有界尾读**（seek 到 `len-max`、行对齐、计 `truncated_before`）→ `CommandTranscriptWindow{offset,next_offset,truncated_before,output}`。**窗口从文件回读，不驻内存**；stdin 写满时非阻塞+poll+deadline；后台执行全部是 reaped child（`CompletionSupervisor`，`max_active: 32`），无 detached daemon。
- **K8E 对应：** **完全没有** transcript/PTY/replay（grep proto/sandboxmatrix/sandboxcli/sandboxd/docs 零命中）。`ExecStream` 是原始 stdout 分块（SSE，4200B 行缓冲），无 stderr 交错、无偏移续读、无持久执行日志。

### L5. Disk-only observability + activity-gated sampling + process topology
- **机制：** NDJSON `Record::{Span,Event,Sample}`（Span 带 trace/parent/status，id `<proc>-<seq>`）；sink 固定容量两段轮转（`max_disk_bytes` 4MiB 默认，flock 跨进程，fail-open）；**读永不触发采集**、idle daemon 零工作、零内存历史（disk-only state spec：≤1MiB query heap、无 mmap、`AnonHugePages=0`）；宿主侧按 cgroup/Docker 采样（不唤醒 daemon，`activity_revision` 单调计数门控轮询）；进程拓扑按 **namespace identity**（`/proc/P/ns/pid` + `ns/mnt` 与 holder 比对）归属，无需 CAP_SYS_ADMIN、只读 cgroup 挂载也能用；响应上限 500 记录 / 256KiB。
- **K8E 对应：** 无 Prometheus/OTel 端点；counter 是进程内原子量（gateway 重启即清零）；无事件流（execs/files/claims/approvals 无记录）；sandboxd 无进程拓扑。`SandboxMatrix.status` 只暴露均值与计数。

### L6. Provider abstraction (manager port traits)
- **机制：** manager 定义端口 trait（`SandboxRuntime` / `SandboxDaemonInstaller` / `SandboxDaemonClient`），Docker provider 实现之；**全部状态 label-driven**（`eos.sandbox_id` / `eos.auth_token` / resource profile，gateway 重启后 `recover_sandboxes()` 从 label 重建）；容器 spec `cap_add:[SYS_ADMIN,NET_ADMIN]`、`no-new-privileges`、`cgroupns_mode:PRIVATE`、`init:true`、loopback-only 发布端口。
- **K8E 对应：** 运行时可插拔已由 **RuntimeClass**（gvisor/kata/firecracker）承担，等价于 provider 抽象的一半；但「控制面跟踪/恢复从 label 重建」没有对等物（session 状态在 CRD + 客户端 state 文件，pod label 仅用于 warm claim）。

### L7. Protocol framing, in-band auth, explicit limits, readiness handshake
- **机制：** NDJSON 一请求一响应；认证是 **in-band JSON 字段**（`_sandbox_daemon_auth_token` / `_sandbox_gateway_auth_token`，非 header），unix socket 免认证、TCP 必认证；限额 `max_request_bytes: 16MiB` + `request_read_timeout_s: 30`，`take(limit+1)` 拒绝超大；**readiness 是应用层握手**（`OperationRequest{op:"sandbox_daemon_ready", id:"docker-readiness"}` → 必须回 `status:"ready"` + 匹配 `sandbox_id`；"A bare TCP connect through Docker's port proxy is not a reliable readiness signal."）；HTTP 面是显式 allowlist 4 条路由（`/health` 原子量、`/files/list` 唯一 HTTP-only 例外、`/forward/...` 转发），并文档化「HTTP 无应用认证，勿对外发布」。
- **K8E 对应：** 协议层有真实短板：sandboxd 单请求 64KB 读上限（`main.zig`）、10MB 读上限、gRPC 未配 `MaxRecvMsgSize`（默认 4MB）；sandboxd `:2024` **无认证**（全靠 CNP）；warm pool 健康检查是 **1.5s TCP dial** `:2024`（`warmPodHealthCheck`），非应用层就绪握手 —— 与 e-sandbox 明确否定的做法一致。

### L8. Single catalog, feature-gated projection, no combined executable
- **机制：** 一个 operation catalog + Cargo features 门控 → 3 个 CLI 可执行文件（manager/runtime/observability）+ 1 个 MCP 二进制（`--set management|runtime|observability`）；`validate_projection` 强制 1:1 name/order/arg/flag 唯一性；契约+目录+客户端钉死在不可变 core commit（外部 adapter 消费不可变版本）。
- **K8E 对应：** K8E 单二进制是硬约束（多可执行文件**不复制**）；对等可取的是「**单一语义源 + 每 adapter 投影**」：proto 为权威，CLI/skill/SDK 从同一源生成/校验（当前 CLI 手工维护，无投影校验）。

### L9. Security posture honesty + transactional destroy + PDEATHSIG recovery
- **机制：** README 明示 non-guarantee：*"It is not a hardened microVM boundary for mutually untrusted tenants."*（诚实声明边界）；destroy 是 **`TeardownTransaction` 可重试 ledger**（kill holder → ns fds → mounts → leases → veth，任一步失败可重试）；崩溃恢复靠 **PDEATHSIG**（holder 随 daemon 死亡 → 持久化记录 provably dead → `reap_persisted_handles` 直接收尸，**无需分布式 liveness 协议**）。
- **K8E 对应：** K8E 隔离**更强**（RuntimeClass 微 VM/系统调用过滤），但无威胁模型文档；`allowed_hosts` 存入 CRD 却**未执行**（CNP 是 blanket `world:53/443`）；`DestroySession` 是顺序步骤而非可重试 ledger；`run_registry` 重建是空 stub（重启后 in-flight PollRun 404）。

## 2. Gap / Lessons matrix

列：Lesson（e-sandbox 机制）| K8E today | Proposed improvement | Priority | Effort | Risk

| # | Lesson from ephemeral-sandbox | K8E today | Proposed improvement | Pri | Effort | Risk |
|---|------------------------------|-----------|----------------------|-----|--------|------|
| M1 | **Workspace-session isolation over shared sandbox**（per-session mount-ns overlay on shared `workspace_root`，复用单元 = session） | 1:1 session→pod；warm 复用需全量 reset；PVC session 必冷启动；`RunSubAgent` 是子 pod（同 PVC） | 第二级会话单元：pod 内 per-session overlay + cgroup（仅作为**优化**，信任边界仍 pod 级）；或先落地「subagent 复用父 pod overlay」 | P1 | L | M |
| M2 | **Content-addressed layerstack**（SHA-256 CAS、OCC publish `ManifestConflict`、lease GC、zstd delta、autosquash） | KIP-10 客户端 tar.gz 全量；大 workspace 因 64KB/10MB/4MB 上限失败；`ListFiles` mtime=0 无 diff | 服务端内容寻址快照层（纯 Go，无 CSI）：immutable layers + staging + lease GC + `ListFiles(since)` 落地；增量/恢复/模板复用 | P1 | L | M |
| M3 | **Operation contract/catalog/client 分层**（静态 spec、route manifest、`build_request_from_values` 单点校验） | proto 为唯一契约 ✓；CLI 手工映射、无 catalog 校验、SDK 无生成源 | proto 注释即 catalog 种子 → 生成/校验 CLI flags 与 SDK stub；`SandboxService` 路由表（system vs sandbox scope）显式化 | P2 | M | L |
| M4 | **PTY transcript windowing + replay**（master 在 daemon、`OUTPUT_REACTOR`、file-backed `transcript_window` 有界尾读、offset 续读） | 无 transcript/PTY/replay；`ExecStream` raw stdout 有损（4200B 行缓冲、无 stderr 交错、无持久日志） | sandboxd 增加 PTY + 文件转录（`/workspace/.k8e_transcripts/<sid>.log`）+ 窗口化读取 RPC；gRPC `GetTranscript`（offset/limit）；stderr 交错保留 | P1 | M | M |
| M5 | **Disk-only observability + activity-gated sampling + namespace-identity process topology**（NDJSON Span/Event/Sample、读不触发采集、≤1MiB 查询堆、500/256KiB 响应上限） | 无 Prometheus/OTel；counter 进程内、重启清零；无事件流、无进程拓扑 | gateway 暴露 Prometheus 端点（warm/cold/claim latency histogram）；sandboxd NDJSON 事件流（exec/files/claims/approvals）存 PVC 侧 or 控制面 ring；采样按 `activity_revision` 门控 | P1 | M | L |
| M6 | **Provider abstraction**（manager port traits：Runtime/Installer/DaemonClient；label-driven 恢复） | RuntimeClass 已承担运行时可插拔 ✓；控制面跟踪无 label 重建对等物（状态在 CRD + 客户端文件） | 不为 Docker provider 引入新抽象（K8s-native 已覆盖）；仅借鉴「从 pod label/CRD 重建 in-flight 状态」（修 `run_registry` stub） | P2 | L | M |
| M7 | **Protocol framing/auth/limits**（16MiB/30s 限额、`take(limit+1)` 拒超大、in-band auth token、unix-socket 免认证/TCP 必认证） | sandboxd 单读 64KB（WriteFile 碎化）、10MB 读上限、gRPC 默认 4MB；`:2024` 无认证（仅 CNP） | 修上限：gRPC `MaxRecvMsgSize` 配置 + streaming WriteFile/ReadFile；sandboxd 分块读（如 1MiB/请求）或流式；每 session 注入 token，sandboxd 校验 | P1 | S–M | L–M |
| M8 | **Warm-pool readiness handshake**（`sandbox_daemon_ready` 应用层握手：status ready + 匹配 sandbox_id，显式否定 bare TCP） | `warmPodHealthCheck` 1.5s TCP dial `:2024` + Running/Ready；venv 惰性初始化可能在 claim 后才完成 | sandboxd 增加 `POST /ready`（返回 sandbox_id + venv 状态）；warm claim 前调用；TCP dial 降级为兜底 | P1 | S | L |
| M9 | **Single catalog, multi-adapter projection**（feature 门控、3 exe + MCP `--set`、projection 校验） | 单二进制（多 exe **不复制**）；CLI 手工维护 | 取「单一语义源 + 投影校验」：proto/catalog → CLI flag 与 Python/TS SDK 生成；MCP 若做也吃同一 catalog（KIP-8 CLI-first 不推翻） | P2 | M | L |
| M10 | **Security posture honesty + in-band daemon auth**（README non-guarantee、HTTP 面 allowlist + 文档化无认证边界） | 无威胁模型文档；`allowed_hosts` 未执行（blanket `world:53/443`）；sandboxd 无认证、无审计（仅证书签发审计） | (a) 执行 `allowed_hosts`（`toFQDNs` 或 egress proxy，KIP-15 G10/G11）；(b) `:2024` 每 session token；(c) 发布 threat-model + non-guarantees 文档（README/skill） | P0 (a) / P1 (b,c) | M | M |
| M11 | **Transactional destroy + PDEATHSIG recovery**（`TeardownTransaction` 可重试 ledger；parent-death 使持久化记录 provably dead） | `DestroySession` 顺序步骤（Terminating → CNP 删 → reset → relabel → CRD 删），非幂等 ledger；pod 级等价物 = kubelet 容器退出 | destroy 改幂等分步 + 每步校验（可重试、可续跑）；依赖 kubelet/容器退出态做 provably-dead 判定，补 `run_registry` 重建 | P2 | M | M |
| M12 | **Capped reaped background execution**（无 detached daemon、`CompletionSupervisor`、`max_active: 32`、terminal order 有界） | KIP-11 background 用 EXIT-trap + `.k8e_bg` 文件；无 per-session cap；`run_registry` 重建 stub | 强制 `max_background_runs`（默认 5，Perplexity parity，KIP-15 G9）；补全 `RebuildRunRegistry`；结果文件上限对齐 | P1 | S | L |

## 3. Ranked recommendations

优先级同时考量：K8E 目标（隔离 agent 执行 at scale、warm pool、gRPC-first、单二进制 <100MB）、e-sandbox 机制成熟度、成本收益。

| # | Recommendation | Rationale (tied to k8e goals) | Matrix rows |
|---|----------------|-------------------------------|-------------|
| R1 | **服务端内容寻址快照层（layerstack-lite，纯 Go）** | 直接修复 KIP-10 大 workspace 失败 + 无 diff 的硬伤；内容寻址 + lease GC 让 snapshot 成为可扩展制品（模板复用、阶段间续跑），是「agent 执行 at scale」的地基；无 CSI 依赖、纯 Go 实现符合单二进制约束 | M2, M7 |
| R2 | **PTY transcripts + 窗口化回放（`GetTranscript` RPC）** | 把有损 `ExecStream` 升级为可回放、有界内存的执行日志（file-backed 窗口读回）；agent 调试/审计/重放直接受益；gRPC-first（新 RPC 进 proto，CLI `log` 命令为薄 wrapper） | M4 |
| R3 | **Warm pool 应用层就绪握手（`POST /ready`）** | 成本 S、风险 L，收益直接：TCP dial 是 e-sandbox 明确否定的不可靠信号；握手校验 sandbox_id + venv 就绪后 claim，杜绝「claim 了还没初始化完的 pod」—— 直接服务 warm pool 目标 | M8, M7 |
| R4 | **协议/认证加固：修上限 + 每 session in-band token** | 64KB/10MB/4MB 上限让真实负载（大文件、快照恢复）必然失败；streaming WriteFile/ReadFile 修复 gRPC 路径；`allowed_hosts` 执行（P0 安全项，KIP-15 G10/G11 呼应） | M7, M10 |
| R5 | **Observability：Prometheus 端点 + NDJSON 事件流（activity-gated）** | 「at scale」运营必需：warm/cold 命中率、claim 延迟直方图（现只有均值）、exec/approval 事件审计；e-sandbox 的 disk-only + 读不触发采集模式保证 sandboxd 零额外唤醒；gateway 侧实现，不新增进程 | M5, M11 |

## 4. Non-goals（明确不复制）

| e-sandbox 形态 | K8E 决策 | Why |
|----------------|----------|-----|
| Rust workspace（20 crates）+ 多可执行文件（3 CLI exe + MCP 二进制） | **不复制** | 违反单二进制 <100MB 硬约束；K8E 的「多 adapter」只体现在同一二进制内的子命令 / 外部薄 SDK |
| Docker-provider-only 运行时模型（容器内 `cap_add SYS_ADMIN/NET_ADMIN` + namespace 隔离） | **不复制** | K8E 信任边界是 RuntimeClass 加固隔离（gVisor/Kata/Firecracker）；e-sandbox 自身声明"not a hardened microVM boundary for mutually untrusted tenants" |
| **Workspace-session 共享作为安全边界**（per-session mount-ns overlay 摊在一个 daemon 下） | **不复制为信任边界** | 多租户互不信任时该模型明确不合格；仅作为 pod 内部复用的**优化**，信任边界保持 pod 级（M1 注明） |
| NDJSON-over-TCP 第二线协议 | **不复制** | gRPC-first（KIP-15）；sandboxd HTTP `:2024` 保持为 pod 内实现细节，不入公共 API |
| OCC 三路 line-merge（Myers，8MB） | **暂缓** | 内容寻址 + 乐观并发先落地；三路合并等真实并发冲突场景出现后再评估（避免为 0.1% 场景引入复杂度） |
| MCP stdio 服务器 / MCP-first adapter | **不复制** | KIP-8 已定 CLI-first + skill 分发；MCP 若做，也吃同一 proto/catalog（M9），不另起语义 |
| 每 workspace 级 cgroup CPU/mem/PID 强制 | **不复制** | e-sandbox 自身列为 non-goal；K8E 在 pod/RuntimeClass 层强制即可 |

## 5. Acceptance criteria for KIP-16 approval

1. **Matrix triaged**：M1–M12 每行 P0/P1 项均有一条对应 GitHub issue（owner + effort estimate）；P2 项记录保留/延后理由。
2. **Top-3 落地**：R1/R2/R3 进入具体设计/实现 KIP（或显式否决并记录理由），时间窗对齐 KIP-15 Wave 1–2。
3. **Non-goals 批准**：维护者确认 §4 不复制清单；任何后续提案不得违反单二进制 <100MB、gRPC-first、RuntimeClass 隔离三约束。
4. **安全项闭环**：`allowed_hosts` 执行决策（执行 toFQDNs / egress proxy / 或从 API 移除并文档化）在批准前有明确结论；threat-model + non-guarantees 文档（README/skill 段落）合入。
5. **Living doc**：矩阵在实现推进中更新为 Adopted/Deferred/Rejected；e-sandbox 的 agent-sandbox 安全模型（独立于本文调查的 runtime）发布后，重跑一次 diff 更新本 KIP。

## Issue tracking

KIP-16 acceptance criterion #1 (each P0/P1 matrix row gets a GitHub issue):

| Matrix row | Issue | Status |
|---|---|---|
| M10 allowed_hosts enforcement (P0) | [#510](https://github.com/xiaods/k8e/issues/510) | open — needs design decision (toFQDNs / egress proxy / API removal) |
| M2 layerstack snapshot layer (P1) | [#511](https://github.com/xiaods/k8e/issues/511) | **slice 1 shipped** — real mtimes + `ListFiles(since)` diff foundation; CAS layerstore + delta layers remain |
| M4 PTY transcript + windowed replay (P1) | [#512](https://github.com/xiaods/k8e/issues/512) | **Wave 3: shipped** — file-backed transcript + GetTranscript RPC + `log` CLI (transcript window semantics; PTY master variant deferred) |
| M5 observability (P1) | [#513](https://github.com/xiaods/k8e/issues/513) | **slice 1+2 shipped** — Prometheus collector (#517) + sandboxd NDJSON event stream (#518); query endpoint + process topology remain |
| M1 workspace-session reuse (P1) | [#514](https://github.com/xiaods/k8e/issues/514) | open |
| M7 protocol limits | implemented in-tree (Wave 1 R4) | — |
| M8 ready handshake | implemented in-tree (Wave 1 R3) | — |
| M12 background caps | implemented in-tree (Wave 2 M12) | — |

## Status of implementations

Wave 1 (2026-08-10, agent-assisted) shipped R3 + R4 + M12:

- **R3 — warm-pool application-layer ready handshake**: sandboxd new `POST /ready` endpoint (`{"status":"ready","venv":bool}` via new `venv.isReady()`); `defaultWarmPodHealthCheck` now calls `/ready` first and falls back to the old TCP dial only for sandboxd images that predate the endpoint. Tests: `TestReadyHandshake_*`, `TestDefaultWarmPodHealthCheck_ReadyHandshakePreferred`.
- **R4 — protocol limits**: gRPC gateway `MaxRecvMsgSize`/`MaxSendMsgSize` raised to 64MiB (server + client `dialOpts` on all three dial sites); sandboxd file read cap 10MiB→64MiB; sandboxd request body now heap-allocated + Content-Length-driven (was fixed 64KiB stack buffer, which silently truncated WriteFile payloads) with a 64MiB cap and 413 rejection.
- **M12 — capped background execution**: `maxBackgroundRuns` (default 5) per session, `ResourceExhausted` beyond the cap; per-session not global. Tests: `TestExecBackground_CapEnforced`, `TestExecBackground_CapPerSession`. M10 (`allowed_hosts` enforcement, P0) tracked as issue #510 pending a design decision (toFQDNs / egress proxy / API removal).
- **M4/R2 — file-backed transcripts + windowed replay (issue #512)**: sandboxd `transcript.zig` appends `cmd/stdout/stderr` lines per session under `/workspace/.k8e_transcripts/<sid>.log`; `GET /transcript?session=&offset=&limit=` serves line-aligned, offset-resumable windows (256KiB cap); new gRPC `GetTranscript` + `k8e-sandbox-cli log <sid> [--offset --limit --follow]`. Exec/background bodies now carry `session_id` so transcripts record. Tests: Zig `transcript_test.zig` (4 window/offset/eof cases, Linux CI), Go `TestGetTranscript_*` (proxy + no-transcript empty window), `TestSandboxdRequestBodies` session_id propagation.
- **M2 slice 2 — content-addressed layerstore (issue #511)**: new `pkg/sandboxlayer` (pure Go): SHA-256 CAS layers, atomic staging+publish (fsync+rename), manifest leases, `Delta()` for incremental transfer, lease-driven `GC()`, `SizeBytes()`. Snapshot CLI wired: save stores payload as CAS layer + manifest (dedup), restore reads via layerstore (legacy tar fallback), delete releases manifest lease + GC. Tests: 9 unit (store/dedup/manifest/delta/GC/large 1MiB) + 4 CLI-level. Foundation for incremental/diff snapshots.

Remaining waves: M1 workspace-session reuse, M2 zstd delta layers + server-side registry, M5 observability.

## Related work

- [KIP-3](./kip-3-agentic-ai-sandbox-matrix.md) — matrix / sessions / Cilium allowlist 设计（`allowed_hosts` 未执行即源于此）
- [KIP-8](./kip-8-skill-cli-replace-mcp.md) — CLI-first distribution
- [KIP-10](./kip-10-sandbox-snapshot.md) — snapshots（R2/M2 直接改进对象）
- [KIP-11](./kip-11-background-sandbox-execution.md) — background（M12 改进对象）
- [KIP-12](./kip-12-sandbox-ports-env-secrets.md) — ports + env/secrets
- [KIP-15](./kip-15-sandbox-api-perplexity-alignment.md) — API 完备性基线（本 KIP 的 gap 对齐面）
- Ephemeral-AI-Lab/ephemeral-sandbox — 参考实现（本文档所有 lesson 已对源核验，含 `sandbox_daemon_ready`/`docker-readiness` 握手、`ProtocolLimits`、`ManifestConflict`、`CommandTranscriptWindow`、disk-only state spec）

## Decision record

| Decision | Choice | Rationale |
|----------|--------|-----------|
| 借鉴方式 | **机制入、形态不入** | 取 lifecycle/snapshot/observability/protocol 机制；拒 Rust crates、多 exe、Docker-only、NDJSON 协议 |
| 与 KIP-15 关系 | KIP-16 是 KIP-15 的 **lessons 输入 + 实现手段清单** | KIP-15 定 "what to build"（完备性）；KIP-16 定 "borrow how"（机制来源） |
| 最高优先级 | R1 快照层 + R4 协议/认证加固 | 两者互为依赖：快照大文件传输正是 64KB/4MB 上限的受害者 |
| warm pool 就绪信号 | TCP dial → 应用层握手（保留兜底） | e-sandbox 明确论证 bare TCP 不可靠；握手成本 S |
| observability 形态 | gateway 侧 Prometheus + sandboxd NDJSON 事件流（activity-gated） | 不新增进程、不唤醒 idle daemon；对齐 disk-only 原则 |
