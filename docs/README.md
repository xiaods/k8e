# K8E docs

Architectural decisions live as **KIPs** (K8E Improvement Proposals). Every
content file under `docs/` has a `kip-N-*.md` filename. This index is the
source of truth for document status; the status line inside each KIP should
match the table below.

Status values:

| Status | Meaning |
|--------|---------|
| **Implemented** | Landed in tree. Later KIPs may have evolved the design; those notes are in the KIP. |
| **Partially implemented** | Core shipped; remainder is named in the KIP. |
| **Outdated** | Historical. Superseded or abandoned — keep for the decision trail, do not implement. |

Last audited: **2026-08-24**, against the in-tree sandbox matrix, gRPC proto,
`sandboxd`, CLI, E2B compat layer, and dsh plugin family.

## Index

### Platform / datastore

| KIP | Title | Status |
|-----|-------|--------|
| [KIP-1](kip-1-native-etcd-storage-client.md) | Native etcd storage client (replace kine) | Implemented |
| [KIP-2](kip-2-upgrade-dependencies-to-kubernetes-1.35.md) | Kubernetes 1.35 dependency upgrade | Implemented (tree now tracks **v1.35.5-k3s1**) |
| [KIP-6](kip-6-embedded-etcd-design.md) | Embedded etcd as sole datastore | Implemented |
| [KIP-7](kip-7-embedded-etcd-fuse.md) | Fuse official `embed.StartEtcd` (`pkg/embedw`) | Implemented |

### Sandbox matrix (core)

| KIP | Title | Status |
|-----|-------|--------|
| [KIP-3](kip-3-agentic-ai-sandbox-matrix.md) | Agentic AI Sandbox Matrix (sessions, warm pool, gRPC, Cilium) | Implemented |
| [KIP-25](kip-25-sandbox-warm-pool.md) | `SandboxWarmPool` CRD: fields, adaptive sizing, idle TTL | Implemented — default install stages a `size: 1` pool |
| [KIP-8](kip-8-skill-cli-replace-mcp.md) | SKILL + CLI replace MCP | Implemented |
| [KIP-9](kip-9-sandbox-workspace-manifest.md) | Workspace manifest (`--manifest` / `--git-repo`) | Implemented |
| [KIP-10](kip-10-sandbox-snapshot.md) | Workspace snapshot | Implemented — original client `tar.gz` evolved into KIP-16 M2 CAS layerstore |
| [KIP-11](kip-11-background-sandbox-execution.md) | Background exec + poll | Implemented — runs in the **same** session pod, capped (`maxBackgroundRuns`, default 5); dedicated background pool was not shipped |
| [KIP-12](kip-12-sandbox-ports-env-secrets.md) | Compute-layer positioning: env/secrets + ports | Implemented — env/secret_refs in-tree; ports delivered as [KIP-24](kip-24-sandbox-service-exposure.md) gateway reverse proxy (not the original Service+Ingress design) |
| [KIP-13](kip-13-immutable-root-package-isolation.md) | Immutable root: pip/npm isolation into `/workspace` | Partially implemented — Python lazy venv shipped; `npm_config_*` redirect **not** shipped |
| [KIP-14](kip-14-mtls-dynamic-cert-issuance.md) | mTLS dynamic client certs (Login RPC) | Implemented |
| [KIP-17](kip-17-sandbox-cli-profiles-and-apikey-ttl.md) | CLI multi-profile + API key TTL | Implemented |

### Completeness / architecture reviews

| KIP | Title | Status |
|-----|-------|--------|
| [KIP-15](kip-15-sandbox-api-perplexity-alignment.md) | Perplexity-aligned API completeness review | Partially implemented — compute surface mostly closed; remaining: ShareFile/artifacts, generic InstallPackages, egress-proxy credential injection |
| [KIP-16](kip-16-sandbox-architecture-lessons-ephemeral.md) | Lessons from ephemeral-sandbox (CAS, transcripts, obs, limits) | Implemented — P0/P1 matrix shipped; P2 leftovers named in the KIP |

### E2B / terminals / dsh / ingress

| KIP | Title | Status |
|-----|-------|--------|
| [KIP-18](kip-18-sandbox-e2b-compat.md) | E2B-compatible HTTP/Connect API | Implemented — remaining honest 501s documented in the KIP (PTY closed by KIP-19 M4) |
| [KIP-19](kip-19-sandbox-pty-terminal-primitive.md) | PTY terminal primitive (7 RPCs) | Implemented (M1–M4) |
| [KIP-20](kip-20-dsh-k8e-sandbox-plugin.md) | dsh-k8e-sandbox plugin family | Implemented (`@k8e-sandbox/*`) |
| [KIP-21](kip-21-host-advertise-ip-resolution.md) | Loopback-proof advertise-IP for ingress | Implemented |
| [KIP-22](kip-22-sandbox-advertise-hostname.md) | `--sandbox-advertise-hostname` SAN + cert regen | Implemented |
| [KIP-23](kip-23-endpointslice-migration.md) | Ingress bridge: Endpoints → EndpointSlice | Implemented (was “KIP-21 follow-up”; numbered KIP-23 to close the gap before KIP-24) |
| [KIP-24](kip-24-sandbox-service-exposure.md) | Service exposure via k8e API Gateway reverse proxy | Implemented (M1–M3); live-cluster e2e (M4) still pending |

### Outdated (do not implement)

| KIP | Title | Status |
|-----|-------|--------|
| [KIP-4](kip-4-sandbox-mcp-skill.md) | Sandbox MCP skill (`k8e sandbox-mcp`) | **Outdated** — superseded by [KIP-8](kip-8-skill-cli-replace-mcp.md). `pkg/sandboxmcp/` does not exist. |
| [KIP-5](kip-5-openclaw-sandbox-management.md) | OpenClaw via MCP | **Outdated** — depended on KIP-4 MCP. Agents consume the CLI + skill (KIP-8) or the dsh plugin (KIP-20). |

Agent-facing repo conventions live under [`docs/agents/`](agents/). Those files (`domain.md`, `issue-tracker.md`, `triage-labels.md`) are skill conventions, not KIPs.

## Numbering notes

- **KIP-23** was originally titled “KIP-21 follow-up”. It is a separate decision
  (Endpoints → EndpointSlice) that shipped with KIP-21 in PR #550/#551, and is
  numbered here so the sequence is unique.
- **KIP-24** was drafted as `sandbox-expose-tunnel.md`. Code, proto, CLI, and
  SKILL.md already called it KIP-24; the filename now matches.
- **KIP-25** was the unnumbered `sandbox-warm-pool.md` operator how-to. Default install now stages `manifests/sandbox-matrix/default-warm-pool.yaml` (`size: 1`).
- There is no KIP-26+. New proposals take the next free integer.

## How to add a KIP

1. Copy the header table from a recent KIP (`Author | Updated | Status`).
2. Start Status as `Proposed` or `Accepted`.
3. Link it from this index in the same turn as the file is added.
4. When it lands, set Status to `Implemented` (or `Partially implemented` with
   the remainder named). Do not leave “Accepted — implemented” as a third dialect.
5. When a later KIP supersedes the design, keep the original file, mark it
   **Outdated** or add an implementation-errata note — do not delete it.
