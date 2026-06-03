# KIP-9: Sandbox Workspace Manifest

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-06-03 | Implemented |

## Summary

新增 `--manifest` flag 到 `k8e sandbox create`，支持声明式定义 sandbox 初始工作区。Agent 通过 YAML 文件指定文件、目录和 Git 仓库，CLI 在 CreateSession 后自动 materialize 到 PVC，无需 Agent 手动执行多次 `write` + `git clone`。

参考 [OpenAI Agents Sandbox Manifest](https://openai.github.io/openai-agents-python/sandbox/guide/#manifest) 设计。

## Motivation

KIP-8 的 CLI 模式要求 Agent 手动构建 workspace：

```bash
SID=$(k8e sandbox create | jq -r .session_id)
echo "# Task" | k8e sandbox write $SID /workspace/task.md
echo "print('hello')" | k8e sandbox write $SID /workspace/script.py
k8e sandbox run "git clone https://github.com/org/repo.git /workspace/repo" --session-id $SID
k8e sandbox run "python3 /workspace/script.py" --session-id $SID
```

4 次 CLI 调用 + 4 次 gRPC 往返，Agent 需要维护 session ID 跨命令，容易出错。

Manifest 模式：

```bash
k8e sandbox create --manifest workspace.yaml | jq -r .session_id
# manifest 内容已自动 materialize，直接开始工作
k8e sandbox run "python3 /workspace/script.py" --session-id $SID
```

## Design

### Manifest 格式

```yaml
# workspace.yaml
entries:
  - file:
      path: task.md
      content: |
        # Analysis Task
        Read the data in repo/data.csv and produce a summary report.
  - dir:
      path: output
  - dir:
      path: scripts
  - file:
      path: scripts/analyze.py
      content: |
        import pandas as pd
        df = pd.read_csv("/workspace/repo/data.csv")
        print(df.describe())
  - gitRepo:
      path: repo
      repo: https://github.com/example/project.git
      ref: main
```

### CLI 命令

```bash
# 从 manifest 文件创建 session
k8e sandbox create --manifest workspace.yaml

# 指定 session-id
k8e sandbox create --manifest workspace.yaml --session-id my-session

# 结合其他 flag
k8e sandbox create --manifest workspace.yaml --runtime kata --allowed-hosts pypi.org,github.com

# 简单的 git clone（不需要完整 manifest 文件）
k8e sandbox create --git-repo https://github.com/org/repo.git
k8e sandbox create --git-repo https://github.com/org/repo.git --git-ref develop --git-path src
```

### Materialization 流程

```
k8e sandbox create --manifest workspace.yaml
  │
  ├── 1. 解析 YAML → []Entry
  ├── 2. CreateSession (gRPC) → session_id
  ├── 3. 按顺序 materialize 每个 entry:
  │     ├── File: WriteFile(gRPC) 写入内容
  │     ├── Dir: Exec(gRPC) "mkdir -p <path>"
  │     └── GitRepo: Exec(gRPC) "git clone --depth 1 -b <ref> <repo> <path>"
  ├── 4. materialize 失败 → DestroySession + 报错
  └── 5. 写 state 文件 + 输出 {"session_id":"...","entries_materialized":3}
```

**CLI 侧 materialization**（不修改 proto/gRPC）：
- 所有 entry 由 CLI 进程通过已有的 gRPC 方法依次执行
- 不需要新增 proto RPC，不需要改 orchestrator
- 约 100 行 Go 代码

### 错误处理

```bash
# 部分成功
$ k8e sandbox create --manifest workspace.yaml
# File task.md ✓
# Dir output/ ✓
# GitRepo repo ✗ (clone failed: authentication required)
{"error":"manifest materialization failed at entry 3/3: git clone: authentication required"}
exit 1
# session 已自动 destroy
```

### `k8e sandbox run` 也支持 --manifest

`run` 本身会 auto-create session，所以也可以接受 `--manifest`：

```bash
k8e sandbox run "python3 /workspace/analyze.py" --lang bash --manifest workspace.yaml
```

内部：先 create + materialize，再 exec code。Agent 一个命令完成全部准备+执行。

### 实现范围

| Entry 类型 | 实现 | 说明 |
|-----------|------|------|
| `file` | WriteFile RPC | 支持 `content` (内联) 和 `source` (本地文件路径，未来) |
| `dir` | Exec RPC `mkdir -p` | |
| `gitRepo` | Exec RPC `git clone --depth 1` | 默认 shallow clone 节省空间；支持 `ref` (branch/tag/commit) |

**未来扩展**（本 KIP 不做）：
- `localFile` / `localDir`：从 Agent 主机复制文件到 sandbox
- `s3Mount` / `gcsMount`：远程存储挂载
- `SandboxWorkspace` CRD：命名 workspace 模板，跨 session 复用

### 命令变更

`k8e sandbox create` 新增 flags：

| Flag | 描述 |
|------|------|
| `--manifest <path>` | YAML manifest 文件路径 |
| `--git-repo <url>` | Git 仓库 URL（快捷方式，等价于单 entry manifest） |
| `--git-ref <ref>` | Git ref（配合 `--git-repo`，默认 main） |
| `--git-path <path>` | 仓库在 workspace 中的路径（默认 repo） |

`k8e sandbox run` 新增 flags：

| Flag | 描述 |
|------|------|
| `--manifest <path>` | 同 create（仅在 auto-create session 时生效） |

### 文件变更

| 文件 | 变更 |
|------|------|
| `pkg/sandboxcli/manifest.go` | 新文件：YAML 解析 + materialization 逻辑 |
| `pkg/sandboxcli/commands.go` | `create` 和 `run` 命令新增 `--manifest` flag |
| `skills/k8e-sandbox/SKILL.md` | 新增 manifest 使用示例 |

## 相关 KIP

- [KIP-8](./kip-8-skill-cli-replace-mcp.md) — CLI sandbox 命令（本 KIP 的前置依赖）
- [KIP-10](./kip-10-sandbox-snapshot.md) — Workspace 快照持久化（与本 KIP 互补）
