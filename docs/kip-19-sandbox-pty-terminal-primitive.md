# KIP-19: sandbox PTY 终端原语

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-17 | Accepted — M1–M3 已实现（proto 7 RPC、sandboxd `pty.zig`、gateway `terminal.go`、dsh `spawnTerminal`）；M4（E2B 兼容层 `pty.*` 闭合）待落地 |

> 关联 KIP：KIP-14（mTLS 动态证书）、KIP-16（沙箱架构教训 / catalog M9）、KIP-18（E2B 兼容）、**KIP-20（dsh-k8e-sandbox 插件——其 Phase 2 的 `spawnTerminal` 依赖本 KIP 先行）**。
> 本 KIP 是 KIP-20 已决问题 #2 的展开：为 k8e sandbox 增加 PTY 原语，使 dsh 的 terminal seam（以及 E2B SDK 的 `pty.*` 面）能被完整实现。

## 实现状态（2026-08-17）

- **M1 proto / sandboxd / gateway**：已实现并合入 main（PR #544）。`proto/sandbox/v1/sandbox.proto` 含 7 个 `Terminal*` RPC；`sandboxd/src/pty.zig`（PTY 分配 / 会话首领启动 / 输入输出泵 / `TIOCSWINSZ` 尺寸 / 前台组 / 信号 / 会话树 TERM→KILL）+ 终端会话表；`pkg/sandboxmatrix/grpc/terminal.go`（RPC 处理器 + `TerminalStream` SSE 代理 + terminal_id 路由注册表）。
- **M2 测试**：`sandboxd/src/pty_test.zig`、`pkg/sandboxmatrix/grpc/terminal_test.go`。
- **M3 dsh 消费**：`@k8e-sandbox/dsh-k8e-sandbox-subprocess` 的 `spawnTerminal` 已实现（直连 gRPC `createTerminal`，随 `@k8e-sandbox/*@0.1.1` 发布）。
- **M4 E2B 兼容层 `pty.*` 闭合**：**未实现**——`pkg/sandbox/e2b/envd.go` 仍拒绝 `pty` 请求（“PTY sessions are not supported”），待 KIP-18 兼容层接入本原语。

## 摘要

给 k8e sandbox 的 gRPC 网关与 `sandboxd` 增加一套 **PTY 终端原语**：在沙箱 pod 内分配伪终端（`/dev/ptmx`）、把请求的 argv 作为**会话首领 + 控制终端**进程启动（不做 shell 解释）、双向字节流（输入写 master、输出读 master）、窗口尺寸调整（`TIOCSWINSZ`）、前台进程组查询与信号（`tcgetpgrp` / `kill -PGID`）、以及会话树级别的终止（TERM → grace → KILL）。

表面是 7 个 gRPC RPC（`CreateTerminal` / `TerminalStream` / `TerminalWrite` / `TerminalResize` / `TerminalForeground` / `TerminalSignal` / `TerminalDestroy`），底层是 `sandboxd` 的一个 `pty` 模块 + 终端会话表 + 网关的终端路由注册表。

它一次性服务两个消费者：

1. **dsh 的 `spawnTerminal`**（`SubprocessRuntime.spawnTerminal` → `SubprocessTerminalHandle`），让 KIP-20 的 `@k8e-sandbox/dsh-k8e-sandbox-subprocess` 在 Phase 2 真正实现 terminal seam，进而让 dsh 的 `terminal-bash`（持久终端）跑进 gVisor/Kata/Firecracker pod。
2. **E2B SDK 的 `pty.create` / `pty.sendInput` / `pty.resize` / `pty.kill`**（KIP-18 E2B 兼容面当前缺的一角），与已有的 `/exec/stream` + `/exec/processes` + `/exec/attach` 补齐 E2B 进程控制面。

## 背景与动机

### 现状：没有 PTY

- `sandboxd`（沙箱 pod 内 PID 1 的 Zig 守护进程，监听 `:2024`）目前只有管道式 exec：`/exec`（单次、stdout/stderr 分管道）、`/exec/stream`（SSE 流、stdout 单管道、stdin 经 `/exec/stdin`）、`/exec/background`（后台 + poll）、`/exec/signal`、`/exec/processes`、`/exec/attach`。
- 这些 exec 全走 `fork + dup2(pipe)`，**没有控制终端**：没有 `setsid` 会话、没有 `TIOCSCTTY`、没有前台进程组、没有 `TIOCSWINSZ` 尺寸、没有行规程（line discipline）下的 echo/规范模式/信号（`Ctrl-C` 不会产生 `SIGINT` 给前台组）。
- gRPC 面 `proto/sandbox/v1/sandbox.proto` 的 `Exec`/`ExecStream` 也都没有 PTY 概念。

### 为什么需要 PTY

真实终端不是"一条字节管道"，而是一个有控制终端的**会话（session）**：

- **任务控制（job control）**：`Ctrl-C`（`SIGINT`）、`Ctrl-Z`（`SIGTSTP`）要送达**前台进程组**，而不是某个固定 pid。
- **前台进程组可查可杀**：`tcgetpgrp` / `kill(-PGID)` 是交互式 shell、TUI 程序、`vim`/`less`/REPL 的硬前提。
- **窗口尺寸**：`TIOCSWINSZ` + `SIGWINCH` 是 `htop`/`vim`/全屏 TUI 正确渲染的前提。
- **行规程**：echo、规范模式、`Ctrl-D` EOF 等，都由内核在 PTY 上完成，管道做不到。

dsh 的 `SubprocessRuntime.spawnTerminal` 明确把这些需求写进契约：`write`（输入文本，无隐式换行）、`output`（按投递顺序的 UTF-8 输出字节）、`inspectForeground`（前台 pgid + 是否在等输入）、`signalForeground`（`SIGINT/SIGTERM/SIGKILL/SIGTSTP/SIGHUP`）、`terminate`（整棵会话树 TERM→grace→KILL 且等待 quiescence）。

## 目标与非目标

### 目标

1. 在 `sandboxd` 分配 PTY 并启动 argv 作为控制终端会话首领（`setsid` + `TIOCSCTTY` + `dup2` 到 0/1/2）。
2. 双向字节流：输入写 master、输出读 master；输出有可重连的环形缓冲（对齐 `/exec/attach` 的 replay 模型）。
3. 窗口尺寸调整、前台进程组查询、前台进程组信号、会话树终止（TERM→grace→KILL，可证明 quiescence）。
4. gRPC 面：7 个 RPC，网关路由到 session 的 pod，`TerminalStream` 复用现有 `ExecStream` 的 SSE→gRPC 代理模式。
5. 服务 dsh `spawnTerminal` 与 E2B `pty.*` 两个消费者，语义与 E2B 的 pty 面对齐。

### 非目标

- 不做窗口标题/OSC 序列解析、彩色解析、输出渲染——PTY 面只做**透明字节传递**，呈现归上层（dsh UI / E2B SDK）。
- 不做跨 pod 终端迁移（pause/resume、pod 回收即终端消亡，见"保真度差距"）。
- 不做多 reader 的实时扇出（单 reader + 环形缓冲 replay；第二个实时 reader 推迟）。
- 不改动 `Exec`/`ExecStream` 的既有语义；PTY 是独立新面。
- 不在本 KIP 内做 `input_waiting` 的 /proc 精确证明：本 KIP 只承诺 **positive-proof-only** 语义（M1 恒 `false`，见"已决问题"）。

## 现状盘点

| 组件 | 现状 | 需要新增 |
|---|---|---|
| `sandboxd`（Zig，`src/main.zig`） | 裸 HTTP/1.1，线程/连接，路由表硬编码 | `/pty/*` 7 个端点 + `pty.zig` 模块 |
| `sandboxd` `execctl.zig` | 进程控制表：pid → {stdin_fd, 配置快照, 环形输出, done}，自旋锁保护 | 并行的**终端会话表**（terminal_id → {master_fd, 会话首领 pid, rows/cols, 环形输出, 前台 pgid 缓存, done}） |
| 网关 `pkg/sandboxmatrix/grpc/server.go` | `sandboxdPost/Get` 代理、`ExecStream` 代理 `/exec/stream` SSE | 7 个 RPC 处理器 + `TerminalStream` SSE 代理 + terminal_id 路由注册表 |
| 网关 `mTLSStreamInterceptor` | 流式 RPC 除 `ExecStream` 外强制 mTLS（`server.go:959`） | 把 `TerminalStream` 纳入与 `ExecStream` 相同的处置并显式登记 |
| proto | `SandboxService` 单服务 | 新增 7 个 RPC + 消息 + `TerminalSignal` 枚举（首个 `oneof` 用于终端流） |

## 设计

### 5.1 终端语义（PTY 分配与进程启动）

`CreateTerminal` 在 sandboxd 内执行：

1. 打开 `/dev/ptmx` 得到 master fd；`ioctl(TIOCGPTN)` 取 pty 号、`ioctl(TIOCSPTLCK, 0)` 解锁；打开对应 `/dev/pts/N` 得到 slave fd。
2. `fork`；子进程：
   - `setsid()` 成为新会话首领（脱离沙箱 init 的会话，避免继承控制终端）；
   - `ioctl(slave, TIOCSCTTY, 0)` 把 slave 设为本会话控制终端；
   - `dup2(slave, 0/1/2)`，关闭多余 fd；
   - `chdir(workdir)`、应用 `env`（复用 `exec.zig` 的 `buildEnvp` 语义：默认 PATH/VIRTUAL_ENV + 用户 env 覆盖）；
   - `execve(argv[0], argv, envp)` —— **argv 不经过 `/bin/sh -c`**，这是与 `Exec` 的本质区别。
3. 父进程：持有 master fd，把它登记进终端会话表，返回 `terminal_id` + 会话首领 pid。

`argv` 由调用方提供完整数组（`argv[0]` 即程序），因此 dsh 可以启动 `bash`（交互式）作为终端，而不是 `sh -c "bash"` 这种失去任务控制的退化形态。

### 5.2 proto 面（`proto/sandbox/v1/sandbox.proto` 追加）

沿用单 `SandboxService`（与 `ExecStream`/`GetTranscript` 的增量历史一致；独立 `TerminalService` 见"备选方案"）。

```proto
// ── PTY terminal primitive (KIP-19) ─────────────────────────────────────────
rpc CreateTerminal(CreateTerminalRequest) returns (CreateTerminalResponse);
rpc TerminalStream(TerminalStreamRequest) returns (stream TerminalStreamResponse);
rpc TerminalWrite(TerminalWriteRequest)   returns (TerminalWriteResponse);
rpc TerminalResize(TerminalResizeRequest) returns (TerminalResizeResponse);
rpc TerminalForeground(TerminalForegroundRequest) returns (TerminalForegroundResponse);
rpc TerminalSignal(TerminalSignalRequest) returns (TerminalSignalResponse);
rpc TerminalDestroy(TerminalDestroyRequest) returns (TerminalDestroyResponse);

message CreateTerminalRequest {
  string session_id = 1;
  repeated string argv = 2;   // argv[0] 即程序；不 shell 解释
  string workdir = 3;
  map<string,string> env = 4; // 非敏感环境；secret 仍走 CreateSession 的 secret_refs
  int32  rows = 5;            // 初始窗口行数（>=1）
  int32  cols = 6;            // 初始窗口列数（>=1）
}
message CreateTerminalResponse {
  string terminal_id = 1;     // 不透明、branded；后续所有 terminal RPC 的句柄
  int32  pid = 2;             // 会话首领 pid（沙箱 pid 空间）
}

message TerminalStreamRequest { string terminal_id = 1; }
message TerminalStreamResponse {
  oneof frame {
    bytes data = 1;           // 终端输出字节，按投递顺序
    TerminalExit exit = 2;    // 终帧：顶层进程关闭
  }
}
message TerminalExit {
  int32  exit_code = 1;       // 进程退出码；0 = 正常
  string signal    = 2;       // 终止信号名（正常退出为空）
}

message TerminalWriteRequest {
  string terminal_id = 1;
  bytes  data = 2;            // 原始字节，无隐式换行
}
message TerminalWriteResponse { bool ok = 1; }

message TerminalResizeRequest {
  string terminal_id = 1;
  int32 rows = 2;
  int32 cols = 3;
}
message TerminalResizeResponse { bool ok = 1; }

message TerminalForegroundRequest { string terminal_id = 1; }
message TerminalForegroundResponse {
  int32 process_group_id = 1; // 当前前台进程组 id；-1 表示无法解析
  bool  input_waiting = 2;    // 尽力而为；MVP 恒 false（见保真度差距）
}

enum TerminalSignal {
  TERMINAL_SIGNAL_UNSPECIFIED = 0;
  TERMINAL_SIGNAL_INT  = 1;   // SIGINT
  TERMINAL_SIGNAL_TERM = 2;   // SIGTERM
  TERMINAL_SIGNAL_KILL = 3;   // SIGKILL
  TERMINAL_SIGNAL_TSTP = 4;   // SIGTSTP
  TERMINAL_SIGNAL_HUP  = 5;   // SIGHUP
}
message TerminalSignalRequest {
  string terminal_id = 1;
  TerminalSignal signal = 2;
}
message TerminalSignalResponse { int32 process_group_id = 1; } // 实际收到信号的 pgid

message TerminalDestroyRequest {
  string terminal_id = 1;
  int32  grace_ms = 2;        // TERM → grace → KILL 升级上限
}
message TerminalDestroyResponse { bool ok = 1; }
```

设计要点：

- `terminal_id` 是**不透明 branded id**（遵循仓库"跨边界 id 必须 branded"惯例），网关生成并持有 `terminal_id → session` 映射；后续 RPC 只带 `terminal_id`，不重复带 `session_id`。
- `TerminalStreamResponse` 用 `oneof` 区分数据帧与终帧——这是 proto 里**第一个 oneof**，因为终端流是判别式的（数据 vs 退出事实），比"字段为空即终帧"更无歧义。
- `TerminalSignal` 覆盖 dsh `SubprocessTerminalSignal` 的全集（`INT/TERM/KILL/TSTP/HUP`）。

### 5.3 sandboxd 实现（`src/pty.zig` + 终端会话表）

新增 `pty.zig`，并在 `main.zig` 路由表追加端点：

| 端点 | 方法 | 作用 | 映射 RPC |
|---|---|---|---|
| `/pty/create` | POST | 分配 PTY + 启动 argv 为控制终端会话首领 | `CreateTerminal` |
| `/pty/stream` | GET | SSE 流：终端输出（单 reader）+ 终帧 `{"exit":N,"signal":"…"}` | `TerminalStream` |
| `/pty/input` | POST | 写 master（base64 载荷，同 `/exec/stdin` 风格） | `TerminalWrite` |
| `/pty/resize` | POST | `ioctl(TIOCSWINSZ)` | `TerminalResize` |
| `/pty/foreground` | GET | `tcgetpgrp(master)` + 尽力 `input_waiting` | `TerminalForeground` |
| `/pty/signal` | POST | `kill(-pgid, sig)` 前台进程组 | `TerminalSignal` |
| `/pty/destroy` | POST | 会话树 TERM→grace→KILL + 等 quiescence + 关 master + 注销 | `TerminalDestroy` |

**终端会话表**（并行于 `execctl`，固定槽位 + 自旋锁）：

```
terminal_id → {
  master_fd,        // PTY master
  pid,              // 会话首领（session leader）
  rows, cols,       // 当前尺寸
  ring[BUFFER_MAX], // 输出环形缓冲（attach/replay；64KiB，对齐 execctl）
  fg_pgid,          // 前台 pgid 缓存（tcgetpgrp 惰性刷新）
  done,             // 顶层进程已收割
}
```

**关键并发设计——daemon 侧泵线程**：sandboxd 是"线程/连接"模型，`/pty/stream` 连接可能随时断开，但 master 必须有人持续排水，否则 slave 侧写满内核缓冲区后前台进程会阻塞。因此每个终端在创建时启动一个**daemon 拥有的 reader 线程**，独占 master fd：

1. 循环 `read(master)` → 追加环形缓冲 → 唤醒当前 stream 订阅者（条件变量/通知）。
2. 顶层进程退出 → 读满余量 → 置 `done` → 广播终帧 → 结束 pump。
3. `destroy` 时关闭 master、通知 pump 退出、注销表项。

这样 `TerminalStream` 订阅者可以中途断开/重连而不影响终端运行；重连走环形缓冲 replay（对齐 `/exec/attach` 语义），`truncated` 标记缓冲溢出。

**会话树终止**：`TerminalDestroy` 不以"杀一个 pid"为目标，而是枚举该会话（`ps -eo sid=,pgid=,stat=`，sid == 会话首领 pid 且非 Z/X 态）的所有进程组，先 `kill -TERM -- -PGID`，`grace_ms` 内轮询空，未空则 `kill -KILL`，最后校验无存活组、无存活首领 pid 才返回 `ok`（复用 E2B `terminal.ts` 的 `sessionProcessGroups`/`awaitSessionEmpty` 语义，但下沉到 sandboxd 原生实现，不再靠多次 `Exec 'ps'` 拼装）。

**PTY 分配与 Zig 版本**：Zig std 的 PTY 辅助随版本不同（`std.posix.openpty` 是否可用取决于 Zig 0.16）。实现按最小依赖走 `/dev/ptmx` + `ioctl(TIOCGPTN/TIOCSPTLCK)` + 打开 `/dev/pts/N`，避免依赖 std 的 PTY 封装；`grantpt/unlockpt` 由 `TIOCGPTN/TIOCSPTLCK` 等效替代。此细节在实现时按仓库当前 Zig 版本落地。

### 5.4 gateway 实现

| RPC | 网关动作 |
|---|---|
| `CreateTerminal` | 解析 session → pod IP → `POST /pty/create` → 生成 branded `terminal_id` → 记录 `terminal_id → session_id` → 返回 |
| `TerminalStream` | `GET /pty/stream`，把 SSE 帧映射为 `TerminalStreamResponse`（data 帧 → `data`，exit 帧 → `exit`）；复用 `ExecStream` 的 SSE→gRPC 流代理 |
| `TerminalWrite` / `TerminalResize` / `TerminalForeground` / `TerminalSignal` | 查 `terminal_id → session` → pod IP → 对应 sandboxd 端点（unary） |
| `TerminalDestroy` | 对应 `POST /pty/destroy`；成功后从路由注册表移除 `terminal_id` |

**terminal_id 路由注册表**：与现有后台 run 注册表（`GetSessionResponse.background_runs`）同构——`CreateTerminal` 登记、`TerminalDestroy` 移除、session 销毁/pod 回收时 GC。注册表是有界的（上限对齐后台 run 注册表），`terminal_id` 经注册表解析为 session → pod IP，因此**任意控制面节点都能服务任意终端 RPC**（节点无关，延续 KIP-18 P1 的"沙箱自有 pid / 节点无关"哲学）。

**mTLS**：`TerminalStream` 是流式 RPC，须显式纳入 `mTLSStreamInterceptor` 的处置（与 `ExecStream` 一致），并写测试证明"未认证的 TerminalStream 被拒、认证的通过"。

### 5.5 生命周期与并发

- 终端随 pod 生命周期：session 的 pod 回收（destroy / 温暖池回收 / pause）即终端消亡；`TerminalDestroy` 是显式提前回收，不销毁则随 pod 一并回收。
- `TerminalWrite` / `TerminalResize` / `TerminalSignal` / `TerminalForeground` 对同一终端的并发调用，由终端会话表的自旋锁串行化 master/`fg_pgid` 访问（对齐 `execctl` 的锁模型）。
- `TerminalDestroy` 幂等：已注销/已消亡的 terminal_id 返回干净的 not-found（对齐 `/exec/signal` 对已收割 pid 的处理）。
- 终端数量有界（沙箱 pod 内固定槽位，如 64，对齐 `execctl` 的 64 槽）；槽满则 `CreateTerminal` fail-loud，而不是静默降级。

## 与 dsh terminal seam 的映射

`@k8e-sandbox/dsh-k8e-sandbox-subprocess`（KIP-20 Phase 2）实现 `SubprocessRuntime.spawnTerminal`，映射如下：

| dsh `SubprocessTerminalHandle` | k8e RPC |
|---|---|
| 创建（`rows`/`cols`/`argv`/`cwd`/`env`） | `CreateTerminal` |
| `output`（Readable，UTF-8 输出字节） | `TerminalStream`（data 帧 → 流；exit 帧 → `done`） |
| `write(data)` | `TerminalWrite` |
| `inspectForeground()` | `TerminalForeground` |
| `signalForeground(sig)` | `TerminalSignal` |
| `terminate()`（TERM→grace→KILL，等 quiescence） | `TerminalDestroy` |
| `pid` | `CreateTerminalResponse.pid` |
| `done`（exitCode/signal） | `TerminalExit` |

> dsh 的 `spawnTerminal` handle 契约本身不含 resize；但 `dsh-terminal` 更上层（`terminal-bash` 的终端 UI）需要 resize。`TerminalResize` 为这一层服务，dsh 侧 provider 可用它按需补充（或在终端 UI 层调用）。

## 与 E2B pty 兼容的映射（KIP-18）

E2B SDK 的 `sandbox.pty.*` 面可直接落到本原语：

| E2B SDK | k8e RPC |
|---|---|
| `pty.create({ rows, cols, cwd, envs, onData })` | `CreateTerminal` + `TerminalStream`（`onData` 回调） |
| `pty.sendInput(pid, data)` | `TerminalWrite`（pid→terminal_id 映射归 KIP-18 兼容层，见下） |
| `pty.resize(pid, rows, cols)` | `TerminalResize` |
| `pty.kill(pid)` | `TerminalSignal`(KILL) / `TerminalDestroy` |

E2B 兼容层（KIP-18）用 `pid` 寻址，而本原语只暴露 canonical `terminal_id`（`CreateTerminalResponse` 同时返回 `terminal_id` 与 `pid`）。**`pid → terminal_id` 映射归 KIP-18 兼容层自建**：兼容层在 `pty.create` 时记录两者关联即可闭合。网关不引入 pid 别名——避免 pid 复用导致的双身份歧义，也保持原语身份单一。

## 保真度差距（诚实清单）

| 维度 | 目标 | 现状/限制 |
|---|---|---|
| `input_waiting` | 证明前台组正阻塞于 `read(0)` | M1 恒 `false`（positive-proof-only：`false` 含"不可证明"，绝不表示"确定不在等"）；精确的 `/proc wchan` 判定因 gVisor/Kata/Firecracker 的 procfs 语义不同，留待后续 KIP 按 runtime 验证 |
| 多 reader 实时扇出 | 多个并发 `TerminalStream` | 单 reader + 环形缓冲 replay；第二个实时 reader 推迟 |
| 输出完整性 | 全量输出 | 环形缓冲有界（64KiB），断线重连只回放尾部，`truncated` 标记；已连接的活跃 reader 不受此限（pump 直发） |
| 跨 pod 迁移 | pause/resume 后终端仍在 | 不支持：终端绑定 pod 的 pid 空间与 PTY，pause/resume 或 pod 回收即消亡 |
| 窗口标题/OSC | 解析标题、光标 | 不做解析，透明传字节，归上层 |
| `Ctrl-C` 语义 | 前台组收 SIGINT | 由 PTY 行规程天然提供（`ISIG` 默认开启）；raw 模式程序可关 |

这些差距不阻塞 MVP（dsh terminal 与 E2B pty 都接受 `input_waiting=false`、单 reader、pod 绑定）。

## 安全

- **控制终端权限**：PTY 在沙箱 pod 内分配，master 只由 sandboxd 持有，slave 归终端进程；网关进程不接触 master fd，不放大主机权限。
- **argv 不 shell 解释**：杜绝 `Exec` 的 `/bin/sh -c` 注入面；argv 逐项传递（NUL 拒绝，对齐 dsh `serializeValues`）。
- **env 不注入 secret**：`TerminalWrite` 面只传字节；secret 仍走 `CreateSession` 的 `secret_refs`，不进 `CreateTerminal.env`。
- **会话树终止可证明**：`TerminalDestroy` 用 `ps` 会话枚举 + 存活校验，避免"杀了首领、孤儿进程组还活着"；`grace_ms` 有界，拒绝无界等待。
- **mTLS**：`TerminalStream` 纳入 mTLS 流拦截器，与 `ExecStream` 同策略，杜绝未认证的终端流。
- **资源上限**：终端表有界（固定槽位），`TerminalDestroy`/session 回收负责 GC；避免终端泄漏撑爆 sandboxd。

## 验收标准

- [ ] `CreateTerminal` 在沙箱内分配 PTY，argv 作为会话首领 + 控制终端启动（`ps -o sid=,tpgid=` 验证 sid == pid，tpgid == pid）。
- [ ] `TerminalWrite` 写 master → 终端进程在 slave 收到；`TerminalStream` 收到 echo/输出，顺序正确，UTF-8 不破。
- [ ] `TerminalResize` 后 `stty size`（或 `TIOCGWINSZ`）反映新 rows/cols，SIGWINCH 送达。
- [ ] `TerminalForeground` 返回正确前台 pgid；`TerminalSignal`(INT/TSTP/TERM/KILL/HUP) 送达该 pgid。
- [ ] `TerminalDestroy`：TERM→grace→KILL 升级，返回时 `ps` 证明该会话无存活进程组；幂等，重复 destroy 干净 not-found。
- [ ] `TerminalStream` 断线重连回放环形缓冲；`truncated` 在缓冲溢出时置位；终帧 exit_code/signal 正确。
- [ ] 网关：terminal_id 路由注册表在 create/destroy/session 销毁时正确增删；`TerminalStream` 走 mTLS 且被未认证调用拒绝。
- [ ] dsh 快照：`@k8e-sandbox/dsh-k8e-sandbox-subprocess` 的 `spawnTerminal` 在 mock/真实网关上演 `bash` 终端，验证 write/read/Ctrl-C/resize/destroy 全链路。
- [ ] E2B 兼容：`pty.create/sendInput/resize/kill` 经兼容层闭合（KIP-18）。
- [ ] 文档：本 KIP + proto 注释 + `SKILL.md`/README 一处更新说明 PTY 面。

## 实现地图

| 阶段 | 内容 | 位置 |
|---|---|---|
| M1 | proto：7 RPC + 消息 + `TerminalSignal` 枚举 + `oneof` | `proto/sandbox/v1/sandbox.proto`，`make generate` 重生成 `pb` |
| M1 | sandboxd：`pty.zig`（分配/启动/输入/输出泵/尺寸/前台/信号/销毁）+ 终端会话表 + 路由 | `sandboxd/src/pty.zig`、`main.zig` |
| M1 | gateway：7 个 RPC 处理器 + `TerminalStream` SSE 代理 + terminal_id 路由注册表 + mTLS 登记 | `pkg/sandboxmatrix/grpc/server.go`（+ 新 `terminal.go`） |
| M2 | 单元/集成测试 | `sandboxd/src/pty_test.zig`、`pkg/sandboxmatrix/grpc/*_test.go` |
| M3 | dsh 消费：KIP-20 `spawnTerminal` 接本原语 | `plugins/deepseek-harness/packages/dsh-k8e-sandbox-subprocess` |
| M4 | E2B 兼容层 `pty.*` 闭合 | KIP-18 兼容层 |

## 测试计划

- **sandboxd 单测（Zig）**：PTY 分配与 slave 打开、`setsid`/`TIOCSCTTY` 会话首领断言、`tcgetpgrp` 前台组、`TIOCSWINSZ` 尺寸、`kill(-pgid)` 前台信号、会话树枚举 + TERM→KILL 升级 + 空集校验、环形缓冲 replay 与 `truncated`、并发 input/signal 串行化。
- **gateway 单测（Go）**：RPC↔sandboxd 代理、`TerminalStream` SSE 帧→`oneof` 映射、terminal_id 注册表增删/GC、mTLS 拦截器对 `TerminalStream` 的通过/拒绝。
- **e2e**：真实 K8E 集群里 `CreateTerminal` 启 `bash` → write `echo hi\n` → stream 读到输出 → resize → `Ctrl-C`（`\x03`）→ 前台组收 SIGINT → destroy → `ps` 证明无孤儿。
- **dsh keyless 快照**：脚本化模型 + mock gateway，外部断言 transcript（对齐 KIP-20 的测试契约）。
- **契约**：`k8e-sandbox-cli catalog`（KIP-16 M9）纳入 7 个终端 RPC，作为 CLI/SDK 生成的校验输入。

## 备选方案

| 方案 | 为什么不选 |
|---|---|
| 单条双向 `Terminal` bidi stream（控制+数据混流） | 需自造帧多路复用协议；与 k8e 现有 unary-heavy + SSE 代理风格不符，网关代理与测试更重 |
| 用 `Exec` + `script`/`tmux` 模拟 PTY | 在沙箱内再起一层进程/会话管理器，前台组语义丢失，脆弱且非原生 |
| 只做 dsh，跳过 E2B pty 兼容 | 同一原语即服务两者；E2B `pty.*` 是 KIP-18 已知缺口，不合并即重复造 |
| 独立 `TerminalService`（不改 `SandboxService`） | 可隔离，但网关当前单服务注册，拆第二个服务收益小；RPC 命名已带 `Terminal` 前缀，语义清晰 |
| 终帧用"字段为空即终帧"而非 `oneof` | 判别式协议用 `oneof` 更无歧义，且这是新增协议无兼容包袱 |

## 已决问题

| # | 问题 | 决定 |
|---|---|---|
| 1 | `input_waiting` 精确化 | **positive-proof-only，M1 恒 `false`**。`false` 语义为"不可证明"，绝不表示"确定不在等"（与 dsh 契约的 provable 措辞及 E2B 对齐）。精确判定依赖 `/proc` wchan，而 gVisor/Kata/Firecracker 的 procfs 语义不同，留待后续 KIP 按 runtime 验证 |
| 2 | E2B pid 寻址 | **`pid → terminal_id` 映射归 KIP-18 兼容层**。网关只暴露 canonical `terminal_id`（`CreateTerminalResponse` 已同时返回 pid 供需要者），不引入 pid 别名，避免 pid 复用歧义 |
