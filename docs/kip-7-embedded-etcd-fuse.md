# KIP-7: Embedded etcd 融合方案 — 集成官方 embed 包

> **状态**: Implemented
> **作者**: xiaods
> **日期**: 2025-05-20
> **关联**: KIP-6 (Embedded etcd 设计方案)

---

## 1. 背景

当前 K8E 对 embedded etcd 的封装完全自行实现（`pkg/etcd/etcd.go` ~600 行），手动管理 listeners、TLS 配置、server 生命周期。而 etcd 官方从 v3.5 开始提供了稳定的 `go.etcd.io/etcd/server/v3/embed` 包，支持一行代码嵌入 etcd。

### 当前 vs 目标

**当前**（自封装）:
```
K8E Server → pkg/etcd/etcd.go (自行管理 listener/server 生命周期)
```

**目标**（官方 embed 集成）:
```
K8E Server → embed.StartEtcd(cfg) → 自动管理所有生命周期
```

---

## 2. etcd embed API 能力

参考源码：`go.etcd.io/etcd/server/v3/embed`

### 核心 API

```go
import "go.etcd.io/etcd/server/v3/embed"

cfg := embed.NewConfig()
cfg.Dir = "/var/lib/k8e/etcd"
cfg.Name = "k8e-server-1"
cfg.ListenPeerUrls = []url.URL{*mustParse("http://0.0.0.0:2380")}
cfg.ListenClientUrls = []url.URL{*mustParse("http://0.0.0.0:2379")}
cfg.MaxRequestBytes = 10 * 1024 * 1024   // 10MB
cfg.MaxConcurrentStreams = 1000
cfg.QuotaBackendBytes = 2 * 1024 * 1024 * 1024  // 2GB

e, err := embed.StartEtcd(cfg)
defer e.Close()

select {
case <-e.Server.ReadyNotify():
    log.Println("etcd ready!")
case <-time.After(60 * time.Second):
    e.Server.Stop()
}
```

### 返回的 Etcd 结构

```go
type Etcd struct {
    Peers   []*peerListener
    Clients []net.Listener
    Server  *etcdserver.EtcdServer   // 核心服务器
    Close()                          // 优雅关闭
    Err()    <-chan error            // 错误通道
    Config() Config                  // 当前配置
}
```

### embed.Config 关键字段

| 字段 | 类型 | 说明 |
|---|---|---|
| `Dir` | string | 数据目录 |
| `WalDir` | string | WAL 目录 |
| `Name` | string | 节点名称 |
| `ListenPeerUrls` | []url.URL | Peer 监听 |
| `ListenClientUrls` | []url.URL | Client 监听 |
| `AdvertisePeerUrls` | []url.URL | Peer 广播 |
| `AdvertiseClientUrls` | []url.URL | Client 广播 |
| `QuotaBackendBytes` | int64 | 存储上限 |
| `MaxRequestBytes` | uint | 最大请求 |
| `MaxConcurrentStreams` | uint32 | 最大并发流 |
| `AutoCompactionMode` | string | "periodic" / "revision" |
| `TickMs` / `ElectionMs` | uint | Raft 参数 |
| `ClientTLSInfo` | transport.TLSInfo | 客户端 TLS |
| `PeerTLSInfo` | transport.TLSInfo | Peer TLS |
| `EnableGRPCGateway` | bool | gRPC 网关 |
| `EnableDistributedTracing` | bool | OpenTelemetry |
| `ServiceRegister` | func(\*grpc.Server) | 自定义 gRPC 服务 |
| `UserHandlers` | map[string]http.Handler | 自定义 HTTP handler |

---

## 3. 实施方案

### 3.1 替换自封装代码

**新建 `pkg/embedw/` 封装包**，桥接 `embed.StartEtcd()` 和 K8E 管理逻辑：

```
pkg/embedw/
├── config.go    # K8E Config → embed.Config 转换
├── lifecycle.go # 启动/停止/健康检查
└── metrics.go   # 指标适配
```

**核心转换函数**:

```go
func (e *ETCD) buildEmbedConfig() *embed.Config {
    cfg := embed.NewConfig()
    
    cfg.Name = e.name
    cfg.Dir = dbDir(e.config)
    cfg.WalDir = walDir(e.config)
    
    cfg.ListenPeerUrls = e.config.PeerURLs
    cfg.ListenClientUrls = e.config.ClientURLs
    cfg.AdvertisePeerUrls = e.config.AdvertisePeerUrls
    cfg.AdvertiseClientUrls = e.config.AdvertiseClientURLs
    
    // 性能调优（覆盖 embed 默认值）
    cfg.MaxRequestBytes = 10 * 1024 * 1024
    cfg.MaxConcurrentStreams = 1000
    cfg.QuotaBackendBytes = e.config.QuotaSize
    cfg.MaxLearners = e.config.MaxLearners
    
    // 禁用不必要功能（减小体积和攻击面）
    cfg.EnablePprof = false
    cfg.EnableGRPCGateway = false   // K8E 有自己的 gRPC 网关
    
    // TLS
    if !e.config.PeerTLSInfo.Empty() {
        cfg.PeerTLSInfo = *e.config.PeerTLSInfo
    }
    if !e.config.ClientTLSInfo.Empty() {
        cfg.ClientTLSInfo = *e.config.ClientTLSInfo
    }
    
    return cfg
}
```

**替换 cluster() 方法**:

```go
func (e *ETCD) cluster(ctx context.Context) error {
    cfg := e.buildEmbedConfig()
    
    etcdInstance, err := embed.StartEtcd(cfg)
    if err != nil {
        return fmt.Errorf("failed to start embedded etcd: %w", err)
    }
    e.embed = etcdInstance
    
    select {
    case <-etcdInstance.Server.ReadyNotify():
        logrus.Info("embedded etcd is ready")
    case <-time.After(60 * time.Second):
        etcdInstance.Server.Stop()
        return fmt.Errorf("embedded etcd startup timeout")
    case <-ctx.Done():
        etcdInstance.Close()
        return ctx.Err()
    }
    
    // 保持原有 client 初始化逻辑
    return e.startClient(ctx)
}
```

**优雅关闭**:

```go
func (e *ETCD) Close() {
    if e.embed != nil {
        e.embed.Close()  // embed.Close() 处理所有清理
    }
}
```

### 3.2 自定义 gRPC 服务注册

利用 embed 包的 `ServiceRegister` 回调，在 etcd gRPC server 上注册自定义服务：

```go
func (e *ETCD) registerCustomServices(srv *grpc.Server) {
    // 注册 K8E 自定义 gRPC 服务（如 sandbox orchestrator）
    pb.RegisterSandboxServiceServer(srv, e.orchestrator)
}

cfg.ServiceRegister = e.registerCustomServices
```

### 3.3 用户自定义 HTTP Handler

利用 embed 包的 `UserHandlers` 添加 K8E 特定的 HTTP 端点：

```go
cfg.UserHandlers = map[string]http.Handler{
    "/healthz": http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
        w.WriteHeader(http.StatusOK)
        w.Write([]byte("ok"))
    }),
    "/readyz": e.readyHandler,
}
```

### 3.4 分布式追踪

启用 OpenTelemetry 支持：

```go
if e.config.EnableTracing {
    cfg.EnableDistributedTracing = true
    cfg.DistributedTracingAddress = e.config.TracingEndpoint
    cfg.DistributedTracingServiceName = "k8e-etcd"
    cfg.DistributedTracingSamplingRatePerMillion = e.config.TracingSampleRate
}
```

---

## 4. 清理清单

### 必做

| 任务 | 文件 | 状态 |
|---|---|---|
| 移除 `build.zig` SQLite CFLAGS | `build.zig:101-102` | 待执行 |
| 审查 `go.etcd.io/bbolt` 是否被直接导入 | - | 待执行 |
| 更新 `managed.go` 注释 | `pkg/cluster/managed.go:4` | 待执行 |
| 替换 `etcd.cluster()` 为 embed 启动 | `pkg/etcd/etcd.go` | 待执行 |
| 替换关闭逻辑 | `pkg/etcd/etcd.go` | 待执行 |

### 可选（后续优化）

| 任务 | 说明 |
|---|---|
| 保留手动 TLS 回退 | 如果 embed 包的 TLS 配置不够灵活 |
| metrics 端点迁移 | 使用 embed 内置 metrics server 替代自定义端点 |
| 动态配置热更新 | 利用 embed.Config 结构实现运行时调优 |

---

## 5. 兼容性保证

| 能力 | 状态 | 说明 |
|---|---|---|
| etcd client API | ✅ 兼容 | `e.Server.Client()` 返回标准 clientv3.Client |
| S3 备份 | ✅ 兼容 | 通过 client API 操作 |
| Learner 节点 | ✅ 兼容 | embed.Config.MaxLearners |
| 快照/恢复 | ✅ 兼容 | 通过 client API 操作 |
| 集群 join/leave | ✅ 兼容 | embed 包原生支持 |
| TLS 通信 | ✅ 兼容 | embed.Config TLS 字段 |
| 分布式追踪 | ✅ 新增 | embed 原生支持 OpenTelemetry |
| gRPC Gateway | ✅ 可选 | K8E 已自带，建议禁用 |

---

## 6. 风险与缓解

| 风险 | 影响 | 缓解措施 |
|---|---|---|
| embed 包 API 变更 | 需修改代码 | 固定 etcd 版本；依赖稳定后再升级 |
| 自定义功能行为差异 | 运行时异常 | 并行运行旧/新代码对比日志 |
| TLS 行为差异 | 集群无法启动 | 保留手动 TLS 回退能力 |
| 二进制大小增加 | 可能超出 100MB | 构建后检查；禁用不需要的 embed 功能 |
| etcd 版本升级耦合 | 升级窗口变窄 | 封装层隔离版本差异 |

---

## 7. go.mod

无需更改 `go.mod` 中的 etcd 依赖版本。`embed` 包来自 `go.etcd.io/etcd/server/v3`，已在 `go.mod` 中声明：

```
go.etcd.io/etcd/server/v3 v3.6.7  →  github.com/k3s-io/etcd/server/v3 v3.6.7-k3s1
```

嵌入方式不变。

---

## 8. 里程碑时间线

| 阶段 | 内容 | 预计时间 |
|---|---|---|
| Phase 1 | 创建 `pkg/embedw/` 封装包 | 1-2 天 |
| Phase 2 | 渐进替换 `etcd.cluster()` | 1 天 |
| Phase 3 | 清理 build.zig 中 SQLite 代码 | 1 小时 |
| Phase 4 | 全流程回归测试 | 1-2 天 |
| Phase 5 | 性能基准对比 | 半天 |
| 发布 | 合并到 main | — |
