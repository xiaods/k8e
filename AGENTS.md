# AGENTS.md

This file provides guidance to Codex (Codex.ai/code) when working with code in this repository.

## Project Overview

K8E is a CNCF-conformant Kubernetes distribution packaged as a single binary under 100MB, purpose-built for secure, isolated AI agent execution at scale. It provides built-in sandbox orchestration (warm pools, session management), a gRPC gateway for sandbox operations, a CLI command group (`k8e sandbox`) bridging AI agents to sandbox infrastructure, and client SDKs in Python and TypeScript.

## Architecture (Big Picture)

The repository is organized as a **Zig-based Go project**:

```
.
├── build.zig                      # Zig build definitions (Go compilation, cross-compilation)
├── main.go                        # CLI entry point — registers all commands
├── cmd/                           # Individual command entry points
│   ├── server/                    # Control plane + optional agent
│   ├── agent/                     # Agent-only (kubelet + containerd)
│   ├── kubectl/                   # kubectl wrapper
│   └── sandbox-gateway.go         # Sandbox gRPC gateway (cli/cmds)
├── pkg/
│   ├── cli/cmds/                  # CLI flag definitions + command wiring
│   │   ├── root.go                # App setup, global flags
│   │   ├── server.go              # Server struct + all server flags
│   │   ├── agent.go               # Agent struct + all agent flags
│   │   ├── sandbox.go             # sandbox CLI command group
│   │   └── sandbox_gateway.go     # sandbox-gateway command
│   ├── server/                    # Server daemon orchestration
│   ├── agent/                     # Agent daemon orchestration
│   ├── daemons/                   # Individual daemon implementations
│   ├── sandboxmatrix/             # Core sandbox orchestration
│   │   ├── controller.go          # Warm pool reconciler, session GC
│   │   ├── api/v1alpha1/          # CRD types (SandboxMatrix, SandboxWarmPool, SandboxSession)
│   │   └── grpc/
│   │       ├── server.go          # gRPC SandboxService (create/destroy/exec sessions)
│   │       ├── orchestrator.go    # Orchestration logic (sub-agents, confirm actions)
│   │       └── pb/                # Generated protobuf Go code
│   ├── sandbox/client/           # gRPC client + skill installation
│   │   ├── client.go              # gRPC client with TLS auto-discovery
│   │   └── install.go             # Skill installation for claude code/codex/pi
│   ├── sandboxcli/                # CLI command handlers for k8e sandbox
│   │   ├── commands.go            # 10 sandbox command handlers
│   │   ├── session.go             # Session state persistence + flock locking
│   │   ├── snapshot.go            # Workspace snapshot save/restore
│   │   └── manifest.go            # Declarative workspace manifest
│   ├── sandboxmatrix/grpc/        # gRPC gateway — proxies exec/file ops to sandboxd pods
│   ├── configfilearg/             # Config file argument parsing
│   ├── deploy/                    # Kubernetes manifests and Helm charts
│   ├── apis/                      # Internal API types
│   ├── bootstrap/                 # Cluster bootstrap (token generation)
│   ├── token/                     # Token management
│   └── ...                        # certmonitor, secretsencrypt, cgroups, vpn, etc.
├── proto/sandbox/v1/              # Protobuf definitions for sandbox gRPC service
├── sandbox/                       # Sandbox container runtime shim (sandboxd)
├── sandboxd/                      # Runtime daemon in Zig (exec, files, networking)
│── skills/k8e-sandbox/            # SKILL.md for agent CLI integration
└── tests/unit.go                  # Test helper utilities
```

### Key Architectural Flows

**Agent submitting work via CLI:**
```
AI Agent (shell command)
  → k8e sandbox run "code" (direct gRPC with TLS)
    → sandbox-grpc-gateway:50051 (TLS gRPC)
      → Orchestrator (K8s API: create/destroy pods)
      → sandboxd HTTP proxy (port 2024 inside sandbox pods)
        → Isolated container (gVisor/Kata/Firecracker)
```

**Direct SDK usage:**
```
Python/TypeScript SDK
  → gRPC SandboxServiceClient (direct, no MCP overhead)
    → sandbox-grpc-gateway
      → Same path as above
```

## Building

```bash
# Full build (simulator + all targets)
make            # or: zig build all

# Build k8e binary only
make k8e        # or: zig build k8e

# Regenerate protobuf/generated code
make generate   # or: zig build generate

# Format code
make format     # or: zig build fmt

# Clean build artifacts
make clean      # or: zig build clean

# Package for distribution
make package          # Full package
make package-cli      # CLI only
make package-airgap   # Airgap package
```

Build outputs go to `./zig-out/`.

## Running & Testing

```bash
# Run the full test suite
make test       # or: zig build test

# Run a specific Go test (examples)
go test ./pkg/server/... -run TestServerStart -v -count=1
go test ./pkg/sandboxmatrix/... -v -count=1
go test ./pkg/sandbox/client/... -v -count=1
go test ./tests/... -v -count=1 -timeout 120s

# Verify compilation of a single package
go build ./pkg/sandboxmatrix/
go vet ./pkg/sandbox/client/
```

Integration tests require a running K8E cluster and are invoked via `make test`.

## Key Design Decisions

- **Zig build system**: Manages Go compilation, cross-compilation, and CGo-free compilation
- **Single binary**: All components (server, agent, CLI tools) compile into one `k8e` binary; behavior is determined by subcommand
- **No separate config files for most things**: Configuration is passed via CLI flags and environment variables; a config file loader (`pkg/configfilearg`) bridges the two
- **Sandbox sessions are Kubernetes pods**: Each agent workload runs in an isolated pod with a pluggable RuntimeClass (gVisor, Kata, Firecracker)
- **gRPC-first**: The sandbox API is gRPC; the CLI commands and Python/TypeScript SDKs are thin wrappers around it
- **Warm pools**: Pre-booted sandbox pods reduce session startup latency; the `SandboxWarmPool` CRD lets users configure pool size and runtime per namespace
- **Cross-process session reuse**: The `tenantID` field on sessions allows multiple CLI calls to share the same sandbox session via state files and `FindActiveSession()`

## Agent skills

See `docs/agents/` for the configuration files that engineering skills consume:

- **Issue tracker**: [docs/agents/issue-tracker.md](docs/agents/issue-tracker.md) — issues live in GitHub Issues, managed via `gh` CLI
- **Triage labels**: [docs/agents/triage-labels.md](docs/agents/triage-labels.md) — standard five-role label vocabulary
- **Domain docs**: [docs/agents/domain.md](docs/agents/domain.md) — how skills should consume `docs/adr/`, key terms, and repo structure