# K8E Sandbox vs OpenAI Agents Sandbox — 对齐分析

| 分析日期 | 对照版本 |
|---------|---------|
| 2026-05-30 | OpenAI Agents SDK v0.14+ (beta) vs K8E KIP-8 |

## 架构对比

| 维度 | OpenAI Agents Sandbox | K8E Sandbox (KIP-8) | 对齐状态 |
|------|----------------------|---------------------|---------|
| **接入层** | Python SDK `SandboxAgent` + `Runner` | `k8e sandbox` CLI（gRPC 直连） | ⚠️ 不同范式，但互补 |
| **隔离后端** | UnixLocal / Docker / 托管提供商 | Kubernetes Pod + RuntimeClass（gVisor/Kata/FC） | ✅ K8E 隔离更强 |
| **通道协议** | SDK 内部工具调用 | gRPC（protobuf）→ sandboxd HTTP :2024 | ✅ gRPC 性能更优 |
| **会话管理** | SDK-owned / developer-owned 生命周期 | State 文件 `~/.k8e/sandbox/{tenant}/state.json` + tenant 跨进程复用 | ✅ 概念对齐 |
| **审批门禁** | 外置运行时审批 | `confirm` + `approve` CLI 命令（内存 channel） | ✅ 都有，K8E 更显式 |

## 功能对比

### 1. Workspace 定义（Manifest）

| | OpenAI | K8E | 差距 |
|---|--------|-----|------|
| 文件/目录 | `File`, `Dir`, `LocalFile`, `LocalDir` | `k8e sandbox write` (逐一写入) | 🔴 K8E 无声明式 workspace |
| Git 仓库 | `GitRepo(repo, ref)` | `k8e sandbox run "git clone ..."` | 🔴 K8E 需手动 clone |
| 远程挂载 | S3/GCS/Azure/Box Mount | 无（仅 FQDN egress 白名单） | 🔴 K8E 无远程存储挂载 |
| 环境变量 | Manifest `env` | 无 | 🟡 可在 sandboxd 层加 |
| 用户/权限 | `User`, `Permissions`, `run_as` | 无（sandbox 内 root） | 🟡 安全增强方向 |

**建议**：K8E 当前 workspace 构建靠 Agent 手动执行 CLI 命令。后续 KIP 可引入 `SandboxWorkspace` CRD 或 Manifest 概念——声明式定义初始工作区（GitRepo、文件模板、挂载），控制器在 CreateSession 时自动 materialize 到 PVC。

### 2. 工具能力（Capabilities）

| | OpenAI | K8E | 差距 |
|---|--------|-----|------|
| Shell 执行 | `Shell` → `exec_command`, `write_stdin` (PTY) | `run` (Exec + ExecStream) | ✅ 对齐 |
| 文件读写 | `Filesystem` → `apply_patch`, `view_image` | `write` / `read` / `list` (全量) | 🟡 K8E 无 patch 编辑、无图片查看 |
| 技能发现 | `Skills` (lazy_from, LocalDir, GitRepo) | SKILL.md 在 Agent 侧（非 sandbox 内） | 🟡 概念相似但位置不同 |
| 记忆 | `Memory` (跨 run 持久化经验) | 无 | 🔴 缺失 |
| 上下文压缩 | `Compaction` | 无 | 🔴 缺失 |

**patch 编辑**：OpenAI 的 `apply_patch` 允许 Agent 用 unified diff 精确修改文件，而不是全量重写。对大文件场景效率更高。K8E 当前只有全量 `write`。

**建议**：
- `apply_patch` 可以作为 `sandboxd` 新增 HTTP endpoint (`POST /files/patch`)，然后通过 `k8e sandbox patch <sid> <path>` (stdin diff) 暴露
- `Memory` 和 `Compaction` 是 Agent SDK 层概念，K8E 作为基础设施层不需要内置——由上层 Agent 框架处理

### 3. 快照与持久化（Snapshots）

| | OpenAI | K8E | 差距 |
|---|--------|-----|------|
| 本地快照 | `LocalSnapshotSpec` | 无 | 🔴 缺失 |
| 远程快照 | `RemoteSnapshotSpec` | 无 | 🔴 缺失 |
| 会话恢复 | `session_state`, `RunState` | State 文件 + tenant FindActiveSession | ✅ 跨进程复用对齐 |
| TTL | 无（快照持久） | SandboxMatrix `sessionTTL` + GC | 🟡 K8E 偏临时 |

**关键差距**：OpenAI 的 manager 可以让用户保存沙箱 workspace 快照并在后续 run 中恢复。K8E 的 PVC 在 session destroy/GC 后即删除——文件丢失。

**建议**：
- 短期：允许 `create` 指定 `--keep-workspace` 跳过 GC 的 PVC 删除
- 中期：`SandboxSnapshot` CRD —— 对 PVC 做 Filesystem 快照（CSI snapshot），实现 save/restore

### 4. 托管提供商（Hosted Providers）

OpenAI 支持 8 个托管提供商作为 sandbox backend（Modal, Cloudflare, E2B, Daytona, Runloop, Vercel, Blaxel）。K8E 只有一个 backend（Kubernetes Pods with RuntimeClass）。

**这不是差距——是定位差异**：OpenAI 是 Agent SDK（统一编程接口），K8E 是基础设施（统一运行时）。K8E 的 gRPC 接口可以被任何上层 SDK 封装为 provider。

**建议**：编写一个 `K8ESandboxClient` 实现 OpenAI 的 sandbox client 协议，让 K8E 成为 OpenAI Agents SDK 的一个托管提供商。这样 K8E 获得所有上层能力（Manifest、Capabilities、Snapshots）而无需自己重复实现。

### 5. 安全模型

| | OpenAI | K8E | 对齐 |
|---|--------|-----|------|
| 文件权限 | 用户/组 + `run_as` | 无（sandbox 内 root） | 🔴 |
| 网络隔离 | 容器级别 | Cilium eBPF `toFQDNs` per-session | ✅ K8E 更强 |
| 运行时隔离 | Docker / 托管 VM | gVisor / Kata / Firecracker | ✅ K8E 更强 |
| 审批门禁 | 外置 | `confirm` + `approve` 内置 | ✅ 都有 |
| 只读文件系统 | 支持 | `readOnlyRootFilesystem: true` | ✅ |
| 深度限制 | 无 | 子沙箱 max depth=1 | ✅ K8E 额外保护 |

**安全是 K8E 的强项**——eBPF 内核级网络隔离 + 微虚拟机运行时隔离。但文件系统权限粒度不如 OpenAI。

## 核心结论

### K8E 的优势（不可替代）

1. **生产级隔离**：gVisor/Kata/Firecracker 微虚拟机，非 Docker 容器可比
2. **eBPF 网络策略**：Cilium `toFQDNs` per-session，内核级强制执行
3. **Warm pool**：预启动 Pod 池，<500ms session 冷启动
4. **多租户**：tenant 隔离 + SandboxMatrix CRD 策略
5. **统一运行时**：任何 Agent 框架都能通过 CLI 或 SDK 接入

### 需要补足的差距（按优先级）

| 优先级 | 差距 | 建议方案 | KIP |
|--------|------|---------|-----|
| **P0** | 无 Manfiest（声明式 workspace） | `SandboxWorkspace` CRD + `--manifest` flag | KIP-9 |
| **P0** | 无快照（workspace 无法持久化） | PVC CSI snapshot + `k8e sandbox snapshot save/restore` | KIP-10 |
| **P1** | 无 patch 编辑 | `sandboxd` 新增 `/files/patch` + `k8e sandbox patch` | 小改动 |
| **P1** | OpenAI SDK provider | 实现 `K8ESandboxClient` 成为托管提供商 | 新项目 |
| **P2** | 文件系统权限 | `run_as` + sandboxd 用户切换 | KIP-11 |
| **P2** | 远程存储挂载 | S3/GCS CSI driver + Manifest mount entries | KIP-12 |

### 当前 KIP-8 已经对齐的部分

```
OpenAI Sandbox                K8E Sandbox (KIP-8)
─────────────                 ────────────────────
exec_command      ←────────→  k8e sandbox run
exec_stream       ←────────→  k8e sandbox run --raw
write_file        ←────────→  k8e sandbox write (stdin)
read_file         ←────────→  k8e sandbox read
list_files        ←────────→  k8e sandbox list
session create    ←────────→  k8e sandbox create
session destroy   ←────────→  k8e sandbox destroy
session resume    ←────────→  state file + --tenant
approval gate     ←────────→  k8e sandbox confirm + approve
```

**结论**：KIP-8 的设计方向正确。CLI 命令集覆盖了 OpenAI sandbox 核心执行能力的 80%。剩余的 20%（Manifest、Snapshot、Patch）是声明式工作区管理和状态持久化的增强，应作为后续 KIP 补齐。K8E 作为基础设施层的定位——通过 gRPC 为上层 Agent 框架提供统一运行时——与 OpenAI 的 SDK 层是互补而非竞争关系。
