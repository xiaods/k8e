# CLAUDE.md

Guidance for Claude Code when working in this repository.

## Project Overview

K8E is a CNCF-conformant Kubernetes distribution packaged as a single `k8e`
binary under 100MB, purpose-built for secure, isolated AI agent execution at
scale. Kubernetes is the substrate; the product on top of it is a **sandbox
matrix**:

- Warm-pooled sandbox Pods with a pluggable RuntimeClass — `gvisor` by default,
  plus `kata` and `firecracker`.
- A gRPC `SandboxService` (34 RPCs) served behind `sandbox-grpc-gateway:50051`
  with mTLS.
- `k8e-sandbox-cli` — the standalone CLI an AI agent drives from a shell
  (24 subcommands).
- An E2B-compatible HTTP surface, an MCP adapter, and skills/plugins for Claude
  Code, Codex, Pi and DeepSeek Harness (dsh).

**There is no `k8e sandbox` command group.** The `k8e` binary only carries
`k8e sandbox-apikey` and `k8e e2b-server`; every session/exec/file command lives
in the separate `k8e-sandbox-cli` binary.

## Architecture (Big Picture)

The repository is organized as a **Zig-based Go project**:

```
.
├── build.zig                      # Zig build graph: Go compile, cross-compile, Zig targets
├── Makefile                       # Thin wrappers over `zig build <target>`
├── main.go                        # CLI entry point; also holds the //go:generate directives
├── cmd/                           # One package per binary entry point
│   ├── server/                    # Builds bin/k8e (control plane + agent; no bundled runtimes)
│   ├── k8e/                       # Release multicall binary (`hack/package-cli`); symlink aliases
│   ├── sandboxcli/                # Builds k8e-sandbox-cli — the agent-facing CLI
│   ├── agent/                     # Agent-only (kubelet + containerd)
│   ├── kubectl/                   # kubectl wrapper
│   └── ...                        # cert, completion, containerd, ctr, encrypt, etcdsnapshot, token
├── pkg/
│   ├── cli/
│   │   ├── cmds/                  # CLI flags + command wiring
│   │   │   ├── root.go            # NewApp(), global flags
│   │   │   ├── server.go          # ServerConfig + every server/sandbox flag
│   │   │   ├── agent.go           # AgentConfig + every agent flag
│   │   │   ├── app_commands.go    # Command-set registration (incl. `sandbox-apikey`)
│   │   │   └── e2b_server.go      # `k8e e2b-server` + K8E_E2B_* flags
│   │   ├── commands/              # Server/agent command implementations (Funcs())
│   │   └── e2bserver/             # Embedded E2B HTTP server runtime
│   ├── server/                    # Server daemon orchestration (incl. e2b_embedded.go)
│   ├── agent/                     # Agent daemon orchestration
│   ├── daemons/                   # Individual daemon implementations
│   ├── sandboxmatrix/             # Core sandbox orchestration
│   │   ├── controller.go          # Warm-pool reconciler, session GC / idle reaping
│   │   ├── leader.go              # Lease-based leader election for the reconcilers
│   │   ├── ratelimit/             # Per-tenant gRPC token buckets
│   │   ├── api/v1alpha1/          # CRDs: SandboxMatrix, SandboxSession,
│   │   │                          #   SandboxWarmPool, SandboxTemplate
│   │   └── grpc/                  # SandboxService impl, orchestrator, PTY terminal, pb/
│   ├── sandboxcli/                # k8e-sandbox-cli handlers (24 subcommands)
│   │   ├── commands.go            # run/status/write/read/push/pull/subagent/confirm/benchmark/…
│   │   ├── session.go            # Session state + flock (resolveSession)
│   │   ├── snapshot.go            # Workspace snapshot save/restore
│   │   ├── manifest.go            # Declarative workspace manifest
│   │   ├── apikey.go              # `sandbox-apikey` (keys.json Secret)
│   │   ├── connect.go             # Writes connection config + installs the skill
│   │   ├── install.go             # Skill install for claude / codex / pi / dsh
│   │   ├── login.go               # mTLS client cert issuance
│   │   ├── profile.go             # Multi-profile config
│   │   └── skills/k8e-sandbox/    # Embedded SKILL.md (//go:embed, installed by `connect`)
│   ├── sandbox/
│   │   ├── client/                # gRPC client with TLS auto-discovery
│   │   ├── apikey/                # API key store
│   │   └── e2b/                   # E2B-compatible HTTP/Connect layer
│   ├── sandboxmcp/                # MCP HTTP adapter (`k8e-sandbox-cli mcp-serve`)
│   ├── sandboxlayer/              # CAS snapshot layerstore
│   ├── configfilearg/             # Maps config-file keys onto CLI flags
│   ├── deploy/                    # Kubernetes manifests and Helm charts
│   ├── embedw/                    # Fuses the official embed.StartEtcd
│   └── ...                        # apis, bootstrap, token, certmonitor, secretsencrypt, cgroups
├── proto/sandbox/v1/sandbox.proto # SandboxService: 34 RPCs
├── sandboxd/                      # Sandbox runtime daemon in Zig (exec, files, PTY) — :2024
├── sandbox/                       # Dockerfile for the k8e-sandbox runtime image
├── manifests/                     # sandbox-matrix/, sandbox-mcp/, metrics-server/, *.yaml
├── plugins/deepseek-harness/      # dsh plugin family (TypeScript, @k8e-sandbox/*)
├── docs/                          # KIPs (kip-1 … kip-26) + docs/agents/
├── hack/                          # download / generate / package / e2e scripts
├── contrib/                       # install.sh + cluster util scripts
└── tests/                         # unit.go helpers + mcp/*.py smoke tests
```

### Key Architectural Flows

**An AI agent submitting work (the primary path):**
```
AI agent (shell)
  → k8e-sandbox-cli run "code"        # or exec / read / write / push / pull
    → sandbox-grpc-gateway:50051 (TLS gRPC; mTLS cert from `k8e-sandbox-cli login`)
      → SandboxService (pkg/sandboxmatrix/grpc/server.go)
        → Orchestrator (K8s API: warm-claim, create, pause, destroy Pods)
          → sandboxd HTTP :2024 inside the sandbox Pod
            → Isolated container (RuntimeClass: gvisor | kata | firecracker)
```

**E2B-compatible clients:**
```
e2b SDK (python/js)   or   `k8e e2b-server`
  → E2B HTTP/Connect surface (pkg/sandbox/e2b, default 127.0.0.1:3676)
    → in-process gRPC gateway → same path as above
```

**MCP clients:**
```
MCP client → k8e-sandbox-cli mcp-serve (HTTP /mcp, default 127.0.0.1:8443)
  → same gRPC SandboxService
```

**DeepSeek Harness:**
```
dsh → plugins/deepseek-harness (@k8e-sandbox/*) → k8e_sandbox_* tools → k8e-sandbox-cli
```

## Building

```bash
make                 # zig build all — everything, including cross-platform CLIs
make k8e             # zig build k8e — bin/k8e from cmd/server (no bundled runtimes)
make deps            # zig build deps — go mod tidy
make generate        # zig build generate — protobuf, CRD deepcopy, bindata
make format          # zig build fmt — go fmt ./... plus zig fmt build.zig
make test            # zig build test — go test -v ./...
make clean           # zig build clean — bin dist build .zig-cache zig-out .cni-build
make package         # zig build package — release artifacts
make package-cli     # zig build package-cli — CLI-only artifacts
make package-airgap  # zig build package-airgap — air-gap package
make e2e E2E_PROFILE=l1   # hack/e2e/run.sh (l1 | l2 | l3); needs linux/amd64 bin/k8e + docker
```

Zig targets that `make` does not wrap: `zig build sandboxd`, `zig build
sandboxcli`, `zig build shim`, `zig build runc`, `zig build cni`, `zig build
download`.

Build outputs:

- `bin/` — `bin/k8e` (built from `cmd/server`) plus the symlink aliases created
  by `zig build k8e` (`k8e-agent`, `k8e-server`, `kubectl`, `crictl`, `ctr`, …).
- `zig-out/bin/` — the Zig-built `sandboxd` and the cross-compiled
  `k8e-sandbox-cli-<os>-<arch>` binaries.

Dev vs release binary: `hack/package-cli` builds the multicall `cmd/k8e` entry,
which stages `containerd`/`runc`; the `cmd/server` build bundles no container
runtime, so only the `l1` e2e profile works with it.

## Running & Testing

```bash
# Full suite
make test                                        # or: zig build test

# Targeted package tests
go test ./pkg/sandboxmatrix/... -count=1         # controller, grpc, ratelimit
go test ./pkg/sandboxcli/... -count=1            # CLI handlers, sessions, snapshots
go test ./pkg/sandbox/e2b/... -count=1           # E2B compat surface
go test ./pkg/sandbox/client/... -count=1
go test ./pkg/sandboxmcp/... -count=1
go test ./tests/... -v -count=1 -timeout 120s

# Fast compile / vet check for a subtree
go build ./pkg/...
go vet ./pkg/sandbox/client/
```

Cluster-backed integration tests need a running K8E cluster and run through
`make e2e E2E_PROFILE=l1|l2|l3` (driven by `hack/e2e/run.sh`).

## Generated code — do not hand-edit

Regenerate with `make generate` (which runs `hack/generate`) instead of editing
these in place:

- `pkg/sandboxmatrix/grpc/pb/**` and `proto/**/*.pb.go` — protobuf output
- `pkg/generated/**`, `pkg/deploy/zz_generated_bindata.go`,
  `pkg/static/zz_generated_bindata.go` — codegen + staged-asset bindata
- `pkg/sandboxmatrix/api/v1alpha1/zz_generated_deepcopy.go` — CRD deepcopy

## Key Design Decisions

- **Zig build system**: manages Go compilation, cross-compilation and CGo-free
  compilation.
- **Multicall binary**: server, agent, kubectl, crictl and ctr all live in one
  `k8e` file and dispatch on `argv[0]` via symlink aliases. The sandbox CLI is
  deliberately *separate* (`k8e-sandbox-cli`) because agents invoke it directly.
- **Sandbox sessions are Kubernetes Pods**: each agent workload runs in an
  isolated Pod with a pluggable RuntimeClass (`gvisor` default, `kata`,
  `firecracker`).
- **Warm pools**: the `SandboxWarmPool` CRD pre-boots Pods to cut session
  startup latency; the default install stages `size: 1`.
- **gRPC-first**: `proto/sandbox/v1/sandbox.proto` defines `SandboxService`
  (34 RPCs). The CLI, the E2B layer, the MCP adapter and the dsh plugin are all
  wrappers over it.
- **Cross-invocation session reuse**: sessions are keyed by `tenantID`. The CLI
  persists `~/.k8e/sandbox/<tenant>/state.json` under `flock` and resolves the
  active session in `resolveSession()` (`pkg/sandboxcli/session.go`).
- **Sandbox namespace**: defaults to `sandbox-matrix`; set it with
  `--sandbox-namespace` / `K8E_SANDBOX_NAMESPACE`. Everything that touches
  sandbox Pods, Secrets and Services must resolve the same value.
- **Leader election**: warm-pool reconciliation, session GC and idle reaping run
  under a `sandboxmatrix-controller` Lease so only one node acts, while the
  gateway keeps serving on every node (`pkg/sandboxmatrix/leader.go`).
- **Configuration**: CLI flags and environment variables only; there is no
  standalone config file. `pkg/configfilearg` bridges the two by mapping config
  file keys onto flags.
- **API keys**: 64 hex characters, provisioned in the `sandbox-apikeys` Secret
  in the sandbox namespace. e2b SDK clients require the `e2b_` prefix; the
  server strips it.

## Agent skills and repo conventions

`pkg/sandboxcli/skills/k8e-sandbox/SKILL.md` is the single `//go:embed` source
of the sandbox skill. `k8e-sandbox-cli connect` installs it as
`k8e-sandbox/SKILL.md` into every supported harness:

| Harness | Destination |
|---------|-------------|
| claude | `~/.claude/skills/`, `~/.agents/skills/` |
| codex | `~/.codex/skills/`, `~/.agents/skills/` |
| pi | `~/.pi/agent/skills/`, `~/.agents/skills/` |
| dsh | `$DSH_HOME/skills` (default `~/.dsh/skills`), `~/.agents/skills/` |

Editing the markdown is enough — it is embedded with `//go:embed`, so there is
no generation step to run.

Repo conventions that engineering skills consume live under `docs/agents/`:

- **Issue tracker**: [docs/agents/issue-tracker.md](docs/agents/issue-tracker.md) — issues live in GitHub Issues, managed via the `gh` CLI
- **Triage labels**: [docs/agents/triage-labels.md](docs/agents/triage-labels.md) — `needs-triage`, `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`
- **Domain docs**: [docs/agents/domain.md](docs/agents/domain.md) — how skills consume KIPs; the status index is [docs/README.md](docs/README.md) (KIP-1 … KIP-26, statuses `Implemented` / `Partially implemented` / `Outdated`)

`skills-lock.json` at the repo root tracks *external* skills (source
`mattpocock/skills`); it does not cover the in-tree k8e-sandbox skill.
