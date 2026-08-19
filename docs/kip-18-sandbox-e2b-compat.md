# KIP-18: E2B-compatible sandbox API for K8E

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-15 | Accepted — implemented in PR #541 |

## Summary

Make the **official `e2b` SDK** (JS and Python, unmodified) work against K8E by
speaking the E2B protocol: the control plane at the **root** (CubeSandbox
style, so `apiUrl` is the bare origin), the envd surface under `/e2b/envd`,
and a signed-file door at `/files` (the `/e2b/api` prefix remains as a
compatibility alias). The protocol layer translates every call into the
existing `sandbox.v1.SandboxService` gRPC gateway and, for primitives with no
gateway RPC, down into the in-pod `sandboxd` daemon. Migrating an E2B
application to K8E is configuration, not code:

```ts
import { Sandbox } from 'e2b';

const sbx = await Sandbox.create({
  apiKey: process.env.E2B_API_KEY, // must be e2b_<hex> — SDK validateApiKey rejects a bare hex token
  apiUrl: 'http://127.0.0.1:3676',
  sandboxUrl: 'http://127.0.0.1:3676/e2b/envd',
});
```

Dormice's compatibility contract and CubeSandbox's operational semantics are
the model: **faithful by default, honest about what is not supported** — every
unsupported surface answers a machine-readable
`unimplemented`/`invalid_argument` with a hint, never a silent partial
behavior; lifecycle and timeout semantics (pause / resume / auto-pause /
NEVER_TIMEOUT / fine-grained errors) follow CubeSandbox.

**This KIP is implemented.** PR #541 ships the final architecture below
(embedded e2b + embedded sandbox-matrix controller behind the Cilium Gateway
API, with the E2B protocol surface split between the host e2b layer and
`sandboxd`). The implementation-status section (§8) lists exactly what the PR
lands and what remains as honest 501s.

## Motivation

K8E's sandbox matrix already exposes a full gRPC `SandboxService` (sessions,
exec, exec-stream, files, background runs, pip, snapshots — KIP-3/10/11/16).
But the ecosystem's *de facto* agent-sandbox wire protocol is **E2B**, and
thousands of agents ship with `e2b` SDK integrations already written. Making
K8E speak E2B lets those agents run on K8E with a two-line config change:

- **Ecosystem interop** — the official `e2b` npm / PyPI packages, unmodified.
- **Bridging the AI-agent world** — agents that target E2B templates can target
  K8E sandboxes without code changes.
- **A second, higher-level API** — E2B's REST/Connect surface is simpler than
  raw gRPC and gives SDK authors a stable target.

This KIP does **not** replace the gRPC gateway; the E2B server is a thin
protocol adapter on top of it (the same relationship Dormice has between its
native API and its E2B compat layer).

## 1. Architecture (final, implemented)

Two architectural decisions were made together and are both shipped:

1. **envd (E2B ecosystem) capabilities move into k8e's `sandboxd`** — the
   in-pod daemon becomes the real "Environment Daemon": it owns the process
   table (pids are the sandbox's own, node-independent), the native
   filesystem/process-control primitives, and the E2B watch trio; the
   host-side e2b layer keeps protocol translation (auth, envelope, error
   dialect, deadline registry) and transparently proxies what sandboxd owns.
2. **sandbox-matrix AND e2b-server both embed in k8e-server; the Cilium
   Gateway API is the only external door.** The Gateway API fronts ALL
   external gRPC (:50051) and HTTP (:80/:443) traffic, so neither the
   controller nor the e2b surface is extracted into a separate process — they
   stay in k8e-server (e2b is on by default; `--disable-e2b` to turn off) and
   the Gateway routes to the host-resident services via headless Service +
   Endpoints.

### 1.1 One process: sandbox-matrix + e2b both embed in k8e-server

```
external clients
  e2b SDK (apiUrl → Gateway)          CLI / SDK (endpoint → Gateway)
  │ HTTP :80/:443                       │ gRPC :50051 (mTLS)
  ▼                                     ▼
Cilium Gateway API (GatewayClass e2b / Gateway :80,:443,:50051)
  │ HTTPRoute → controller Service      │ TCPRoute (L4 passthrough, mTLS kept)
  ▼                                     ▼
┌──────────────────────────────────────────────────────────────┐
│ k8e-server host process                                       │
│   ┌────────────────┐   ┌──────────────────────────────────┐  │
│   │ e2b (embedded) │   │ sandbox-matrix controller       │  │
│   │  protocol       │   │  - warm pool reconciler         │  │
│   │  translation,   │──▶│  - GC / idle reaper / resetting │  │
│   │  process views, │   │  - gRPC SandboxService :50051   │  │
│   │  deadline reg   │   │  - leader election (Lease)      │  │
│   └────────────────┘   └──────────────────────────────────┘  │
│   :3676 (e2b HTTP)          :50051 (gRPC, mTLS)              │
│   (dial loopback gateway)                                    │
└──────────────────────────────────────────────────────────────┘
  │ gRPC / HTTP (native downshift, pod IP)
  ▼
sandboxd (:2024 in-pod)  ← the real Environment Daemon
   ├─ /exec, /exec/stream, /files/{read,write,list}
   ├─ /files/{stat,mkdir,move,remove}   (native fs)
   ├─ /exec/stdin*, /exec/signal        (process control)
   ├─ /exec/processes, /exec/attach     (process table, node-independent)
   ├─ /watch/{create,events,remove}     (inotify watch trio)
   └─ in-stream exit frame (data: {"exit":N}) — no marker files
```

Key properties:

- **One process** (k8e-server) owns both the E2B protocol translation and the
  sandbox orchestration. e2b logic dials the in-process gateway over
  loopback (`127.0.0.1:<GRPCPort>`); no separate Deployment, no extra image.
- **The Gateway API is the only external door.** :50051 gRPC and :80/:443
  e2b HTTP both enter through Cilium, routed to host-resident services via
  headless Service + Endpoints (`e2b-server`, `sandbox-grpc-gateway`).
- **sandboxd stays the single in-sandbox daemon** (one process per sandbox,
  PID 1), now with the process table, native fs/process-control primitives,
  and the watch trio on top of its existing surface.
- **k8e-server keeps everything embedded** — sandbox CA issuance, orchestrator,
  warm-pool reconcilers (leader-gated), gRPC gateway, and (with e2b enabled
  by default) the e2b HTTP surface.

### 1.2 Gateway ingress (from the original Part A)

Every externally exposed sandbox port flows through a **Cilium Gateway API**
front door in the sandbox-matrix namespace (`manifests/sandbox-matrix/
e2b-gateway.yaml`): a `GatewayClass` (`io.cilium/gateway-controller`) + a
`Gateway` with three listeners:

- **HTTP :80** — e2b control plane (`/` → `e2b-server` Service);
- **HTTPS :443** — envd surface (`/e2b`, `/e2b/api`, `/files` → `e2b-server`
  Service), TLS terminated at the Gateway (`sandbox-e2b` secret),
  `gatewayAPI.enableAlpn` on for the HTTP/2 envd Connect backend;
- **TCP :50051** — sandbox gRPC `SandboxService`, a **TCPRoute (L4
  passthrough)** to the headless `sandbox-grpc-gateway` Service + Endpoints
  pointing at the host's `--advertise-address`.

> **Errata (KIP-21):** the Endpoints address is resolved **loopback-proof** —
> `advertiseIP()` never emits loopback (`127.0.0.1`/`::1`) because Kubernetes
> rejects loopback Endpoint addresses, which left the Gateway with no healthy
> backends on a flag-less default install. Resolution order:
> `--advertise-address` → non-loopback `--bind-address` → default-route
> interface (`utilnet.ChooseHostInterface`). If no routable address can be
> resolved, `e2b-gateway.yaml` is **not staged** and the server logs an
> actionable `--advertise-address <node-ip>` hint. See
> [docs/kip-21-host-advertise-ip-resolution.md](kip-21-host-advertise-ip-resolution.md).

The gRPC listener is deliberately L4 passthrough, **not** GRPCRoute with TLS
termination: the gateway speaks strong mTLS (client certs signed by the sandbox
CA, verified by `mTLSAuthInterceptor`), and terminating TLS at the Gateway
would break the client-certificate flow. TCPRoute forwards the byte stream
unchanged, so existing SDK/CLI client certs keep working. The e2b HTTP surface
is plain HTTP/Connect, so Gateway-terminated TLS is correct there. This
requires the Gateway API CRDs (`gateway-api-crds.yaml`, v1.6.1 standard +
experimental TLSRoute/TCPRoute — required by Cilium 1.20) staged before the
Cilium HelmChart and `gatewayAPI.enabled: true` in `manifests/cilium.yaml`.

### 1.3 sandboxd-as-envd: where each capability lives

| E2B envd capability | Owner | Implementation |
|---|---|---|
| `process.Process/Start` (stream) | sandboxd | Connect-RPC stream, in-stream exit frame (`data: {"exit":N}`), first frame carries the in-guest pid |
| `process.Process/Connect` | sandboxd | reattach via `/exec/attach` — SSE replay of the buffered output (buffer-replay is the correct semantic; see §8) |
| `process.Process/List` | sandboxd | `/exec/processes` → `{pid, alive, config}` (sandbox-owned, node-independent) |
| `process.Process/SendInput` / `CloseStdin` / `SendSignal` | sandboxd | native `/exec/stdin`, `/exec/stdin/close`, `/exec/signal` |
| `filesystem.Filesystem/*` | sandboxd | native `/files/{stat,mkdir,move,remove,list}`; `ListDir` depth-aware; `Stat` returns `symlink_target` |
| `filesystem.Filesystem/WatchDir` | sandboxd | **501** (streaming surface); the SDK's polling trio below is what the SDK actually uses |
| `CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` | sandboxd | inotify per-watcher event ring: `/watch/create`, `/watch/events` (incremental cursor), `/watch/remove` |
| `/files` upload/download | e2b layer → sandboxd | `ReadFile`/`WriteFile` (multipart/octet/gzip/Range) |
| Signed URLs / HMAC auth | e2b layer | protocol-level auth, not a sandbox primitive |
| Connect-RPC envelope *authentication* (HMAC tokens, signed URLs) | e2b layer | per-sandbox credentials minted by the controller; sandboxd has no cluster context |
| Control plane REST (create/connect/kill/pause/resume/list/timeout) | e2b layer | the sandbox lifecycle, not a sandbox primitive |
| Error dialect translation (gRPC code → `{code,message}`) | e2b layer | at the boundary |
| Deadline registry / pause / metadata persistence | e2b layer via `stateStore` | CRD-backed in embedded mode (`crdStateStore`); in-memory standalone |

The controller (e2b layer) is a **transparent proxy** for everything sandboxd
owns: it verifies the HMAC token, then forwards the request to sandboxd
unchanged and streams the response back — no re-enveloping, no SSE stripping,
no marker files. The one exception is the in-process gateway hop for the
gRPC-backed RPCs (session lifecycle, exec, file read/write/list), which is
what the gateway itself already provides.

### 1.4 Multi-node consistency

With sandbox-matrix and e2b both embedded, every control-plane node runs an
e2b instance. The E2B bookkeeping must not live in per-process maps that
diverge when the Gateway API routes a request to a different node:

| State | Standalone (per-node) | Embedded (multi-node consistent) |
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
- The process table is **sandbox-owned** (§5): `Process/List` and
  `Process/Connect` are served from sandboxd's table (`/exec/processes`,
  `/exec/attach`), so they are node-independent. The subscriber broadcast
  (output fan-out to live HTTP streams) stays per-node — it is
  connection-local by nature.

### 1.5 Why embed (vs the earlier extract direction)

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

## 2. Deployment and flags

**Embedded (default).** e2b starts inside k8e-server after
`sandboxmatrix.Register` returns, unless `--disable-sandbox-matrix`
(e2b dials the sandbox gRPC gateway, which is part of the matrix) or
`--disable-e2b` (`K8E_DISABLE_E2B`) is set. It dials the in-process gRPC
gateway over loopback (`127.0.0.1:<GRPCPort>`) via the existing
`client.NewClientWithEndpoint` — zero new Gateway-interface work, the trust
model is unchanged (loopback + LocalAuth).

**`--e2b-listen`** (new flag, `K8E_E2B_LISTEN`, default `0.0.0.0:3676`): the
e2b HTTP listen address. Must be 0.0.0.0 (or the advertise IP) so the
cluster's headless e2b-server Service/Endpoints can reach it; the Gateway
API is the only external door. **No separate Deployment:** `e2b-gateway.yaml`
uses headless Service + Endpoints pointing at `--advertise-address` for BOTH
the e2b HTTP (:3676) and the sandbox gRPC (:50051) host services — same
pattern as the existing `sandbox-grpc-gateway` bridge.

**Standalone compat wrapper.** `k8e e2b-server` remains a long-running
subcommand (registered in the main `k8e` binary alongside `server`); it dials
the gRPC gateway the same way the sandbox CLI does — mTLS bootstrap with the
API key, local auto-discovery (`127.0.0.1:50051`) by default,
`--endpoint`/`--apikey` for remote gateways — and listens for HTTP on
`--listen` (default `127.0.0.1:3676`, Dormice's port). In standalone mode it
uses the in-memory `stateStore` (single-node semantics, documented).

**Routing** follows CubeSandbox: the control plane is mounted **at the root**
(`/sandboxes`, `/v2/sandboxes`, …) because the official SDK points `apiUrl`
at the bare origin — the way it builds paths is `new URL('/sandboxes',
apiUrl)`, and a prefix survives only because the SDK treats apiUrl as opaque.
The `/e2b/api` prefix is kept as a compatibility alias for clients already
wired to it (same handlers, same auth). The envd surface stays under
`/e2b/envd` and signed files at `/files`; a root `/health` probe serves
orchestrators (CubeSandbox-style).

```
e2b SDK / CLI / browser
   │ HTTP (REST + Connect-RPC + plain files)          │ gRPC (mTLS, SandboxService)
   │ apiUrl → Gateway :80/:443                        │ endpoint → Gateway :50051
   ▼                                                  ▼
Cilium Gateway API (GatewayClass e2b / Gateway :80,:443,:50051)
   │ HTTPRoute / → e2b-server Service  (control plane)   │ TCPRoute (L4 passthrough)
   │ HTTPRoute /e2b*, /files → e2b-server Service        │   ↓ mTLS preserved
   ▼                                                     ▼
k8e-server (embedded e2b, net/http)              sandbox-grpc-gateway Service
   │ gRPC (loopback dial via pkg/sandbox/client)         │ Endpoints → host
   ▼                                                     ▼
sandbox.v1.SandboxService gateway ──► sandboxd (:2024 in-pod)
                                         ├─ /exec, /exec/stream, /files/{read,write,list}
                                         ├─ /files/{stat,mkdir,move,remove}   (native fs)
                                         ├─ /exec/stdin*, /exec/signal        (process control)
                                         ├─ /exec/processes, /exec/attach     (process table)
                                         └─ /watch/{create,events,remove}     (inotify watch trio)
```

The left hop is the gateway for everything that has a gRPC RPC (session
lifecycle, exec, file read/write/list); the right hop is `sandboxdClient`,
which dials sandboxd's HTTP API directly for the primitives that have no
gateway RPC (fs stat/mkdir/move/remove, process stdin/signal, process
table/attach, watch). Both resolve the pod IP from `GetSession` — the same
source the gateway uses.

## 3. Protocol surface

| Prefix | What lives there | Auth |
|--------|------------------|------|
| `/` (root) | Control plane (REST): create / connect / get / kill / timeout / pause / resume / metrics / list | `Authorization: Bearer <key>` **or** `X-API-Key: <key>` **or** `X-API-KEY: e2b_<key>` |
| `/e2b/api` | Same control plane, compatibility alias | same |
| `/e2b/envd` | envd surface (Connect-RPC JSON + plain HTTP files) | `E2b-Sandbox-Id` + `X-Access-Token` (HMAC) |
| `/files` | Signed file URLs (daemon root — the SDK's `new URL('/files', sandboxUrl)` strips the envd prefix) | HMAC signature in the query |
| `/health` | Root liveness probe (no auth, like real envd's) | none |

**Error dialect** (Dormice-verified): every non-2xx body is
`{ code, message }`. The control plane's `code` mirrors the numeric status
(the JS SDK checks `error.code === 404`); the envd Connect surface's `code` is
the protocol string (`not_found`, `invalid_argument`, `unimplemented`, …).

**Fine-grained control-plane errors** (CubeSandbox semantics): a sandbox
mid-lifecycle answers **503 + `Retry-After`** (not 409/500); warm-pool
capacity refusal maps to **409** (a resource conflict, not a rate limit);
a genuinely destroyed sandbox is **404** (the SDK's `kill() === false` key);
a paused-sandbox lifecycle refusal is 409. The gateway's gRPC codes
(`NotFound`, `ResourceExhausted`, `Unavailable`, `FailedPrecondition`,
`Canceled`) are translated to this dialect at the boundary.

**Time-layered route timeouts** (CubeSandbox's 30 s / 120 s / 240 s router
split, adapted to K8s gateway RPC latency): ordinary control-plane routes
get 60 s (create waits for a pod IP), `connect`/`pause`/`resume` get 120 s
(lifecycle transitions), snapshot/rollback-style routes are reserved 240 s,
and streaming paths (process Start/Connect, file download) are never cut.

**envd version claim:** `0.6.1` (Dormice's measured sweet spot: the SDK will
upload octet-stream — which we support — and refuse xattr metadata options
client-side — which we do not support — instead of us accepting data and
silently dropping it).

## 4. Mapping to K8E gRPC

| E2B verb | K8E RPC / sandboxd | Notes |
|----------|---------|-------|
| `POST /sandboxes` (create) | `CreateSession` | `envVars→env`; `templateID→runtime_class` (`base`/absent → default runtime; known runtimes `gvisor|kata|firecracker` accepted, else 404 `template not found`); `metadata.name` becomes the k8e `session_id` → **idempotent create** (same name ⇒ same sandbox, Dormice extension); `timeout` → daemon-side deadline registry (stateStore-backed) |
| `POST /sandboxes/:id/connect` | `GetSession` | wake/extend deadline; returns session view (201 if resumed) |
| `GET /sandboxes/:id` | `GetSession` | info view: `sandboxID, clientID, templateID, metadata, state, startedAt, endAt, cpuCount, memoryMB, diskSizeMB, envdVersion` |
| `DELETE /sandboxes/:id` (kill) | `DestroySession` | 204; second kill → 404 (SDK's `kill()===false` keys on it) |
| `POST /sandboxes/:id/timeout` | — (deadline registry) | extend deadline from now; `-1` = NEVER_TIMEOUT (clear deadline); 204 |
| `POST /sandboxes/:id/pause` | `PauseSession` | release pod (CPU/memory), keep PVC + session; ephemeral (EmptyDir) refused 409; 204 |
| `POST /sandboxes/:id/resume` | `ResumeSession` | re-create pod with the same PVC; 201 |
| `GET /sandboxes/:id/metrics` | — | **`[]`** — K8E has no metrics pipeline yet; honest absence |
| `GET /v2/sandboxes` (list) | `ListSessions` | phase/state filter, `x-next-token` pagination |
| `GET /e2b/envd/health` | `GetSession` | 204 running / 502 not |
| `process.Process/Start` (stream) | `ExecStream` → sandboxd | live SSE streaming; sandboxd reports the in-guest pid in the first frame; **exit code in-stream** (`data: {"exit":N}`), no marker files |
| `process.Process/Connect` (stream) | sandboxd `/exec/attach` | reattach to a (possibly already-finished) process: SSE replay of the buffered output; falls back to sandboxd when the local table has no record (cross-node) |
| `process.Process/List` | sandboxd `/exec/processes` | sandbox-owned process table — living processes only, node-independent |
| `process.Process/SendInput` | sandboxd `/exec/stdin` | base64 stdin → the process's live stdin pipe (native) |
| `process.Process/CloseStdin` | sandboxd `/exec/stdin/close` | EOF on the process's stdin pipe |
| `process.Process/SendSignal` | sandboxd `/exec/signal` | SIGKILL / SIGTERM by in-guest pid (proto3 JSON enum names accepted) |
| `process.Process/Update`, `StreamInput`, `UpdatePTY` | — | **501 `unimplemented`** — no PTY in sandboxd; `StreamInput` is a **permanent 501** (no SDK calls it — see §8) |
| `filesystem.Filesystem/Stat`, `MakeDir`, `Move`, `Remove` | sandboxd `/files/{stat,mkdir,move,remove}` | native in-pod syscalls (no shell), paths resolve under `/workspace`; `Stat` returns `symlink_target`; `MakeDir`/`Move` return `EntryInfo` |
| `filesystem.Filesystem/ListDir` | sandboxd `/files/list` | **depth-aware** `EntryInfo` listing |
| `filesystem.Filesystem/WatchDir` | — | **501 `unimplemented`** (streaming surface; the SDK does not use it) |
| `filesystem.Filesystem/CreateWatcher`, `GetWatcherEvents`, `RemoveWatcher` | sandboxd `/watch/{create,events,remove}` | inotify per-watcher event ring; `GetWatcherEvents` is incremental (SDK `WatchHandle` semantics) |
| `GET/POST /e2b/envd/files` | `ReadFile`/`WriteFile` | multipart + octet-stream + gzip; Range on download |
| `GET/POST /files` (signed URLs) | `ReadFile`/`WriteFile` | Dormice signature scheme (§Signing) |

**Deadline registry.** K8E's `CreateSession` takes no per-sandbox TTL (the TTL
comes from the `SandboxMatrix` CRD). E2B `timeout` semantics are enforced by
the e2b server itself: a `sandboxID → deadline` map, extended by
`connect`/`timeout`, cleared by the `-1` NEVER_TIMEOUT sentinel, with a GC
loop that calls `DestroySession` (kill deadline) or `PauseSession`
(autoPause deadline) when it passes. **Persistence** is via the `stateStore`
interface: embedded mode uses the CRD-backed `crdStateStore` (SandboxSession
annotation `sandbox.k8e.io/e2b-state`), so a control-plane node restart keeps
E2B TTL semantics and any node can enforce deadlines; the standalone
`k8e e2b-server` uses the honest in-memory registry (a restart loses the
registry, sessions survive, GC defaults to K8E's own `ExpiresAt`).
`autoPause` at the deadline releases the pod and keeps the PVC — exactly
E2B's `lifecycle.on_timeout="pause"` (CubeSandbox's auto-pause); an ephemeral
session that cannot pause degrades to kill, honestly.

**Session view fields.** The Python SDK's generated models hard-require
`clientID` and the full info-view field set (`cpuCount, diskSizeMB, endAt,
envdVersion, memoryMB, startedAt, state, templateID`); Dormice measured that
their absence is a `KeyError` before user code runs. The e2b server fills
them from config (`--node-id`, default `k8e`), defaults (`cpuCount: 1`,
`memoryMB` from `--default-memory`, `diskSizeMB` default 10 GiB), and the
deadline registry. `endAt` is **omitted** for never-timeout sandboxes
(CubeSandbox's honest absence — a fabricated year-out would read as
"expires soon" to SDK arithmetic); paused sandboxes report `state: paused`
and survive a `connect`.

## 5. Process surface

E2B's defining behavior — `background: true` is the same wire as a foreground
run, `disconnect()` never kills, `connect(pid)` reattaches — means a
process's lifetime must be decoupled from any HTTP response. The process
table is **owned by sandboxd** (KIP-18 P1): pids are the sandbox's own, so
they are node-independent and survive the e2b layer entirely.

- **sandboxd table** (`execctl.zig`): every exec registers an Entry — the
  command snapshot (for `Process/List`) + a 64 KiB ring buffer of recent
  output (for attach) + a done flag. `GET /exec/processes` returns
  `{pid, alive, config}`; `GET /exec/attach?pid=N` replays the buffered
  output as SSE; the stream closes with `data: {"exit":N}` carrying the exit
  code after `wait4` (retiring the marker-file hack).
- **e2b layer**: `Process/List` reads the sandbox-owned table (cross-node
  consistent, fallback to the local subscriber table); `Process/Connect`
  falls back to `/exec/attach` when the local table has no record
  (cross-node Start); `runProcessStream` captures the in-stream exit code
  (`parseExitFrame`).
- **In-guest pid bridge.** The first SSE frame of `/exec/stream` carries the
  in-guest pid (`data: {"pid":N}`); the e2b server parses it and records it
  on the process record (table-mutex-guarded). Until the frame arrives the
  control verbs answer `not_found` honestly — the process exists but its
  in-guest pid is not known yet. This is why `SendInput`/`SendSignal` are
  **native**, not 501: sandboxd's process-control table maps in-guest pid →
  open stdin pipe via `/exec/stdin` (base64 → pipe write), `/exec/stdin/close`
  (EOF), and `/exec/signal` (SIGKILL/SIGTERM; a pid that already exited reaps
  to `not_found` so the SDK sees `kill() === false`).

`Process/Start` maps to k8e's `ExecStream`:

1. The SDK sends `/bin/bash -l -c <command>` (or `-c`). We translate: the
   `-c` argument becomes the sandboxd command; any other shape is an
   in-stream `invalid_argument` ("only shell commands are supported").
2. The command runs as-is; the exit code arrives **in-stream**: sandboxd's
   `/exec/stream` closes with a `data: {"exit":N}` SSE frame. Absent (killed
   by timeout / SIGKILL / stream cut) ⇒ `exitCode: -1, status: killed`.
3. Output frames are `{event:{data:{stdout: <base64>}}}`. K8E's `/exec/stream`
   merges stdout+stderr into one pipe, so all output rides the `stdout`
   channel (documented limitation; the SDK concatenates stdout anyway).
   The gateway leaks sandboxd's `data: ` SSE framing into gRPC chunks, so the
   e2b server strips SSE framing before re-enveloping.
4. The stream ends with `{event:{end:{exitCode, exited}}}` then a
   `FLAG_END_STREAM` frame. A nonzero exit is a result on the wire, never an
   error frame.

`Process/Connect` semantics (P1 follow-up, decided): k8e's `Process/Start` is
a foreground stream that runs until the process exits (its output rides the
Start stream); `Connect` therefore reconnects to an *already-finished*
process's buffered output — exactly what `/exec/attach` replays. Live tailing
a still-running process from a second consumer would only matter for sandboxd
*background* processes (`/exec/background`), which have their own poll
surface; not a gap for the E2B Connect path. A keepalive decorator
(`keepaliveSubscriber`, mutex-guarded, idle 15 s) keeps long streams alive
through proxy timeouts. PTY stays unsupported: sandboxd has no PTY and the
SDK's `pty.create` path cannot be faked honestly.

## 6. Filesystem surface

K8E's sandboxd originally had `write`/`read`/`list` only — no `stat`, `mkdir`,
`move`, `remove`. KIP-18 **downshifts those four operations into sandboxd**
(native syscalls in the in-pod daemon, no shell, no `Exec` round-trip): new
`/files/stat`, `/files/mkdir` (409 `already exists`), `/files/move` (rename),
`/files/remove` (recursive) endpoints, with **path resolution under
`/workspace`** (E2B's `/home/user/...` default is `/workspace` in K8E; `..`
and absolute escape attempts are rejected). The e2b server calls sandboxd
directly over HTTP (`sandboxdClient`, pod IP from `GetSession` — the same
source the gateway uses), preserving the trust model: the pod IP is not
routable outside the cluster, and the e2b server already holds the session
credential.

| RPC | sandboxd | Notes |
|-----|----------|-------|
| `Stat` | `/files/stat` | `statx` → `EntryInfo` (type, size, mode, uid, gid, mtime) + **`symlink_target`** (readlink, Linux-tested) |
| `ListDir` | `/files/list` | **depth-aware** `EntryInfo` listing (P1 audit item, resolved) |
| `MakeDir` | `/files/mkdir` | existing ⇒ `already_exists` (SDK reads it as `makeDir()===false`); returns `EntryInfo` |
| `Move` | `/files/move` | `rename`; missing source ⇒ `not_found`; **returns a real `EntryInfo`** (stat-after-move) |
| `Remove` | `/files/remove` | recursive delete (missing ⇒ no-op, like `rm -rf`) |
| `WatchDir` | — | **501** (streaming); the SDK's polling trio is what ships (below) |
| `CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` | `/watch/{create,events,remove}` | inotify per-watcher event ring; `GetWatcherEvents` is incremental with a cursor (SDK `WatchHandle` semantics) |

Doing these natively (instead of GNU `stat`/`mkdir`/`mv`/`rm` via a shell)
removes the shell-piping and quoting surface, makes failures precise
(`already exists` / `not found` instead of a mangled `stderr` string), and
does not depend on the sandbox image shipping coreutils.

`/files` upload/download ride `WriteFile`/`ReadFile` (multipart parts carry
the destination path in the filename; octet-stream takes `?path=`; gzip
`Content-Encoding` is decoded before writing, per the SDK's `write(gzip)`).
Downloads get extension-based `Content-Type` and single-range `Range` support
(206 + `Content-Range`).

## 7. Auth and signing

- **Control plane**: `X-API-KEY: e2b_<token>` (or bare `X-API-Key` /
  `Authorization: Bearer`). The `e2b_` prefix is the SDK's convention, not a
  secret; the bare token is compared (constant-time) against the API key(s)
  the e2b server accepts.

  **Embedded k8e-server (default) loads the same `sandbox-apikeys` Secret
  the gRPC gateway uses**, reloading every 30s. `k8e sandbox-apikey create`
  is therefore enough — you do **not** also have to set `--e2b-apikey` for
  official SDK clients to authenticate. The reload keeps the last parsed
  Secret snapshot and **re-evaluates key expiry every tick even if a later
  Secret read/parse fails**, so an expired key never stays authenticated
  just because the Secret became unreadable. Optional extras:

  - `--e2b-apikey` / `K8E_E2B_APIKEY` — extra static token (unioned with the Secret)
  - `K8E_SANDBOX_APIKEY` — fallback when `--e2b-apikey` is empty (same env the CLI uses)
  - standalone `k8e e2b-server --apikey` / `K8E_SANDBOX_APIKEY` still needs the
    token passed explicitly (it is also the gRPC login credential)

  **The key is hex-only — by design.** The official `e2b` SDKs validate the
  key client-side (`validateApiKey`, `/^e2b_[0-9a-f]+$/`) *before* any
  request, so a key containing non-hex characters (e.g. an old `k8e-…`
  token) can never be presented by an unmodified SDK — the SDK throws
  `AuthenticationError` locally. K8E's own key generator
  (`k8e sandbox-apikey create <name>`) therefore emits a bare 64-hex `key`
  **and** an `e2b_key` (`e2b_` + hex) ready to paste into the SDK:

  ```bash
  k8e sandbox-apikey create e2b --ttl never
  # { "key": "<64 hex>", "e2b_key": "e2b_<64 hex>", ... }

  export E2B_API_KEY=$(k8e sandbox-apikey create e2b --ttl never | jq -r .e2b_key)
  ```

  ```ts
  const sbx = await Sandbox.create({
    apiKey: process.env.E2B_API_KEY, // must be e2b_<hex>, not the bare hex
    apiUrl: 'http://127.0.0.1:3676',
    sandboxUrl: 'http://127.0.0.1:3676/e2b/envd',
  });
  ```

  Passing the bare hex token as `apiKey` fails *inside the SDK* (no HTTP
  request is sent). Passing `e2b_` + hex to a server with an empty keyring
  used to 401 even when the Secret held that token; the embedded server now
  accepts Secret keys. The server strips the `e2b_` prefix from both the
  configured key and the presented credential. `k8e e2b-server` warns at
  startup when `--apikey` is not hex-compatible and still starts: a legacy
  `k8e-…` key remains valid for the gRPC gateway login, only SDK clients
  cannot use it — rotate with `k8e sandbox-apikey create`. If no key is
  configured and the Secret is empty/unreadable, the control plane rejects
  every request with 401.
- **envd**: `E2b-Sandbox-Id` header + `X-Access-Token`
  `HMAC(signingSecret, "envd:"+sandboxID)` — stateless verify, minted at
  create and echoed in the session view. The signing secret comes from
  `--signing-secret` / `K8E_E2B_SIGNING_SECRET`, falling back to the server's
  sandbox CA key on the node (stable across restarts), else a random
  per-process key with a warning (tokens die on restart).
- **Signed file URLs** (Dormice-pinned scheme):
  `v1_` + base64(sha256(`path:operation:username:token[:expiration]`)),
  `=` padding stripped. Verified constant-time; the signature itself
  identifies the sandbox (no headers needed — browsers add none).

## 8. Implementation status — what PR #541 ships

### 8.1 Done

- **P1 — sandboxd-owned process table (complete).** `execctl.zig` Entry:
  command snapshot (for `Process/List`) + 64 KiB ring buffer of recent output
  (for attach) + done flag; `GET /exec/processes` returns `{pid, alive,
  config}`; `GET /exec/attach?pid=N` replays the buffered output as SSE;
  `data: {"exit":N}` closes the stream with the exit code (retiring the
  marker-file hack). Pids are the sandbox's own — node-independent.
  `exec.zig` registers with the command, appends output to the ring buffer,
  marks done on reap, and emits the exit frame after `wait4`. The e2b layer:
  `Process/List` reads the sandbox-owned table (cross-node consistent,
  fallback to the local subscriber table); `Process/Connect` falls back to
  `/exec/attach` when the local table has no record; `runProcessStream`
  captures the in-stream exit code (`parseExitFrame`). `wrapWithExitCode` /
  `readExitCode` / marker files removed.
- **P1 follow-ups (decided and shipped):**
  - **`Connect` attach = buffer replay is the correct semantic** (see §5).
  - **`StreamInput` is a permanent 501, documented.** Verified against the
    official SDK sources: neither `packages/python-sdk` nor `packages/js-sdk`
    ever calls `StreamInput` — `send_stdin` goes through the unary
    `SendInput` RPC. Implementing HTTP/2 client_stream in sandboxd (Zig)
    would cost significant work with zero SDK consumers. The 501 hint tells
    clients to use `SendInput`. Custom low-level clients needing streamed
    stdin are out of scope.
  - **Watch trio (shipped).** `watch.zig` implements per-watcher inotify
    event rings with `/watch/create`, `/watch/events` (incremental cursor —
    SDK `WatchHandle` semantics), `/watch/remove`; the e2b layer wires
    `CreateWatcher` / `GetWatcherEvents` / `RemoveWatcher` to them.
    Streaming `WatchDir` remains 501 (the SDK does not use it).
  - **KeepAlive heartbeat (shipped).** `keepaliveSubscriber` (mutex-guarded
    writes, idle 15 s) keeps Start/Connect streams alive through proxy
    timeouts.
  - **P2 semantic details (all shipped):** `Move` returns a real `EntryInfo`
    (stat-after-move); `MakeDir` returns an entry too; `symlink_target`
    implemented (sandboxd readlink + e2b passthrough, Linux-tested);
    `SendSignal` numeric-enum wire confirmed compatible (proto3 JSON enum
    names `SIGNAL_SIGKILL`/`SIGNAL_SIGTERM` accepted); `ListDir` is
    depth-aware.
- **P2 — embedded e2b in k8e-server (complete).** `runEmbeddedE2B`
  (`pkg/server/e2b_embedded.go`) starts the e2b HTTP server inside
  k8e-server after `sandboxmatrix.Register`, dialing the in-process gRPC
  gateway over loopback, with the CRD-backed `crdStateStore` injected. On by
  default; `--disable-e2b` (`K8E_DISABLE_E2B`) or `--disable-sandbox-matrix`
  turns it off; `--e2b-listen` (`K8E_E2B_LISTEN`, default `0.0.0.0:3676`)
  sets the listen address.
- **Multi-node consistency (shipped).** `stateStore` interface with the
  in-memory `sandboxRegistry` and the CRD-backed `crdStateStore`
  (SandboxSession annotation `sandbox.k8e.io/e2b-state`); every node's
  `gcLoop` enforces deadlines with idempotent `PauseSession`/`DestroySession`
  (no leader-gating needed); the process table is sandbox-owned so
  `Process/List`/`Connect` are node-independent. Reconcilers stay
  leader-gated (`leader.go`).
- **Gateway manifests (shipped).** `manifests/sandbox-matrix/e2b-gateway.yaml`:
  HTTPRoute :80/:443 → e2b-server headless Service + Endpoints (host
  `--advertise-address`), TCPRoute :50051 L4 passthrough → headless
  `sandbox-grpc-gateway` (mTLS preserved).
- **`k8e e2b-server` kept as a thin compat wrapper** (same logic, in-memory
  store, documented single-node semantics).

### 8.2 Remaining 501s / honest absences (by design)

| Surface | Status | Why |
|---|---|---|
| PTY: `process.Process/Update`, `UpdatePTY`, SDK `pty.create` | 501 | sandboxd has no PTY; cannot be faked honestly |
| `process.Process/StreamInput` | 501 (**permanent**) | no official SDK calls it (verified); `send_stdin` uses unary `SendInput` |
| `filesystem.Filesystem/WatchDir` (streaming) | 501 | the SDK uses the polling trio, which is shipped |
| `GET /sandboxes/:id/metrics` | `[]` | K8E has no metrics pipeline yet — honest absence |
| xattr `metadata` (`user.e2b.*`) | not returned | no SDK surface depends on it for the supported flows |
| `domain` in the create response | omitted | SDK tolerates absence (`isinstance str` else `None`); k8e's sandboxUrl is explicit |
| `Connect` by tag (`ProcessSelector.tag`) | unimplemented | SDK never sends tag (verified) |
| Basic auth (`Authorization: Basic`) | not needed | k8e claims envd 0.6.1 ≥ 0.4.0, so the SDK never sends Basic (verified) |

## 9. Compatibility notes

**Works with the official SDK out of the box:** create → connect → getInfo →
list → kill → timeout (incl. `-1` NEVER_TIMEOUT) → **pause → resume**
(workspace PVC survives; files intact) → auto-pause at the deadline →
auto-resume on the next request (connect / exec / file I/O), live
`commands.run` streaming, `commands.list`, `commands.connect`, file
write/read/list/stat/rename/makeDir/remove, byte round-trips, signed-URL
download/upload, watch (`watchDir` via the polling trio), `metadata.name`
idempotency.

**Honest 501s (SDK methods throw, with a machine-readable hint):** PTY
(`pty.create`), `process.Process/Update`, `StreamInput`, `UpdatePTY`,
streaming `WatchDir` (the polling trio works), metrics (returns `[]`),
templates registry (only runtime-class names accepted as `templateID`),
pause of an ephemeral (EmptyDir) sandbox (409 — no persistent workspace to
survive the release).

`sendStdin`/`closeStdin` and `sendSignal`/`handle.kill` are **not** in that
list: sandboxd gained a process-control table plus `/exec/stdin`,
`/exec/stdin/close`, `/exec/signal`, and the e2b server wires
`SendInput`/`CloseStdin`/`SendSignal` to them natively, with the in-guest pid
bridged through the first `/exec/stream` frame. The remaining 501s are all
K8E-runtime gaps (no PTY/metrics in sandboxd, `StreamInput` and streaming
`WatchDir` deliberately not implemented — no SDK consumers), not protocol
gaps. Pause/resume requires a **persistent session** (tenant set → workspace
PVC); ephemeral sessions cannot pause without losing their files, so the
refusal is honest (CubeSandbox deletes a paused sandbox without waking it for
the same reason).

## 10. Alternatives considered

- **Extend sandboxd (Zig) to speak envd in-pod** — rejected as a full move:
  the gateway already provides pod-IP resolution, session env/secret
  injection, TLS, and truncation; duplicating that in a second runtime
  surface is more surface for the same protocol. **Adopted partially** as
  "ability downshift" and then as **sandboxd-as-envd for the primitives
  sandboxd should own**: the process table (`/exec/processes`, `/exec/attach`,
  in-stream exit), the native filesystem/process-control endpoints
  (`/files/*`, `/exec/stdin*`, `/exec/signal`), and the watch trio are
  sandboxd's, while protocol translation (auth, envelope, error dialect,
  pause/resume, signed URLs, deadline registry) stays in the e2b layer.
- **Extract the controller + e2b into one standalone Deployment** — the
  earlier direction; rejected in favor of **embedding both in k8e-server**
  with the Gateway API as the only door (§1.5 — single-binary positioning,
  the Gateway already provides ingress, and e2b is not a scale hotspot).
- **A grpc-gateway / Connect-go plugin** — the E2B wire is hand-rolled
  JSON-codec Connect-RPC with a specific envelope; Dormice's measured
  behavior (and its quirks, like `data: ` SSE leakage) is easier to match
  with a small explicit protocol layer than with generated plumbing.
- **Adopt the native gRPC API in the SDKs** — ecosystem agents already speak
  E2B; a compat layer reaches them with zero client changes.

## 11. Test plan

- **sandboxd (Zig):** Connect codec envelope round-trips and frame-boundary
  splitting; process surface (Start/Connect/List/SendInput/SendSignal
  semantics); in-stream exitCode; execctl ring buffer / attach / done;
  watch per-watcher event ring (create/events/remove, incremental cursor);
  files stat/mkdir/move/remove/list (depth, symlink); transcript replay.
  Baseline: **44/44 on Linux** (OrbStack debian runner — the macOS sandbox
  blocks fork tests); cross-compiles for x86_64/aarch64/riscv64-linux-musl.
- **e2b (Go):** existing suite against the sandboxd stub — control plane
  lifecycle, deadline registry + GC, pause/resume, idempotent create, process
  Start/Connect cross-node (attach fallback), watch trio handlers, keepalive,
  signed URLs, error dialect, envd stream parsing. **Full suite + `-race`
  green.**
- **controller:** Go unit tests for the embedded wiring (e2b HTTP server
  starts after `sandboxmatrix.Register`; both listeners on :50051 and
  :3676), loopback gateway dial, `--disable-e2b` skipping, leader election
  still gating reconcilers only.
- **deploy:** manifest assertions (Gateway routes point at the host headless
  Services, TCPRoute passthrough).
- **live cluster** (open item, as before): Gateway programmed, :50051 mTLS
  passthrough, :443 envd Connect, pause/resume through Gateway, official
  `e2b` SDK end-to-end (Dormice's `e2b.test.ts` and CubeSandbox's SDK tests
  are the models).

## 12. Risks / open questions

- **Cert bootstrap**: sandboxd connecting requires the controller to hold the
  sandbox CA. Secret rotation path must be defined (currently host files).
- **Loopback gateway dial**: embedded e2b dials `127.0.0.1:<GRPCPort>` via
  the existing sandbox client (mTLS + LocalAuth). This is a real socket hop
  inside the process, but it reuses the battle-tested client path and keeps a
  clean seam if a future in-process `Gateway` adapter replaces it.
- **PTY / metrics backlog**: sandboxd has no PTY and K8E has no metrics
  pipeline; adding them un-gates the remaining 501 surfaces. `StreamInput`
  and streaming `WatchDir` are deliberately not scheduled (no SDK consumers).
- **Pause with memory retention** (CubeSandbox's full snapshot pause /
  resume) would need a VM snapshot engine (CubeCoW-style); today's pause is
  the honest filesystem-only variant (release pod, keep PVC, cold-boot
  resume).
- **Template registry**: map E2B template names to k8e session images
  (runtime-class names are accepted today).
- **e2e suite**: run the official `e2b` package against a live cluster and
  validate the Cilium Gateway API ingress (Gateway programmed status,
  HTTPRoute → e2b-server Service reachability, HTTPS listener with the
  `sandbox-e2b` TLS secret, envd Connect stream end-to-end through the
  Gateway with ALPN/HTTP2).

## Appendix — Borrowable ideas from CubeSandbox (deep review)

### Purpose

A deep review of [CubeSandbox]'s E2B implementation, run to find ideas the KIP-18
e2b layer (`pkg/sandbox/e2b`, `k8e e2b-server`) does **not** already have.
This document is the borrowable-idea backlog: each item names what CubeSandbox
does, where, why it matters for K8E, and a rough effort. It is intended as
input for follow-up KIPs, not as a commitment to implement everything.

**Verification honesty:** items marked **[verified]** were checked line-by-line
against both the CubeSandbox source and the K8E side during the review; the
rest come from sub-agent reports with file:line references, read in full but
not re-spotted by the reviewer. CubeSandbox code quoted here is Apache-2.0.

### Review scope (five angles)

| Angle | Files read | Items |
|-------|-----------|-------|
| Control-plane lifecycle | `CubeAPI/src/services/sandboxes.rs` (2451 ln), `handlers/sandboxes.rs`, `models/mod.rs`, `docs/guide/lifecycle.md`, templates/snapshots/volumes services | 15 |
| Data-plane proxy | `CubeProxy/lua/*.lua` (rewrite, path_rewrite, sandbox_backend, sandbox_state, backend_cache, header_filter, request_host, log_phase, admin_phase, proxy_registry, utils…) | 12 |
| SDK compat & files API | `sdk/python|node|go/*`, `sdk/files-api-streaming.md`, `web/src/lib/api.ts` | 16 |
| Auth / limits / errors / ops | `middleware/{auth,rate_limit}.rs`, `error/mod.rs`, `logging/*.rs`, `config/mod.rs`, `docs/guide/{authentication,https-and-domain,sandbox-logs}.md` | 15 |
| Extensions & e2e harness | `tests/e2e/sdk_compat/**`, `tests/skills/sdk-e2e-cases/`, troubleshooting docs | 12 |

The backlog below consolidates those 70 findings into **A–F groups** (A
security, B correctness, C capabilities, D ops, E e2e harness, F SDK compat),
merging overlaps (e.g. traffic tokens appear once as A2; connection-pool
reset lives in C5).

**Already borrowed by KIP-18 (excluded from this backlog):** root control-plane
mount (+`/e2b/api` alias), time-layered route timeouts (60/120/240 s),
NEVER_TIMEOUT `-1` with `endAt` omission, fine-grained errors (503+Retry-After,
409, 404), Bearer / `X-API-Key` / `X-API-KEY: e2b_<key>` auth, envd HMAC tokens
(`E2b-Sandbox-Id` + `X-Access-Token`), pause/resume (filesystem-only, keep PVC),
auto-pause at deadline, auto-resume on traffic, native sandboxd fs/process
downshift, process table, honest 501s, metrics `[]`.

**Resolved by PR #541 after this review:** the exit-code marker files are
retired (sandboxd emits in-stream `data: {"exit":N}` frames — F7's "emit
`exitCode` explicitly" recommendation is done), the watch polling trio is
implemented via sandboxd inotify (C4's polling trio, §6), the process table
is sandbox-owned and node-independent (C2's stale-state concern is bounded by
the CRD state store, §1.4), and the deadline registry is persisted through
`stateStore` (the "persist the deadline registry" open item, §4). Items still
open from this backlog are marked as such in their entries; the rest remain
input for follow-up KIPs.

---

### A. Security gaps (do first — real risk)

#### A1. Create-time env-var validation **[verified]** — effort S

CubeSandbox `validate_env_vars` (`services/sandboxes.rs:633-678`) rejects at
create time, each with a 400 naming the exact offending variable:

- name must match `[a-zA-Z_][a-zA-Z0-9_]*` and be ≤ 256 bytes;
- **case-insensitive deny-list** of runtime-loader / path-override names:
  `BASH_ENV, ENV, LD_PRELOAD, LD_AUDIT, LD_LIBRARY_PATH, LD_ORIGIN_PATH,
  DYLD_INSERT_LIBRARIES, DYLD_LIBRARY_PATH, GCONV_PATH, PATH, PYTHONPATH,
  NODE_PATH, JAVA_TOOL_OPTIONS, _JAVA_OPTIONS, GEM_PATH, RUBYOPT, RUBYLIB,
  PERL5LIB, PERLLIB, CLASSPATH, IFS`;
- value ≤ 4096 B, no NUL bytes, no control characters (tab allowed).

**K8E gap (verified):** `normalizeEnvVars` (`pkg/sandbox/e2b/server.go:401`)
copies agent-supplied envVars into the pod verbatim with zero validation.
KIP-18 accepts envVars from untrusted agent code, so `LD_PRELOAD` /
`PYTHONPATH`-style injection is a real sandbox-escape vector. Small,
well-tested, directly portable — the top pick of this whole review.

#### A2. Data-plane traffic access token — effort M

When `network.allow_public_traffic=false`, CubeProxy enforces
`e2b-traffic-access-token` / `cube-traffic-access-token` on host-based,
path-based, and gRPC-ingress routes (`docs/guide/https-and-domain.md:146-149`);
the create response carries it as `trafficAccessToken`
(`models/mod.rs:282-283`). Token is delivered **only on create**, not on
connect/resume — the SDK must persist it.

**K8E gap:** envd HMAC tokens protect the control/envd channel, but a sandbox's
own HTTP service exposed through the gateway is world-readable. A per-sandbox
traffic token secures user-service traffic when public access is disabled.
The 403-vs-404 policy (E2B's `restrict-public-access` contract deliberately
leaks existence; "the sandbox ID space is already unguessable") and
"enforce on cached entries too" are the two decisions to copy.

#### A3. Delegated auth callback with X-Request-Path + X-Request-Method — effort M

When `AUTH_CALLBACK_URL` is set, `unified_auth` (`middleware/auth.rs:79-182`)
extracts the credential (Bearer beats X-API-Key) and POSTs it to an external
endpoint, forwarding `X-Request-Path` and `X-Request-Method`; HTTP 200 ⇒ allow,
any other status ⇒ 401, callback unreachable ⇒ 500. The docs warn that the
same path serves GET/POST/DELETE/PATCH, so a path-only whitelist lets a
read-only key escalate to delete; regression tests assert GET vs DELETE on the
same path arrive as distinct `X-Request-Method` values (`auth.rs:250-288`).

**K8E gap:** k8e's constant-time key compare is static-key-only. A callback
hook is a standards-free extension point for enterprise IdP/key-store
integration, and the method-scoping lesson (read key must not kill sandboxes)
is a vulnerability class to defend from day one.

---

### B. Correctness / compatibility (cheap, high value)

#### B1. Resume dedupe + distinct mid-pause/killed states **[verified]** — effort S

`sandbox_state.lua` gate: `pausing` ⇒ 503 + `Retry-After: 2` (don't race an
in-flight pause); `killing`/`killed` ⇒ 410; `paused` ⇒ fire an internal resume
sub-request that **blocks the data-plane request until the sandbox is alive**,
then optimistically set the local state to `running` — "a burst of concurrent
requests that arrived during the pause don't all launch their own resume
sub-requests". Unknown states pass through with a one-time log (fail-open).

**K8E gap (verified):** `wakeForTraffic` (`gateway.go:188`) distinguishes only
paused/dead; every concurrent request fires its own `ResumeSession` gRPC, and
there is no `pausing`/`killing` gate. k8e's state machine has no
resumed-but-stale-pod-IP purge either (see C2).

#### B2. Lifecycle request-shape tolerance — effort S

Accept nested `lifecycle{onTimeout,autoResume}` **and** flattened
`autoPause`/`autoResume`; `autoResume` accepts bare bool or `{enabled:true}`;
snake_case aliases (`on_timeout`, `auto_resume`, `envs`, `distribution_scope`)
all deserialize; nested wins over flattened (`models/mod.rs:129-171,197-256`,
`resolve_lifecycle_flags` at `services/sandboxes.rs:1020-1036`).

**K8E gap:** k8e parses only a flat `autoPause` bool. Different e2b SDK
releases and hand-rolled clients send different shapes; accepting all of them
with a precedence rule beats 400ing.

#### B3. Refresh endpoint: relative deadline extension — effort S

`POST /sandboxes/:id/refreshes {duration: 0..=3600}` extends the idle deadline
by N seconds (vs `set-timeout`, which sets an absolute TTL); 204 on success,
400 out of range (`handlers/sandboxes.rs:538-570`,
`services/sandboxes.rs:496-515`).

**K8E gap:** k8e has absolute `/timeout` but no relative extend. "Give me N
more seconds" is the standard agent keep-alive idiom without knowing the
current TTL.

#### B4. Per-failure-kind Retry-After + precise-quota 409 pass-through — effort S

Backend `130490` (pausing) ⇒ 503 `Retry-After: 2`; `130589` (resume-before-
delete failed) ⇒ 503 `Retry-After: 5`; resume capacity rejection ⇒ 409 whose
body is the backend's exact message ("resume rejected by
paused_resource_release_ratio policy: need 1024MB > quota 512MB")
(`services/sandboxes.rs:27-32,719-762`).

**K8E gap:** k8e hardcodes `Retry-After: 2` for all lifecycle conflicts
(`gwerror.go:80`). Different retry hints per cause let SDKs back off
correctly; echoing precise quota numbers turns a 409 into a usable capacity
diagnostic.

#### B5. Session-view field derivation rules — effort S

`startedAt` fallback chain (explicit `started_at` → `create_at` → now); `endAt`
omitted for never-timeout; `envdVersion` read from an annotation with a
conservative fallback; `pausing` as a distinct list state; `volumeMounts`
(name/path/readOnly) in **both** list and detail (`services/sandboxes.rs:
118-152,825-853`, `models/mod.rs:292-357`).

**K8E:** k8e hardcodes `envdVersion`; deriving from session annotations (or a
`--envd-version` flag) is the honest variant.

#### B6. Opaque pagination cursors + empty-token normalization — effort S

List default limit 100 (v1 passes 200), max enforced; empty/whitespace
`nextToken` normalized to None, because "an empty next_token= query restarts
pagination silently in some builds" (`models/mod.rs:560-574`,
`services/snapshots.rs:74-106`).

**K8E gap:** k8e's `nextToken` is a numeric offset (`control.go:362`).
Opaque-cursor semantics + explicit caps prevent silent restarts.

#### B7. Streaming-route hygiene: never gzip/intercept Connect streams — effort S

Streaming locations set `proxy_buffering off; gzip off;
proxy_intercept_errors off` with the rationale: "iter_raw() does not
decompress Content-Encoding — gzip magic bytes misread as a 2.17 GiB Connect
frame header", and "streaming envd errors must pass through verbatim;
intercepting them would replace protocol-native error frames with nginx error
pages". Applied only to a verified allowlist
(`process.Process/(Start|Connect)|filesystem.Filesystem/WatchDir`) with 7206 s
read/send timeouts.

**K8E:** k8e writes raw Connect frames over hijacked conns; any middleware or
reverse proxy in front that compresses or intercepts corrupts the 5-byte frame
header exactly as described. A regression guard + explicit auditable list of
which routes are streaming is the borrowable artifact.

#### B8. Kill a paused sandbox without waking it — effort S

DELETE of a paused sandbox removes the tombstone directly: no resume-before-
delete, no capacity admission, no idle-timeout reset, no volume re-attach
(`docs/guide/lifecycle.md:111-130`).

**K8E:** killing a paused session shouldn't need to revive the pod — that
burns resume churn and can fail on capacity. Borrow the delete-without-wake
path and its "this is not a resume" invariants.

---

### C. New capabilities

#### C1. Idle-driven auto-pause via per-sandbox last-active feed **[verified]** — effort M

`log_phase.lua` stamps a per-sandbox "last active" timestamp into a shared
dict on every request, with **sub-second write coalescing** (`if now_ms - prev
>= 1000` — a 1k-QPS sandbox costs 1 dict write/sec, not 1k); the sidecar pulls
it incrementally via `/admin/last_active?since=<unix_ms>` for idle decisions.
Dict-full LRU eviction is surfaced as a WARN. This is the entire input to
CubeSandbox's auto-pause.

**K8E gap:** k8e's auto-pause is deadline-driven only (registry `gcExpired`).
The e2b-server is the single chokepoint that sees every request; a coalesced
activity timestamp per sandbox, pullable with `?since=`, is what turns
E2B-style idle reclamation into a real k8e capability. Composes with k8e's
existing pause/resume RPCs.

#### C2. Pod-IP resolution cache with jittered TTL + negative-cache sentinel **[verified]** — effort M

`sandbox_backend.lua` caches (sandbox, port) → backend address with randomized
TTL (`math.random(timeout_min, timeout_max)` — avoids synchronized stampedes
across proxies); optional fields cached as explicit `false` because "ngx dict
returns nil both for 'not cached' and 'evicted'"; a `meta_cached` marker is
written first so dependent entries can never outlive it; **the cache-hit path
still re-runs token enforcement** — "a single warm entry would let
unauthenticated callers bypass the gate for the whole cache TTL".

**K8E gap (verified):** `sandboxdClient.podIP` (`sandboxd.go:43`) does a fresh
`GetSession` gRPC round-trip on **every** filesystem/stdin/signal call.
Caching the pod IP per sandbox with jittered TTL + an explicit not-found
sentinel cuts a gateway round-trip out of every FS op. Companion invariant
(A4): destroy/resume must cascade-evict every keyed entry (pod-IP cache,
lastErr, activity), or a recreated same-ID sandbox serves stale state —
k8e's `registry.del` only removes the in-memory row today.

#### C3. Sandbox logs endpoint (v1/v2 shapes) — effort M

v1 returns `{logs:[{timestamp,line}], logEntries:[{timestamp,message,level,
fields}]}`; v2 is cursor+limit+direction with level mapping (debug/warn/error,
info fallback); when the backend endpoint is missing it returns a synthetic
"(log streaming pending…)" entry with 200 instead of erroring
(`services/sandboxes.rs:396-473`).

**K8E:** no logs route. The v1/v2 shape pair matches both old and new e2b
SDKs. **Do not copy** the 200-with-placeholder fallback — k8e's honest-501
philosophy should answer 501 instead of masking a missing backend.

#### C4. WatchDir filesystem event stream — effort M

`files.watch_dir(path)` returns a Watcher iterable yielding fs events
(name/type) pushed from a long-lived HTTP stream framed with the 5-byte
Connect envelope (flag + BE32 length), end-of-stream flag 0x02 carrying an
error trailer, 64 MB frame cap, close via context manager / AbortController
(`sdk/python/cubesandbox/_filesystem.py`, `sdk/node/src/filesystem.ts`,
`sdk/go/envd.go`).

**K8E:** 501 today. sandboxd could expose
`/filesystem.Filesystem/WatchDir` directly, reusing the envd wire protocol
KIP-18 already speaks — push-based fs events instead of polling.

#### C5. Snapshot / rollback / clone surface — effort L

Snapshot create reuses the template's stored create_request JSON (falling back
to a minimal payload built from annotations); refuses non-READY statuses
(`ensure_snapshot_ready`); rollback returns operationID+status only after a
terminal READY state; deletes run synchronously under the 240 s long-route
timeout. Clone fan-out is all-or-nothing (kill siblings on any failure) with a
refcounted temp-snapshot cleanup released when the last clone dies. Rollback
**resets data-plane connection pools** — "the rollback restarts the sandbox
process and old keep-alive sockets point at a torn-down kernel".

**K8E:** CLI-level snapshot save/restore exists but no e2b-facing surface.
Ready-gating, create-request reuse, synchronous-with-long-timeout, and the
connection-invalidation step are the right shape for a KIP-18
snapshot/rollback endpoint (aligns with KIP-10).

#### C6. Template alias derivation + canonical-ID prefix guard — effort S

Name→alias derivation: strip tag after last `:` only when no `/` follows
(registry-port aware: `registry:5000/team/app:v2` → `app`), take the last path
component, enforce `^[a-z0-9][a-z0-9-]{0,63}$`, and **reject `tpl-`/`snap-`
prefixes** so an alias can never collide with a canonical ID
(`services/templates.rs:104-124,336-392`).

**K8E:** e2b references templates by ID; e2b SDKs reference by name/alias. The
derivation rules + prefix guard are the precise compat behavior KIP-18's
template-registry open item needs.

#### C7. paused_resource_release_ratio — effort L

Node-level knob `[0,1]` (default 0, zero-value-safe) releasing a fraction of
paused sandbox CPU/mem quota back to the scheduler, with a local admission
check on every resume that 409s with exact numbers when capacity is short;
ratio=0 keeps resume always-guaranteed. Docs: "Reserved quota still counts
toward scheduler CPU/memory usage, so pause-heavy nodes are naturally
deprioritized" (`docs/guide/lifecycle.md:227-262`).

**K8E:** filesystem-only pause keeps the pod, so its resource requests still
consume node allocatable — the same "paused sandboxes eat scheduling quota"
problem. A tunable ratio (or scale-down) with admission-checked resume is the
direct K8s analog and the biggest density win in this review.

#### C8. Traffic-token / sandbox-exposed services (see A2) — effort M

#### C9. Plaintext gRPC ingress with HTTP-status → google.rpc.Code mapping — effort L

nginx `:9090` server proxies native gRPC (`$cube_ingress_protocol = "grpc"`);
`utils.lua` maps `[400]=3, [403]=7, [404]=5, [410]=9, [503]=14` and answers
errors as HTTP 200 + `grpc-status`/`grpc-message` trailers via an internal
location, because "ngx.exit from rewrite produces a DATA END_STREAM without
trailers that grpcio rejects". Clients set `:authority` to
`<container_port>-<sandbox_id>` (no DNS).

**K8E:** the envd surface is JSON-Connect only (`useBinaryFormat: false`);
the E2B envd services are gRPC-defined and the official SDK can use
gRPC/binary-Connect transport — a compat gap. If k8e adds a gRPC listener,
the status-code table and the trailer-vs-DATA-END_STREAM discipline are the
exact missing machinery.

#### C10. maskRequestHost: preserve Host on the envd port, template-rewrite others — effort S

`request_host.lua`: `ENVD_PORT = 49983`; if the port is the envd port, return
the original Host unchanged — "The envd data plane keeps its original Host,
matching E2B's maskRequestHost contract". Other ports render a per-sandbox
stored template replacing `${PORT}`, and stamp `X-Forwarded-Host` with the
original host when rewritten.

**K8E:** k8e re-originates requests so Host never flows to sandboxd, but the
E2B contract is that the envd port's traffic is Host-transparent while other
container ports may be masked. Documents why k8e's sandboxUrl-based identity
must stay host-agnostic.

#### C11. Content-Length inheritance trap for internal sub-requests — effort S

`sandbox_state.lua` passes `body = ""` to `ngx.location.capture`: without it,
capture reuses the parent's Content-Length (e.g. "112" from a POST /execute),
then the location's `proxy_pass_request_body off` strips the body but leaves
the inherited Content-Length — the Go http.Server blocks reading the promised
bytes that never arrive, parking the connection until keepalive timeout.

**K8E:** this is literally a Go net/http behavior. If the e2b server ever
issues an internal HTTP call while a request body is in flight (an in-process
resume hook, a control-plane callback), the rule is: always neutralize
Content-Length on internal sub-requests.

#### C12. Loopback admin surface with incremental since-pagination — effort M

A `127.0.0.1` admin server (`admin_phase.lua` + nginx `8082`) exposes
`/admin/last_active?since=`, `/admin/state`, `/admin/meta`, cache invalidation,
and dict `free_space()`/`heartbeat_last_pushed_ms`, gated by an optional
`X-Cube-Admin-Token` (mismatch → 403).

**K8E:** the e2b-server has no management surface: /health is static, and the
control plane cannot push state into or pull activity from a running server.
A loopback admin listener (shared-token gated) with incremental
since-pagination is the clean pattern for injecting per-sandbox metadata,
forcing cache invalidation, and polling the last-active feed (C1).

#### C13. Self-registered heartbeat with self-healing re-push — effort S

`proxy_registry.lua` publishes itself to Redis every tick (single-writer:
worker 0 only); on any Redis failure it flips `registry_pushed = false` —
"Consider the registry row stale too; force a re-push on next tick so we
recover cleanly after Redis flushed its state" — so a heartbeat alone never
claims a dead registry row. First publish uses `ngx.timer.at(0, ...)` because
"cosockets are disabled during init_worker_by_lua* itself".

**K8E:** if the e2b-server ever runs multi-instance (per-node), none currently
self-announces. Borrow: single-writer publishing, and the "force re-register
on error so heartbeat and registry row can't diverge" self-heal.

#### C14. Path-based proxy routing with Location/cookie-path rewriting — effort M

`path_rewrite_phase.lua` parses `/sandbox/<id>/<port>(/<rest>)?`, strips the
prefix upstream, and adds `X-Forwarded-Prefix /sandbox/$ins_id/$container_port`
plus `proxy_redirect` and `proxy_cookie_path` rewrites so apps emitting
root-relative Location headers and cookie paths stay scoped under the sandbox
prefix.

**K8E:** the complete recipe if k8e ever exposes a proxy door into a sandbox's
own web server — three one-liners that eliminate the "redirect escapes the
sandbox URL" bug class.

---

### D. Ops / observability

#### D1. X-Request-ID generation + downstream propagation **[verified]** — effort S

`SetRequestIdLayer::x_request_id(MakeRequestUuid)` stamps every request;
CubeAPI forwards its own `request_id` as a query param to CubeMaster
(`routes.rs:231`, `cubemaster/mod.rs:222`).

**K8E gap (verified):** no request-id anywhere in `pkg/sandbox/e2b` (grep
confirms). An edge-generated ID echoed on responses and injected into log
fields lets one agent call be traced across e2b-server → gateway gRPC →
sandboxd :2024, and dedupes concurrent auto-resumes.

#### D2. Per-credential keyed rate limiting + anonymous fallback bucket — effort S

`RateLimiter::keyed(Quota::per_second(...))` keyed on the `X-API-Key` value
(fallback key `anonymous` when absent); LRU-bounded key set so unbounded key
cardinality can't OOM; 429 `{code,message}`; applied only when auth is
configured (`middleware/rate_limit.rs:16-35`, `routes.rs:214-226`).

**K8E:** has 503+Retry-After for backend overload but no per-tenant 429 — one
runaway agent loop can starve the control plane. Keying on credential keeps
NAT'd agents fair. **Improvement over CubeSandbox:** k8e already sets
Retry-After for 503 — reuse it on the 429 too (CubeSandbox's 429 omits it).

#### D3. Structured event-log dialect: named events + flat fields — effort S

`LogEvent = {timestamp, level, event, #[serde(flatten)] fields}` with
domain-verb event names; handlers emit a Debug `api.request` event (handler,
sandbox_id, limit) and an Info `api.response` event (count), so every API call
is a request/response pair (`logging/mod.rs:63-99`, `handlers/sandboxes.rs:
396-419`).

**K8E:** adopt "event name + flat fields" for the e2b server's log output —
grep-able, metric-able, and request/response pairing makes tailing one agent's
session trivial.

#### D4. Non-blocking async rolling file logger + flush on shutdown — effort M

`FileLogger` sends into an mpsc channel from `log()` (never blocks the request
path), background writer persists, UTC-midnight daily rotation,
`flush()` round-trips a oneshot awaited on graceful shutdown
(`logging/file.rs:44-148`, `main.rs:239-244`).

**K8E:** the e2b server must never let disk waits stall sandbox RPCs, and
draining logs on SIGTERM prevents losing a session's tail.

#### D5. Pluggable log backends: trait + MultiLogger + FilteredLogger — effort S

`trait Logger { log(); flush(); name() }`; MultiLogger fans out concurrently;
FilteredLogger drops below a min level; NoopLogger for tests; composed at
startup as `Filtered(Multi(file))` with `--debug` flipping the min level
(`logging/mod.rs:121-142`, `multi.rs`, `filtered.rs`).

#### D6. OTLP exporter as LogRecord backend (events ≠ trace spans) — effort M

`OtlpLogger` converts LogEvent → OTLP LogRecord via a tonic exporter; the
module argues tracing-subscriber exports spans while LogEvent is an
application event, so events ship as LogRecords (`logging/otlp.rs:5-32`).
**Stub in CubeSandbox — borrow the contract and rationale, not code.**

#### D7. HTTP webhook log backend with batch + flush-interval batching — effort M

`HttpLoggerConfig { url, batch_size=100, flush_interval_secs=5 }`: POSTs
`{"events": [...]}` when the buffer fills or a ticker fires
(`logging/http.rs:17-49`). Also a stub — borrow the contract.

#### D8. Unauthenticated /health with cheap operational payload — effort S

GET /health mounted outside the auth-wrapped router; returns
`{status: "ok", sandboxes: <count>}` (`routes.rs:72`, `handlers/health.rs`).
**Don't copy the hardcoded `sandboxes: 0` stub — wire a real counter.**

#### D9. Config env-var conventions: per-field docs, prefix, `__` nesting — effort S

Every `ServerConfig` field's doc comment names its env var and default
(`CUBE_API_BIND`, `CUBE_MASTER_ADDR`, `RATE_LIMIT_PER_SEC`…); built via
`config::Environment::default().separator("__")`; dotenv loads `.env`;
precedence CLI > env > default (`config/mod.rs:8-121`).

**K8E:** already bridges CLI/env via configfilearg; adopting an explicit
`K8E_E2B_` prefix + `__` nesting + documented-per-field env vars makes the
e2b server operator-friendly.

---

### E. Test infrastructure (highest leverage for the open-item e2e suite)

#### E1. Backend-neutral adapter DSL drives the official e2b SDK — effort M

Cases import only a shared `SandboxAdapter` (info/run_command/run_code/
write_file/read_file/pause/resume_or_connect/get_host/traffic_access_token/
kill/close); each backend is a thin adapter. `E2BAdapter` wraps the official
e2b Python SDK (`import e2b_code_interpreter` then `e2b` fallback), renames
`env_vars`→`envs`, introspects `_accepts_keyword` for SDK-version tolerance,
catches `CommandExitException` into `CommandResult(exit_code)`, normalizes
dataclass-vs-dict info and `sandboxID`/`sandbox_id`/`id` aliases, and pages
`Sandbox.list()` with a REST fallback. Same case file is parametrized over
`SDK_E2E_BACKENDS` via `pytest_generate_tests`
(`tests/e2e/sdk_compat/adapters/*`, `conftest.py`).

**K8E:** the open item names Dormice's `e2b.test.ts` and CubeSandbox's SDK
tests as models. A thin adapter keeps the case suite stable as e2b SDK
versions drift and lets the same cases later run against Dormice/real E2B as
a reference oracle.

#### E2. Canonical capability map: skip unsupported surfaces, don't fail — effort S

`BACKEND_CAPABILITIES` is the single source of truth; `requires_capability`
markers evaluate in the `sdk_sandbox` fixture and `pytest.skip` with the
capability name when absent; unknown backends resolve to empty. Domains:
lifecycle, commands, filesystem, run_code, pause_resume, network_*,
platform_lifecycle, host_mount, volume_plugin, auth_simple_key.

**K8E:** KIP-18 defines honest 501s (PTY, watch, metrics, template registry,
ephemeral pause). A capability map turns every 501 surface into an
explainable skip ("backend k8e does not support capability pty") and makes
the honest-501 contract continuously auditable — when sandboxd gains a
capability, flipping one set bit un-gates the cases.

#### E3. Hermetic-by-default gates: `--run-e2e` opt-in — effort S

Without `--run-e2e`, `pytest_collection_modifyitems` marks every non-`framework`
test skipped; only pure-logic unit tests marked `framework` run. The PR gate
stays hermetic; live runs need the explicit flag. Also a `.env` auto-loader
with shell-export precedence.

**K8E:** can land a live e2b e2e suite without breaking `go test ./...` /
`zig build test`; the suite's own logic (error classification, backoff) still
runs offline.

#### E4. Session preflight: aggregate checks, fail fast with one diagnostic — effort S

Session-scoped preflight checks template presence + ready-like status,
`/health` reachability, SDK deps importable, API key presence; aggregates all
errors then `pytest.exit(returncode=2)`; failures carry actionable hints
(volume misconfigured → deploy-guide URL; auth "if this is 200 the server
likely was not started with CUBE_API_KEY"; platform-lifecycle probe of
`heartbeat_last_pushed_ms`).

#### E5. Sanitized operation trace + JSONL events; auto-dump on failure — effort M

`TracingSandboxAdapter` wraps every op into a bounded 100-event deque with
timestamp, sanitized input/output, duration, success; `sanitize()` redacts
token/secret/api_key keys and Authorization Bearer, truncates >2048 chars,
records file contents as length only; on failure the last ~10 ops print and
`test_result` JSONL events carry error + sandbox_info + trace. Reporter is an
independent redaction boundary.

**K8E:** e2b protocol bugs (SSE framing, exit-code markers, signed URLs, error
dialect, pause/resume) need "what was the last SDK call and what came back" —
the debugging backbone Go unit tests lack.

#### E6. Assert the wire dialect and terminal semantics, not just happy paths — effort M

The adapter normalizes E2B's raise-on-nonzero-exit into
`CommandResult(exit_code)` so tests assert exit 127 / non-zero / timeout;
killed-sandbox tests assert connect+command fail and match terminal markers
(404/terminated/killed/does-not-exist/already) via `is_terminal_failure`;
network probes distinguish REJECT vs DROP; volume tests assert delete-while-
bound 409 then 204 after unbind.

**K8E:** the error dialect (404 destroyed, 409 conflict, 503+Retry-After
mid-lifecycle, 501 unimplemented, `{code,message}` bodies) is the core
contract — e2e must assert both HTTP statuses and SDK-visible behavior
(`kill()===false` on second kill, `makeDir()===false` on existing dir):
"fail for the right product bug".

#### E7. Priority ladder + env-gated markers define runnable scopes — effort S

smoke/p0/p1/p2/p3/slow priority markers; env-gated `requires_cubeproxy`,
`volume`, `requires_internet`; README documents exact per-scope commands
(PR gate: `-m 'smoke or p0'` single backend; daily: `-m 'p0 or p1'` dual
backend; slow lifecycle: `-m 'p1 and slow'`). New env vars must land in
`env.example` + README.

#### E8. Per-test sandbox ownership + robust teardown — effort M

Each test creates its own sandbox; teardown `safe_kill`: info() state check,
resume-or-connect if paused, kill(), close(), then REST DELETE fallback;
returns a diagnostic list instead of raising so teardown never hides the
original failure; `SDK_E2E_KEEP_SANDBOX_ON_FAILURE=true` preserves only failed
setup/call sandboxes.

**K8E:** leaked sandbox pods are the classic live-e2e failure; with KIP-18's
pause/resume and deadline registry, teardown must wake paused sessions before
destroy and fall back to gateway `DestroySession` when the HTTP path fails.

#### E9. Seed-state-then-verify for lifecycle; control-plane ≠ data-plane readiness — effort S

Lifecycle cases seed a kernel variable + checkpoint file, perform the
transition (pause/resume, auto-pause/auto-resume, reentrant resume), then
verify both ("value + 1 == 42" and file contents intact);
`wait_until_data_plane_ready` probes `true` exit instead of trusting
`running`; design rule: no fixed sleeps, backoff-polling wait helpers with
last-observed diagnostics.

#### E10. Hermetic offline tests pin harness logic to the real wire messages — effort S

`test_create_retry.py` unit-tests capacity-error classification against the
exact wire string traced through source ("CubeMaster returned error code
130597: no more resource"), the SDK ApiError shape (numeric code lost →
detection must use message markers, with code-word-boundary tests), and
backoff growth/cap/budget with monkeypatched sleep/random.

**K8E:** pinning classification to the exact strings k8e's server emits keeps
server and harness honest, and is useful today before any live cluster exists.

#### E11. Provision-on-demand platform resources + raw REST probes — effort M

Module fixtures build templates at test time (build → poll READY → delete-
with-retry in finally); a small REST ApiClient drives volume CRUD, 404-after-
delete, delete-while-bound 409 → unbind 204 with retry-wait helpers,
per-sandbox read-only enforcement, template alias lifecycle, and an auth
401-enforcement probe that decides skip.

#### E12. CI wiring: hermetic tests in the PR gate, live e2e on-demand — effort M

Per-SDK hermetic tests via one shared runner script (single source of truth
for CI and local); the live suite deliberately wired into no workflow — runs
manually/on-demand with fully documented env; a written matrix says which
scope runs where (offline, smoke/P0, nightly P1, slow lifecycle).

---

### F. SDK compatibility details (KIP-18 SDKs + wire-format tolerance)

#### F1. Extension surface through the e2b `metadata` map — effort S

CubeSandbox ships a Cube-only feature (host bind-mounts) inside the standard
`Sandbox.create(metadata={...})` dict, so it works with the **unmodified
official e2b SDK** — e2b forwards metadata verbatim; the server interprets the
`host-mount` JSON value (`docs/guide/persistent-storage.md`,
`sdk/python/cubesandbox/sandbox.py create(metadata=...)`).

**K8E:** this is the pattern to standardize on for the whole KIP-18 extension
surface: any k8e-specific capability (volume pins, sandboxd fs-op flags,
network policy) rides `metadata` + create-response fields
(`envdAccessToken`, `trafficAccessToken`) and e2b-named headers
(`e2b-traffic-access-token`, `X-Access-Token`), keeping "official SDK
unmodified" while still shipping k8e-only features.

#### F2. Upload-encoding fallback: octet-stream first, multipart retry on 4xx — effort S

All three SDKs POST /files as `application/octet-stream` first; if envd
rejects it (≥400), they transparently retry as multipart/form-data (field
`file`, filename=path) before surfacing an error — because older envd only
accepts multipart (`sdk/python/cubesandbox/_filesystem.py write()`,
`sdk/node/src/filesystem.ts`, `sdk/go/envd.go writeFile()`).

**K8E:** k8e supports both encodings but the SDK-side retry-on-rejection
semantics are the borrowable behavior; the documented 6-step /files 502 debug
checklist is a ready-made ops playbook.

#### F3. Dual timeout: reset-on-chunk idle abort + server-side hard deadline header — effort S

The SDK implements an inactivity timeout (timer resets on every received
chunk, so a long active stream is never killed) AND sends `Connect-Timeout-Ms`
so envd enforces a hard wall-clock deadline server-side
(`sdk/node/src/stream.ts createIdleTimeout`, `sdk/python/cubesandbox/
_commands.py`).

**K8E:** honoring the deadline header inside sandboxd (server-side
enforcement) plus reset-on-chunk idle aborts in k8e SDKs is the correct
semantics for agent workloads that print slowly for a long time — a hard
total deadline would kill legitimate long jobs.

#### F4. Never buffer stream-backed request bodies in a transport rewrite — effort S

CubeSandbox's IPOverrideTransport rebuilt requests by reading
`request.content`, which raises `httpx.RequestNotRead` for multipart/file-
object bodies; the fix preserves original stream semantics
(`sdk/files-api-streaming.md`, `sdk/python/cubesandbox/_transport.py`).

**K8E:** if k8e's gateway/SDK rewrites requests (e.g. proxy-IP override),
buffering stream bodies breaks large multipart uploads — "preserve stream
semantics; never force body into memory".

#### F5. Split control-plane vs data-plane clients with different timeout policies — effort S

Two HTTP stacks: control-plane with an overall 30 s timeout that fails fast;
data-plane with no overall deadline, connect/keepalive tuning, and a
DNS-bypass transport that dials a fixed proxy IP while preserving the virtual
Host header (and pinning SNI for TLS) — `curl --resolve` semantics
(`sdk/go/transport.go`, `sdk/node/src/transport.ts`,
`sdk/python/cubesandbox/_transport.py`).

**K8E:** the virtual-host-with-IP-dial trick is how a k8e sandbox gateway can
let e2b SDKs (which resolve `<port>-<sandboxID>.<domain>`) reach sandboxd
pods through a fixed gateway IP with Host-header routing.

#### F6. Batch file write with partial-failure reporting — effort S

`write_files(files)` writes a list of (path, data) pairs, stops at the first
error, and throws `PartialWriteError` carrying how many files were written
before failure (`sdk/python/cubesandbox/_filesystem.py write_files`,
`WriteFiles` in Go/Node).

**K8E:** multi-file upload with accurate progress-on-failure instead of an
opaque "write failed" — useful for agent workspace seeding.

#### F7. Exit-code recovery from proto3 JSON status strings — effort S

envd serializes process end events as proto3 JSON, which omits a zero-valued
`exitCode` field entirely; SDKs recover the code from status strings
('exit status 0', 'signal N' → 128+N, 'exited' → 0) and from both
`exitCode`/`exit_code` spellings (`sdk/go/envd.go processEndEvent.exitCode()`,
`sdk/python/cubesandbox/_commands.py`, `sdk/node/src/commands.ts`).

**K8E:** if k8e's sandboxd/process API emits Connect events with proto3-style
JSON (or any JSON that omits default values), clients will misread exit 0 as
"no exit code". **Resolved in PR #541:** sandboxd emits the exit code
explicitly (`data: {"exit":N}` in-stream frame; the marker file is retired,
§5) — keep the tolerant parser on the SDK side for real envd servers.

#### F8. Config/env compat: `CUBE_*` with `E2B_*` fallback + omit-when-None — effort S

Config reads `CUBE_*` env vars with `E2B_*` fallback (Go
`firstEnv('CUBE_API_URL','E2B_API_URL')`); optional create fields are OMITTED
when None so the server keeps its own defaults; lifecycle enums validated
client-side into clean errors; duplicate aliases (`env_vars` vs `envs`) must
match when both given (`sdk/go/config.go`, `sdk/node/src/config.ts`,
`sdk/python/cubesandbox/_config.py`).

**K8E:** for agents already configured for e2b, accepting `E2B_*` env names
gives zero-config drop-in.

#### F9. Volume mounting: e2b-shaped Volume API + per-attachment read_only — effort M

Volume mirrors E2B exactly (create/connect/list/get_info/destroy,
`volume_mounts={path: volume}`) with a VolumeMount wrapper for per-attachment
read_only; client-side validation turns opaque 4xx into clean errors; e2b keys
mounts by `volume.name` → SDK resolves to backend volumeID; tokens only on
create/get-single (never in list), masked in repr/logs
(`sdk/python/cubesandbox/_volume.py`, `sdk/go/models.go`).

#### F10. Host-mount path-restriction model — effort M

hostPath must be under an allow-listed prefix (default `/data/shared/`,
configurable; `/` forbidden at startup); `filepath.Clean` resolves `..`;
trailing-slash prefix matching prevents prefix spoofing (`/data/shared_evil`
rejected); validation at CREATE time; readOnly via MS_RDONLY does NOT override
POSIX perms (UID must match); nested mounts give "shared read-only parent +
per-agent writable child" with deterministic parent-before-child application
order (`docs/guide/persistent-storage.md`).

**K8E:** k8e is built for secure multi-tenant agent fleets; this is a
complete, proven policy for hostPath/volume mounting, mapping directly onto
agent team workspaces.

---


### Do-not-copy list (CubeSandbox defects)

1. **429 without Retry-After** — only 503 carries it; k8e should set it on
   both.
2. **Empty-credential passthrough** (`auth.rs:459-463`) — a footgun; k8e keeps
   no-credential = 401.
3. **200-with-placeholder for missing log backend** — masks errors; k8e's
   honest-501 philosophy should answer 501.
4. **Hardcoded `sandboxes: 0` in /health** — wire a real counter.

### Recommended landing order

| Batch | Items | Why |
|-------|-------|-----|
| Immediate (S) | A1 env validation, B1 resume dedupe, B2 lifecycle shape tolerance, B3 refresh, B6 cursor pagination, D1 X-Request-ID, D2 per-key 429, F7 exit-code recovery | security + correctness, small and independent |
| Short (M) | C2 pod-IP cache + cascade invalidation, C1 idle auto-pause, A2 traffic token, B8 delete-without-wake, B7 streaming-route guard, C12 admin surface | new behavior/density, single-module |
| Medium (L) | A3 auth callback, E1/E2 e2e harness + capability map, C5 snapshot/rollback/clone, C3 logs endpoint, C9 gRPC ingress | cross-layer |
| Long (L) | C7 paused_resource_release_ratio | resource-layer, biggest economic upside |


## References

- **KIP-18 (this document)** — architecture: embedded e2b + embedded
  sandbox-matrix controller behind the Cilium Gateway API; sandboxd-as-envd.
- Dormice — `packages/server/src/e2b/{index,control,protocol,envd,process-table,signing}.ts`,
  `packages/server/src/e2b/envd/{process,files,filesystem,watch,shared}.ts`,
  `packages/server/src/e2b/compat.test.ts`,
  `e2e/src/e2b.test.ts` (official-SDK black-box suite).
- E2B — `spec/envd/process/process.proto`,
  `spec/envd/filesystem/filesystem.proto`,
  `packages/python-sdk/e2b/envd/{process,filesystem}/*_connect.py`
  (connectrpc Endpoint wiring), `packages/python-sdk/e2b/envd/utils.py`
  (Basic auth `authentication_header`), `packages/python-sdk/e2b/envd/
  versions.py` (ENVD_DEFAULT_USER threshold).
- K8E — `proto/sandbox/v1/sandbox.proto`,
  `pkg/sandboxmatrix/grpc/{server,orchestrator,env}.go`,
  `pkg/sandboxmatrix/{controller,leader}.go`,
  `pkg/sandbox/e2b/{server,control,envd,files,process,registry,sandboxd,
  gateway,protocol,gwerror,state_store,crd_state}.go`,
  `pkg/server/e2b_embedded.go`, `pkg/cli/cmds/e2b_server.go`,
  `sandboxd/src/{main,exec,execctl,files,watch,events,background,processes,
  transcript}.zig`,
  `pkg/sandbox/client/client.go`,
  `manifests/sandbox-matrix/e2b-gateway.yaml`.
- CubeSandbox — `CubeAPI/src/{routes,handlers/sandboxes,middleware/{auth,rate_limit}}.rs`,
  `CubeAPI/src/{services/sandboxes,error/mod,models/mod}.rs`,
  `CubeProxy/lua/*.lua` (rewrite, path_rewrite, sandbox_backend,
  sandbox_state, backend_cache, header_filter, request_host, log_phase,
  admin_phase, proxy_registry), `sdk/{python,node,go}/`,
  `tests/e2e/sdk_compat/`, `docs/guide/{lifecycle,authentication,https-and-domain,
  persistent-storage,sandbox-logs,snapshot-rollback-clone}.md` — the
  Appendix's borrowable-ideas backlog cites each idea's exact file:line.
- Cilium Gateway API — `docs.cilium.io/en/latest/network/servicemesh/gateway-api/`
  (GatewayClass/Gateway/HTTPRoute/GRPCRoute/TCPRoute, prerequisites:
  kube-proxy replacement + L7 proxy, Gateway API CRD v1.6.1 for Cilium
  1.20.x, `gatewayAPI.enabled` + `gatewayAPI.enableAlpn` Helm values);
  Gateway API spec — `gateway-api.sigs.k8s.io`.

[Dormice]: https://github.com/BitMiracle-AI/Dormice
[CubeSandbox]: https://github.com/TencentCloud/CubeSandbox
