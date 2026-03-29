# KIP-3: Agentic AI Sandbox Matrix

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-03-29 | Draft |

## Summary

Introduce a Kubernetes-native **Agentic AI Sandbox Matrix** into K8E — a system that gives AI agents a full isolated Linux environment with controlled network access, warm pool pre-provisioning, and multi-agent orchestration. External agents communicate via a gRPC API (aligned with the agentbox open-source reference). Egress control is enforced by K8E's built-in Cilium via dynamic per-session `CiliumNetworkPolicy` with `toFQDNs`, eliminating the need for a custom proxy process. Isolation backends are pluggable: Firecracker microVMs, gVisor, and Kata Containers via Kubernetes `RuntimeClass`.

## Motivation

K8E is positioned as the Kubernetes platform for AI/ML workloads and agent runtimes (see README). However, it currently lacks:

- A native mechanism to provision isolated, ephemeral execution environments for AI agents
- A warm pool to absorb the burst of session creation requests ("thousands of sessions per minute" at Perplexity scale)
- A standard gRPC API that LLM orchestrators, Python agents, and TypeScript clients can call directly
- Per-session network egress control without running a separate proxy sidecar
- Multi-agent orchestration with filesystem-based IPC and a two-level hierarchy

This proposal closes that gap by building the Sandbox Matrix as a first-class K8E feature, open-source and Kubernetes-native.

## Background

### Perplexity Computer — Production Reference

Perplexity Computer (launched February 2026) runs thousands of sandbox sessions per minute. Key architectural decisions relevant to this design:

- Each session runs in its own isolated Kubernetes pod; a Go binary (`envd`) manages lifecycle via gRPC
- A FUSE daemon mounts a persistent filesystem at `/workspace`; sub-agents communicate by writing files, not via APIs — classic Unix message-passing
- Zero-trust networking: sandboxes have no direct network access; outbound traffic routes through an egress proxy that injects credentials server-side; no API keys visible inside the sandbox
- Two-level agent hierarchy enforced architecturally: parent spawns children via `run_subagent`; children cannot spawn further agents (no grandchildren)
- `confirm_action` tool enforces mandatory user approval before irreversible operations — safety is structural, not prompt-based
- Warm pool of pre-booted sandboxes enables sub-500ms session claim latency

### agentbox — Open-Source Reference Implementation

[agentbox](https://github.com/Michaelliv/agentbox) is the closest open-source reference for the communication architecture:

```
┌─────────────┐     gRPC      ┌──────────────────┐     HTTP      ┌─────────────────────┐
│   Client    │──────────────▶│   gRPC Server    │──────────────▶│  Container:2024     │
│ (TS/Python) │               │ (SandboxManager) │               │  (process_api)      │
└─────────────┘               └──────────────────┘               └─────────────────────┘
                                       │                                   │
                                       ▼                                   ▼
                              ┌──────────────────┐               ┌─────────────────────┐
                              │   Egress Proxy   │◀──────────────│   HTTP_PROXY env    │
                              │  (JWT allowlist) │               │  (pip, git, etc.)   │
                              └──────────────────┘               └─────────────────────┘
```

Key patterns adopted from agentbox:

| agentbox component | K8E equivalent | Notes |
|---|---|---|
| `process_api.py` (PID 1, HTTP :2024) | `sandboxd` Zig binary | Same role, rewritten in Zig for cross-platform static build |
| `SandboxManager` (container lifecycle) | `SandboxMatrix` controller | Kubernetes-native, CRD-driven |
| `grpc_server.py` (SandboxService) | gRPC Gateway | Same proto API surface |
| `egress_proxy.py` (JWT allowlist) | Cilium `toFQDNs` + Envoy | Replaced by K8E's built-in Cilium — zero extra process |
| gVisor `runsc` runtime | `RuntimeClass: gvisor` | Same isolation technology |

The proto API surface is intentionally aligned with agentbox so existing agentbox clients work with minimal changes.

### kubernetes-sigs/agent-sandbox

The official Kubernetes SIG Apps subproject provides:

- `Sandbox` CRD: declarative API for stateful singleton pods with stable identity
- `SandboxWarmPool`: pre-warmed pod pool for fast allocation
- `SandboxTemplate` + `SandboxClaim`: template-based session management
- Python SDK: `Client → Gateway → Router → Sandbox Pod` traffic pattern

K8E's Sandbox Matrix builds on these concepts and extends them with the gRPC communication layer, Firecracker support, and Cilium-native egress control.

### Cilium Egress Control — `toFQDNs`

K8E bundles Cilium as its CNI. Cilium's DNS-based policy feature (`toFQDNs`) allows per-pod egress control by domain name, enforced at the eBPF kernel level via the Cilium DNS proxy and Envoy L7 filter:

```yaml
egress:
- toFQDNs:
  - matchName: "pypi.org"
  - matchName: "files.pythonhosted.org"
  toPorts:
  - ports:
    - port: "443"
      protocol: TCP
```

The `SandboxMatrix` controller generates one `CiliumNetworkPolicy` per session, populated with the session's `allowedHosts`. On session destroy, the policy is deleted. This replaces agentbox's standalone egress proxy entirely — no extra process, no JWT signing key, no `HTTP_PROXY` env var.

### Isolation Backend Comparison

| Backend | Isolation Level | Boot Time | Integration Path |
|---|---|---|---|
| `firecracker` | Hardware microVM (KVM, Rust VMM) | ~125ms | containerd `aws.firecracker` shim + devmapper snapshotter |
| `gvisor` | Syscall interception (userspace kernel) | ~10ms | containerd `runsc` shim |
| `kata` | VM-backed (QEMU / Cloud Hypervisor) | ~500ms | containerd `kata-runtime` shim |

Firecracker is the strongest isolation option and what Perplexity Computer uses in production. It requires KVM on the host node (`/dev/kvm`). K8E applies the Firecracker `RuntimeClass` only when KVM is present.

## Design

### Architecture Overview

```
┌──────────────────────────────────────────────────────────────────────────┐
│                          External Agents                                 │
│                                                                          │
│   Python Client          TypeScript Client        LLM / MCP Tool        │
│   (grpc stub)            (grpc stub)              (grpc stub)           │
└──────────────┬───────────────────┬────────────────────┬─────────────────┘
               │                   │                    │
               └───────────────────┴────────────────────┘
                                   │ gRPC :50051
                                   ▼
┌──────────────────────────────────────────────────────────────────────────┐
│                        K8E Control Plane                                 │
│                                                                          │
│   ┌─────────────────────────┐    ┌──────────────────────────────────┐   │
│   │   gRPC Gateway          │    │   SandboxMatrix Controller       │   │
│   │   SandboxService :50051 │───▶│   - WarmPool reconciler          │   │
│   │   session registry      │    │   - CiliumNetworkPolicy generator│   │
│   └────────────┬────────────┘    └──────────────────────────────────┘   │
└────────────────┼─────────────────────────────────────────────────────────┘
                 │ HTTP :2024
                 ▼
┌──────────────────────────────────────────────────────────────────────────┐
│              Sandbox Pod  (runtimeClass: firecracker | gvisor | kata)    │
│                                                                          │
│   ┌──────────────────────────┐    ┌──────────────────────────────────┐  │
│   │  sandboxd  (PID 1)       │    │  /workspace  (PVC + FUSE)        │  │
│   │  HTTP :2024              │    │  shared across sub-agents        │  │
│   │  /exec  /files  /stream  │    └──────────────────────────────────┘  │
│   └──────────────────────────┘                                          │
│                                                                          │
│   Egress: enforced by Cilium eBPF (toFQDNs) — no proxy process         │
└──────────────────────────────────────────────────────────────────────────┘
                 │ allowed FQDNs only
                 ▼
         External Services (pypi.org, github.com, ...)
```

### gRPC API — `sandbox.proto`

The proto API is aligned with agentbox's `sandbox.proto` and extended with K8E-specific methods for sub-agent orchestration.

```protobuf
syntax = "proto3";
package sandbox.v1;

service SandboxService {
  // Session lifecycle
  rpc CreateSession(CreateSessionRequest)   returns (CreateSessionResponse);
  rpc DestroySession(DestroySessionRequest) returns (DestroySessionResponse);

  // Code execution
  rpc Exec(ExecRequest)             returns (ExecResponse);
  rpc ExecStream(ExecRequest)       returns (stream ExecStreamResponse);

  // File I/O
  rpc WriteFile(WriteFileRequest)   returns (WriteFileResponse);
  rpc ReadFile(ReadFileRequest)     returns (ReadFileResponse);
  rpc ListFiles(ListFilesRequest)   returns (ListFilesResponse);

  // Package management
  rpc PipInstall(PipInstallRequest) returns (PipInstallResponse);

  // K8E extensions: multi-agent orchestration
  rpc RunSubAgent(RunSubAgentRequest)       returns (RunSubAgentResponse);
  rpc ConfirmAction(ConfirmActionRequest)   returns (ConfirmActionResponse);
}

message CreateSessionRequest {
  string   session_id  = 1; // optional; generated if empty
  string   tenant_id   = 2; // optional; for persistent storage
  repeated string allowed_hosts = 3; // egress allowlist; empty = defaults
  string   runtime_class = 4; // firecracker | gvisor | kata; default: gvisor
}

message ExecRequest {
  string session_id = 1;
  string command    = 2;
  int32  timeout    = 3; // seconds; default 30
  string workdir    = 4; // default /workspace
}

message RunSubAgentRequest {
  string parent_session_id = 1;
  string agent_type        = 2; // research | coding | general
  string workspace_path    = 3; // shared sub-path under /workspace
}
```

**Default `allowed_hosts`** (matching agentbox defaults):
`pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `github.com`, `raw.githubusercontent.com`, `objects.githubusercontent.com`, `crates.io`, `static.crates.io`

### CRD API

#### `SandboxMatrix`

```yaml
apiVersion: k8e.cattle.io/v1alpha1
kind: SandboxMatrix
metadata:
  name: default
  namespace: sandbox-matrix
spec:
  warmPoolSize: 5                  # number of pre-provisioned idle pods
  runtimeClass: firecracker        # firecracker | gvisor | kata
  sessionTTL: 1800                 # seconds; 0 = no TTL
  defaultAllowedHosts:
    - pypi.org
    - files.pythonhosted.org
    - registry.npmjs.org
    - github.com
    - raw.githubusercontent.com
  resourceLimits:
    memory: "4Gi"
    cpu: "4"
status:
  readyWarmCount: 5
  activeSessions: 12
```

#### `SandboxSession`

```yaml
apiVersion: k8e.cattle.io/v1alpha1
kind: SandboxSession
metadata:
  name: session-abc123
  namespace: sandbox-matrix
spec:
  tenantID: tenant-1
  allowedHosts:
    - pypi.org
    - files.pythonhosted.org
  runtimeClass: firecracker
  parentSessionID: ""              # set for sub-agents
  depth: 0                         # 0 = orchestrator, 1 = sub-agent; max 1
status:
  phase: Active                    # Warm | Active | Terminating
  podName: sandbox-abc123
  podIP: 10.42.1.55
  workspacePVC: workspace-abc123
  createdAt: "2026-03-29T11:00:00Z"
  expiresAt: "2026-03-29T11:30:00Z"
```

#### `SandboxWarmPool`

```yaml
apiVersion: k8e.cattle.io/v1alpha1
kind: SandboxWarmPool
metadata:
  name: default
  namespace: sandbox-matrix
spec:
  templateRef:
    name: default-sandbox-template
  size: 5
  runtimeClass: firecracker
status:
  readyCount: 5
  pendingCount: 0
```

### Per-Session `CiliumNetworkPolicy`

The controller generates this resource on `CreateSession` and deletes it on `DestroySession`:

```yaml
apiVersion: cilium.io/v2
kind: CiliumNetworkPolicy
metadata:
  name: sandbox-session-<session-id>
  namespace: sandbox-matrix
spec:
  endpointSelector:
    matchLabels:
      sandbox.k8e.io/session-id: <session-id>
  egress:
  # Allow DNS resolution (required for toFQDNs tracking)
  - toEndpoints:
    - matchLabels:
        k8s:io.kubernetes.pod.namespace: kube-system
        k8s:k8s-app: kube-dns
    toPorts:
    - ports:
      - port: "53"
        protocol: ANY
      rules:
        dns:
        - matchPattern: "*"
  # Allow only session-specific allowedHosts on HTTPS
  - toFQDNs:
    - matchName: "pypi.org"
    - matchName: "files.pythonhosted.org"
    # ... dynamically populated from SandboxSession.spec.allowedHosts
    toPorts:
    - ports:
      - port: "443"
        protocol: TCP
  # Allow in-cluster gRPC gateway only
  - toEndpoints:
    - matchLabels:
        app: sandbox-grpc-gateway
    toPorts:
    - ports:
      - port: "50051"
        protocol: TCP
  # Deny everything else (implicit Cilium default-deny when policy exists)
```

Cilium's eBPF datapath enforces this at the kernel level. The Cilium DNS proxy intercepts DNS responses and tracks IP-to-FQDN mappings dynamically — no static CIDR lists required.

### `sandboxd` — PID 1 Init Process (Zig)

`sandboxd` is written in **Zig** and runs as PID 1 inside every sandbox container. It mirrors agentbox's `process_api.py` in role, but Zig is chosen for three reasons that align directly with K8E's existing toolchain:

1. **K8E already uses Zig as its build system** (`build.zig` / `zig build`). `sandboxd` becomes the first native Zig binary in the project, built via a new `zig build sandboxd` step — no new toolchain required.
2. **Cross-platform single binary with zero runtime dependencies.** Zig compiles to a fully static binary for any target (`linux/amd64`, `linux/arm64`, `linux/riscv64`) via `zig build -Dtarget=<triple>`. This matches K8E's multi-arch support (x86_64, ARM64, RISC-V) without CGO or libc dependencies.
3. **Minimal footprint.** A Zig HTTP server with subprocess management compiles to ~200KB stripped, appropriate for a PID 1 init process that must start in <10ms inside a Firecracker microVM.

`sandboxd` requires no `HTTP_PROXY` environment variable — Cilium handles egress transparently at the eBPF layer.

#### HTTP API (`:2024`)

```
POST /exec
  Body: {"command": "...", "timeout": 30, "workdir": "/workspace"}
  Response: {"stdout": "...", "stderr": "...", "exit_code": 0}

GET  /exec/stream
  Body: {"command": "..."}
  Response: text/event-stream (SSE), one chunk per stdout/stderr line

POST /files/write
  Body: {"path": "script.py", "content": "...", "mode": "w"}

GET  /files/read?path=script.py
  Response: {"content": "..."}

GET  /files/list?since=<unix-timestamp>
  Response: {"files": [{"path": "...", "modified": ...}]}
```

#### Zig Source Layout

```
sandboxd/
├── build.zig          # standalone build file; also imported by root build.zig
└── src/
    ├── main.zig       # HTTP server, signal handling, zombie reaping (PID 1)
    ├── exec.zig       # subprocess spawn, stdout/stderr capture, SSE streaming
    └── files.zig      # /workspace read/write/list handlers
```

#### `build.zig` Integration

The root `build.zig` gains a `sandboxd` step that cross-compiles for all K8E target architectures:

```zig
// In build.zig — new sandboxd step
const sandboxd_step = b.step("sandboxd", "Build sandboxd init process (Zig)");
const targets = [_][]const u8{ "x86_64-linux-musl", "aarch64-linux-musl", "riscv64-linux-musl" };
for (targets) |triple| {
    const exe = b.addExecutable(.{
        .name = b.fmt("sandboxd-{s}", .{triple}),
        .root_source_file = b.path("sandboxd/src/main.zig"),
        .target = b.resolveTargetQuery(
            std.Target.Query.parse(.{ .arch_os_abi = triple }) catch unreachable,
        ),
        .optimize = .ReleaseSafe,
    });
    const install = b.addInstallArtifact(exe, .{ .dest_dir = .{ .override = .{ .custom = "bin" } } });
    sandboxd_step.dependOn(&install.step);
}
```

The `all` step depends on `sandboxd_step`, so `zig build` produces `bin/sandboxd-x86_64-linux-musl`, `bin/sandboxd-aarch64-linux-musl`, and `bin/sandboxd-riscv64-linux-musl` alongside the main `k8e` binary.

The correct architecture binary is copied into the `k8e-sandbox` container image at Docker build time:

```dockerfile
# sandbox/Dockerfile
ARG TARGETARCH
COPY bin/sandboxd-${TARGETARCH}-linux-musl /sandboxd
ENTRYPOINT ["/sandboxd"]
```

`sandboxd` forwards signals to child processes and reaps zombies as PID 1. It does not manage credentials or network policy — those are handled by Cilium and the gRPC gateway respectively.

### Warm Pool — Session Claim Flow

```
CreateSession(req)
       │
       ▼
  Any warm pod available?
       │
   YES │                          NO
       ▼                           ▼
  Atomically relabel            Create new pod
  warm → active                 (cold start ~125ms Firecracker)
  (<500ms)                           │
       │                             ▼
       └──────────────────► Register session in etcd
                            Generate CiliumNetworkPolicy
                            Mount /workspace PVC
                            Return session_id
```

Warm pods have `sandboxd` already running and a base deny-all `CiliumNetworkPolicy` applied. On claim, the controller replaces the deny-all policy with the session-specific `toFQDNs` policy.

### Two-Level Sub-Agent Orchestration

```
Orchestrator Session (depth=0)
│   /workspace/session-abc/
│
├── RunSubAgent(type=research)  ──► Sub-agent Session (depth=1)
│       writes /workspace/session-abc/results/research.json
│
├── RunSubAgent(type=coding)    ──► Sub-agent Session (depth=1)
│       writes /workspace/session-abc/results/code.py
│
└── ListFiles(/workspace/session-abc/results/)
        reads aggregated results from both sub-agents
```

Rules enforced by the controller:
- `depth` is set to `parent.depth + 1` on sub-agent creation
- If `depth >= 1`, `RunSubAgent` returns `PERMISSION_DENIED` — no grandchildren
- Sub-agents share the parent's `/workspace` PVC (read-write mount)
- Each sub-agent gets its own `CiliumNetworkPolicy` with the same `allowedHosts` as the parent

### `confirm_action` — Architectural Safety

Before any irreversible operation (send email, delete file, make purchase), the agent calls `ConfirmAction`. The gRPC gateway creates a pending approval record in etcd and long-polls until the external caller approves or the timeout expires. This is enforced at the API level, not via prompt instructions.

```
Agent calls ConfirmAction(action="delete /workspace/report.pdf")
       │
       ▼
  Gateway creates PendingApproval{id, action, session_id} in etcd
  Returns approval_id to agent
       │
       ▼
  Agent polls ConfirmAction(approval_id=...) — blocks
       │
  External caller approves via separate API call
       │
       ▼
  Gateway resolves approval → agent proceeds
  (or timeout → agent receives CANCELLED)
```

## New Files and Packages

```
proto/sandbox/v1/
└── sandbox.proto                    # gRPC API definition

sandboxd/                            # Zig project — PID 1 init process
├── build.zig                        # standalone Zig build (imported by root build.zig)
└── src/
    ├── main.zig                     # HTTP server :2024, signal handling, zombie reaping
    ├── exec.zig                     # subprocess spawn, stdout/stderr capture, SSE streaming
    └── files.zig                    # /workspace read/write/list handlers

sandbox/
├── Dockerfile                       # k8e-sandbox base image; copies sandboxd binary for TARGETARCH
└── rootfs/                          # pre-installed: python3, node, bash, standard Unix tools

pkg/sandboxmatrix/
├── api/v1alpha1/
│   ├── types.go                     # CRD Go types
│   └── zz_generated_deepcopy.go
├── grpc/
│   ├── server.go                    # SandboxService gRPC implementation
│   └── orchestrator.go              # RunSubAgent + ConfirmAction
├── pool.go                          # WarmPool reconciler
└── policy.go                        # CiliumNetworkPolicy generator per session

manifests/sandbox-matrix/
├── crds.yaml                        # SandboxMatrix, SandboxSession, SandboxTemplate, SandboxWarmPool
├── runtimeclasses.yaml              # RuntimeClass: firecracker, gvisor, kata
├── grpc-gateway.yaml                # Deployment + Service for gRPC gateway :50051
└── network-policy.yaml              # Base deny-all CiliumNetworkPolicy for warm pods
```

## Modified Files

| File | Change |
|------|--------|
| `build.zig` | Add `sandboxd` step: cross-compiles `sandboxd/src/main.zig` for `x86_64-linux-musl`, `aarch64-linux-musl`, `riscv64-linux-musl`; `all` step depends on it |
| `pkg/agent/templates/` containerd config | Add Firecracker devmapper snapshotter + `aws.firecracker` shim config block; add `runsc` shim config block |
| `pkg/server/server.go` | Register SandboxMatrix controller, gRPC gateway, and `sandboxd` mutation webhook |
| `pkg/deploy/` | Add `manifests/sandbox-matrix/` to bundled manifests |
| `pkg/cli/cmds/server.go` | Add `--disable-sandbox-matrix` flag |

## Implementation Tasks

### Task 1 — CRD Types and Manifests

Define `SandboxMatrix`, `SandboxSession`, `SandboxTemplate`, `SandboxWarmPool` Go types in `pkg/sandboxmatrix/api/v1alpha1/`. Generate deepcopy. Produce CRD YAMLs in `manifests/sandbox-matrix/crds.yaml`.

Verification: `kubectl get crd | grep sandbox` shows all four CRDs after `k8e server` starts.

### Task 2 — `sandboxd` Init Process (Zig)

Implement `sandboxd/src/main.zig`: HTTP server on `:2024` with `/exec`, `/exec/stream` (SSE), `/files/write`, `/files/read`, `/files/list`. Runs as PID 1, forwards signals, reaps zombies. Cross-compiled via `zig build sandboxd` for `x86_64-linux-musl`, `aarch64-linux-musl`, `riscv64-linux-musl`. The correct architecture binary is embedded into `sandbox/Dockerfile` via `COPY bin/sandboxd-${TARGETARCH}-linux-musl /sandboxd`.

No CGO, no libc dependency, no runtime. The binary is ~200KB stripped.

Verification: `curl http://<pod-ip>:2024/exec -d '{"command":"python3 -c \"print(42)\""}'` returns `{"stdout":"42\n","exit_code":0}`. `zig build sandboxd` produces three static binaries in `bin/`.

### Task 3 — Firecracker and gVisor RuntimeClass Integration

Extend K8E's containerd config template (`pkg/agent/templates/`) with:
- Firecracker: devmapper snapshotter + `aws.firecracker` shim (requires `firecracker-containerd`)
- gVisor: `runsc` shim

Add `manifests/sandbox-matrix/runtimeclasses.yaml` with `RuntimeClass` objects for `firecracker`, `gvisor`, `kata`. Add `/dev/kvm` presence check in agent startup; skip Firecracker `RuntimeClass` on non-KVM nodes.

Verification: `kubectl get runtimeclass` shows `firecracker`, `gvisor`, `kata`. Pod with `runtimeClassName: firecracker` boots in a microVM; `dmesg` inside shows a fresh kernel.

### Task 4 — gRPC Gateway

Implement `pkg/sandboxmatrix/grpc/server.go`: gRPC service on `:50051` implementing `SandboxService`. Session registry backed by etcd (reuses K8E's embedded etcd). Routes `Exec`/`WriteFile`/`ReadFile` calls to the target pod's `sandboxd` via HTTP. Deploy as `manifests/sandbox-matrix/grpc-gateway.yaml`.

Verification: Python client `stub.Exec(ExecRequest(session_id=..., command="hostname"))` returns the sandbox pod's hostname.

### Task 5 — Cilium-Based Egress Control

Implement `pkg/sandboxmatrix/policy.go`: `BuildCiliumNetworkPolicy(session *SandboxSession) *ciliumv2.CiliumNetworkPolicy` generates a per-session CNP with `toFQDNs` entries from `session.Spec.AllowedHosts`. Controller applies the CNP on `CreateSession` and deletes it on `DestroySession`. Base deny-all CNP in `manifests/sandbox-matrix/network-policy.yaml` covers warm pods.

Verification: Inside sandbox, `pip install requests` succeeds (pypi.org allowed). `curl https://example.com` times out (not in allowlist). Cilium Hubble shows correct allow/deny verdicts. No custom proxy process running.

### Task 6 — Warm Pool Manager

Implement `pkg/sandboxmatrix/pool.go`: reconciler watching `SandboxWarmPool` spec. Creates pods to match `warmPoolSize` with label `sandbox.k8e.io/state: warm`. On `CreateSession`, atomically transitions label `warm → active` and registers session. Replenishes pool after each claim.

Verification: `warmPoolSize: 5` with `runtimeClass: firecracker` → 5 microVM-backed pods in `warm` state. `CreateSession` returns in <500ms end-to-end.

### Task 7 — Sub-Agent Orchestration and `confirm_action`

Implement `pkg/sandboxmatrix/grpc/orchestrator.go`:
- `RunSubAgent`: creates child `SandboxSession` with shared PVC mount and `depth = parent.depth + 1`; rejects if `depth >= 1`
- `ConfirmAction`: creates `PendingApproval` record in etcd; long-polls until approved or timed out

Verification: Orchestrator spawns two sub-agents in parallel; both write to `/workspace/results/`; parent reads via `ListFiles`. Attempt to spawn grandchild returns `PERMISSION_DENIED`. `ConfirmAction` blocks until external approval.

### Task 8 — K8E Server Integration

Register SandboxMatrix controller, gRPC gateway, and `sandboxd` mutation webhook in `pkg/server/server.go`. Add all manifests to `pkg/deploy/` bundle. Add `--disable-sandbox-matrix` opt-out flag. Apply Firecracker `RuntimeClass` only when `/dev/kvm` is present on the node.

Verification: Fresh `k8e server --cluster-init` → gRPC gateway reachable on `:50051` → Python client creates session, runs code, egress enforced by Cilium `toFQDNs`, session destroyed, CNP cleaned up.

## Security Considerations

### Isolation Layers (defense in depth)

1. **RuntimeClass isolation** — Firecracker microVM (hardware KVM boundary) or gVisor (userspace kernel) contains the workload at the OS level
2. **Cilium eBPF egress** — `toFQDNs` policy enforced at kernel level; sandbox cannot reach non-allowlisted hosts regardless of what code runs inside
3. **No credentials in sandbox** — API keys and OAuth tokens are never mounted into sandbox pods; the gRPC gateway holds session state server-side
4. **Resource limits** — Each sandbox pod has CPU and memory limits (default: 4 CPU, 4Gi RAM) enforced by Kubernetes
5. **Filesystem isolation** — Only `/workspace` is writable; the container image root filesystem is read-only
6. **Two-level hierarchy cap** — Prevents cascading agent creation and unbounded resource consumption
7. **`confirm_action` gate** — Irreversible operations require explicit external approval; cannot be bypassed by prompt injection

### Why Root Inside Sandbox Is Safe

Following agentbox's reasoning: gVisor and Firecracker both provide strong enough isolation that running as root inside the sandbox is safe. "Root" inside a gVisor sandbox has no privileges outside gVisor's userspace kernel. "Root" inside a Firecracker microVM has no privileges outside the VM's hardware boundary.

## Compatibility

- **Existing K8E clusters**: Sandbox Matrix is additive. No existing APIs or behaviors change. Disable with `--disable-sandbox-matrix`.
- **Zig toolchain**: `sandboxd` requires Zig ≥ 0.14. K8E already uses Zig as its build system, so no new toolchain is introduced. The `zig build sandboxd` step is additive and does not affect the existing `zig build k8e` Go build path.
- **Cilium version**: Requires Cilium ≥ 1.14 for `toFQDNs` stability. K8E's bundled Cilium version satisfies this.
- **Firecracker**: Only activated on nodes with `/dev/kvm`. Clusters without KVM support use gVisor or Kata as fallback.
- **kubernetes-sigs/agent-sandbox**: The `SandboxWarmPool` and `SandboxTemplate` CRDs are designed to be compatible with the upstream `agents.x-k8s.io/v1alpha1` API group for future alignment.

## Breaking Changes

None. This is a purely additive feature. All new CRDs, controllers, and manifests are opt-out via `--disable-sandbox-matrix`.

## References

- [agentbox](https://github.com/Michaelliv/agentbox) — open-source reference implementation
- [kubernetes-sigs/agent-sandbox](https://github.com/kubernetes-sigs/agent-sandbox) — Kubernetes SIG Apps official subproject
- [Perplexity Sandbox API](https://www.perplexity.ai/hub/blog/sandbox-api-isolated-code-execution-for-ai-agents) — production architecture reference
- [Cilium DNS-based policies](https://docs.cilium.io/en/latest/security/dns/) — `toFQDNs` documentation
- [Firecracker microVM](https://github.com/firecracker-microvm/firecracker) — AWS microVM technology
- [Zig language](https://ziglang.org/) — cross-platform systems programming language used for `sandboxd`
- [KIP-1](./kip-1-native-etcd-storage-client.md) — Native etcd storage client
- [KIP-2](./kip-2-upgrade-dependencies-to-kubernetes-1.35.md) — Kubernetes 1.35 dependency upgrade
