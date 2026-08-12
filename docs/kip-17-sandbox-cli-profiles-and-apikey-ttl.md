# KIP-17: Sandbox CLI multi-profile config + API key TTL

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-12 | Accepted (implemented with #538) |

## Summary

Make remote sandbox auth usable for humans and AI agents that talk to **more than one** K8E cluster, and make bootstrap API keys **short-lived by default**.

1. **Multi-profile `profiles.yaml`** — named endpoints + optional per-profile cert directories, selected by `--profile` / `K8E_SANDBOX_PROFILE`.
2. **API key TTL** — `k8e sandbox-apikey create` defaults to **30 days**; gateway Login rejects expired keys.
3. **Compatible with KIP-14 / issue #538 mTLS** — profiles only choose *where* certs live; Login still issues 90-day client certs with 30-day lazy renew.

## Motivation

Users need multiple gateway endpoints without juggling env vars. A naive `~/.k8e/config.yaml` **collides in name** with the server daemon flag file `/etc/k8e/config.yaml` (written at install / parsed by `configfilearg`). Those two files are **not the same format** and must stay distinct (same lesson as kip-4 sandbox-mcp: never overload `config.yaml`).

| File | Owner | Purpose | Format |
|------|-------|---------|--------|
| `/etc/k8e/config.yaml` | **k8e server/agent** | Daemon CLI flags (`write-kubeconfig-mode`, `tls-san`, …) | flag keys → `configfilearg` |
| `~/.k8e/sandbox/profiles.yaml` | **k8e-sandbox-cli** | Named remote gateways + cert dirs | `version` + `profiles` map |
| `~/.k8e/sandbox/config.json` | **k8e-sandbox-cli** | Last successful connect stamp | JSON `{mode,endpoint,agents}` |
| `~/.k8e/sandbox/{ca,client}.*` | **k8e-sandbox-cli** | mTLS material for active cert dir | PEM |

API keys remain immortal-or-TTL secrets in `sandbox-matrix/sandbox-apikeys` (not in any local yaml).

## Design

### Part A — Profile file

**Canonical path:** `~/.k8e/sandbox/profiles.yaml`  
(or `$K8E_SANDBOX_CERT_DIR/profiles.yaml` when cert dir is overridden)

**Path resolution (first hit wins):**

| Priority | Source |
|----------|--------|
| 1 | `K8E_SANDBOX_CONFIG` (explicit path) |
| 2 | `~/.k8e/sandbox/profiles.yaml` (canonical) |
| 3 | `~/.k8e/config.yaml` (**legacy only** — deprecation warning on stderr; migrate away) |

**Why not `config.yaml` for the CLI:**

- Server already owns `/etc/k8e/config.yaml` as the only well-known `config.yaml` for k8e.
- `configfilearg` strips unknown keys; dropping a profiles document there would silently break server startup if someone merged files.
- A verb-named file (`profiles.yaml`) next to certs matches “client multi-cluster” mental model.

**Schema (`version: 1`):**

```yaml
# ~/.k8e/sandbox/profiles.yaml  — NOT /etc/k8e/config.yaml
version: 1
current_profile: default   # used when --profile / env unset
profiles:
  default:
    endpoint: 127.0.0.1:50051
    # cert_dir omitted → default cache (~/.k8e/sandbox or K8E_SANDBOX_CERT_DIR)
  prod:
    endpoint: sandbox.prod.example:50051
    cert_dir: ~/.k8e/sandbox-prod
    device_name: laptop-prod
```

| Field | Required | Meaning |
|-------|----------|---------|
| `endpoint` | yes (for remote) | gRPC `host:port` |
| `cert_dir` | no | mTLS material dir for this profile (`ca.crt`, `client.crt`, `client.key`, `endpoint` stamp) |
| `device_name` | no | Login audit field (`K8E_SANDBOX_DEVICE_NAME`) |

**Selection order for active profile name:**

1. CLI `--profile`
2. `K8E_SANDBOX_PROFILE`
3. `current_profile` in the file
4. `default` if that profile exists

**Dial resolution order (endpoint / apikey / cert_dir):**

1. Explicit CLI flags (`--endpoint`, `--apikey`)
2. Process env (`K8E_SANDBOX_ENDPOINT`, `K8E_SANDBOX_APIKEY`, `K8E_SANDBOX_CERT_DIR`)
3. Active profile fields
4. Built-in defaults (local auto-discovery)

Applying a profile with `cert_dir` sets `K8E_SANDBOX_CERT_DIR` for the process so `pkg/sandbox/client` and last-used `config.json` stay co-located with the certs.

**Legacy:** `~/.k8e/sandbox/config.json` remains the last-used connection stamp written by `connect` / `login`. Profiles are the durable multi-cluster source of truth.

**Non-goals (this KIP):**

- Storing API keys inside profiles.yaml (still env / flag only)
- Writing profiles into `/etc/k8e/config.yaml` (server-only)
- Interactive profile wizard
- Syncing profiles via cluster CRDs

### Part B — API key TTL

**Storage:** Secret `sandbox-matrix/sandbox-apikeys` key `keys.json`.

**Format v2:**

```json
{
  "version": 2,
  "keys": {
    "my-agent": {
      "key": "k8e-…",
      "created_at": "2026-08-12T03:00:00Z",
      "expires_at": "2026-09-11T03:00:00Z",
      "ttl_days": 30
    }
  }
}
```

**Legacy v1** (`{"name":"k8e-…"}` flat map) still loads; keys without `expires_at` never expire until deleted.

**Create CLI:**

```bash
k8e sandbox-apikey create my-agent              # default TTL 30d
k8e sandbox-apikey create my-agent --ttl 90d
k8e sandbox-apikey create my-agent --ttl never  # no expiry
```

**List** returns names plus `expires_at` / `expired` (never prints the secret).

**Gateway:** on `loadAPIKeys` (every 30s) and Login lookup, only **non-expired** keys enter the reverse index. Expired keys fail Login with `invalid API key`.

Default client cert lifetime (KIP-14 / #538) stays **90 days** with lazy renew at **&lt;30 days** remaining — independent of bootstrap API key TTL.

## Acceptance

- [x] Proposal checked into `docs/kip-17-…`
- [x] Profile file load + `--profile` / env resolution
- [x] Per-profile `cert_dir` isolates mTLS material
- [x] API key create default TTL 30d; server rejects expired
- [x] Legacy keys.json and config.json still work
- [x] SKILL / README document profiles + TTL + mTLS paths
- [x] Unit tests for parse, TTL, profile resolve

## Implementation map

| Area | Path |
|------|------|
| Shared API key file codec | `pkg/sandbox/apikey/` |
| Profile load/resolve | `pkg/sandboxcli/profile.go` |
| CLI flags | `cmd/sandboxcli/main.go`, create/list |
| Gateway load | `pkg/sandboxmatrix/grpc/server.go` |
| Docs | this KIP, `skills`/embedded `SKILL.md`, README Step 4 |

## Alternatives considered

| Option | Why not |
|--------|---------|
| One cert dir, multiple endpoint stamps only | Still no named ergonomics; agents need stable `--profile prod` |
| Store keys in profiles.yaml | Secrets in user config files leak into backups/chat |
| Reuse name `~/.k8e/config.yaml` | Collides with server `/etc/k8e/config.yaml` in user mental model |
| Force-migrate legacy keys to 30d | Breaks existing clusters; migrate only on new create |
