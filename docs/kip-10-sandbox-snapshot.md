# KIP-10: Sandbox Workspace Snapshot

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-06-03 | Implemented |

## Summary

新增 `k8e sandbox snapshot` 命令组，支持保存和恢复 sandbox workspace 快照。当前 workspace PVC 在 session destroy 或 TTL GC 后删除，所有文件丢失。快照机制允许 Agent 持久化工作成果并在后续 session 中恢复。

参考 [OpenAI Agents Sandbox Snapshots](https://openai.github.io/openai-agents-python/sandbox/guide/#snapshotspec) 设计。

## Motivation

KIP-8 的 session 生命周期是短暂的：

```
Agent 工作流:
  1. create session → 安装依赖 + 克隆仓库 (2min)
  2. 执行分析任务 (30min)
  3. destroy session → 所有文件丢失 ❌
  
  下次类似任务:
  4. create session → 重新安装依赖 + 克隆仓库 (2min) 😞
```

典型场景：
- **长任务中断**：训练进行到一半，需要暂停，下次继续
- **多阶段工作流**：阶段 1 生成数据 → 阶段 2 分析 → 两个阶段可能跨天
- **模板复用**：预装常用工具链（Python 2.7 + 3.11 + Node 20）的工作区模板，每次 clone 不必重装
- **协作**：用户 A 在 workspace 里准备好数据，用户 B 基于同一 workspace 继续工作

## Design

### 命令

```bash
# 保存当前 session 的 workspace 为命名快照
k8e sandbox snapshot save <session-id> <name>

# 列出所有快照
k8e sandbox snapshot list

# 从快照恢复为新 session
k8e sandbox snapshot restore <name>
  [--runtime gvisor] [--tenant <id>] [--allowed-hosts ...]

# 删除快照
k8e sandbox snapshot delete <name>
```

### 快照存储

```
~/.k8e/sandbox/snapshots/
  my-template/
    workspace.tar.gz      ← PVC 内容打包
    metadata.json         ← 快照元数据
  project-a-stage1/
    workspace.tar.gz
    metadata.json
```

**为什么用 tar.gz 而非 CSI snapshot？**

| 方案 | 优点 | 缺点 |
|------|------|------|
| tar.gz 文件 | 不需要 CSI driver，直接可用；可跨集群传输；占用空间小 | 大 workspace 保存慢；不在 K8s 存储层 |
| CSI VolumeSnapshot | K8s 原生；Restic 可做增量 | 需要 CSI snapshotter 部署；集群依赖；不可跨集群 |

**选择 tar.gz**：K8E 是单二进制分发，不应依赖可选的 CSI driver。tar.gz 简单可靠，适用于 Agent 工作区（通常 < 1GB）。

### metadata.json

```json
{
  "name": "my-template",
  "created_at": "2026-05-30T10:00:00Z",
  "session_id": "sess-abc123",
  "tenant_id": "my-project",
  "size_bytes": 52428800,
  "file_count": 142
}
```

### 实现流程

**save**：
```
1. 校验 session 存在且 Active
2. Exec: tar czf /tmp/_snapshot.tar.gz -C /workspace .
3. ReadFile: 下载 _snapshot.tar.gz 内容
4. 写入 ~/.k8e/sandbox/snapshots/<name>/workspace.tar.gz
5. 写入 metadata.json
6. 输出 {"ok":true,"name":"my-template","size_bytes":...}
```

**restore**：
```
1. 读取 metadata.json + workspace.tar.gz
2. CreateSession (gRPC)
3. WriteFile: 上传 workspace.tar.gz 到 sandbox
4. Exec: tar xzf /tmp/_snapshot.tar.gz -C /workspace
5. Exec: rm /tmp/_snapshot.tar.gz
6. 写 state 文件
7. 输出 {"session_id":"...","restored_from":"my-template"}
```

**list**：
```
1. 扫描 ~/.k8e/sandbox/snapshots/ 目录
2. 读取每个子目录的 metadata.json
3. 格式化输出
```

**delete**：
```
1. 删除 ~/.k8e/sandbox/snapshots/<name>/ 目录
```

### 输出格式

```bash
# save
$ k8e sandbox snapshot save sess-abc my-template
{"ok":true,"name":"my-template","size_bytes":52428800,"file_count":142}

# list
$ k8e sandbox snapshot list
{"snapshots":[{"name":"my-template","created_at":"...","size_bytes":52428800},{"name":"project-a","created_at":"...","size_bytes":1048576}]}

# restore → 返回 session_id（和 create 格式一致）
$ k8e sandbox snapshot restore my-template
{"session_id":"sess-new-xxx","pod_ip":"10.42.1.55","restored_from":"my-template"}

# restore + 开始工作（等价于 restore + run）
$ SID=$(k8e sandbox snapshot restore my-template | jq -r .session_id)
$ k8e sandbox run "python3 train.py --resume" --session-id $SID

# delete
$ k8e sandbox snapshot delete my-template
{"ok":true}
```

### 与 manifest 组合

```bash
# 从快照恢复 + 叠加 manifest（新文件添加到已有 workspace）
# 三步：
SID=$(k8e sandbox snapshot restore my-template | jq -r .session_id)
# 手动 write 新文件
k8e sandbox run "python3 /workspace/stage2.py" --session-id $SID
```

### 边界处理

| 场景 | 行为 |
|------|------|
| workspace 过大（>500MB） | 输出 warning 到 stderr，继续保存 |
| session 已销毁 | 报错 "session not active" |
| 同名快照已存在 | `--force` 覆盖，否则报错 |
| restore 时原 session 模板不存在 | CreateSession 用默认值（gvisor, 默认 allowed-hosts） |
| 快照文件损坏 | metadata 校验失败，报错 |

### 文件变更

| 文件 | 变更 |
|------|------|
| `pkg/sandboxcli/snapshot.go` | 新文件：快照 save/restore/list/delete 逻辑 |
| `pkg/sandboxcli/commands.go` | 新增 `SnapshotCommand()` 命令组 |
| `pkg/cli/cmds/sandbox.go` | sandbox 命令组注册 snapshot 子命令 |
| `skills/k8e-sandbox/SKILL.md` | 新增 snapshot 使用示例 |

## 相关 KIP

- [KIP-8](./kip-8-skill-cli-replace-mcp.md) — CLI sandbox 命令（本 KIP 的前置依赖）
- [KIP-9](./kip-9-sandbox-workspace-manifest.md) — Workspace manifest（与本 KIP 互补：manifest 定义初始状态，snapshot 持久化中间状态）
