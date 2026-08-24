# Domain Docs

How the engineering skills should consume this repo's domain documentation.

## Before exploring, read these

- **[`docs/README.md`](../README.md)** — KIP index and status. **This is the source of truth** for which proposals are Implemented, Partially implemented, or Outdated.
- The KIP that touches the area you're working in (KIP-1 … KIP-25). Status on the KIP header must match the index; if they disagree, trust the index and fix the header.
- **`CLAUDE.md` / `AGENTS.md`** at the repo root — project overview, architecture, build/test instructions.
- **`CONTEXT.md`** — if it exists in the future, read it for domain language and glossary.

## File structure

```
/
├── CLAUDE.md / AGENTS.md        # Project overview + architecture
├── docs/
│   ├── README.md                # KIP index + status (read this first)
│   ├── kip-1-…kip-25-*.md       # Architectural Decision Records
│   ├── agents/                  # Skill conventions (this file; not KIPs)
│   └── assets/
├── pkg/
│   ├── sandboxcli/              # CLI command handlers (k8e-sandbox-cli)
│   ├── sandbox/client/          # gRPC client + skill installation
│   ├── sandbox/e2b/             # E2B-compatible HTTP/Connect layer (KIP-18)
│   ├── sandboxmatrix/           # Core sandbox orchestration (warm pools, sessions)
│   ├── sandboxlayer/            # CAS snapshot layerstore (KIP-16 M2)
│   ├── server/                  # Control plane daemon
│   ├── agent/                   # Agent daemon (kubelet + containerd)
│   └── ...
├── pkg/sandboxcli/skills/k8e-sandbox/  # Embedded SKILL.md (installed via connect)
├── plugins/deepseek-harness/    # dsh-k8e-sandbox plugin family (KIP-20)
├── sandbox/                     # Sandbox container runtime shim
└── sandboxd/                    # Runtime daemon (Zig)
```

Do **not** treat KIP-4 / KIP-5 as current design: they are Outdated (MCP). There is no `docs/sandbox-mcp-quickstart.md` and no `pkg/sandboxmcp/`.

## Key terms

When naming domain concepts (in issue titles, PRDs, refactors), use the terms as defined here:

- **Sandbox** — an isolated execution environment (Kubernetes pod with a pluggable RuntimeClass: gVisor, Kata, Firecracker).
- **Session** — a user-agent's live interaction with a sandbox, tracked via CRD (`SandboxSession`).
- **Warm pool** — pre-booted sandbox pods that reduce session startup latency (`SandboxWarmPool` CRD).
- **SKILL + CLI** — `k8e-sandbox-cli` commands; the shell-based bridge between AI agents and the sandbox orchestration layer (replaced MCP as of KIP-8).
- **Tenant ID** — the `tenantID` field used for cross-process session reuse via state files and `FindActiveSession()`.
- **Orchestrator** — the gRPC-side logic that creates/destroys pods and manages sandbox lifecycle.
- **E2B layer** — the embedded HTTP/Connect adapter (`pkg/sandbox/e2b`) that makes the official `e2b` SDK work against K8E (KIP-18).
- **Expose** — registering an in-sandbox service port for reverse-proxy through the k8e API Gateway (`/k8e/expose/<session>/<port>/`, KIP-24). Not Cloudflare tunnel, not a pod LoadBalancer.

## Flag ADR conflicts

If your output contradicts an existing ADR, surface it explicitly:

> _Contradicts KIP-3 (agentic AI sandbox matrix) — but worth reopening because…_

Check [`docs/README.md`](../README.md) first: contradicting an **Outdated** KIP (KIP-4, KIP-5) is expected; contradicting an **Implemented** KIP needs a reason.
