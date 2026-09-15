# AGENTS.md

Guidance for AI coding agents (Claude Code, Codex, and others) working in this
repository.

> Claude Code reads this file directly — there is no separate `CLAUDE.md`. Keep
> `AGENTS.md` as the single source of agent guidance.

## What this is

K8E is a CNCF-conformant Kubernetes distribution packaged as a single `k8e`
binary under 100MB. Kubernetes is the substrate; the product on top of it is a
**sandbox matrix** for isolated AI-agent execution:

- warm-pooled sandbox Pods — RuntimeClass `gvisor` (default), `kata`,
  `firecracker`
- a gRPC `SandboxService` (34 RPCs) behind `sandbox-grpc-gateway:50051` with
  mTLS
- `k8e-sandbox-cli` — the CLI an agent drives from a shell (24 subcommands)
- an E2B-compatible HTTP surface, an MCP adapter, and skills/plugins for Claude
  Code, Codex, Pi and DeepSeek Harness (dsh)

**There is no `k8e sandbox` command group.** The `k8e` binary carries only
`k8e sandbox-apikey` and `k8e e2b-server`; every session/exec/file command lives
in `k8e-sandbox-cli`.

## Where things live

The build is a **Zig build graph** (`build.zig`) driving Go compilation;
`Makefile` is a thin wrapper over the common `zig build` steps.

```
.
├── build.zig / Makefile     # build graph / `make` wrappers
├── main.go                  # CLI entry; holds the //go:generate directives
├── cmd/                     # one package per binary entry point
│   ├── server/              # → bin/k8e (control plane + agent, no bundled runtimes)
│   ├── k8e/                 # release multicall binary (hack/package-cli)
│   ├── sandboxcli/          # → k8e-sandbox-cli, the agent-facing CLI
│   ├── agent/ kubectl/ cert/ ctr/ encrypt/ etcdsnapshot/ token/ …
├── pkg/
│   ├── cli/cmds/            # flag + command wiring (server.go, agent.go, e2b_server.go)
│   ├── server/ agent/ daemons/
│   ├── sandboxmatrix/       # the core: warm-pool + session reconciliation,
│   │                        #   leader.go (Lease), ratelimit/, api/v1alpha1 CRDs,
│   │                        #   grpc/ (SandboxService impl + orchestrator + pb/)
│   ├── sandboxcli/          # the 24 subcommands + skills/k8e-sandbox/SKILL.md
│   ├── sandbox/             # client/ (gRPC + TLS discovery), apikey/, e2b/
│   ├── sandboxmcp/          # MCP HTTP adapter (`mcp-serve`)
│   ├── sandboxlayer/        # CAS snapshot layerstore
│   ├── configfilearg/       # maps config-file keys onto flags
│   ├── deploy/ embedw/
│   └── …                    # apis, bootstrap, token, certmonitor, cgroups
├── proto/sandbox/v1/sandbox.proto   # SandboxService: 34 RPCs
├── sandboxd/                # Zig daemon running inside the sandbox Pod (:2024)
├── sandbox/ manifests/ plugins/deepseek-harness/
├── docs/                    # KIPs (docs/README.md = index) + docs/agents/
├── hack/                    # download / generate / package / e2e scripts
└── tests/                   # unit.go helpers + mcp/*.py smoke tests
```

## Request paths

Every client funnels into the same gRPC `SandboxService`:

```
k8e-sandbox-cli  run/exec/read/write/push/pull … ──┐
dsh → plugins/deepseek-harness (@k8e-sandbox/*) ───┤
e2b SDK → E2B HTTP/Connect (pkg/sandbox/e2b :3676) ─┤→ SandboxService :50051 (mTLS)
MCP client → k8e-sandbox-cli mcp-serve (:8443) ─────┘      │
                                                           ▼
                            Orchestrator — K8s API: warm-claim, create,
                            pause, destroy Pods
                                                           ▼
                            sandboxd :2024 inside the sandbox Pod
                                                           ▼
                            RuntimeClass gvisor | kata | firecracker
```

## Build & test

```bash
make                 # zig build all — everything incl. cross-platform CLIs
make k8e             # bin/k8e from cmd/server (no bundled runtimes)
make deps            # go mod tidy
make generate        # protobuf, CRD deepcopy, bindata
make format          # go fmt ./... + zig fmt build.zig
make test            # go test -v ./...
make clean           # bin dist build .zig-cache zig-out .cni-build
make package         # release artifacts
make package-cli     # CLI-only artifacts
make package-airgap  # air-gap package
make e2e E2E_PROFILE=l1
```

Not wrapped by `make`: `zig build sandboxd | sandboxcli | shim | runc | hcsshim
| cni | download`.

Outputs:

- `bin/` — `bin/k8e` from `cmd/server` plus the symlink aliases (`k8e-agent`,
  `k8e-server`, `kubectl`, `crictl`, `ctr`, …).
- `zig-out/bin/` — Zig-built `sandboxd` and cross-compiled
  `k8e-sandbox-cli-<os>-<arch>`.

Targets:

```bash
make test                                    # full suite
go test ./pkg/sandboxmatrix/... -count=1     # controller, grpc, ratelimit
go test ./pkg/sandboxcli/... -count=1        # CLI handlers, sessions, snapshots
go test ./pkg/sandbox/e2b/... -count=1       # E2B compat surface
go test ./pkg/sandbox/client/... -count=1
go test ./pkg/sandboxmcp/... -count=1
go test ./tests/... -v -count=1 -timeout 120s
go build ./pkg/...                           # fast compile check
go vet ./pkg/<subtree>/
```

### E2E

`make e2e E2E_PROFILE=<suite>` → `hack/e2e/run.sh <suite>`. Only
`hack/e2e/suites/l1.sh` and `l2.sh` exist today; `l3` is not implemented.

The harness brings up server + agent in containers and needs a **linux** binary
matching the Docker daemon's architecture. `bin/k8e` is built for the host, so
on macOS build one first:

```bash
E2E_GOARCH=amd64 ./hack/e2e/build-linux.sh          # → bin/k8e-linux-<arch>
E2E_BINARY=$PWD/bin/k8e-linux-amd64 hack/e2e/run.sh l1
```

`hack/e2e/Dockerfile` ships its own containerd 1.7.28 + runc 1.5.1, so the
`cmd/server` build — which bundles no runtime — runs **every** suite, not just
`l1`. `hack/package-cli`/`cmd/k8e` (the release multicall binary, which stages
`containerd`/`runc` from `hack/download`) is only needed for real installs.

## Generated code — do not hand-edit

Regenerate with `make generate` (runs `hack/generate`):

- `pkg/sandboxmatrix/grpc/pb/**`, `proto/**/*.pb.go` — protobuf
- `pkg/generated/**`, `pkg/deploy/zz_generated_bindata.go`,
  `pkg/static/zz_generated_bindata.go` — codegen + staged-asset bindata
- `pkg/sandboxmatrix/api/v1alpha1/zz_generated_deepcopy.go` — CRD deepcopy

## Conventions that will bite you

- **Sandbox namespace**: `sandbox-matrix` by default (`--sandbox-namespace` /
  `K8E_SANDBOX_NAMESPACE`). Everything that touches sandbox Pods, Secrets and
  Services must resolve the same value.
- **Sessions are Pods**, keyed by `tenantID`. The CLI persists
  `~/.k8e/sandbox/<tenant>/state.json` under `flock` and resolves the active
  session in `resolveSession()` — cross-invocation reuse is the normal path.
- **Leader election**: warm-pool reconciliation, session GC and idle reaping run
  under a `sandboxmatrix-controller` Lease, so exactly one node acts while the
  gateway keeps serving on every node.
- **Configuration**: CLI flags and environment variables only — there is no
  standalone config file. `pkg/configfilearg` bridges the two.
- **API keys**: 64 hex characters in the `sandbox-apikeys` Secret. e2b SDK
  clients require the `e2b_` prefix; the server strips it.
- **Multicall binary**: server, agent, kubectl, crictl and ctr all live in one
  `k8e` file and dispatch on `argv[0]` via symlink aliases. The sandbox CLI is
  deliberately separate because agents invoke it directly.

## Agent skills & repo docs

`pkg/sandboxcli/skills/k8e-sandbox/SKILL.md` is the single `//go:embed` source
of the sandbox skill; `k8e-sandbox-cli connect` installs it as
`k8e-sandbox/SKILL.md` into every supported harness. Editing the markdown is
enough — it is embedded, so there is no generation step.

| Harness | Destination |
|---------|-------------|
| claude | `~/.claude/skills/`, `~/.agents/skills/` |
| codex | `~/.codex/skills/`, `~/.agents/skills/` |
| pi | `~/.pi/agent/skills/`, `~/.agents/skills/` |
| dsh | `$DSH_HOME/skills` (default `~/.dsh/skills`), `~/.agents/skills/` |

Engineering conventions consumed by skills live under `docs/agents/`:

- [issue-tracker.md](docs/agents/issue-tracker.md) — issues live in GitHub
  Issues, managed via the `gh` CLI
- [triage-labels.md](docs/agents/triage-labels.md) — `needs-triage`,
  `needs-info`, `ready-for-agent`, `ready-for-human`, `wontfix`
- [domain.md](docs/agents/domain.md) — how skills consume KIPs; the status index
  is [docs/README.md](docs/README.md)

`skills-lock.json` tracks *external* skills (source `mattpocock/skills`); it
does not cover the in-tree k8e-sandbox skill.
