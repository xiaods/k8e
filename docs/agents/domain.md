# Domain Docs

How the engineering skills should consume this repo's domain documentation.

## Before exploring, read these

- **`docs/`** — Architectural Decision Records (KIP-1 through KIP-5). Read the ADR that touches the area you're working in.
- **`CLAUDE.md`** at the repo root — project overview, architecture, build/test instructions.
- **`CONTEXT.md`** — if it exists in the future, read it for domain language and glossary.

## File structure

This is a single-context repo:

```
/
├── CLAUDE.md                    # Project overview + architecture
├── docs/                    # KIP-1 … KIP-6 (architectural decisions)
│   ├── kip-1-native-etcd-storage-client.md
│   ├── kip-2-upgrade-dependencies-to-kubernetes-1.35.md
│   ├── kip-3-agentic-ai-sandbox-matrix.md
│   ├── kip-4-sandbox-mcp-skill.md
│   ├── kip-5-openclaw-sandbox-management.md
│   ├── kip-6-embedded-etcd-remove-kine-sqlite.md
│   └── sandbox-mcp-quickstart.md
├── docs/kip-6-embedded-etcd-design.md      # Embedded etcd 设计方案
├── docs/kip-7-embedded-etcd-fuse.md        # Embedded etcd 融合方案（集成官方 embed 包）
├── pkg/                         # Main source code
│   ├── sandboxmcp/              # MCP server bridging AI agents → sandbox
│   ├── sandboxmatrix/           # Core sandbox orchestration (warm pools, sessions)
│   ├── server/                  # Control plane daemon
│   ├── agent/                   # Agent daemon (kubelet + containerd)
│   └── ...
├── skills/k8e-sandbox-skill/   # MCP skill files for agent install
├── sdk/python/                  # Python gRPC client SDK
├── sandbox/                     # Sandbox container runtime shim
└── sandboxd/                    # Runtime daemon (Zig)
```

## Key terms

When naming domain concepts (in issue titles, PRDs, refactors), use the terms as defined here:

- **Sandbox** — an isolated execution environment (Kubernetes pod with a pluggable RuntimeClass: gVisor, Kata, Firecracker).
- **Session** — a user-agent's live interaction with a sandbox, tracked via CRD (`SandboxSession`).
- **Warm pool** — pre-booted sandbox pods that reduce session startup latency (`SandboxWarmPool` CRD).
- **MCP** — Model Context Protocol; the JSON-RPC bridge between AI agents and the sandbox orchestration layer.
- **Tenant ID** — the `tenantID` field used for cross-process session reuse via `FindActiveSession()`.
- **Orchestrator** — the gRPC-side logic that creates/destroys pods and manages sandbox lifecycle.

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly:

> _Contradicts KIP-3 (agentic AI sandbox matrix) — but worth reopening because…_
