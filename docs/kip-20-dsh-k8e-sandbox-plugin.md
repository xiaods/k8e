# KIP-20: dsh-k8e-sandbox — 让 DeepSeek Harness 以插件方式调用 k8e-sandbox

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-24 | Implemented & published — `@k8e-sandbox/*` (npm); Phase 1+2 gRPC transport, KIP-19 terminals, prefs web UI, KIP-24 expose/allow-hosts tools |

> 关联 KIP：KIP-3（sandbox matrix）、KIP-8（skill-cli 取代 MCP）、KIP-14（mTLS 动态证书）、KIP-16（沙箱架构教训 / catalog）、KIP-17（CLI 多 profile 与 API key TTL）、KIP-18（E2B 兼容）、**KIP-19（sandbox PTY 终端原语——本 KIP Phase 2 的 `spawnTerminal` 依赖它先行）**。
> 关联仓库：`deepseek-harness`（下称 **dsh**）——插件化 agent harness，vendored Cordis，“everything is a plugin”。

## 实现状态（2026-08-24）

- **Phase 1 + Phase 2 已实现**并发布 npm `@k8e-sandbox/*`（PR #553/#554 及后续）：
  所有者服务（owner）、CLI/gRPC 双传输 client、fs / subprocess provider（流式 stdio 与
  `spawnTerminal`，后者依赖 KIP-19）、host-ui / client-ui、bundle、KIP-24 expose/allow-hosts 工具。加载方式：
  `dsh plugin --profile <name> add @k8e-sandbox/dsh-k8e-sandbox-bundle`。
- **未实现（可选）**：模型面工具的 snapshot/pause/resume/ps/confirm 子集（当前工具包提供 session 状态/销毁、前台 exec、后台 exec + poll、expose；其余仍可走 CLI）。
- **已知限制**：`@deepseek-ai/dsh-*` 系列为私有包（公共 npm 不可用），运行时由 dsh 安装提供 peer 依赖。
- **非目标仍成立**：PTY 原语已由 KIP-19 提供，本 KIP 消费它，不再自造。

## 摘要

本 KIP 提出 `dsh-k8e-sandbox`：一个**完全树外（out-of-tree）**的 dsh 插件族，让 DeepSeek Harness 把"执行世界"（文件读写 + 进程执行）整体搬到 k8e-sandbox 里，与 k8e 现有的 `k8e-sandbox-cli` / gRPC 网关 / 温暖池共用同一套基础设施。

核心结论有三条：

1. **不改 dsh 源码**。dsh 把能力抽象成"缝"（capability seam）：`ctx.fs`（文件系统）、`ctx.subprocess`（子进程）等都是可替换的 Service Provider。k8e 插件只需实现这两条缝的 provider，挂到 Cordis 树上即可。加载方式是 dsh 已经原生支持的树外 bundle：`dsh plugin --profile <name> add <package>`。
2. **照搬 dsh 仓库里的 E2B POC 作为模板**。E2B POC（`@deepseek-ai/dsh-e2b` + `fs-e2b` + `subprocess-e2b`）已经证明了"一个沙箱所有者服务 + 两个 OS 适配器"这套组合：挂上 `ctx.fs` + `ctx.subprocess` 后，bash、terminal、LSP 这些上层消费者**自动**进入沙箱，无需为它们各写一个 fork。
3. **分两阶段落地**：Phase 1 用 `k8e-sandbox-cli` 作为传输层（零新 k8e 面，复用 mTLS/profile/state），先打通 fs + 单次 exec；Phase 2 直连 gRPC（从 `proto/sandbox/v1/sandbox.proto` 生成 TS 客户端），补齐流式 stdio 与进程组终止的子进程保真度。

## 背景与动机

k8e 目前给 AI agent 的入口是两条：

- **`k8e-sandbox-cli`（SKILL 驱动）**：agent 通过 SKILL.md 把目标拆成一串 `k8e-sandbox-cli run/write/read/...` 命令。它在 Claude Code / Codex / Pi 上工作良好，但**每个操作都是一次 CLI 进程 spawn**，且 agent 的"文件"和"执行"被暴露成命令行字符串，而不是 harness 原生的结构化工具（`read`/`edit`/`bash`/`terminal`）。
- **gRPC / Python / TS SDK**：直接编程接口，但 agent harness 不会自己写 SDK 调用。

DeepSeek Harness 是一个**插件化**的 agent harness：它的 Bash、文件读写、终端、LSP 全都构建在两个可替换的 provider 缝上。因此对 dsh 而言，"接入 k8e-sandbox"的正确姿势不是再写一套 skill 命令，而是**实现 `ctx.fs` + `ctx.subprocess` 两条缝的 provider**，让 dsh 里所有依赖这两条缝的能力（bash、terminal、LSP、`read`/`write`/`edit` 工具）一次性地跑进 k8e 沙箱。

这样 k8e 得到的是：

- **原生工具体验**：dsh 的 `bash` 工具、`str_replace_editor`/文件工具、terminal、LSP 全部在 gVisor/Kata/Firecracker pod 里执行，agent 无感知。
- **复用基础设施**：温暖池、session CRD、tenant 复用、mTLS、快照、审批流，全都不重造。
- **不侵入 dsh**：插件独立成包，随 k8e 仓库维护，dsh 一行源码都不改。

## 目标与非目标

### 目标

1. 一个 k8e 仓库内的树外 dsh 插件族，通过 `dsh plugin add` 安装，**不修改 dsh 源码**。
2. 实现 `ctx.fs` 与 `ctx.subprocess` 两条缝的 k8e provider，让 dsh 的 bash/文件/终端/LSP 在 k8e 沙箱中运行。
3. 会话生命周期映射到 k8e `SandboxSession`：创建/销毁（可选暂停/恢复）、tenant 复用、温暖池收益。
4. 与 k8e 现有 CLI/SKILL 共享同一套 mTLS、profile、API key、审批语义（KIP-14 / KIP-17）。

### 非目标

- 不改动 dsh 的 `packages/`、`docs/architecture.md`、agent-loop。
- 不把 k8e 变成 dsh 的 MCP server（KIP-8 已否掉 MCP 路线）。
- 不在本 KIP 内实现 PTY 终端（k8e gRPC 当前无 PTY 原语，见"保真度差距"）。
- 不在本 KIP 内做"k8e session ↔ dsh session"的双向持久化绑定；dsh 的 session log 仍由 dsh 自己管，k8e 只提供执行世界。
- 不追求与 E2B SDK 的二进制兼容（那是 KIP-18 的范畴）。

## 关键约束与前提

### dsh 侧（已核对源码）

- **一切都是插件**：每个贡献通过 `ctx.effect()` / `ctx.on()` 挂在 Cordis 上下文上；"扩展 dsh = 在别的插件旁边挂一个插件"，没有需要 patch 的特权核心。
- **能力缝 = Service Definition / Service Provider / Consumer 三角色**。缝是可替换的：换一个 provider 就能换掉整个执行世界（`docs/architecture.md` 的"Where new behavior goes"表明确写了：加文件系统能力 → 注册 `ctx.fs` provider；加 shell → 注册 `ctx.shell` backend，而本地 bash backend 内部通过 `ctx.subprocess` spawn）。
- **E2B POC 是权威模板**（`packages/e2b/`）：`@deepseek-ai/dsh-e2b`（`ctx.e2b` 所有者服务，持有一个共享沙箱 handle，负责创建/清理）、`@deepseek-ai/dsh-fs-e2b`（`ctx.fs`）、`@deepseek-ai/dsh-subprocess-e2b`（`ctx.subprocess`）。E2B README 明确：`dsh-bash-local` / `dsh-terminal-bash` / `dsh-lsp-stdio` **不需要 E2B 专用 fork**，因为它们只委托 `ctx.fs` / `ctx.subprocess`。
- **两条缝的抽象契约**：
  - `FileSystem`（`ctx.fs`）：`resolve` / `processPath` / `fileUrl` / `contains` / `stat` / `lstat` / `readText` / `streamText` / `readBytes` / `listDir` / `writeText` / `editText`，外加 `sandboxMode` getter（backend 是否自带隔离）。
  - `SubprocessRuntime`（`ctx.subprocess`）：`resolveExecutable` / `spawn` / `spawnTerminal`。`spawn` 返回带 stdin/stdout/stderr、`terminate()`（SIGTERM→grace→SIGKILL，进程组范围）、`waitForExit()` 的 live handle。
- **树外加载**：`dsh plugin --profile <name> add <package>` 把包安装进 profile；bundle = `package.json` 里声明 `"dsh": { "bundle": { "patch": "./cordis.patch.yml" } }` 的 npm 包；profile = `$DSH_HOME/profiles/<name>` 下的 `package.json`（含 `dsh.profile.bundles` 有序列表）+ 用户 `cordis.patch.yml`。加载顺序：各 bundle patch → profile patch → home patch → `--patch` 覆盖。**这就是"不侵入源码"的官方通道**（`docs/user/develop/basic/publish.md`）。

### k8e 侧（已核对源码）

- gRPC `SandboxService`（`proto/sandbox/v1/sandbox.proto`）：`CreateSession` / `GetSession` / `ListSessions` / `DestroySession` / `PauseSession` / `ResumeSession` / `Exec` / `ExecStream` / `WriteFile` / `ReadFile` / `ListFiles` / `PipInstall` / `RunSubAgent` / `ConfirmAction` / `ApproveAction` / `Login` / `PollRun` / `GetTranscript` / `GetEvents` / `Snapshot*` / `GetProcesses`。
- `k8e-sandbox-cli` 提供与上述 RPC 一一对应的命令（含 `--raw` 流式、`--background` + `poll`、`ps`、`snapshot`、`confirm`/`approve`、`catalog`）。
- mTLS 材料在 `~/.k8e/sandbox/`（`ca.crt` / `client.crt` / `client.key`），由 `connect` / `login` 建立，90 天证书 + <30 天惰性续期；多集群用 `profiles.yaml`（KIP-17）。
- 仓库内**只有 Go gRPC 客户端**（`pkg/sandbox/client`）；Python/TS SDK 不在本仓库。仓库内无 TS 客户端。

## 总体设计

### 与 E2B POC 的同构

k8e-sandbox 与 E2B 在抽象层面是同一类东西：一个远端 Linux 执行世界 + 会话生命周期 + 文件/进程两个底层原语。因此 k8e 插件族直接镜像 E2B 的三包结构：

| E2B POC 包 | `ctx` key | k8e 对应包（本 KIP） |
|---|---|---|
| `@deepseek-ai/dsh-e2b` | `ctx.e2b` | `@k8e-sandbox/dsh-k8e-sandbox`（`ctx.k8eSandbox`） |
| `@deepseek-ai/dsh-fs-e2b` | `ctx.fs` | `@k8e-sandbox/dsh-k8e-sandbox-fs` |
| `@deepseek-ai/dsh-subprocess-e2b` | `ctx.subprocess` | `@k8e-sandbox/dsh-k8e-sandbox-subprocess` |

差别只在**传输层**：E2B 用其 SDK（API key），k8e 用 `k8e-sandbox-cli`（Phase 1）或直连 gRPC + mTLS（Phase 2）。

### 包结构

插件族以 `plugins/deepseek-harness/` 目录放在 k8e 仓库内（单仓，独立于 k8e 的 Go 构建），每个子包是一个可独立发布的 npm 包：

```
plugins/deepseek-harness/
├── packages/
│   ├── dsh-k8e-sandbox/            # 所有者服务：ctx.k8eSandbox（会话生命周期 + 共享客户端）
│   ├── dsh-k8e-sandbox-fs/         # ctx.fs provider（K8eFileSystem extends FileSystem）
│   ├── dsh-k8e-sandbox-subprocess/ # ctx.subprocess provider（K8eSubprocessRuntime extends SubprocessRuntime）
│   ├── dsh-k8e-sandbox-client/     # 内部传输抽象：CliClient / GrpcClient 共用一个接口
│   ├── dsh-k8e-sandbox-tool/       # 可选：模型面工具（session/snapshot/confirm/ps 等沙箱专属动词）
│   └── dsh-k8e-sandbox-bundle/     # bundle 包：dsh.bundle.patch → cordis.patch.yml，挂载上面所有行
├── package.json                    # pnpm workspace
├── pnpm-workspace.yaml
└── tsconfig.base.json
```

> npm scope 统一为 `@k8e-sandbox/`，命名统一为 `dsh-k8e-sandbox`：所有者 `@k8e-sandbox/dsh-k8e-sandbox`、fs `@k8e-sandbox/dsh-k8e-sandbox-fs`、subprocess `@k8e-sandbox/dsh-k8e-sandbox-subprocess`、client `@k8e-sandbox/dsh-k8e-sandbox-client`、tool `@k8e-sandbox/dsh-k8e-sandbox-tool`、bundle `@k8e-sandbox/dsh-k8e-sandbox-bundle`；目录短名与之对齐（`dsh-k8e-sandbox*`）。

### 加载方式（非侵入通道）

用户侧零改 dsh 源码：

```sh
# 初始化 profile（首次会自动引入 @deepseek-ai/dsh-base）
dsh plugin --profile k8e add <path-or-tarball-or-github>
# 或发布到 npm 后：
dsh plugin --profile k8e add @k8e-sandbox/dsh-k8e-sandbox-bundle

# 验证层 + 启动
dsh --profile k8e --dump-config
dsh --profile k8e
```

`dsh-k8e-sandbox-bundle/package.json` 声明 `"dsh": { "bundle": { "patch": "./cordis.patch.yml" } }`；`cordis.patch.yml` 插入四行（顺序关键——先所有者，再 fs/subprocess，后可选工具）：

```yaml
- insert:
    - id: k8e-sandbox
      name: '@k8e-sandbox/dsh-k8e-sandbox'
      config:
        # endpoint / apikey / profile / cert_dir 走与 CLI 相同的解析；留空则用 ~/.k8e/sandbox
        cwd: /workspace
        runtimeClass: gvisor
    - id: k8e-sandbox-fs
      name: '@k8e-sandbox/dsh-k8e-sandbox-fs'
    - id: k8e-sandbox-subprocess
      name: '@k8e-sandbox/dsh-k8e-sandbox-subprocess'
```

依赖注入与 E2B 相同：`K8eFileSystem` 和 `K8eSubprocessRuntime` 都 `static inject = ['k8eSandbox']`，靠 Cordis 的依赖声明保证加载顺序（不需要手工 boot 序列）。挂载这两行后，dsh 的 `ctx.fs` / `ctx.subprocess` 即被 k8e 实现占据——dsh 里所有委托这两条缝的消费者自动进入沙箱。

## 能力缝映射

### 7.1 所有者服务 `ctx.k8eSandbox`

镜像 `E2BRuntime`：持有**一个** k8e session，暴露 `getClient()`（或 `getSession()`），在 dispose 时销毁（或按配置暂停）session。

**会话粒度（已决）：每 dsh 会话一个 k8e session（E2B 语义）。** 即 `ctx.k8eSandbox` 随 dsh 会话的加载/卸载而创建/销毁，不做"按 tenant 全局共享一个 pod"的默认行为；`tenant` 仅作为**显式**的跨进程复用开关保留（见下表"复用"行）。

| 职责 | k8e 映射 |
|---|---|
| 创建执行世界 | `CreateSession`（`runtimeClass`、`tenant`、`allowed_hosts`、`env`、`secret_refs`），`cwd` 默认 `/workspace` |
| 共享 handle | 单例懒初始化 `Promise<SessionHandle>`；fs/subprocess 在第一次操作前 await |
| 清理 | dispose → `DestroySession`（可配 `pauseOnDispose` 走 `PauseSession` 以保留 PVC） |
| 复用 | 可选：显式 `tenant` 映射 k8e 的 `tenant_id`，跨 dsh 进程复用同一 pod（KIP-3/`FindActiveSession`）；默认不开启 |

配置项（Schemastery，与 E2B 同风格）：`endpoint?` / `apiKey?` / `profile?` / `certDir?` / `cwd` / `runtimeClass` / `timeoutMs` / `tenant?` / `allowedHosts?` / `env?` / `pauseOnDispose?`。解析顺序复用 CLI 语义：flag → env（`K8E_SANDBOX_*`）→ profile → 默认（本地自动发现）。

### 7.2 文件系统 provider `ctx.fs`

`K8eFileSystem extends FileSystem`，`static inject = ['k8eSandbox']`，覆盖：

| seam 方法 | k8e 映射 | 备注 |
|---|---|---|
| `resolve` | `Exec 'realpath -mz …'`（绝对路径正则化 + 存在性探测） | 相对路径按 `cwd` 解析；targetKey 用规范化绝对路径 |
| `processPath` / `fileUrl` / `contains` | 纯路径运算（posix） | 执行世界 = 沙箱 Linux，直接返回沙箱内绝对路径 |
| `stat` / `lstat` | `Exec 'stat -c …'` 或 `ListFiles` 探测 | k8e 无原生 stat RPC，走 exec 兜底 |
| `readText` / `readBytes` / `streamText` | `ReadFile` | `ReadFile` 返回整文件内容，无流式；大文件读走 exec 分块或受 `maxBytes` 限制 |
| `listDir` | `ListFiles`（过滤直接子项） | `ListFiles` 是扁平列表（path + mtime），需自行拼目录条目与类型 |
| `writeText` | `WriteFile` + `Exec 'mv'/'ln -T'` 原子发布 | `WriteFile` 无原子 rename；用 staging 目录 + `mv` 保证原子与锁 |
| `editText` | `readText` → 本地字面替换 → `writeText`（同锁） | 版本守卫用 `FsVersion` |
| `sandboxMode` | 返回沙箱后端模式（gVisor/Kata/Firecracker 视为隔离执行世界） | 让 dsh 工具层如实广播"已在沙箱内" |

`FsVersion` 由 `(mtime, size, path)` 哈希生成（`ListFiles`/`stat` 只给 mtime，无 mode/size 时退化到内容哈希或 `readText` 后哈希）。

### 7.3 子进程 provider `ctx.subprocess`

`K8eSubprocessRuntime extends SubprocessRuntime`，`static inject = ['k8eSandbox']`：

| seam 方法 | k8e 映射 |
|---|---|
| `resolveExecutable` | `Exec "command -v …"`（绝对路径先 `test -f/-x`） |
| `spawn(spec)` | `Exec{background:true}` 拿 `run_id`，包装器把 pid/status 写入沙箱文件，stdout/stderr 走 `ExecStream` 或 `PollRun`；`terminate()` 用 `Exec 'kill …'` + `GetProcesses` 观察进程组存活 |
| `spawnTerminal` | **Phase 2（依赖 KIP-19 sandbox PTY 终端原语）**；Phase 1 返回明确的"未实现"错误，terminal 消费者降级或禁用 |

实现方式与 E2B `subprocess-e2b` 的 `process.ts` 同构：单次 `Exec` 无法表达"常驻进程 + 管道 stdio + 进程组终止"，所以 provider 用一个远端 wrapper（`setsid` 建进程组、发布 pid/status 到私有文件、tee 输出），终止时 `kill -- -PGID` 并按 `graceMs` 升级到 SIGKILL。

### 7.4 上层消费者（免费获得）

挂载 `ctx.fs` + `ctx.subprocess` 后，**无需为它们写任何 k8e fork**：

- `dsh-bash-local`（`bash` 工具）→ 委托 `ctx.subprocess` + `ctx.fs`。
- `dsh-terminal-bash`（terminal）→ 委托 `ctx.subprocess`（PTY 部分见差距）。
- `dsh-lsp-stdio`（LSP）→ 委托 `ctx.subprocess`。
- 文件工具（`read`/`write`/`edit`）→ 委托 `ctx.fs`。

这正是 E2B README 描述的"换 provider 换掉整个执行世界"。

### 7.5 可选：模型面工具 `dsh-k8e-sandbox-tool`

fs/subprocess 覆盖不了 k8e 的**沙箱专属动词**。提供一个模型面工具（挂 `ctx.tools`，schema 随 prompt 装配），暴露：`session`（create/get/list/destroy/pause/resume）、`snapshot`（save/list/restore/delete）、`ps`、`confirm`/`approve`、`poll`。这些是 k8e 独有能力，不进 fs/subprocess 缝，作为 Consumer 补齐"缝"的第三角。Phase 1 可先省（SKILL 已覆盖），Phase 2 再上。

## 传输层设计

### 客户端抽象

fs/subprocess 不直接依赖"CLI 还是 gRPC"。`dsh-k8e-sandbox-client` 定义一个内部接口（最小方法集：`createSession/destroySession/exec/execStream/writeFile/readFile/listFiles/pollRun/getProcesses/...`），两个实现：

- `CliK8eClient`：包装 `k8e-sandbox-cli`（spawn 子进程，解析 JSON / `--raw` 流）。
- `GrpcK8eClient`：从 `proto/sandbox/v1/sandbox.proto` 生成 TS（`@grpc/grpc-js` + `ts-proto` 或 `protobuf-ts`），mTLS 读 `~/.k8e/sandbox/` 证书，到期经 `Login` RPC 惰性续期。

### Phase 1：CLI 包装（MVP）

- **复用** `k8e-sandbox-cli` 的 connect/mTLS/profile/state 逻辑，零新 k8e 面。
- fs 全量 + `spawn` 单次 exec + `--background`/`poll` 后台执行。
- 缺点：每操作一次进程 spawn；`--raw` 只有合并 stdout 流，无独立 stderr 流、无 stdin 管道。

### Phase 2：直连 gRPC（补保真度）

- 从 proto 生成 TS 客户端（放在 `dsh-k8e-sandbox-client/`，随 proto 一起由 `make generate` 触发）。
- mTLS 材料与 CLI 共享（同一 cert 目录 / 同一 profile），续期逻辑复用 KIP-14 语义。
- 用 `ExecStream` 做流式 stdout，`PollRun`/`GetTranscript` 做增量与后台，`GetProcesses` + `Exec kill` 做进程组终止，逼近 E2B 的保真度。

## 保真度差距（诚实清单）

| 维度 | E2B SDK | k8e gRPC 现状 | 影响与对策 |
|---|---|---|---|
| 流式 stdout | 有 | `ExecStream` 只有合并 `chunk` | Phase 1 走 `--raw` 合并流；Phase 2 用 `GetTranscript`/`PollRun` 增量；stderr 区分受限于协议 |
| 独立 stderr | 有 | `Exec` 分 stdout/stderr，`ExecStream` 合并 | 单次 exec 可用；流式场景先合并 |
| stdin 到运行中进程 | 有 | `Exec` 无 stdin 字段 | 进程启动时用 wrapper 传文件/stdin；运行中 stdin 缺口 |
| PTY / 终端 | 有 | 无 PTY 原语 | Phase 1 `spawnTerminal` 明确报未实现、terminal 消费者降级；Phase 2 依赖 KIP-19（sandbox PTY 终端原语） |
| 原子写 | `files` API | `WriteFile` 无 rename | 用 staging + `Exec mv` 补偿 |
| stat（mode/size） | 有 | `ListFiles` 只有 path+mtime | `Exec 'stat'` 兜底；`FsVersion` 退化策略 |
| 进程组终止 | `kill` | 无终止 RPC | `Exec kill -- -PGID` + `GetProcesses` 存活观察 |
| 目录列表（类型/子项） | 有 | 扁平列表 | 前端拼装 + `stat` 补类型 |

这些差距**不阻塞 MVP**，但决定了 Phase 1 只能承诺"单次 exec + 文件读写"级别的体验，Phase 2 才能补齐流式与进程组。

## 配置面

Schemastery schema（`dsh-k8e-sandbox` 的 `Config`），字段尽量与 CLI flag 对齐：

| 字段 | 默认 | 说明 |
|---|---|---|
| `endpoint` | 空（本地自动发现） | gRPC `host:port` |
| `apiKey` | 空（读 mTLS/环境） | bootstrap 或远程一次性密钥，绝不写进沙箱 |
| `profile` | 空 | `~/.k8e/sandbox/profiles.yaml` 的命名 profile |
| `certDir` | `~/.k8e/sandbox` | mTLS 材料目录 |
| `cwd` | `/workspace` | 沙箱工作目录 |
| `runtimeClass` | `gvisor` | gvisor / kata / firecracker |
| `timeoutMs` | 300000 | 会话生命周期；到期销毁 |
| `tenant` | 空 | 显式跨进程 session 复用（非默认；默认每 dsh 会话一个 k8e session） |
| `allowedHosts` | 空 | egress 白名单 |
| `env` | 空 | 非敏感环境变量 |
| `pauseOnDispose` | false | dispose 时 pause 而非 destroy（保 PVC） |

解析优先级与 SKILL.md 一致：flag → env → profile → 默认。

## 安全

- **mTLS 复用**：直接读 `~/.k8e/sandbox/` 证书；私钥不出本机；续期复用 KIP-14。
- **凭据隔离**：`apiKey` 只用于 gateway 握手，绝不通过 `env` 注入沙箱；dsh 自身的 `DEEPSEEK_API_KEY` 等凭据形环境变量由 `scrubbedParentEnv` 在 spawn 前剔除。
- **secret 走 SecretRef**：非敏感配置才进 `env`（KIP-12 语义），敏感值用 `secret_refs`（`ENV=secretName:key`）。
- **审批流**：破坏性动作映射到 k8e `confirm` → `approve`（人在环），与 SKILL 的安全红线一致；dsh 侧也可接 `interaction`/`approval` 能力。
- **沙箱即隔离**：`sandboxMode` 如实上报后端隔离（gVisor/Kata/Firecracker），让 dsh 工具层正确广播"已在沙箱内"而不误报 host 执行。

## 验收标准

- [ ] 树外 bundle 能通过 `dsh plugin --profile k8e add …` 安装并 `dsh --profile k8e --dump-config` 显示 `dsh-k8e-sandbox` 层。
- [ ] 挂载后 `ctx.fs` / `ctx.subprocess` 被 k8e 实现占据，dsh 的 `bash` 与文件工具在沙箱 pod 内执行（用 `ps`/`list` 验证是沙箱而非 host）。
- [ ] fs：`read/write/edit/list/stat` 通过 `WriteFile/ReadFile/ListFiles/Exec` 正确映射；原子写与版本守卫有单测。
- [ ] subprocess：`spawn` 单次 exec 返回正确 stdout/stderr/exit_code；`terminate` 走 kill+存活观察；`resolveExecutable` 正确。
- [ ] 会话生命周期：dispose → DestroySession；`pauseOnDispose` → PauseSession；`tenant` 复用同一 pod。
- [ ] mTLS/profile/env 解析与 CLI 一致（KIP-17 表）。
- [ ] 快照/审批/`ps` 等专属动词（可选工具）行为与 SKILL 对齐。
- [ ] keyless dsh 快照：一个脚本化模型跑在 mock k8e gateway 上，产出可复放的 transcript。
- [ ] 文档：本 KIP + 插件 README + `SKILL.md` 一处更新说明"dsh 用户也可用插件接入"。

## 实现地图

| 阶段 | 内容 | 位置 |
|---|---|---|
| Phase 1 | 所有者服务 + CLI 客户端抽象 | `plugins/deepseek-harness/packages/dsh-k8e-sandbox{,-client}` |
| Phase 1 | fs provider（CLI 传输） | `.../dsh-k8e-sandbox-fs` |
| Phase 1 | subprocess provider（单次 exec + 后台） | `.../dsh-k8e-sandbox-subprocess` |
| Phase 1 | bundle 包 + cordis.patch.yml | `.../dsh-k8e-sandbox-bundle` |
| Phase 2 | gRPC TS 客户端（proto 生成 + mTLS 续期） | `.../dsh-k8e-sandbox-client/src/grpc`，proto 生成接 `make generate` |
| Phase 2 | 流式 stdio / 进程组终止 / 模型面工具 | subprocess、`dsh-k8e-sandbox-tool` |
| Phase 2 | `spawnTerminal`（**依赖 KIP-19（sandbox PTY 终端原语）先落地**） | subprocess + k8e 侧 PTY RPC |
| 后续 | — | — |

## 测试计划

- **单测**：配置解析、路径映射、`FsVersion` 生成、`listDir` 拼装、原子写状态机、进程组终止状态机（Vitest，mirror `fs-e2b`/`subprocess-e2b` 的 spec）。
- **快照**：一个 mock k8e gateway（或 `CliK8eClient` 打到假 CLI）跑 keyless dsh replay，外部断言 transcript。
- **e2e**：真实 K8E 集群（`make test` 集成环境）验证 bash/文件/终端进入沙箱 pod、温暖池收益、destroy/pause。
- **契约**：`k8e-sandbox-cli catalog`（KIP-16 M9）作为 CLI 客户端的生成/校验输入，防止 CLI 面漂移。

## 备选方案

| 方案 | 为什么不选 |
|---|---|
| 只写一个 dsh 的 SKILL（复用 `k8e-sandbox-cli`） | 命令字符串而非原生工具，文件/执行不成结构化能力，terminal/LSP 无法受益 |
| 在 dsh 仓库内加 `packages/k8e/*` | 侵入 dsh 源码，违反"树外"约束 |
| 做一个 k8e MCP server 给 dsh | KIP-8 已否掉 MCP 路线；且 dsh 的 MCP 是额外适配层，不如 provider 缝原生 |
| 直连 gRPC 一步到位（跳过 CLI） | 需先补齐 proto TS 生成 + mTLS 续期，Phase 1 拉长；CLI 包装能更快验证缝的语义 |
| 复用 E2B 兼容层（KIP-18）接入 dsh 的 E2B provider | 依赖 KIP-18 的 E2B 兼容面是否到位，且绕开了 k8e 原生 session/snapshot/审批能力 |

## 已决问题

| # | 问题 | 决定 |
|---|---|---|
| 1 | npm scope | 统一 `@k8e-sandbox/dsh-*`（所有者 `@k8e-sandbox/dsh-k8e-sandbox` 等，见"包结构"） |
| 2 | PTY 时间表 | `spawnTerminal` 放在 Phase 2，且**依赖 KIP-19（sandbox PTY 终端原语）先行**；本 KIP 只到"非 PTY 子进程" |
| 3 | session 复用粒度 | **每 dsh 会话一个 k8e session（E2B 语义）**；`tenant` 仅作显式跨进程复用开关，不作默认全局共享 |
