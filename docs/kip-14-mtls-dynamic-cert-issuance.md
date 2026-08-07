# KIP-14: mTLS 动态证书签发

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-06-04 | Proposed |

## Summary

将 sandbox gRPC 网关从当前的 **API key bearer token + 单向 TLS** 认证升级为 **mTLS 动态证书签发**。核心变化：

1. **独立沙箱 CA**：服务端启动时自动生成 ECDSA P-256 CA，同时签服务端 server cert 和客户端 client cert，单 CA 双向信任。
2. **Login RPC**：新增 Bootstrap 接口，客户端提交 CSR + API key（首次）或旧 client cert（续期），服务端签发 30 天短命客户端证书。
3. **业务 RPC 强制 mTLS**：`CreateSession`、`Exec` 等所有业务 RPC 从 API key metadata 认证切换为 mTLS 证书认证，从 `PeerCertificates[0].Subject.CommonName` 提取身份。
4. **懒续期**：客户端在连接建立时检测证书有效期，不足时自动续期，无需后台常驻进程。
5. **轻量吊销**：API key 删除时对应证书指纹加入内存吊销列表，Login 和业务 RPC 均检查。

本 KIP 同时移除 `GetCACert` RPC（功能由 `LoginResponse.ca_cert` 取代）和 API key interceptor（业务 RPC 不再走 bearer token）。

## Motivation

### 当前状态

| 维度 | 现状 | 问题 |
|------|------|------|
| 服务端 TLS | `NewServerTLSFromFile`，复用 `serving-kube-apiserver.crt/key` | 无客户端证书验证；共享 K8s API Server 证书 |
| 客户端认证 | API key 通过 `authorization: Bearer <key>` metadata 传输 | 每次请求携带长期凭证，泄露风险高 |
| API key 存储 | K8s Secret `sandbox-matrix/sandbox-apikeys`，30 秒 reload | O(n) 线性查找，无身份绑定 |
| 客户端 TLS | TOFU 模式获取"CA cert"（实为 leaf cert），缓存到 `~/.k8e/sandbox/ca.crt` | `GetCACert` 返回的是 leaf cert 不是 CA cert；无客户端身份证书 |
| 安全边界 | API key Secret 不存在时**所有请求免认证通过** | 默认不安全 |
| 续期 | 无 | API key 是静态长期凭证 |

### 目标

- 客户端证书短期化（30 天），降低泄露影响面
- 业务请求不传输长期凭证（mTLS 握手后无需 API key）
- 身份绑定到证书 CN，审计日志可追踪
- 首次 Login 后用户无感（懒续期）
- CA 体系独立于 K8s 集群 CA，权限边界清晰

## Design

### Part A — CA 层级

```
/var/lib/k8e/server/tls/
├── sandbox-ca.crt          # 沙箱专用 CA 证书（ECDSA P-256，启动时自动生成）
├── sandbox-ca.key          # CA 私钥（0600）
├── sandbox-server.crt      # gRPC 网关 server cert（由 sandbox-ca 签发）
├── sandbox-server.key      # server 私钥（ECDSA P-256）
└── sandbox-issued.json     # 已签发客户端证书记录
```

- CA 在 `server.go` 的 `Start()` 中检测，不存在则自动生成（与 `serving-kube-apiserver` 自举逻辑一致）
- CA 文件丢失 → 重新生成，旧客户端证书全部失效，文档说明风险
- 不依赖 K8s Secret 存储 CA key（尊重单二进制自包含设计）

#### CA 参数

| 参数 | 值 |
|------|-----|
| 算法 | ECDSA P-256 |
| 签名算法 | SHA256 |
| CA 证书有效期 | 10 年（仅用于签发，client 和 server 走自己的短命周期） |
| KeyUsage | `certSign | crlSign` |
| BasicConstraints | `IsCA: true` |

### Part B — gRPC 端口认证策略

**单端口，`tls.VerifyClientCertIfGiven` 模式**，按方法区分认证：

```
                     ┌─────────────────────────────────┐
                     │   gRPC :50051 (单端口)           │
                     │   tls.VerifyClientCertIfGiven    │
                     └──────────┬──────────────────────┘
                                │
              ┌─────────────────┴─────────────────┐
              ▼                                   ▼
     ┌─────────────────┐                ┌─────────────────┐
     │ Bootstrap 方法   │                │ 业务 RPC        │
     │ Login            │                │ CreateSession   │
     │                  │                │ DestroySession  │
     │ 认证方式：        │                │ Exec/ExecStream │
     │ 有 client cert → │                │ WriteFile       │
     │   mTLS 身份续期   │                │ ReadFile        │
     │ 无 client cert → │                │ ListFiles       │
     │   API key metadata│               │ PipInstall      │
     │                  │                │ RunSubAgent     │
     │                  │                │ ConfirmAction   │
     │                  │                │ ApproveAction   │
     │                  │                │ PollRun         │
     │                  │                │                 │
     │                  │                │ 认证方式：       │
     │                  │                │ 强制 mTLS       │
     │                  │                │ 无证书 → 拒绝    │
     └─────────────────┘                └─────────────────┘
```

- 业务 RPC 强制 mTLS：无 client cert → `codes.Unauthenticated`
- Login handler 内部双路径：peer cert 有效 → mTLS 续期；peer cert 为空 → API key 认证
- 本地 loopback（`127.0.0.1`）豁免 mTLS：无 client cert 时可以执行业务 RPC（loopback 视为安全边界）

### Part C — Login RPC

#### Protobuf 定义

```protobuf
rpc Login(LoginRequest) returns (LoginResponse);

message LoginRequest {
  string csr            = 1;  // PEM-encoded PKCS#10 CSR
  string device_name    = 2;  // 如 hostname，仅审计日志
  string client_version = 3;  // k8e 版本号，仅审计日志
}

message LoginResponse {
  string cert       = 1;  // PEM-encoded 客户端证书
  string ca_cert    = 2;  // 沙箱 CA 证书（客户端缓存为信任锚）
  int64  valid_days = 3;  // 证书有效天数（用于客户端计算续期窗口）
}
```

API key 通过 gRPC metadata `authorization: Bearer <key>` 传递，不出现在 proto body 中。

#### Login handler 逻辑

```
handleLogin(req, stream):
  peerCerts := TLS peer certificates from stream context

  if peerCerts non-empty and cert valid (NotBefore ≤ now ≤ NotAfter):
    // 续期路径：mTLS 身份
    keyName = peerCerts[0].Subject.CommonName
  else if peerCerts empty:
    // 首次登录路径：API key 认证
    apiKey = extractBearerToken(stream metadata)
    keyName = lookupAPIKey(apiKey)
    if keyName == "":
      return Unauthenticated
  else:
    // 证书在 TLS 握手层已被拒绝，不会到达这里
    return Unauthenticated

  // 吊销检查
  if revocationList.contains(certFingerprint(keyName)):
    return PermissionDenied

  // 解析 CSR，提取公钥（忽略其 Subject/SAN）
  pubKey = parseCSR(req.csr).PublicKey

  // 签发客户端证书（服务端完全控制证书身份）
  clientCert = signClientCert(
    ca:   sandboxCA,
    pubKey: pubKey,
    cn:   keyName,
    ttl:  30 days,
  )

  // 记录审计
  auditLog("login", keyName, req.device_name, req.client_version, clientCert.fingerprint, sourceIP)

  // 持久化签发记录
  issuedStore.append(keyName, clientCert.fingerprint, issuedAt, expiresAt)

  return LoginResponse{
    cert:       clientCert.PEM,
    ca_cert:    sandboxCA.crt.PEM,
    valid_days: 30,
  }
```

#### 客户端证书模板

| 字段 | 值 |
|------|-----|
| Subject.CommonName | API key 名 |
| NotBefore | now |
| NotAfter | now + 30 days |
| KeyUsage | `digitalSignature` |
| ExtKeyUsage | `clientAuth` |
| BasicConstraints | `IsCA: false` |
| CRLDistributionPoints | 占位 URL（为将来扩展预留） |

- CSR 中的 Subject/SAN 被服务端忽略，服务端完全控制证书身份
- `device_name` / `client_version` 不进证书，仅写入审计日志

### Part D — 服务端 Server Cert 管理

沙箱 CA 同时为 gRPC 网关自身签发 server cert：

- 保存为 `/var/lib/k8e/server/tls/sandbox-server.crt` 和 `sandbox-server.key`
- Server cert 私钥：ECDSA P-256，启动时生成
- SAN：启动时动态收集 = 本机所有网卡 IP + hostname + 环境变量 `K8E_SANDBOX_ADVERTISED_HOSTNAME`
- Server cert 有效期：90 天（长于 client cert，减少服务端重启频率）
- 启动时检测：server cert 不存在或剩余有效期 < 30 天 → 重新签发

### Part E — 客户端证书存储

```
~/.k8e/sandbox/
├── client.key    # 客户端私钥（ECDSA P-256, 0600）
├── client.crt    # 客户端证书（0644）
└── ca.crt        # 沙箱 CA 证书（信任锚，0644）
```

### Part F — 客户端连接流程

`NewClientWithEndpoint()` 内部三态判断：

```
NewClientWithEndpoint(endpoint, apiKey):
  caCrt    = read ~/.k8e/sandbox/ca.crt       // 是否存在
  clientCrt = read ~/.k8e/sandbox/client.crt  // 是否存在
  clientKey = read ~/.k8e/sandbox/client.key  // 是否存在

  switch:
    case caCrt && clientCrt && clientKey && certValid(clientCrt):
      // 路径 1：直接 mTLS 连接
      return dialMTLS(endpoint, caCrt, clientCrt, clientKey)

    case caCrt && (!clientCrt or !clientKey or certExpired(clientCrt)):
      // 路径 2：单向 TLS（验证服务端），先 Login 获取证书，再 mTLS
      clientCrt, clientKey = generateKeyPairAndCSR()
      loginResp = callLoginOverTLS(endpoint, caCrt, apiKey, csr)
      save(loginResp.cert, clientCrt); save(clientKey)
      return dialMTLS(endpoint, caCrt, loginResp.cert, clientKey)

    case !caCrt:
      // 路径 3：无验证连接，Login 获取 ca + client cert，再 mTLS
      clientCrt, clientKey = generateKeyPairAndCSR()
      loginResp = callLoginInsecure(endpoint, apiKey, csr)
      save(loginResp.ca_cert, ca.crt)
      save(loginResp.cert, clientCrt); save(clientKey)
      return dialMTLS(endpoint, caCrt, loginResp.cert, clientKey)
```

- `apiKey` 参数只在路径 2/3 中使用（首次/过期后续期），路径 1 忽略之
- `callLoginInsecure`：`tls.Config{InsecureSkipVerify: true}` —— bootstrap 无法避免，但 API key 认证 + LoginResponse 带回 CA cert 使后续连接安全

### Part G — 懒续期

续期在连接建立时触发，不在独立的后台进程中：

```
// 在 dialMTLS 之前
if certExpiringSoon(clientCrt, threshold=7days):
  csr = generateCSR(clientKey)
  loginResp = callLoginOverMTLS(endpoint, caCrt, clientCrt, clientKey, csr)
  save(loginResp.cert, clientCrt)
  // clientKey 复用，不重新生成
```

- 复用现有私钥，仅签发新证书
- 续期使用 mTLS 身份（Login handler 从 peer cert CN 提取身份），不需要 API key
- 证书已过期 → TLS 握手失败 → 回退到路径 2（用 API key 重新 Login）
- 阈值：7 天

### Part H — 轻量吊销

无需 CRL 文件或 OCSP。

```
var revocationList sync.Map  // certFingerprint → revokedAt

// API key 删除时
onAPIKeyDeleted(keyName):
  for each cert in issuedStore.findByKeyName(keyName):
    revocationList.Store(cert.fingerprint, time.Now())

// Login handler 中
if _, revoked := revocationList.Load(fingerprint); revoked:
  return PermissionDenied

// 业务 RPC mTLS 验证后
peerFingerprint := sha256(peerCert.Raw)
if _, revoked := revocationList.Load(peerFingerprint); revoked:
  return PermissionDenied
```

- 吊销列表存于内存（`/var/lib/k8e/server/tls/sandbox-issued.json` 持久化签发记录，重启时可选择性地通过"API key 已删除"推断吊销）
- 30 天证书过期后吊销条目自动清理（定期扫描 `issued.json` 清理过期记录）

### Part I — 移除的组件

| 移除 | 原因 |
|------|------|
| `GetCACert` RPC | 被 `LoginResponse.ca_cert` 取代 |
| `apiKeyInterceptor` / `apiStreamInterceptor` | 业务 RPC 改用 mTLS 认证 |
| TOFU 逻辑（`tofuConnect`, `tryCachedCert`, `verifyFingerprint`） | 被 Login 三态 bootstrap 取代 |
| 客户端 `dialWithAPIKey` interceptor | 业务 RPC 不再传 API key metadata |

### Part J — 兼容性过渡

硬切换：一次性移除 API key interceptor，老客户端必须执行一次 `k8e sandbox login` 才能继续使用。理由：K8E 用户量小、更新快，灰度期增加代码复杂度不值得。

用户迁移步骤：
```bash
# 升级 k8e 二进制后
k8e sandbox login --apikey sk-xxx --endpoint sandbox.example.com:50051
# → 证书存到 ~/.k8e/sandbox/，后续命令无需 --apikey
k8e sandbox run "echo hello"
```

### 决策记录

| 决策 | 选择 | 理由 |
|------|------|------|
| CA 层级 | 独立沙箱 CA，与 K8s 集群 CA 分离 | 信任域隔离，吊销粒度独立 |
| Bootstrap 接口 | 单 gRPC port + 按方法区分认证 | 不增加端口/网络拓扑复杂度 |
| Login 协议 | 单 RPC（CSR → cert + ca_cert） | 一步到位，无往返延迟 |
| 证书身份 | CN = API key 名 | 审计友好，粒度和当前 API key 一致 |
| 续期 | 懒续期，连接建立时触发 | CLI 无后台进程，最简单 |
| CA 初始化 | 启动时自动生成 | 运维零成本 |
| CSR SAN | 服务端忽略，完全自决 | 客户端不可信 |
| 客户端密钥算法 | ECDSA P-256 | Go `crypto/x509` 原生，gRPC 完全支持 |
| 吊销 | 内存吊销列表（API key 删除时触发） | 30 天短命证书 + 轻量吊销足够 |
| 授权 | 有有效证书 = 可执行所有操作 | 当前无角色概念，授权后续 PR 加 |
| loopback | 豁免 mTLS | 延续 `IsLocalOrHasRole()` 先例 |
| GetCACert RPC | 删除 | `LoginResponse.ca_cert` 取代 |
| API key interceptor | 移除 | mTLS 取代 |
| 过渡策略 | 硬切换 | 用户量小，更新快 |

### 边界处理

| 场景 | 行为 |
|------|------|
| 首次使用，无 client cert | 路径 3：InsecureSkipVerify + Login（API key），获取 ca + client cert |
| client cert 在有效期内 | 路径 1：直接 mTLS 连接 |
| client cert 剩余 < 7 天 | 懒续期触发，mTLS 身份续期（不需 API key） |
| client cert 已过期 | TLS 握手失败 → 回退路径 2：用 API key 重新 Login |
| API key 被删除 | 该 key 签发的所有证书加入吊销列表；已有连接的吊销在下次 RPC 时拒绝 |
| 沙箱 CA 文件丢失 | 启动时重新生成 CA + server cert；所有旧 client cert 失效，客户端需重新 Login |
| 服务重启 | CA 从磁盘加载；吊销列表为空（内存），但已签发证书记录在 `issued.json` 中恢复 |
| Login 请求的 peer cert 有效但已被吊销 | 吊销检查拒绝 |
| 同一 key 多次 Login（多设备） | 每台设备独立生成密钥对和证书，CN 相同但公钥/指纹不同；吊销按指纹粒度 |
| 本地连接（127.0.0.1）| 无 client cert 可通过（loopback 豁免），无需 login |
| 服务端 server cert 过期 | 启动时检测，< 30 天则自动重新签发 |

### 文件变更

| 文件 | 变更 |
|------|------|
| `proto/sandbox/v1/sandbox.proto` | 新增 `Login` RPC、`LoginRequest`、`LoginResponse`；移除 `GetCACert` RPC |
| `pkg/sandboxmatrix/grpc/pb/` | 重新生成 protobuf Go 代码 |
| `pkg/sandboxmatrix/grpc/server.go` | 沙箱 CA 生成/加载；`tls.Config` 改为 `VerifyClientCertIfGiven` + 设置 `ClientCAs`；`Login` handler；移除 `apiKeyInterceptor`/`apiStreamInterceptor`；签发证书逻辑（`signClientCert`）；吊销列表初始化；server cert 签发/更新 |
| `pkg/sandboxmatrix/grpc/cert.go`（新增）| CA 生成、CSR 解析、证书签发、吊销列表、签发记录持久化 |
| `pkg/sandboxmatrix/controller.go` | 传递新 CA 路径和 server cert 路径给 `NewServer` |
| `pkg/sandbox/client/client.go` | 重写连接建立（三态 bootstrap）；懒续期；移除 TOFU 逻辑；移除 `dialWithAPIKey` interceptor；新增 `Login` 客户端方法 |
| `pkg/sandboxcli/commands.go` | 新增 `login` 子命令；`newClientFromCtx` 适配 |
| `pkg/sandboxcli/login.go`（新增）| `LoginCommand()` 实现：`k8e sandbox login --apikey --endpoint [--device-name]` |
| `cmd/sandboxcli/main.go` | 新增 `login` 子命令注册（或仅靠 `sandboxcli` 内部注册） |
| `pkg/cli/cmds/sandbox.go` | 注册 `login` 子命令 |
| `pkg/sandboxcli/skills/k8e-sandbox/SKILL.md` | 文档化 `k8e sandbox login` 流程 |

## 相关 KIP

- [KIP-3](./kip-3-agentic-ai-sandbox-matrix.md) — Sandbox Matrix 总体设计（gRPC 网关架构）
- [KIP-8](./kip-8-skill-cli-replace-mcp.md) — CLI 替换 MCP（CLI 优先的认证体验基础）
- [KIP-12](./kip-12-sandbox-ports-env-secrets.md) — Ports/Preview 与 Env/Secrets（共享 gRPC 端口和服务端部署上下文）
