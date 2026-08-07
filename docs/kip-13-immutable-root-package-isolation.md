# KIP-13: 不可变根文件系统下的包安装隔离

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-06-04 | Proposed |

## Summary

在 sandbox pod 已有 `ReadOnlyRootFilesystem: true` 的前提下，自动将 `pip install` 和 `npm install` 的写入目标重定向到 `/workspace`（唯一的可写挂载点），保证 *系统不可变，所有可变内容都在 `/workspace`*。Agent 使用 `pip install X` / `npm install X` 即可，无需手动激活虚拟环境或配置路径。

## Motivation

### 当前状态

sandbox pod 已通过 `SecurityContext.ReadOnlyRootFilesystem: true` 强制执行只读根文件系统（`orchestrator.go:661`），`/workspace` 由 PVC 或 EmptyDir 挂载为唯一可写路径。但 **没有对包管理器做任何重定向**：

| 操作 | 目标路径 | 当前行为 |
|------|---------|---------|
| `pip install X` | `/usr/local/lib/python3.11/site-packages` | ❌ EROFS 写入失败 |
| `pip install --user X` | `/root/.local` | ❌ EROFS（`~` 在只读根） |
| `npm install`（本地） | `/workspace/node_modules` | ✅ 正常写入 |
| `npm install -g` | `/usr/lib/node_modules` | ❌ EROFS 写入失败 |
| `npm` 缓存（任何 install） | `/root/.npm` | ❌ `mkdir /root/.npm` 失败 → 本地 install 也报错 |

`PipInstall` RPC（`server.go:409`）直接运行 `pip install --no-cache-dir X`，没有任何重定向，在只读根文件系统下 **必定失败**。`npm install` 本地 node_modules 虽在 `/workspace` 可写，但 npm 缓存 `~/.npm` 同样命中只读根，导致 **本地 install 也报错**。

### 目标

Agent 无需了解沙箱内部结构，直接执行 `pip install` / `npm install` / `npm install -g` 即可工作。所有包管理器产生的可变状态（site-packages、node_modules、缓存、全局可执行文件）自动落在 `/workspace`，保证：

- 系统根 `/` 在任何 RuntimeClass 下均不可变
- warm-pool claim 延迟不受影响
- snapshot 恢复后所有已安装包就绪
- Agent 心智模型简单：`run "pip install numpy" --lang bash` 即可

## Design

### 架构

```
sandboxd (PID 1)
  │
  ├─ 每次 /exec 请求注入三层环境变量
  │   Layer 1: 系统默认（本 KIP）— npm_config_*, PATH
  │   Layer 2: Session 级（KIP-12）—— --env / --secret
  │   Layer 3: 条件激活（本 KIP）—— VIRTUAL_ENV + PATH（venv 存在后）
  │
  ├─ 首条 python/pip 命令触发懒创建
  │   python3 -m venv /workspace/.venv（一次性，~1-2s）
  │
  └─ 后续命令复用已有 venv
```

### Python：懒创建 venv（`/workspace/.venv`）

sandboxd 在每次 `/exec` 时检测命令前缀是否以 `pip`/`pip3`/`python`/`python3` 开头。若是，且 `/workspace/.venv` 不存在，则在执行命令前先运行 `python3 -m venv /workspace/.venv`（一次性，stat 守卫）。

venv 创建后，sandboxd 在 **Layer 3** 注入：

```
VIRTUAL_ENV=/workspace/.venv
PATH=/workspace/.venv/bin:<existing>
```

后续所有 python/pip 命令自动在 venv 上下文中运行（`pip` 直接指向 venv 中的 pip，不需要 `python3 -m pip`）。

### Node：重定向缓存与全局安装（npm config）

sandboxd 在 **Layer 1**（系统默认，无条件注入）设置：

```
npm_config_cache=/workspace/.cache/npm
npm_config_prefix=/workspace/.npm-global
PATH=/workspace/.npm-global/bin:<existing>
```

| npm 行为 | 结果 |
|----------|------|
| `npm install express`（CWD 下） | node_modules → `/workspace/node_modules`；缓存 → `/workspace/.cache/npm` |
| `npm install -g prettier` | 安装到 `/workspace/.npm-global`；bin → PATH |
| 缓存 | 始终写入 `/workspace/.cache/npm`，不碰 `~/.npm` |

### 与 KIP-12 环境注入的协作

KIP-12 定义了 session 级 `env` 与 `secret_refs`，由**网关在 exec 时解析注入**。本 KIP 的 Layer 1/3 由 **sandboxd 在 exec 时注入**，位于 Layer 2（session env）之下——session env 可覆盖系统默认值，但不能覆盖 Layer 3 的 venv 激活（确保隔离不受 session 配置干扰）。

最终 child 进程 env 合并顺序：
```
os.environ（sandboxd 继承的）
  → Layer 1 覆盖（npm config / path）
  → Layer 2 覆盖（session env / secrets）
  → Layer 3 覆盖（venv 激活，条件生效）
```

### 懒创建逻辑

```
handleExec(req):
  if req.command starts with "pip" / "python" / "pip3" / "python3":
    if stat("/workspace/.venv") == ENOENT:
      run("python3 -m venv /workspace/.venv")
  
  env = buildEnv(req.env)  // Layer 1 + 2 + 3
  exec_with_env(req.command, req.workdir, env)
```

### Warm-pool 兼容性

- venv 在 `/workspace/.venv` 下，每次 warm claim 后 `/workspace` 为新的 EmptyDir → venv 自然不存在，懒创建触发。
- claim 延迟：**不受影响**（不创建 venv）。
- 懒创建延迟：~1-2s 首条 Python 命令，与 `pip install` 网络下载重叠——实际感知影响为零。
- 从未使用 Python 的 session：零开销（stat 短路，不创建 venv）。

### Snapshot 兼容性

`/workspace/.venv`、`/workspace/.cache/npm`、`/workspace/.npm-global` 均位于 `/workspace` 且非挂载点，KIP-10 的 `tar czf /workspace` 自然打包/恢复，无需特殊处理。恢复后 venv 已存在 → stat 命中 → 懒创建短路。

### 文件变更

| 文件 | 变更 |
|------|------|
| `sandboxd/src/exec.zig` | `ExecRequest` 新增 `env: map<string,string>`（如 KIP-12 所述）；新增 `buildExecEnv()` 合并三层 env；`handleExec` / `runCommand` 构建并传入 envp；`handleExec` 新增懒 venv 创建逻辑（python/pip 前缀检测 + stat + `python3 -m venv`） |
| `pkg/sandboxmatrix/grpc/server.go` | `PipInstall` 可简化——移除手动 `pip install` 拼接，改用通用 exec 路径即可，venv 激活由 sandboxd 保证 |
| `sandbox/Dockerfile` | 无需变更（基础镜像保持干净） |
| `pkg/sandboxcli/skills/k8e-sandbox/SKILL.md` | 文档化：`pip install` / `npm install` 自动隔离到 `/workspace` |

### 边界处理

| 场景 | 行为 |
|------|------|
| venv 在 session 中间被删除 | 下次 python/pip 命令重新创建 |
| `python3 -m venv` 失败（镜像缺少 venv 模块） | 报错返回，提示运维安装 `python3-venv` |
| 命令同时是 `pip install -r`（`-r` 不在命令名前缀） | 前缀检测匹配 `pip` → 正常触发懒创建 |
| 用户在 venv 外手动 `python3 -m pip install` | 匹配 `python` 前缀 → 正常触发 |
| npm 缓存目录已满（EmptyDir 容量耗尽） | 标准 ENOSPC 报错，由现有超时/报错机制处理 |
| snapshot 恢复后 venv 的 python 路径指向旧 sandbox 的绝对路径 | `venv` 支持 `--without-pip` 仍需要 pip；若基础镜像更新 Python 版本，旧 venv 可能不可用——接受此为 snapshot 的可预期降级行为，用户可 `rm -rf /workspace/.venv` 后重新触发 |

## 相关 KIP

- [KIP-3](./kip-3-agentic-ai-sandbox-matrix.md) — Sandbox Matrix 总体设计（pod spec、SecurityContext）
- [KIP-10](./kip-10-sandbox-snapshot.md) — Workspace snapshot（venv 与缓存随 `/workspace` 自然打包）
- [KIP-12](./kip-12-sandbox-ports-env-secrets.md) — Session 级 env/secret 注入（本 KIP 的 Layer 1/3 与 KIP-12 的 Layer 2 协作）
