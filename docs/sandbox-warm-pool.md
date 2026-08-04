# SandboxWarmPool 使用文档

| 更新 | 状态 |
|------|------|
| 2026-08-04 | Current |

`SandboxWarmPool` 是 K8E 沙箱矩阵的预热池 CRD：预先启动一批 sandbox pod（`sandbox.k8e.io/state=warm`），会话创建时原子领取（`warm → active`），避免冷启动延迟。本文档覆盖字段语义与调优，重点是自适应扩缩字段 `maxSize` / `minSize` / `idleTTLSeconds`。

架构背景见 [KIP-3: Agentic AI Sandbox Matrix](kip-3-agentic-ai-sandbox-matrix.md)。

## 字段总览

```yaml
apiVersion: k8e.sh/v1alpha1
kind: SandboxWarmPool
metadata:
  name: default
  namespace: sandbox-matrix
spec:
  templateRef:            # 可选：SandboxTemplate 引用
    name: default-sandbox-template
  size: 5                 # 静态目标池大小（默认 1）
  runtimeClass: gvisor    # 可选：覆盖 --sandbox-default-runtime，支持混合 runtime 集群
  minSize: 2              # 自适应下界（默认 = size）
  maxSize: 8              # 自适应上界（> size 即开启自适应模式）
  idleTTLSeconds: 900     # 可选：本池 warm pod 闲置回收 TTL（默认 = sessionTTL × 2）
status:
  readyCount: 5
  pendingCount: 0
```

| 字段 | 类型 | 默认 | 说明 |
|------|------|------|------|
| `templateRef` | object | 无 | 可选的 `SandboxTemplate` 引用 |
| `size` | int | 1 | 目标池大小（自适应模式下为基线） |
| `runtimeClass` | string | `--sandbox-default-runtime` | 本池 warm pod 的运行时；会话领取时按 `sandbox.k8e.io/runtime-class` 标签精确匹配 |
| `minSize` | int | `size` | 自适应下界 |
| `maxSize` | int | `size` | 自适应上界；**仅当 `maxSize > size` 时开启自适应模式** |
| `idleTTLSeconds` | int | `sessionTTL × 2` | 本池 warm pod 的闲置回收 TTL |

## 基础用法

```bash
# 创建预热池（5 个 gVisor warm pod）
kubectl apply -f - <<'EOF'
apiVersion: k8e.sh/v1alpha1
kind: SandboxWarmPool
metadata:
  name: default
  namespace: sandbox-matrix
spec:
  size: 5
  runtimeClass: gvisor
EOF

# 查看池状态
kubectl get sandboxwarmpool -n sandbox-matrix
```

所有 warm pod 创建时带 `sandbox.k8e.io/runtime-class` 标签；会话领取时只认**同 runtime** 且 **sandboxd 已就绪**（Running + Ready 条件 + `:2024` TCP 拨测）的 warm pod，避免领到仍在启动或已死的 pod。领取成功后控制器立即补池（不等 10s 轮询）。

## 自适应扩缩（`maxSize` / `minSize`）

默认池大小是静态的：突发流量会冷启动，空闲时又白白占用内存。配置 `maxSize > size` 即开启自适应模式：

```yaml
spec:
  size: 2      # 基线
  minSize: 2   # 空闲时不低于 2
  maxSize: 10  # 突发时可扩到 10（受 computeMaxPods 容量上限约束）
```

行为：

1. **突发增长**：reconciler 每轮根据冷启动计数增量调整目标。出现冷启动（池子没接住）后，目标 = `max(size, 近期冷启动数)`，向上夹在 `[minSize, maxSize]`。
2. **需求衰减**：连续 **5 分钟**没有新的冷启动，目标回落。回落只停止继续建 pod；存量多余 warm pod 由闲置回收器按 TTL 清除（见下节），所以想要快速缩容应同时配置较短的 `idleTTLSeconds`。
3. **容量上限**：目标同时受 `computeMaxPods` 约束——跨**所有节点**求和的 allocatable 内存与 CPU（各留 10% buffer）的较小值。

不配置 `maxSize`（或 `maxSize <= size`）时行为与旧版完全一致（静态池），向后兼容。

## 按池闲置回收（`idleTTLSeconds`）

全局默认回收 TTL 是 `SandboxMatrix.spec.sessionTTL × 2`（默认 2 小时），偏粗。本字段允许按池收紧：

```yaml
spec:
  size: 3
  idleTTLSeconds: 300   # warm pod 闲置 5 分钟即回收
```

机制：创建 warm pod 时把该值以 `sandbox.k8e.io/idle-ttl-seconds` 注解写到 pod 上，闲置回收器优先读注解，缺省才回退 `sessionTTL × 2`。适合与自适应缩池配合（需求衰减后快速释放内存），或对内存敏感的多池环境按池差异化配置。

## 容量计算（`computeMaxPods`）

```text
maxPods = min(
  sum(所有节点 allocatable 内存) × 0.9 / 单 pod 内存限制,   # 默认 512Mi
  sum(所有节点 allocatable CPU) × 0.9 / 单 pod CPU 限制      # 默认 500m
)
```

- 跨节点**求和**（warm pod 可调度到任意节点），并新增 CPU 维度，取更紧的约束。
- 节点不可用（无 node / 无 allocatable）时返回 `0`，即不设上限。

## 观测

控制器每 10s（或收到领取信号时）把指标写入 `SandboxMatrix.status`：

| status 字段 | 含义 |
|------------|------|
| `readyWarmCount` | Running 且就绪的 warm pod 数 |
| `activeSessions` | 活跃会话数 |
| `claimedFromWarm` | 累计从预热池领取的会话数 |
| `coldStarts` | 累计冷启动会话数 |
| `avgClaimLatencyMs` | 平均 pod 获取延迟（ms） |
| `maxPods` / `totalPods` | 容量上限 / 当前 sandbox pod 总数 |

```bash
kubectl get sandboxmatrix -n sandbox-matrix -o jsonpath='{.items[0].status}' | jq
```

**命中率** = `claimedFromWarm / (claimedFromWarm + coldStarts)`。调优建议：

- 命中率高（>0.9）且无冷启动 → 池子偏大，可下调 `size` 或调短 `idleTTLSeconds`。
- 命中率低、频繁冷启动 → 上调 `size` / `maxSize`，或检查 warm pod 是否长时间 `readyWarmCount < size`（此时看 pod 状态排查 sandboxd 就绪问题）。
- 计数器是控制器进程启动以来的累计值，跨进程重启会归零。

## 与其他机制的关系

| 机制 | 说明 |
|------|------|
| 就绪门禁 | 领取前要求 Ready 条件 + `:2024` TCP 拨测；Running 但 sandboxd 未就绪的 warm pod 会被跳过，超过 5 分钟未就绪会被回收重建 |
| runtime 标签 | `sandbox.k8e.io/runtime-class` 精确匹配，gVisor 池不会被子会话跨 runtime 领取 |
| 领取即补池 | 领取成功后立即触发一轮 reconcile，池子不短暂亏空 |
| 会话容量 | `CreateSession` 前的 `CheckCapacity` 仍以首个节点内存估算，与 `computeMaxPods`（全节点）口径不同，极端多节点场景以实际调度为准 |

## 验证命令

```bash
# 池与 pod 状态
kubectl get sandboxwarmpool,sandboxmatrix -n sandbox-matrix
kubectl get pods -n sandbox-matrix -l sandbox.k8e.io/state=warm -o wide

# 查看 warm pod 上的 TTL 注解
kubectl get pod -n sandbox-matrix -l sandbox.k8e.io/state=warm \
  -o jsonpath='{range .items[*]}{.metadata.name}{"\t"}{.metadata.annotations.sandbox\.k8e\.io/idle-ttl-seconds}{"\n"}{end}'

# 命中率与延迟
kubectl get sandboxmatrix -n sandbox-matrix -o jsonpath='{.items[0].status}' | jq
```
