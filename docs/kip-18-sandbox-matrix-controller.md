# KIP-18 architecture evolution: sandboxd-as-envd + unified sandbox controller

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-14 | Design (not yet implemented) |

## Purpose

Two architectural decisions, planned together:

1. **envd (E2B ecosystem) capabilities move into k8e's `sandboxd`** — the
   in-pod daemon becomes the real "Environment Daemon": it speaks the E2B
   envd protocol surface directly (Connect-RPC envelope, process
   Start/Connect/List, file upload/download), and the host-side e2b-server
   degrades to a thin transparent proxy.
2. **sandbox-matrix AND e2b-server both embed in k8e-server; the Cilium
   Gateway API is the only external door.** The Gateway API fronts ALL
   external gRPC (:50051) and HTTP (:80/:443) traffic (see Part A of
   KIP-18), so neither the controller nor the e2b surface needs to be
   extracted into a separate process — they stay in k8e-server (e2b is on by
   default; `--disable-e2b` to turn off)
   and the Gateway routes to the host-resident services via headless
   Service + Endpoints.

This is a design document: it lays out the target architecture, the change
inventory, the migration path, and the test plan. Implementation is split
into phases and tracked separately.

## Why now

The previous rounds established the substrate this design builds on:

- **Cilium Gateway API fronts every external sandbox port** (`e2b-gateway.yaml`:
  HTTPRoute :80/:443 → e2b-server, TCPRoute :50051 → host gateway). The
  Gateway is now the single ingress; nothing external dials k8e-server
  directly for sandbox traffic.
- **Leader election** gates the embedded controller's reconcilers
  (`leader.go`), proving the controller can run as an HA unit.
- **Ability downshift** (sandboxd native fs/process-control ops) proved the
  pattern: primitives with no gateway RPC live in sandboxd, translated by
  the e2b layer.

Both decisions push in the same direction: **the sandbox-matrix controller
does not need to be (and arguably should not be) inside k8e-server.** It is a
cluster-level operator, and the e2b protocol is its north-bound API.

## Target architecture

```
external clients
  e2b SDK (apiUrl → Gateway)          CLI / SDK (endpoint → Gateway)
  │ HTTP :80/:443                       │ gRPC :50051 (mTLS)
  ▼                                     ▼
Cilium Gateway API (GatewayClass e2b / Gateway :80,:443,:50051)
  │ HTTPRoute → controller Service      │ TCPRoute (L4 passthrough)
  ▼                                     ▼
┌──────────────────────────────────────────────────────────────┐
│ k8e-server host process                                       │
│   ┌────────────────┐   ┌──────────────────────────────────┐  │
│   │ e2b (embedded) │   │ sandbox-matrix controller       │  │
│   │  protocol       │   │  - warm pool reconciler         │  │
│   │  translation,   │──▶│  - GC / idle reaper / resetting │  │
│   │  process table, │   │  - gRPC SandboxService :50051   │  │
│   │  deadline reg   │   │  - leader election (Lease)      │  │
│   └────────────────┘   └──────────────────────────────────┘  │
│   :3676 (e2b HTTP)          :50051 (gRPC, mTLS)              │
│   (dial loopback gateway)                                    │
└──────────────────────────────────────────────────────────────┘
  │ gRPC / HTTP (native downshift, pod IP)
  ▼
sandboxd (:2024 in-pod)  ← now also speaks the E2B envd protocol
   ├─ /exec, /exec/stream, /files/{read,write,list}
   ├─ /files/{stat,mkdir,move,remove}   (native fs)
   ├─ /exec/stdin*, /exec/signal        (process control)
   └─ /e2b/envd/*                       (NEW: envd protocol surface)
```

Key properties:

- **One process** (k8e-server) owns both the E2B protocol translation and the
  sandbox orchestration. e2b logic dials the in-process gateway over
  loopback (`127.0.0.1:<GRPCPort>`); no separate Deployment, no extra image.
- **The Gateway API is the only external door.** :50051 gRPC and :80/:443
  e2b HTTP both enter through Cilium, routed to host-resident services via
  headless Service + Endpoints (`e2b-server`, `sandbox-grpc-gateway`).
- **sandboxd stays the single in-sandbox daemon** (one process per sandbox,
  PID 1), now with an envd protocol layer on top of its existing native
  primitives.
- **k8e-server keeps everything embedded** — sandbox CA issuance, orchestrator,
  warm-pool reconcilers (leader-gated), gRPC gateway, and (with e2b enabled
  by default)
  the e2b HTTP surface.

## Decision 1 — envd protocol surface in sandboxd

### What moves

| E2B envd capability | Today | Target |
|---|---|---|
| `process.Process/Start` (stream) | e2b-server: `ExecStream` + SSE strip + marker-file exit code | sandboxd: Connect-RPC stream, in-stream exit code (retire marker file) |
| `process.Process/Connect` | e2b-server process table | sandboxd process table (in-sandbox, survives disconnect by design) |
| `process.Process/List` | e2b-server process table | sandboxd `/processes` → Connect response |
| `process.Process/SendInput/CloseStdin/SendSignal` | e2b-server → sandboxd `/exec/*` | sandboxd native (already there) |
| `filesystem.Filesystem/*` | e2b-server → sandboxd `/files/*` (native) | sandboxd Connect-RPC envelope over the same ops |
| `filesystem.Filesystem/WatchDir` | 501 | sandboxd events stream (KIP-16 L5 events) |
| `/files` upload/download | e2b-server: `ReadFile`/`WriteFile` (multipart/octet) | sandboxd native file IO + gzip/range |
| Signed URLs / HMAC auth | e2b-server | stays in controller (protocol-level auth, not sandbox primitive) |

### What stays in the controller

- Connect-RPC envelope *authentication* (envd HMAC tokens, signed URLs) —
  these are per-sandbox credentials minted by the controller; sandboxd has no
  cluster context.
- Control plane REST (create/connect/kill/pause/resume/list/timeout) — the
  sandbox lifecycle, not a sandbox primitive.
- Error dialect translation at the boundary (gRPC code → `{code,message}`).
- The process table *identity mapping* (synthetic pid ↔ guest pid) if the
  controller still needs to reference processes across reconnect; sandboxd
  owns the actual table.

### Wire details

- sandboxd gains a Connect-RPC JSON envelope codec (1 flag byte + BE32 length
  + JSON), same as the e2b layer already speaks — one codec, two sides.
- The controller becomes a **transparent proxy**: it verifies the HMAC token,
  then forwards the Connect frame to sandboxd unchanged and streams the
  response back. No re-enveloping, no SSE stripping, no marker files.
- sandboxd reports `exitCode` in the stream end event (`data: {"exit":N}`),
  retiring `wrapWithExitCode` / `readExitCode` — done in P1.

### Protocol gap audit vs the official E2B envd spec

Source of truth: the official E2B repo (`spec/envd/process/process.proto`,
`spec/envd/filesystem/filesystem.proto`) and the python-sdk's connectrpc
transport (`packages/python-sdk/e2b/envd/{process,filesystem}/*_connect.py`,
`utils.py`). The envd protocol has three surfaces: `process.Process` (9 RPCs),
`filesystem.Filesystem` (9 RPCs), and the `/files` HTTP API. This is the
k8e-side gap inventory against that spec; it is the implementation checklist
for "sandboxd-as-envd".

#### Process surface (9 RPCs)

| E2B RPC | Wire semantics | k8e today | Gap |
|---|---|---|---|
| `Start` (server_stream) | `{start:{pid}}` → `{data:{stdout\|stderr}}` → `{end:{exit_code,exited,status}}` | ✅ sandboxd | in-stream exit frame; SDK consumes `end.exit_code` (verified), `status` string is informational |
| `Connect` (server_stream) | reattach by `ProcessSelector` oneof **pid \| tag** | ✅ pid + sandboxd attach | tag selector unimplemented (SDK never sends tag — verified) |
| `List` | running processes | ✅ | — |
| `SendInput` | stdin bytes | ✅ native (downshifted) | — |
| `CloseStdin` | EOF | ✅ native | — |
| `SendSignal` | `enum Signal { SIGTERM=15, SIGKILL=9 }` | ✅ native | wire is a numeric enum; k8e accepts strings — verify wire compat |
| `Update` | PTY resize | ❌ 501 | sandboxd has no PTY |
| `StreamInput` (client_stream) | client→server stream (`Start`/`Data`/`KeepAlive`) | ❌ 501 | **only bidirectional RPC**; k8e HTTP/1.1 single-connection model cannot carry it — needs HTTP/2 or accept as permanent 501 |
| `UpdatePTY` | PTY session | ❌ 501 | no PTY |

#### Filesystem surface (9 RPCs)

| E2B RPC | Wire semantics | k8e today | Gap |
|---|---|---|---|
| `Stat` | `EntryInfo` incl. `symlink_target`, xattr `metadata` (`user.e2b.*`) | ✅ native | **`symlink_target` not returned** (sandboxd statx no symlink resolve); **xattr metadata unimplemented** |
| `MakeDir` | proto returns `EntryInfo`; **SDK ignores it** (returns bool, `ALREADY_EXISTS` → False) | ✅ | none — SDK does not read the entry (verified) |
| `Move` | proto returns `EntryInfo`; **SDK reads `r.entry or EntryInfo()` fallback** | ✅ | P2 — empty EntryInfo returned when entry omitted (no error, but rename result has no info) |
| `ListDir` | `depth` param + `EntryInfo[]` | ✅ | depth-aware (P1 audit item resolved 2026-08-14) |
| `Remove` | delete path | ✅ | — |
| `WatchDir` (server_stream) | event stream | ❌ 501 (low-level path; the SDK uses the polling trio) | |
| `CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` | non-streaming watch trio | ✅ sandboxd inotify (KIP-18 P1 last gap, 2026-08-14) | inotify per-watcher event ring; GetWatcherEvents is incremental (SDK WatchHandle semantics) |

#### /files HTTP API

multipart/form-data (field `file`), octet-stream (envd ≥0.5.7, streamed),
gzip `Content-Encoding` decode, `Range` download, `write_files` batch with
partial-failure reporting — all ✅ in k8e. No gaps.

#### Transport-layer details (connectrpc SDK behavior)

| Detail | Official SDK | k8e | Gap |
|---|---|---|---|
| Connect JSON envelope (1 flag + BE32 + JSON) | ✅ | ✅ | — |
| Paths `/process.Process/*`, `/filesystem.Filesystem/*` | ✅ | ✅ | — |
| **Basic auth** `Authorization: Basic base64(user:)` (`authentication_header` in `utils.py`) | SDK sends only when `user=None` **and** `envd_version < ENVD_DEFAULT_USER` (0.4.0) | ❌ only `X-Access-Token` | **none (verified)** — k8e claims envd 0.6.1 ≥ 0.4.0, so the SDK never sends Basic; and the SDK reads `envd_version` from the create response's `envdVersion` (camelCase, `sandbox.py from_dict`), which k8e's `sessionView` returns correctly |
| KeepAlive events in Start/Connect streams | ✅ SDK sends/polls | ❌ | no heartbeat — long streams may be cut by proxy timeouts |
| `Connect-Timeout-Ms` header | ✅ SDK sends | ❌ not read | server-side hard deadline unimplemented |
| `domain` field in create response | SDK reads `res.parsed.domain` (`isinstance str` else `None`) | ❌ not returned | none (verified) — SDK tolerates missing domain → `sandbox_domain=None`; k8e's sandboxUrl is explicitly configured so the None is unused, but worth adding for SDKs that derive the data-plane URL |

#### Gap priorities

- **P0**: **resolved — no interop breakage**. Verified against the SDK:
  `EndEvent.status` is informational — the SDK consumes `end.exit_code`
  (command_handle.py) and never parses the `status` string. (The earlier
  `MakeDir`/`Move` entry-return concern was already resolved as
  non-blocking.)
- **P1 (functional gaps, SDK methods throw)**: `StreamInput` (**resolved:
  permanent 501**, no SDK calls it), `ListDir` depth (**resolved**),
  `Connect` by tag (**no SDK usage — low priority**), `WatchDir` family
  (**resolved** — polling trio implemented via sandboxd inotify).
- **P2 (semantic details)**: **resolved 2026-08-14** — `Move` already
  returns a real `EntryInfo` (stat-after-move, verified); `MakeDir` returns
  an entry too; KeepAlive heartbeat implemented (idle 15 s, mutex-guarded
  write); `symlink_target` implemented (sandboxd readlink + e2b passthrough,
  Linux-tested); `SendSignal` numeric-enum wire confirmed compatible
  (proto3 JSON enum names `SIGNAL_SIGKILL`/`SIGNAL_SIGTERM` accepted);
  `domain` in the create response deliberately omitted — the SDK tolerates
  absence (`isinstance str else None`) and k8e's sandboxUrl is explicit.
  xattr `metadata` (`user.e2b.*`) remains unimplemented — no SDK surface
  depends on it for the supported flows.

#### Where each gap lands

Per this design (envd capability in sandboxd, e2b layer = transparent proxy):
P0/P2 protocol details (status format, tag selector, symlink/xattr, Signal
enum, Move entry) belong in **sandboxd**. `StreamInput` was the one hard
case (HTTP/2 client_stream in sandboxd) — resolved as a **permanent 501**:
no official SDK calls it (verified against python/js-sdk sources), so the
Zig HTTP/2 work would have zero consumers. `WatchDir` builds on sandboxd's
KIP-16 L5 events stream.

## Decision 2 — both embed in k8e-server, Gateway API fronts everything

**Final architecture decision**: sandbox-matrix AND e2b-server both embed in
the k8e-server process. The Cilium Gateway API is the ONLY external door for
every sandbox port. This is the opposite of the earlier "extract to a
standalone controller service" direction — the Gateway API already provides
the ingress, so there is no need to also extract the process.

### What this looks like

```
external clients (e2b SDK, CLI)
   │ HTTP :80/:443                  │ gRPC :50051 (mTLS)
   ▼                                ▼
Cilium Gateway API (Gateway :80,:443,:50051)
   │ HTTPRoute → e2b-server Service  │ TCPRoute → sandbox-grpc-gateway Service
   ▼                                ▼
   └──────────────┬─────────────────┘
                  ▼
k8e-server host process
   ├── sandbox-matrix controller (reconcilers, leader-gated)  [sandboxmatrix.Register]
   ├── gRPC SandboxService :50051 (mTLS)                        [same]
   └── e2b HTTP :3676 (embedded, loopback gateway dial)        [NEW]
```

### Implementation

- **Embedded e2b (on by default)**: `--disable-e2b` (`K8E_DISABLE_E2B`) turns
  it off; otherwise the E2B HTTP server starts inside k8e-server after
  `sandboxmatrix.Register` returns. It dials the in-process gRPC gateway over
  loopback (`127.0.0.1:<GRPCPort>`) via the existing
  `client.NewClientWithEndpoint` — zero new Gateway-interface work, the trust
  model is unchanged (loopback + LocalAuth). Skipped automatically when the
  sandbox matrix is disabled (`--disable-sandbox-matrix`), since e2b dials
  its gateway.
- **`--e2b-listen`** (new flag, `K8E_E2B_LISTEN`, default `0.0.0.0:3676`):
  the e2b HTTP listen address. Must be 0.0.0.0 (or the advertise IP) so the
  cluster's headless e2b-server Service/Endpoints can reach it; the Gateway
  API is the only external door.
- **No separate Deployment.** `e2b-gateway.yaml` uses headless Service +
  Endpoints pointing at `--advertise-address` for BOTH the e2b HTTP (:3676)
  and the sandbox gRPC (:50051) host services — same pattern as the existing
  `sandbox-grpc-gateway` bridge.

### Multi-node consistency (deadline / pause / metadata)

With sandbox-matrix and e2b both embedded, every control-plane node runs an
e2b instance. The E2B bookkeeping must not live in per-process maps that
diverge when the Gateway API routes a request to a different node:

| State | Before (per-node) | After (direction B) |
|---|---|---|
| E2B kill deadline (`deadlineAt`, `onDeadline`) | in-memory `sandboxRegistry` map | SandboxSession annotation `sandbox.k8e.io/e2b-state` (JSON) |
| Explicit pause (`pausedByUser`) | in-memory | same annotation (CRD `Phase: Paused` is the authoritative lifecycle flag) |
| Idempotent-create name index (`metadata.name` → sandboxID) | in-memory `byName` map | annotation `sandbox.k8e.io/e2b-name` + List-scan fallback |
| createdAt / metadata / runtimeName echoed in views | in-memory | same `e2b-state` annotation |

Implementation:

- `stateStore` interface (`pkg/sandbox/e2b/state_store.go`) is the persistence
  contract; `sandboxRegistry` (in-memory) and `crdStateStore`
  (`pkg/sandbox/e2b/crd_state.go`) both implement it. The e2b `Server`
  accepts a `Config.StateStore`; the embedded mode injects the CRD store
  (built from the kubeconfig via `dynamic.Interface`), the standalone
  `k8e e2b-server` command falls back to the in-memory store (single-node
  semantics, documented).
- GC: every node's `gcLoop` scans the CRD-backed `ids()` and enforces
  deadlines with idempotent `PauseSession`/`DestroySession` calls — no
  leader-gating needed because the underlying RPCs are already idempotent
  (a second node observing the same expired session finds it already
  paused/destroyed).
- The process table moves toward sandbox-owned (P1): the E2B Process/List
  view is served from sandboxd's process-control table (`/exec/processes`,
  pids are the sandbox's own — node-independent). The subscriber broadcast
  (output fan-out to live HTTP streams) stays per-node — it is connection-
  local by nature. `Connect` re-attachment to a running process is pending
  (see P1 status below).

### Why embed (vs the earlier extract direction)

| Criterion | Embed in k8e-server (final) | Standalone Deployment (earlier draft) |
|---|---|---|
| Single binary, zero extra deploy surface | ✅ (k8e's core positioning) | ❌ another Deployment + RBAC + Secret |
| Gateway API already fronts ports | ✅ Gateway is the ingress | ✅ same |
| e2b ↔ gateway hop | loopback in-process (cheap) | socket gRPC (also cheap) |
| Failure domain | e2b dies with server | isolated |
| Horizontal scaling of e2b | ❌ tied to server | ✅ multi-replica |
| `k8e e2b-server` standalone command | kept for compat (same logic) | kept for compat |

For k8e's single-binary distribution the embed path is the right trade: the
e2b surface is not a scale hotspot (agent sessions, not mass traffic), and
keeping it in-process preserves the zero-ops story. The standalone
`k8e e2b-server` command stays as a thin wrapper for existing users.

## Migration path

| Phase | Work | Verifiability |
|---|---|---|
| P1 | sandboxd envd protocol: Connect codec + process/fs surface + in-stream exitCode | Zig tests + e2b unit tests against sandboxd stub |
| P2 | Embedded e2b in k8e-server (loopback gateway dial, on by default; `--disable-e2b` + `--e2b-listen`) | `go build` + unit test on runEmbeddedE2B wiring; smoke: server serves both :50051 and :3676 |
| P3 | Gateway routes point at host headless Services (e2b-server :3676, sandbox-grpc-gateway :50051); retire the e2b Deployment | live-cluster smoke: create/pause/resume/list + SDK calls through Gateway |
| P4 | e2b on by default (`--disable-e2b` to opt out); document `k8e e2b-server` as thin compat wrapper | SDK e2e against the embedded surface |

### P1 progress (2026-08-14)

**Done — the sandbox-owned process table is complete:**
- `execctl.zig` Entry: command snapshot (for `Process/List`) + 64 KiB ring
  buffer of recent output (for attach) + done flag; `GET /exec/processes`
  returns `{pid, alive, config}`; `GET /exec/attach?pid=N` replays the
  buffered output as SSE; `data: {"exit":N}` closes the stream with the
  exit code (retiring the marker-file hack). Pids are the sandbox's own —
  node-independent.
- `exec.zig`: streaming exec registers with the command, appends output to
  the ring buffer, marks done on reap, and emits the exit frame after
  `wait4`.
- e2b: `Process/List` reads the sandbox-owned table (cross-node consistent,
  fallback to the local subscriber table); `Process/Connect` falls back to
  `/exec/attach` when the local table has no record (cross-node Start);
  `runProcessStream` captures the in-stream exit code (`parseExitFrame`).
- `wrapWithExitCode` / `readExitCode` / marker files removed.
- Zig tests: `execctl_test.zig` (ring buffer, attach, done); full sandboxd
  suite **41/41 on Linux** (OrbStack debian runner — the macOS sandbox
  blocks fork tests, 16 pass / 9 crash there); cross-compiles for
  x86_64/aarch64/riscv64-linux-musl. e2b suite + `-race` green.

**P1 follow-up decisions (2026-08-14):**
- **`Connect` attach = buffer replay is the correct semantic.** k8e's
  `Process/Start` is a foreground stream that runs until the process exits
  (its output rides the Start stream); `Connect` therefore reconnects to an
  *already-finished* process's buffered output — exactly what `/exec/attach`
  replays. Live tailing a still-running process from a second consumer would
  only matter for sandboxd *background* processes (`/exec/background`), which
  have their own poll surface; not a gap for the E2B Connect path.
- **`StreamInput` is a permanent 501, documented.** Verified against the
  official SDK sources: neither `packages/python-sdk` nor `packages/js-sdk`
  ever calls `StreamInput` — `send_stdin` goes through the unary
  `SendInput` RPC. Implementing HTTP/2 client_stream in sandboxd (Zig)
  would cost significant work with zero SDK consumers. The 501 hint tells
  clients to use `SendInput`. Custom low-level clients needing streamed
  stdin are out of scope.

## Test plan

- **sandboxd**: Zig tests for the Connect codec (envelope round-trip,
  frame-boundary splitting), process surface (Start/Connect/List/SendInput/
  SendSignal semantics), WatchDir events, in-stream exitCode — plus the
  protocol-gap items from the audit: `MakeDir`/`Move` returning `EntryInfo`,
  `EndEvent.status` format, `Connect` by tag, `ListDir` depth, `symlink_target`.
- **controller**: Go unit tests for the embedded wiring (`--disable-e2b`
  starts the e2b HTTP server after `sandboxmatrix.Register`; both listeners
  on :50051 and :3676), loopback gateway dial, leader election still gating
  reconcilers only.
- **e2b**: existing suite against the sandboxd stub, now exercising the
  transparent-proxy path (frames forwarded verbatim, no SSE strip) and the
  remaining transport details from the audit (KeepAlive,
  `Connect-Timeout-Ms` handling). Basic-auth threshold behavior is
  **verified non-blocking** (0.6.1 ≥ 0.4.0 threshold, camelCase
  `envdVersion` field match) — no test needed beyond a regression that the
  create response keeps returning `envdVersion`.
- **deploy**: manifest assertions (controller Service/Deployment/RBAC,
  Gateway routes point at the merged Service, host gateway Service retired).
- **live cluster** (open item, as before): Gateway programmed, :50051 mTLS
  passthrough, :443 envd Connect, pause/resume through Gateway.

## Risks / open questions

- **Cert bootstrap**: sandboxd connecting requires the controller to hold the
  sandbox CA. Secret rotation path must be defined (currently host files).
- **Loopback gateway dial**: embedded e2b dials `127.0.0.1:<GRPCPort>`
  via the existing sandbox client (mTLS + LocalAuth). This is a real socket
  hop inside the process, but it reuses the battle-tested client path and
  keeps a clean seam if a future in-process `Gateway` adapter replaces it.
- **sandboxd protocol breadth**: full envd protocol in Zig is significant
  work (Connect codec, process table lifecycle, watch). The protocol gap
  audit above is the implementation checklist; P0/P2 details land in
  sandboxd, P1 decides the `StreamInput` HTTP/2 question, `WatchDir` builds
  on the KIP-16 L5 events stream. P1 progress: the sandbox-owned process
  table + node-independent `Process/List` are done (see P1 progress above);
  `Connect` attach is pending and needs a Linux test environment (macOS
  sandbox blocks the Zig fork tests).
- **e2b on by default**: the embedded surface is enabled unless
  `--disable-e2b` or `--disable-sandbox-matrix` is set; validate on a live
  cluster that the default-on path does not affect servers that never
  configure the Gateway API.

## References

- KIP-18 `docs/kip-18-sandbox-e2b-compat.md` (Part A: Gateway API ingress;
  Part D/E: process + filesystem surface; Appendix: CubeSandbox backlog).
- E2B — `spec/envd/process/process.proto`,
  `spec/envd/filesystem/filesystem.proto`,
  `packages/python-sdk/e2b/envd/{process,filesystem}/*_connect.py`
  (connectrpc Endpoint wiring), `packages/python-sdk/e2b/envd/utils.py`
  (Basic auth `authentication_header`), `packages/python-sdk/e2b/envd/
  versions.py` (ENVD_DEFAULT_USER threshold).
- `pkg/sandboxmatrix/{controller,leader}.go`, `pkg/sandboxmatrix/grpc/
  {server,orchestrator}.go`, `pkg/sandbox/e2b/*.go`, `sandboxd/src/*.zig`.
- Cilium Gateway API docs (GRPCRoute/TCPRoute/BackendTLSPolicy), Gateway API
  v1.6.1 CRDs.
