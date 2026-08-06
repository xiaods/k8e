# KIP-15: K8E Sandbox API — Perplexity-aligned Completeness Review & Design

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods (agent-assisted) | 2026-08-06 | Proposed |

## Summary

对照 [Perplexity Sandbox API](https://www.perplexity.ai/hub/blog/sandbox-api-isolated-code-execution-for-ai-agents) 与 [Agent API `sandbox` tool](https://docs.perplexity.ai/docs/agent-api/tools/sandbox) 的公开设计，评估当前 K8E `SandboxService`（gRPC + CLI）完备性，并给出 API 演进方案。

**产品目标（owner）：** K8E 是 **agent 可调用的中立 sandbox service**（tools 层的隔离执行后端），不是某一家 Agent 运行时、模型或 harness 的附属品。

**一句话结论：** 能力上对齐行业「agent code tool」的 **compute 契约**（session / exec / files / background / artifacts / 安全边界）；交付上保持 **vendor-neutral**：任何 Claude / Codex / Cursor / 自研 agent 都能用同一 gRPC/CLI 契约调用，不绑定 Perplexity/OpenAI 的 Agent API 循环。

不完备处仍集中在：**可观测执行结果、secrets/egress、暂停恢复、文件变更查询、端口预览、制品导出**。

## Sources

| Source | Role |
|--------|------|
| [Perplexity Hub: Sandbox API](https://www.perplexity.ai/hub/blog/sandbox-api-isolated-code-execution-for-ai-agents) | Product design intent (reference, not vendor lock-in) |
| [docs: Agent API sandbox tool](https://docs.perplexity.ai/docs/agent-api/tools/sandbox) | Tool semantics / response shape (reference) |
| K8E `proto/sandbox/v1/sandbox.proto` | Current gRPC surface |
| KIP-3 / 8 / 10 / 11 / 12 / 14 | Prior positioning and partial implementations |

## Positioning (non-negotiable)

### Product identity

```
┌─────────────────────────────────────────────────────────┐
│  Agent harness (Claude Code / Codex / Cursor / custom)  │  ← 不归 K8E
│  模型循环 · tool 选择 · 审批 UX · 记忆 · 计费               │
└──────────────────────────┬──────────────────────────────┘
                           │  标准 tools 调用
                           │  (CLI / gRPC / 可选 MCP·skill 适配)
                           ▼
┌─────────────────────────────────────────────────────────┐
│  K8E Sandbox Service  ← 中立 sandbox tools backend      │
│  session · exec · files · bg · secrets · egress · preview│
└─────────────────────────────────────────────────────────┘
```

| 维度 | 要做 | 不做 |
|------|------|------|
| **角色** | Agent 的 **sandbox tool / service** | 某厂商的 Agent API / 模型网关 |
| **调用方** | 任意能跑 shell 或 gRPC 的 agent | 仅某一 SDK |
| **契约** | 稳定、可文档化、可 codegen 的 gRPC + CLI | 绑定 `tools:[{type:sandbox}]` 专有编排 |
| **语义** | 调用方**显式**传 command/code | 服务端替模型写代码 |
| **生态** | skill / CLI 适配多 harness | guest 内预装某一搜索/LLM SDK |
| **数据** | 自托管、默认可离线/出域可控 | 强制 SaaS 计费与云账号 |

### Layer map vs Perplexity

| Layer | Perplexity | K8E |
|-------|------------|-----|
| **Harness** | Agent API：模型循环、tool 选择、billing、`sandbox` tool 封装 | ❌ 不做；由各 agent 自己完成 |
| **Sandbox tool service** | 博文中的 standalone Sandbox API / container 执行面 | ✅ **K8E 目标本体**（中立） |
| **Client adapter** | 官方 Python/TS SDK | CLI + skill + 可选薄多语言 client |

**对齐目标：** 抄 Perplexity（及同类）在 **sandbox service 能力与结果契约** 上的最佳实践；**不**抄其 harness 外壳，也**不**成为其专有后端。

### Neutrality rules (API design constraints)

1. **Transport neutrality** — 真源是 gRPC `SandboxService`；CLI/skill/MCP 都是 adapter，不得出现「仅某 harness 能用」的字段。
2. **Payload neutrality** — request/response 用通用类型（command、stdout、exit_code、duration_ms、status）；不出现 `perplexity_*` / 厂商 tool 专有枚举。
3. **Orchestration neutrality** — 不实现「模型决定何时 sandbox」；agent 侧 skill 描述 *how* to call，K8E 只 *execute*。
4. **Runtime neutrality** — RuntimeClass（gvisor/kata/firecracker）由部署方选；API 暴露选择，不绑定单一隔离技术品牌叙事。
5. **Secret neutrality** — 密钥来自调用方集群的 K8s Secret / 策略，不要求某一云 secret 产品。
6. **Reference, don’t fork** — Perplexity 文档用于 gap 分析与优先级；实现以 K8E KIP 与 proto 为权威。

---

## 1. Perplexity capability model (extracted)

### 1.1 Hub blog (product)

1. **Deterministic compute for agents** — 模型规划，sandbox 执行。
2. **One pod per session** on Kubernetes；平台管 provision / network / cleanup。
3. **Languages** — blog: Python, JavaScript, SQL + runtime package install；tool docs: Python + bash（以 docs 为准作执行面）。
4. **Stateful FS** — FUSE-backed persistent filesystem；read/write/list；**track modifications since session creation**。
5. **Long workflows** — pause / resume hours later with full state；最多 **5 background processes**。
6. **Zero-trust network** — sandbox **无直连外网**；出站经 **egress proxy**（域匹配 + **代理侧注入凭证**）；代码内**永不出现 raw secrets**。
7. **Resource bounds** — timeouts + resource limits。
8. **Agent API integration** — Sandbox 作为 tool（harness；K8E 不对齐）。

### 1.2 Agent sandbox tool (API-ish surface)

| Concept | Behavior |
|---------|----------|
| Execution unit | container shared across steps in one response |
| Capture | stdout / stderr；~1 MiB truncate；大结果写文件再取回 |
| Status | `completed` / `timed_out` / `failed` + `exit_code` + **`duration_ms`** |
| Background | submit + poll by id |
| Artifacts | `share_file` → list/download by file id |
| Observability | return **code executed**, language, container_id |
| Network (tool mode) | container may have network for package install / HTTP（与 blog zero-trust 叙述有张力；**standalone Sandbox API 以 blog zero-trust 为准**） |

---

## 2. K8E current surface

### 2.1 gRPC `SandboxService` (today)

| RPC | Status | Maps to Perplexity |
|-----|--------|--------------------|
| `CreateSession` | ✅ (+ `env` in #502) | create container/session |
| `DestroySession` | ✅ | teardown |
| `Exec` / `ExecStream` | ✅ | run code / stream |
| `Exec(background)` + `PollRun` | ✅ (KIP-11) | background + poll |
| `WriteFile` / `ReadFile` / `ListFiles` | ✅ | FS ops（list 的 since 在 CLI 侧） |
| `PipInstall` | ✅ | runtime packages (Python-centric) |
| `RunSubAgent` | ✅ K8E-specific | N/A（Perplexity 无对等） |
| `ConfirmAction` / `ApproveAction` | ✅ K8E-specific | governance（Perplexity 侧偏 harness） |
| `Login` | ✅ (KIP-14 mTLS) | auth |
| `GetSession` / `ListSessions` | ❌ | session inspect |
| `PauseSession` / `ResumeSession` | ❌ | pause/resume |
| `ListFiles(since)` 一等字段 | ⚠️ CLI only | modification tracking |
| `ExposePort` / `UnexposePort` | ❌ (KIP-12 A) | preview（Perplexity 公开文未强调） |
| `SecretRef` on create | ❌ (#485 / KIP-12 B) | secret-safe config |
| Artifact registry (share/list/download) | ⚠️ snapshot 间接 | share_file |
| `duration_ms` / structured `status` enum | ⚠️ 弱 | results[] shape |

### 2.2 Isolation & security (infra)

| Capability | K8E | Perplexity |
|------------|-----|------------|
| Isolation | gVisor / Kata / Firecracker RuntimeClass | “isolated K8s pod” |
| Warm pool | ✅ | 未公开细节 |
| Default egress | Cilium deny + `allowed_hosts` | no direct net + egress proxy |
| Secret injection | 明文 `env` only；secret-ref 未落地 | proxy injects credentials |
| Resource limits | matrix CRD / pod limits 部分 | built-in limits |
| Human approval | `confirm`/`approve` | 未作为 sandbox 一等 API |

---

## 3. Gap matrix

| # | Capability | Perplexity | K8E today | Gap | Priority |
|---|------------|------------|-----------|-----|----------|
| G1 | Isolated session lifecycle | create/destroy pod | Create/Destroy | Closed | — |
| G2 | Sync exec + stdout/stderr/exit | results[] | Exec | **Partial**: missing `duration_ms`, typed status, executed-code echo | P1 |
| G3 | Streaming output | (opaque) | ExecStream | Closed enough | P2 polish |
| G4 | Background + poll | background + retrieve | Exec+PollRun | Closed | — |
| G5 | Multi-step stateful FS | FUSE + share files | PVC/EmptyDir + R/W/List | **Partial**: no server-side `since`/diff RPC; no artifact IDs | P1 |
| G6 | Pause/resume hours later | explicit product claim | tenant PVC + snapshot (KIP-10) | **Partial**: snapshot ≠ live pause; no Pause/Resume RPC | P1 |
| G7 | Runtime package install | yes | PipInstall + shell npm | **Partial**: no generic InstallPackages; language-bound | P2 |
| G8 | Language abstraction | python/js/sql or python/bash | CLI `--lang` wraps shell | **Partial**: gRPC 只有 raw command | P2 |
| G9 | Background process cap | max 5 | unlimited per session | Missing soft limit + API status | P2 |
| G10 | Zero-trust egress + secret proxy | egress proxy injects secrets | domain allowlist only | **Major** | P0 |
| G11 | Secrets never in guest env | by design | CRD plaintext env; no secret_refs | **Major** (#485) | P0 |
| G12 | Output size policy | ~1 MiB truncate | sandboxd caps pipes | Document + align constants | P2 |
| G13 | File export API | files.list / content | ReadFile + snapshot | Missing artifact catalog | P1 |
| G14 | Ports / preview URL | not emphasized | KIP-12 A unimplemented | Product plus (coding agents) | P1 |
| G15 | Session introspection | container_id, status | only local state files | Missing GetSession | P1 |
| G16 | Agent harness tool wrapper | first-class | skill/CLI only | Out of scope (KIP-12) | — |
| G17 | Sub-agents / depth | — | RunSubAgent | K8E advantage | keep |
| G18 | Compute-side approval | — | Confirm/Approve | K8E advantage | keep |

**Completeness score (compute only, subjective):**

- **Core exec path:** ~80%  
- **Security posture vs Perplexity zero-trust story:** ~45%  
- **Long-running / resume story:** ~55%  
- **API ergonomics / observability:** ~50%  

**Overall: capable but not “Perplexity-complete” as a public Sandbox API.**

---

## 4. Target API design (K8E v1.5)

### 4.1 Design principles

1. **gRPC-first**, CLI/skill thin wrappers (现有模式)。
2. **Session = unit of isolation**（对齐 Perplexity container/session）。
3. **Exec is the primary unit of work**；language helpers 为语法糖，不替代 shell。
4. **Secrets never land in guest process env from client plaintext** — 仅 `secret_refs` + gateway/egress 解析。
5. **Warm-pool safe mutations only at exec time**（env/secrets/network tokens）。
6. **Do not implement Agent API**；可选提供「tool schema 文档」供 harness 对接。

### 4.2 Proposed service sketch

```protobuf
service SandboxService {
  // --- lifecycle ---
  rpc CreateSession(CreateSessionRequest) returns (CreateSessionResponse);
  rpc GetSession(GetSessionRequest)       returns (GetSessionResponse);
  rpc ListSessions(ListSessionsRequest)   returns (ListSessionsResponse);
  rpc DestroySession(DestroySessionRequest) returns (DestroySessionResponse);
  rpc PauseSession(PauseSessionRequest)   returns (PauseSessionResponse);   // NEW
  rpc ResumeSession(ResumeSessionRequest) returns (ResumeSessionResponse); // NEW

  // --- execution ---
  rpc Exec(ExecRequest)                   returns (ExecResponse);
  rpc ExecStream(ExecRequest)             returns (stream ExecStreamResponse);
  rpc PollRun(PollRunRequest)             returns (PollRunResponse);
  rpc ListRuns(ListRunsRequest)           returns (ListRunsResponse);       // NEW

  // --- filesystem ---
  rpc WriteFile(WriteFileRequest)         returns (WriteFileResponse);
  rpc ReadFile(ReadFileRequest)           returns (ReadFileResponse);
  rpc ListFiles(ListFilesRequest)         returns (ListFilesResponse);      // extend since/glob
  rpc StatFile(StatFileRequest)           returns (StatFileResponse);       // NEW
  rpc DeleteFile(DeleteFileRequest)       returns (DeleteFileResponse);     // NEW

  // --- artifacts (export outside guest) ---
  rpc ShareFile(ShareFileRequest)         returns (ShareFileResponse);      // NEW
  rpc ListArtifacts(ListArtifactsRequest) returns (ListArtifactsResponse);  // NEW
  rpc GetArtifact(GetArtifactRequest)     returns (GetArtifactResponse);    // NEW

  // --- packages ---
  rpc InstallPackages(InstallPackagesRequest) returns (InstallPackagesResponse); // NEW (generalize PipInstall)

  // --- network / preview (KIP-12 A) ---
  rpc ExposePort(ExposePortRequest)       returns (ExposePortResponse);
  rpc UnexposePort(UnexposePortRequest)   returns (UnexposePortResponse);

  // --- governance (keep) ---
  rpc RunSubAgent(RunSubAgentRequest)     returns (RunSubAgentResponse);
  rpc ConfirmAction(ConfirmActionRequest) returns (ConfirmActionResponse);
  rpc ApproveAction(ApproveActionRequest) returns (ApproveActionResponse);

  // --- auth ---
  rpc Login(LoginRequest)                 returns (LoginResponse);
}
```

### 4.3 Message upgrades (high value)

#### CreateSessionRequest (extend)

```protobuf
message CreateSessionRequest {
  string session_id = 1;
  string tenant_id = 2;
  repeated string allowed_hosts = 3;
  string runtime_class = 4;
  map<string,string> env = 5;                 // non-sensitive only
  repeated SecretRef secret_refs = 6;         // NEW — values resolved at exec/egress only
  ResourceLimits resources = 7;               // NEW — cpu/memory/timeout defaults
  int32 max_background_runs = 8;              // NEW — default 5 (Perplexity parity)
  string snapshot_id = 9;                     // NEW — boot from snapshot
}

message SecretRef {
  string secret_name = 1;  // K8s Secret in sandbox NS
  string key = 2;
  string env_var = 3;      // injected at exec time only
}
```

#### ExecRequest / ExecResponse (observability parity)

```protobuf
message ExecRequest {
  string session_id = 1;
  string command = 2;
  int32 timeout = 3;
  string workdir = 4;
  bool background = 5;
  string language = 6;     // NEW optional: python|bash|node|sql — gateway may wrap
  map<string,string> env_overlay = 7; // NEW per-exec non-sensitive overlay
}

message ExecResponse {
  string stdout = 1;
  string stderr = 2;
  int32 exit_code = 3;
  string session_id = 4;
  string run_id = 5;
  string status = 6;       // started|completed|timed_out|failed
  int64 duration_ms = 7;   // NEW
  string language = 8;     // NEW
  bool truncated = 9;      // NEW — stdout/stderr truncated
}
```

#### ListFilesRequest (modification tracking)

```protobuf
message ListFilesRequest {
  string session_id = 1;
  int64 modified_since = 2;  // unix seconds; first-class (today CLI-only)
  string glob = 3;           // optional
}
```

#### Pause / Resume semantics

| Call | Behavior |
|------|----------|
| `PauseSession` | Stop accepting new Exec; optionally freeze/stop pod; **retain PVC**; mark phase `Paused` |
| `ResumeSession` | Re-claim/start pod attached to same PVC; warm-pool **not** used for PVC sessions |
| TTL | Paused sessions still subject to `sessionTTL` unless `pause_extends_ttl` policy |

Snapshot (KIP-10) remains for **named, durable, cross-cluster-exportable** checkpoints；Pause 是 **同集群 live hold**。

#### ShareFile / Artifacts

对齐 Perplexity「大结果不进 stdout」：

```protobuf
message ShareFileRequest {
  string session_id = 1;
  string path = 2;          // guest path
  string filename = 3;      // optional display name
}
message ShareFileResponse {
  string artifact_id = 1;
  int64 bytes = 2;
}
// GetArtifact returns bytes or signed short-lived URL (implementation choice)
```

#### Zero-trust egress (G10) — control plane, not guest

```
guest process
  → only allowed to egress proxy SIDECAR or node proxy
  → proxy matches Host / SNI against policy
  → injects Authorization from K8s Secret
  → guest never sees secret material
```

API 侧：

- `CreateSession.secret_refs` 仅声明引用。
- 可选 `CreateSession.egress_routes[]`：`{ host, secret_ref, inject: header|query }`（二期）。
- 默认继续 **deny-all + allowed_hosts**；打开 route 时才允许匹配域。

### 4.4 CLI mapping

| Perplexity-ish intent | K8E CLI (target) |
|----------------------|------------------|
| create session | `create --env --secret --max-bg 5 --from-snapshot` |
| run code | `run --lang python|bash|node --timeout` |
| background | `run --background` + `poll` |
| files | `write` / `read` / `list --since` / `rm` |
| export artifact | `share <sid> <path>` + `artifact get` |
| pause/resume | `pause` / `resume` |
| expose app | `expose` / `unexpose` (KIP-12 A) |
| approve | `confirm` / `approve` (keep) |

### 4.5 What we deliberately do **not** copy

| Perplexity / industry | K8E decision | Why |
|----------------------|--------------|-----|
| Agent API `tools:[{type:sandbox}]` | Out of scope | Harness; skill/CLI 足够 |
| Model bills + sandbox session billing | Out of scope | Self-hosted |
| Preinstalled vendor SDK calling search from guest | Out of scope | 破坏 zero-trust；应由 harness 调 search |
| SQL as first-class language runtime | Optional later | shell/`python` 可覆盖多数场景 |
| Open internet by default | **Reject** | 与 K8E eBPF 默认拒绝冲突；Perplexity tool docs 与 blog 也不一致，K8E 选 blog/zero-trust |

---

## 5. Implementation roadmap

### Wave 0 — already done / in flight

- Session + Exec + Stream + Background/Poll (KIP-11)
- Files R/W/List, PipInstall, mTLS Login (KIP-14)
- Snapshots (KIP-10)
- Inline env (KIP-12 B part / #502)
- Warm pool + multi RuntimeClass

### Wave 1 — P0 security (must for “Perplexity-class” trust story)

| Item | Issue / KIP | Effort |
|------|-------------|--------|
| `secret_refs` + exec-time inject | #485 / KIP-12 B | M |
| Egress proxy design + domain credential inject | new KIP-16 (follow-on) | L |
| Document secret red line in skill (partially done) | skill | S |

### Wave 2 — P1 API ergonomics & long-run

| Item | Notes |
|------|-------|
| `GetSession` / `ListSessions` | phase, podIP, expires, env keys (not values), run counts |
| `ExecResponse.duration_ms` + status enum + truncated | sandboxd + gateway |
| `ListFiles.modified_since` in proto | move off CLI-only |
| `PauseSession` / `ResumeSession` | PVC sessions first |
| `ShareFile` + artifact store | object in control-plane or signed URL |
| `ExposePort` / `UnexposePort` | KIP-12 A |

### Wave 3 — P2 polish

| Item | Notes |
|------|-------|
| `InstallPackages{ language, packages }` | supersede PipInstall |
| `max_background_runs` enforce | default 5 |
| `StatFile` / `DeleteFile` | FS completeness |
| `ListRuns` | per-session background inventory |
| Output truncate constants shared CLI/docs | ~1 MiB parity note |
| Optional thin Python/TS client | generate from proto |

---

## 6. Example: Perplexity-shaped agent loop on K8E

```text
# 1. Create isolated session (zero-trust hosts + secrets by ref)
SID=$(k8e sandbox create \
  --runtime gvisor \
  --allowed-hosts api.example.com,pypi.org,files.pythonhosted.org \
  --env LOG_LEVEL=info \
  --secret OPENAI_API_KEY=llm-secret:api_key \
  --max-bg 5 | jq -r .session_id)

# 2. Multi-step stateful work
k8e sandbox write $SID /workspace/data.csv < export.csv
k8e sandbox run --lang python --session-id $SID 'import pandas as pd; ...'
k8e sandbox list $SID --since $T0

# 3. Long job
RUN=$(k8e sandbox run --background --session-id $SID 'python train.py' | jq -r .run_id)
k8e sandbox poll $RUN   # until completed|timed_out|failed

# 4. Export large artifact (not via stdout)
ART=$(k8e sandbox share $SID /workspace/out.parquet | jq -r .artifact_id)
k8e sandbox artifact get $ART -o ./out.parquet

# 5. Pause overnight / resume
k8e sandbox pause $SID
# ... hours later ...
k8e sandbox resume $SID
k8e sandbox run --session-id $SID 'ls /workspace'

# 6. Teardown
k8e sandbox destroy $SID
```

---

## 7. Acceptance criteria for “API complete enough”

K8E Sandbox API 可宣称与 Perplexity **compute 层 parity** 当且仅当：

1. **G10+G11**：secret_refs 落地 + 文档化 zero-trust egress（proxy 可分期，但不得把 raw secret 写进 guest env/CRD）。
2. **G2**：每次 Exec 返回 `status` + `duration_ms` + truncation 标志。
3. **G5+G13**：`ListFiles(since)` 一等 + Share/Get artifact。
4. **G6**：Pause/Resume **或** 明确将 Snapshot 定位为 resume 机制并在 skill 中标准化（二选一写进对外承诺）。
5. **G15**：GetSession 可查询 phase / expiry / active runs。
6. 保留 K8E 差异化：RuntimeClass、warm pool、Confirm/Approve、RunSubAgent、Cilium allowlist。

未满足前，对外表述应为：**「shell-first compute provider with strong isolation；partial Perplexity-class API」**，而非完整 Sandbox API 对等。

---

## 8. Related work

- [KIP-3](./kip-3-agentic-ai-sandbox-matrix.md) — matrix / sessions  
- [KIP-8](./kip-8-skill-cli-replace-mcp.md) — CLI-first distribution  
- [KIP-10](./kip-10-sandbox-snapshot.md) — snapshots  
- [KIP-11](./kip-11-background-sandbox-execution.md) — background (already Perplexity-inspired)  
- [KIP-12](./kip-12-sandbox-ports-env-secrets.md) — ports + env/secrets  
- [KIP-14](./kip-14-mtls-dynamic-cert-issuance.md) — mTLS  
- GitHub #483 env (PR #502), #485 secret-refs  

## 9. Decision record

| Decision | Choice | Rationale |
|----------|--------|-----------|
| Product identity | **Neutral sandbox tool service for any agent** | Owner: agents call K8E as tools backend, not vendor runtime |
| Align to industry **sandbox service** capabilities | Yes (Perplexity as primary public reference) | Same job-to-be-done: isolated deterministic exec |
| Align to Perplexity **Agent API harness** | No | Harness stays outside K8E; keeps multi-agent neutrality |
| Vendor-specific guest SDKs | No | Neutrality + smaller attack surface |
| Default network | Deny + allowlist (+ future proxy) | Stronger default than open guest net |
| Pause vs Snapshot | Both: Pause=live hold; Snapshot=named checkpoint | Covers hours-later and export/clone |
| SQL language | Not first-class in v1.5 | Prefer python/bash/node wrappers |
| API authority | gRPC proto + KIPs | Adapters (CLI/skill) must not invent harness-only semantics |

---

## 10. Next actions (suggested issues)

1. **#485** — secret_refs (Wave 1)  
2. **Issue: GetSession + ExecResponse.duration_ms/status/truncated** (Wave 2)  
3. **Issue: ListFiles.modified_since in proto** (Wave 2)  
4. **Issue: PauseSession/ResumeSession** (Wave 2)  
5. **Issue: ShareFile/artifacts** (Wave 2)  
6. **Issue: ExposePort (KIP-12 A)** (Wave 2)  
7. **KIP-16** — egress proxy + credential injection (Wave 1–2)  
