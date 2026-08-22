# dsh-k8e-sandbox

Out-of-tree DeepSeek Harness plugin family (KIP-20). These packages let a dsh
profile route its filesystem and subprocess execution world into k8e-sandbox
via `dsh plugin --profile <name> add`.

## Packages

| Package | Role | `ctx` key |
|---|---|---|
| [`@k8e-sandbox/dsh-k8e-sandbox`](packages/dsh-k8e-sandbox) | Sandbox owner service (session lifecycle + shared CLI/gRPC client) | `ctx.k8eSandbox` |
| [`@k8e-sandbox/dsh-k8e-sandbox-client`](packages/dsh-k8e-sandbox-client) | Transport: `CliK8eClient` (Phase 1) + `GrpcK8eClient` (Phase 2) | library |
| [`@k8e-sandbox/dsh-k8e-sandbox-fs`](packages/dsh-k8e-sandbox-fs) | Filesystem seam provider | `ctx.fs` |
| [`@k8e-sandbox/dsh-k8e-sandbox-subprocess`](packages/dsh-k8e-sandbox-subprocess) | Subprocess seam provider (streaming exec + `spawnTerminal`) | `ctx.subprocess` |
| [`@k8e-sandbox/dsh-k8e-sandbox-tool`](packages/dsh-k8e-sandbox-tool) | Model-surface tools (session status/destroy, exec, background + poll, service expose/unexpose, egress allow-hosts) | `ctx.tools` |
| [`@k8e-sandbox/dsh-k8e-sandbox-bundle`](packages/dsh-k8e-sandbox-bundle) | Installable bundle (`dsh.bundle.patch` → `cordis.patch.yml`) | — |

## Status

Phase 1 (CLI transport) and Phase 2 (direct gRPC: `spawnTerminal` + streaming
`spawn`) are implemented, plus KIP-24 service exposure: an in-sandbox service
port is proxied through the k8e API Gateway (`expose`/`unexpose`), and the
session egress allowlist is freely configurable live (`allow-hosts`). The tree
typechecks cleanly against the dsh checkout; runtime e2e against a live K8E
gateway is in [`e2e.md`](e2e.md).

### Model tools (`ctx.tools`)

| Tool | Purpose |
|------|---------|
| `k8e_sandbox_session_status` | Current session (availability, id, tenant, pod reachability) |
| `k8e_sandbox_session_destroy` | Destroy the session (idempotent) |
| `k8e_sandbox_exec` | Foreground command (stdout/stderr/exit code/duration) |
| `k8e_sandbox_run_background` | Async command; poll with `k8e_sandbox_poll` |
| `k8e_sandbox_poll` | Poll a background run to completion |
| `k8e_sandbox_expose` (KIP-24) | Expose an in-sandbox service port through the k8e API Gateway; returns the public URL |
| `k8e_sandbox_unexpose` (KIP-24) | Remove a public tunnel for a port (idempotent) |
| `k8e_sandbox_allow_hosts` (KIP-24) | Freely configure the session egress allowlist (live CNP re-apply) |

## Test

Fake-ctx runtime tests (no harness, gateway, or cluster): `npm test` mounts the
fs/subprocess providers on a fake `ctx` with a fake owner + fake client and
asserts their mapping, and unit-tests the exec SSE decoder
(`tests/{fs,subprocess,grpc-sse}.test.mjs`, bundled on the fly by
`scripts/test.mjs`).

One-time setup:

```sh
npm install               # devDependencies: esbuild + @grpc/grpc-js + @grpc/proto-loader
scripts/setup-links.sh    # symlink @deepseek-ai/* -> the dsh checkout's lib/
npm test                  # bundle + run tests/*.test.mjs
```

`npm install` prunes extraneous `node_modules` symlinks, so re-run
`scripts/setup-links.sh` after any reinstall. `DSH_CHECKOUT` overrides the dsh
path (default `../../../deepseek-harness`).

## Typecheck

The TypeScript packages target dsh's in-box types (`@deepseek-ai/dsh-fs`,
`@deepseek-ai/dsh-subprocess`, `@deepseek-ai/cordis`, `@deepseek-ai/schemastery`),
which resolve from the dsh installation at profile boot time — not from this
workspace's `node_modules`. To typecheck against a local dsh checkout:

```sh
# 1. Install the gRPC runtime deps (only the client package needs them):
npm install --no-save --cache "$(mktemp -d)" @grpc/grpc-js @grpc/proto-loader

# 2. Generate a paths-mapped check config and run dsh's tsc. The generated
#    tsconfig.check.json maps @deepseek-ai/* to <dsh>/vendor/*/lib/types/*.d.ts
#    and packages/<group>/*/lib/types/*.d.ts; it is gitignored.
python3 - <<'PY'
import json, os
DSH = '/path/to/deepseek-harness'
K8E = os.getcwd()
groups = ['core','prompt','llm','shell','terminal','subprocess','e2b','code-runtime','fs','lsp','skill',
          'compaction','context','goal','feedback','schedule','guard','plan','preset','subagent','jobs',
          'workflow','web','attachment','spill','todo','bundle','extensions','sandbox','hooks','session',
          'session-query','settings','credentials','acp','storage','workspace','sdk','interaction','boot',
          'examples','util','mcp','identity','test-support','host','client','runtime-diagnostics','typert','api']
paths = {
  '@deepseek-ai/cordis': [os.path.join(DSH,'vendor','cordis','lib','types','index.d.ts')],
  '@deepseek-ai/cosmokit': [os.path.join(DSH,'vendor','cosmokit','lib','types','index.d.ts')],
  '@deepseek-ai/schemastery': [os.path.join(DSH,'vendor','schemastery','lib','types','index.d.ts')],
  '@deepseek-ai/dsh-*': [os.path.join(DSH,'packages',g,'*','lib','types','index.d.ts') for g in groups],
  '@k8e-sandbox/dsh-k8e-sandbox': [os.path.join(K8E,'packages','dsh-k8e-sandbox','src')],
  '@k8e-sandbox/dsh-k8e-sandbox-client': [os.path.join(K8E,'packages','dsh-k8e-sandbox-client','src')],
  '@k8e-sandbox/dsh-k8e-sandbox-client/grpc': [os.path.join(K8E,'packages','dsh-k8e-sandbox-client','src','grpc.ts')],
  '@k8e-sandbox/dsh-k8e-sandbox-fs': [os.path.join(K8E,'packages','dsh-k8e-sandbox-fs','src')],
  '@k8e-sandbox/dsh-k8e-sandbox-subprocess': [os.path.join(K8E,'packages','dsh-k8e-sandbox-subprocess','src')],
}
json.dump({'compilerOptions': {
  'target': 'es2024', 'module': 'esnext', 'moduleResolution': 'bundler',
  'allowImportingTsExtensions': True, 'strict': True, 'skipLibCheck': True,
  'esModuleInterop': True, 'noEmit': True, 'types': ['node'],
  'typeRoots': [os.path.join(DSH,'node_modules','@types')], 'paths': paths,
}, 'include': [os.path.join(K8E,'packages','*','src','**','*.ts')]},
  open('tsconfig.check.json','w'), indent=2)
PY

# 3. Run dsh's TypeScript:
/path/to/deepseek-harness/node_modules/.bin/tsc -p tsconfig.check.json
```

## Release

All seven packages are published to npmjs.com under the `@k8e-sandbox`
scope. `scripts/release.mjs` publishes them in dependency-topological order
(dependencies first), so consumers always resolve real published versions;
`pnpm publish` rewrites the in-workspace `workspace:*` ranges to the actual
published versions automatically.

```sh
# Verify the payloads without touching the registry:
node scripts/release.mjs --dry-run

# Bump every package to a shared version and publish (requires npm login +
# @k8e-sandbox org access):
node scripts/release.mjs --version 0.2.0

# Publish the current package.json versions as-is:
node scripts/release.mjs
```

The publishing identity must be logged in to npm (`npm whoami`) and have
publish rights on the `@k8e-sandbox` scope. Scoped packages publish as
public with `--access public`; the `files` whitelist in each package.json
keeps the payloads minimal (only `lib/` and, for the bundle, the
`cordis.patch.yml`).
