# E2E verification — dsh-k8e-sandbox (KIP-20)

How to run the whole chain end-to-end: a K8E sandbox gateway, this plugin
installed into a dsh profile, and a real filesystem / exec / terminal operation
landing in a sandbox pod.

The TypeScript tree already typechecks against the dsh checkout (see
[`README.md`](README.md#typecheck)); this guide verifies the **runtime** path.

## Prerequisites

- A K8E cluster with the sandbox matrix deployed (the `sandbox-matrix` CRDs and
  controller), and the sandbox gRPC gateway listening on `:50051`.
- `k8e-sandbox-cli` on PATH (for `connect`, `ps`, `list`, `log` introspection).
- A dsh installation (`dsh` CLI on PATH).
- (Phase 2 only) `endpoint` reachable from the dsh host, and mTLS material from
  `connect`.

## Step 1 — gateway + mTLS

Start the gateway (via the K8E control plane or `k8e sandbox-gateway`) and
establish mTLS once:

```sh
# local node
k8e-sandbox-cli connect

# remote — API key from the server (default TTL 30d)
k8e sandbox-apikey create my-agent            # -> {"key":"k8e-..."}
k8e-sandbox-cli connect --endpoint <host>:50051 --apikey k8e-...
```

`connect` verifies the gateway, puts `k8e-sandbox-cli` on PATH, and writes
`ca.crt` / `client.crt` / `client.key` under `~/.k8e/sandbox/`. The gRPC client
reads the same certs, so **no separate credential setup** is needed.

Sanity-check the gateway:

```sh
k8e-sandbox-cli status          # -> {"available": true, ...}
k8e-sandbox-cli run 'echo hi'   # -> {"stdout":"hi\n", "exit_code":0, ...}
```

## Step 2 — install the bundle into a dsh profile

```sh
# from this workspace's parent (where the package dirs resolve)
dsh plugin --profile k8e add ./packages/dsh-k8e-sandbox-bundle

# verify the layer, then boot
dsh --profile k8e --dump-config    # shows a "# == @k8e-sandbox/dsh-k8e-sandbox-bundle" layer
dsh --profile k8e
```

The bundle's `cordis.patch.yml` mounts `@k8e-sandbox/dsh-k8e-sandbox` (owner),
`@k8e-sandbox/dsh-k8e-sandbox-fs`, and `@k8e-sandbox/dsh-k8e-sandbox-subprocess`.

> The profile's own `cordis.patch.yml` (`$DSH_HOME/profiles/k8e/cordis.patch.yml`)
> can override the owner row by `id: k8e-sandbox` to set `endpoint` (see below).

## Step 3 — configure endpoint (Phase 2 required)

Phase 1 (filesystem + single-shot exec) works through `k8e-sandbox-cli` with
no endpoint. Phase 2 (`spawnTerminal` + streaming `spawn`) needs the direct
gRPC endpoint, because the gRPC client does no local auto-discovery.

Add to the profile patch (restate the fields you keep):

```yaml
# $DSH_HOME/profiles/k8e/cordis.patch.yml
- id: k8e-sandbox
  name: '@k8e-sandbox/dsh-k8e-sandbox'
  config:
    endpoint: 127.0.0.1:50051     # or the remote gateway host:port
    cwd: /workspace
    runtimeClass: gvisor
```

Without `endpoint`, `getGrpcClient()` fails loud when a terminal is requested —
that is intentional, not a silent fallback.

## Step 4 — verify filesystem (`ctx.fs`)

In a dsh session under the `k8e` profile, ask the agent to read/write/list, or
drive the tools directly. Confirm the operation landed in the sandbox pod, not
the host:

```sh
k8e-sandbox-cli sessions          # an Active session exists
k8e-sandbox-cli list <sid>        # the written file shows up under /workspace
k8e-sandbox-cli read <sid> /workspace/hello.txt
```

## Step 5 — verify exec (`ctx.subprocess` / bash)

Ask the agent to run a shell command (the `bash` tool). It should complete and
report output. Confirm from the outside:

```sh
k8e-sandbox-cli log <sid>         # the exec transcript shows the command
k8e-sandbox-cli ps <sid>          # (during a long command) the process is in the pod
```

## Step 6 — verify terminal (`spawnTerminal`, Phase 2)

Open a persistent terminal (the `terminal` tool) in the dsh session. It should
allocate a PTY in the sandbox pod:

```sh
k8e-sandbox-cli ps <sid>          # the shell (e.g. bash) is the session leader
```

Inside the terminal, check job control and window size:

```sh
tty                               # -> /dev/pts/N  (a real PTY, not a pipe)
stty size                         # reflects rows/cols sent by the harness
# Ctrl-C should interrupt the foreground job (SIGINT via the PTY)
```

## What to check

| Capability | Phase | Success signal |
|---|---|---|
| filesystem read/write/list/edit | 1 | files appear under `/workspace` in the sandbox |
| exec (`bash`) | 1 (CLI) / 2 (streaming) | stdout streams back, exit code correct |
| terminal (`spawnTerminal`) | 2 | `/dev/pts/N`, Ctrl-C / resize / foreground signal work |
| teardown | 1 | disposing the dsh session destroys the sandbox session (`k8e-sandbox-cli sessions` empties) |

## Known limitations

- The KIP-19 `/exec/stream` sandboxd endpoint frames output as raw `data: <raw>`
  SSE; output containing `\n\n` can split a frame. `GrpcK8eClient.execStream`
  inherits this until `/exec/stream` is switched to base64 framing (as
  `/pty/stream` already does). `/pty/stream` (terminal output) is unaffected.
- The gRPC client reads static certs from `~/.k8e/sandbox/`; it does not yet do
  the KIP-14 lazy client-cert renewal. Long-running sessions should re-run
  `k8e-sandbox-cli connect` before the 90-day cert expires (auto-renew kicks in
  at <30 days for CLI-managed flows).
- Terminals are pod-scoped: pausing/resuming or recycling the session pod kills
  them (KIP-19).
