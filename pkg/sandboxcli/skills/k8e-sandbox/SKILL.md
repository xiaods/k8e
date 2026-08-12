---
name: k8e-sandbox
description: "Execute a goal inside the K8E sandbox (gVisor/Kata/Firecracker). Use when the user runs /k8e-sandbox <goal>, $k8e-sandbox <goal>, or asks to run/execute/test code safely off the host."
argument-hint: "<goal>"
user-invocable: true
---

# /k8e-sandbox

Treat this as the **k8e-sandbox** skill command.

**Invocation (same skill, different harness prefixes):**
- Claude Code: `/k8e-sandbox <goal>`
- Codex: `$k8e-sandbox <goal>` (or pick from `/skills`)
- Pi: `/skill:k8e-sandbox <goal>` (or `/k8e-sandbox` when skill commands are enabled)

**Goal from invocation arguments:**

```
$ARGUMENTS
```

If `$ARGUMENTS` is empty and no goal is otherwise provided, ask the user for a sandbox goal and **stop** (do not invent work).

## Binary naming (read this first)

The downloaded file name carries a **platform suffix** — pick the one for the user's machine:

| Platform | Download name |
|----------|---------------|
| Linux amd64 | `k8e-sandbox-cli-linux-amd64` |
| Linux arm64 | `k8e-sandbox-cli-linux-arm64` |
| macOS amd64 | `k8e-sandbox-cli-darwin-amd64` |
| macOS arm64 | `k8e-sandbox-cli-darwin-arm64` |
| Windows amd64 | `k8e-sandbox-cli-windows-amd64.exe` |

It is the **same binary** this skill invokes as `k8e-sandbox-cli` — just under the platform-suffixed name. To make the plain name work without renaming, create a **symlink** (do not rename the file):

```bash
# Example for Linux amd64 — substitute the platform name for other OS/arch
curl -sLO https://github.com/xiaods/k8e/releases/latest/download/k8e-sandbox-cli-linux-amd64
chmod +x k8e-sandbox-cli-linux-amd64
ln -s k8e-sandbox-cli-linux-amd64 k8e-sandbox-cli          # symlink, original file stays
# optionally move both into a PATH dir, e.g. ~/.local/bin/
./k8e-sandbox-cli ... connect                             # connect installs this skill + ensures PATH
```

(Windows: use `mklink k8e-sandbox-cli.exe k8e-sandbox-cli-windows-amd64.exe` in cmd.)

From then on, this skill and all examples use the plain name `k8e-sandbox-cli` — same binary.

If you only see a platform-suffixed name in the user's environment (no symlink yet), use that file directly: `./k8e-sandbox-cli-linux-amd64 status` etc. All spellings are interchangeable; never tell the user they are missing a second binary.

## Hard rules

1. **All code and shell execution goes through `k8e-sandbox-cli`** — never run `python3`, `node`, `pip`, `npm`, `curl`, compilers, or tests on the host for this goal.
2. Prefer auto session mode: `k8e-sandbox-cli run "..."` (creates/reuses session).
3. Parse JSON with `jq` unless `--raw` is used.
4. If the gateway is unreachable, tell the user to run `k8e-sandbox-cli connect` (local) or `k8e-sandbox-cli connect --endpoint <host>:50051 --apikey <key>` (remote). Multi-cluster: `--profile <name>` / `~/.k8e/sandbox/profiles.yaml` (KIP-17).

## Auth & multi-profile (KIP-14 / KIP-17 / #538)

**Do not confuse these files:**

| Path | Who | What |
|------|-----|------|
| `/etc/k8e/config.yaml` | **k8e server/agent** | Daemon flags only |
| `~/.k8e/sandbox/profiles.yaml` | **k8e-sandbox-cli** | Named gateways / cert dirs |
| `~/.k8e/sandbox/config.json` | **k8e-sandbox-cli** | Last connect stamp |

**mTLS bootstrap:** first remote connect/login uses an API key once; CLI stores `ca.crt` + `client.crt` + `client.key` (private key never leaves the machine). Client certs last **90 days** and auto-renew when **&lt;30 days** remain. API keys default to **30-day TTL** (`k8e sandbox-apikey create name`, override with `--ttl 90d|never`).

**Profiles** (`~/.k8e/sandbox/profiles.yaml`, override with `K8E_SANDBOX_CONFIG`):

```yaml
# ~/.k8e/sandbox/profiles.yaml  — NOT /etc/k8e/config.yaml
version: 1
current_profile: default
profiles:
  default:
    endpoint: 10.0.0.1:50051
  prod:
    endpoint: sandbox.prod.example:50051
    cert_dir: ~/.k8e/sandbox-prod
    device_name: laptop-prod
```

```bash
k8e-sandbox-cli --profile prod connect --apikey k8e-...
k8e-sandbox-cli --profile prod run 'echo hi'
# or: export K8E_SANDBOX_PROFILE=prod
```

Priority: flags → env (`K8E_SANDBOX_ENDPOINT` / `APIKEY` / `CERT_DIR` / `PROFILE`) → profile → defaults. Cert dir: `K8E_SANDBOX_CERT_DIR` → `~/.k8e/sandbox`.

## Procedure (always)

### 1. Pre-flight

```bash
command -v k8e-sandbox-cli >/dev/null || { echo "k8e-sandbox-cli not on PATH; run connect again"; exit 1; }
k8e-sandbox-cli status
```

Require `"available": true`. If not available, stop and instruct the user to `connect`.

### 2. Plan

Decompose `$ARGUMENTS` into sandbox-safe steps (install deps → write files → run code → read outputs).

### 3. Execute (examples)

```bash
# Shell / bash (default)
k8e-sandbox-cli run 'echo hello'

# Python
k8e-sandbox-cli run "print(1+1)" --lang python

# Multi-line / files
k8e-sandbox-cli run 'pip install pandas' --lang bash
# write via stdin:
# cat analysis.py | k8e-sandbox-cli write <session_id> /workspace/analysis.py
# k8e-sandbox-cli run 'python3 /workspace/analysis.py' --session-id <session_id>

# Background exec (returns run_id immediately)
k8e-sandbox-cli run 'sleep 30; echo done' --background
k8e-sandbox-cli poll <run-id>            # wait + stream output

# Tenant reuse (share one session across CLI calls)
k8e-sandbox-cli run 'echo hi' --tenant my-project

# Sub-agent: child session sharing parent pod + workspace (no new pod)
k8e-sandbox-cli subagent <parent-sid>
```

Useful commands: `run`, `write`, `read`, `list`, `create`, `get`, `sessions`, `destroy`, `status`, `log`, `events`, `ps`, `poll`, `subagent`, `confirm`, `approve`, `snapshot`, `benchmark`, `catalog`.

### 4. Report

Show stdout/stderr and exit codes from the CLI JSON. Do not claim host-side execution.

## One-time setup (if not connected)

```bash
# Local K8E node
k8e-sandbox-cli connect

# Remote — API key from server (default TTL 30d)
k8e sandbox-apikey create my-agent
# → {"name":"my-agent","key":"k8e-…","ttl_days":30,"expires_at":"…"}
# k8e sandbox-apikey create my-agent --ttl never   # optional non-expiring

k8e-sandbox-cli connect --endpoint <server-ip>:50051 --apikey k8e-...
# Multi-cluster: k8e-sandbox-cli --profile prod connect --apikey k8e-...
```

`connect` authenticates (mTLS), verifies the gateway, puts `k8e-sandbox-cli` on PATH when needed (symlink to `~/.local/bin/k8e-sandbox-cli`), and installs this skill into Claude / Codex / Pi discovery paths.

## Command reference

| Command | Purpose |
|---------|---------|
| `k8e-sandbox-cli --profile <name> …` | Use named profile from `~/.k8e/sandbox/profiles.yaml` |
| `k8e-sandbox-cli connect` | Local/remote auth + install this skill into agent harnesses |
| `k8e-sandbox-cli connect --skill-only` | Re-install this skill only (no gateway dial) |
| `k8e-sandbox-cli login` | Remote mTLS only (no skill install); optional `--device-name` |
| `k8e-sandbox-cli status` | Gateway + session probe |
| `k8e-sandbox-cli run <code>` | Exec in sandbox (`--lang`, `--timeout`, `--raw`, `--session-id`, `--tenant`, `--background`, `--manifest`, `--git-repo`, `--allowed-hosts`) |
| `k8e-sandbox-cli create` | Manual session (`--runtime`, `--env`, `--secret`, `--allowed-hosts`, `--manifest`, `--git-repo`) |
| `k8e-sandbox-cli get <sid>` | Session introspection (phase, runtime, env keys) |
| `k8e-sandbox-cli sessions` | List sessions |
| `k8e-sandbox-cli write/read/list` | Workspace files; `list --since <ts>` for changed-file diff |
| `k8e-sandbox-cli log <sid>` | Replay exec transcript (`--offset`, `--limit`, `--follow`) |
| `k8e-sandbox-cli events <sid>` | Read daemon NDJSON event stream (`--limit`) |
| `k8e-sandbox-cli ps <sid>` | List processes in the sandbox pod (pid, comm, state) |
| `k8e-sandbox-cli poll <run-id>` | Poll a background run (`--follow`) |
| `k8e-sandbox-cli subagent <parent-sid>` | Spawn child session (shares parent's pod + workspace — no new pod) |
| `k8e-sandbox-cli confirm <sid> <action>` | Gate destructive action on human approval (`--timeout`, `--no-wait`) |
| `k8e-sandbox-cli approve <aid>` | Approve a pending confirm (`--reject`, `--reason`) |
| `k8e-sandbox-cli snapshot save <sid> <name>` | Save workspace snapshot (content-addressed, dedup'd) |
| `k8e-sandbox-cli snapshot list` | List saved snapshots |
| `k8e-sandbox-cli snapshot restore <name>` | New session from a snapshot (`--base <snap>` for incremental) |
| `k8e-sandbox-cli snapshot delete <name>` | Delete a snapshot |
| `k8e-sandbox-cli benchmark` | Warm-pool latency metrics (`--pool-size`, `--iterations`) |
| `k8e-sandbox-cli catalog` | Emit machine-readable command surface (SDK generation) |
| `k8e-sandbox-cli destroy <sid>` | Tear down session |

Default run output is JSON: `stdout`, `stderr`, `exit_code`, `session_id`. Use `--raw` to stream text.

## Session modes

| Mode | How | State |
|------|-----|-------|
| Auto (default) | `run` without session id | `~/.k8e/sandbox/default/state.json` |
| Tenant | `--tenant my-project` | `~/.k8e/sandbox/{tenant}/state.json` |
| Manual | `create` → `run --session-id` → `destroy` | none |

## Egress

Default allowed hosts (kernel eBPF): `pypi.org`, `files.pythonhosted.org`, `registry.npmjs.org`, `github.com`, `raw.githubusercontent.com`. Override with `create --allowed-hosts`.

## Security red lines

- `--env` is for non-sensitive config only (stored on CRD). Use `--secret ENV=secret:key` for secrets.
- Never pass host secrets into sandbox flags in chat logs if avoidable.
- Never `sudo` via sandbox CLI.
- Destructive sandbox actions require `confirm` → `approve` (human in the loop); don't skip it.

## Error quick reference

| Exit | Meaning | Action |
|------|---------|--------|
| 2 | TLS / cert / unreachable | `connect` again; clear cert dir `ca.crt` on trust mismatch; check profile `cert_dir` |
| 1 | Command/session error | Read JSON error; recreate session if gone; re-create API key if TTL expired |
| 8 | ResourceExhausted | Wait or free warm pool capacity |

## Your role when this skill is active

**Do:** execute `$ARGUMENTS` entirely via `k8e-sandbox-cli`; prefer `run`; use `--lang python` for Python; use `--raw` for long streams; show real CLI output.

**Don't:** run the goal on the host; skip pre-flight; invent successful output without running the CLI.
