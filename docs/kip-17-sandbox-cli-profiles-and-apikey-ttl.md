# KIP-17: Sandbox CLI multi-profile config + API key TTL

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-08-12 | Accepted (implemented with #538) |

## Summary

Make remote sandbox auth usable for humans and AI agents that talk to **more than one** K8E cluster, and make bootstrap API keys **short-lived by default**.

1. **Multi-profile `config.yaml`** — named endpoints + optional per-profile cert directories, selected by `--profile` / `K8E_SANDBOX_PROFILE`.
2. **API key TTL** — `k8e sandbox-apikey create` defaults to **30 days**; gateway Login rejects expired keys.
3. **Compatible with KIP-14 / issue #538 mTLS** — profiles only choose *where* certs live; Login still issues 90-day client certs with 30-day lazy renew.

## Motivation

Issue #538 sketched:

```yaml
# ~/.k8e/config.yaml
profiles:
  default:
    endpoint: 10.0.0.1:50051
  prod:
    endpoint: prod.k8e.example:50051
    cert_dir: ~/.k8e/sandbox-prod
```

Today the CLI stores a single last-used connection in `~/.k8e/sandbox/config.json` and a single cert cache. Switching clusters requires manual env overrides and easily reuses the wrong mTLS material.

API keys are currently immortal secrets in `sandbox-matrix/sandbox-apikeys`. Bootstrap tokens should be revocable by time as well as by delete.

## Design

### Part A — Profile file

**Path resolution (first hit wins):**

| Priority | Source |
|----------|--------|
| 1 | `K8E_SANDBOX_CONFIG` (absolute or relative path) |
| 2 | `~/.k8e/config.yaml` |

k8e does **not** use `XDG_CONFIG_HOME`; paths stay under `~/.k8e/` (or explicit `K8E_SANDBOX_*` overrides).

**Schema (`version: 1`):**

```yaml
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

- Storing API keys inside `config.yaml` (still env / flag only)
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
| Store keys in config.yaml | Secrets in user config files leak into backups/chat |
| Force-migrate legacy keys to 30d | Breaks existing clusters; migrate only on new create |
