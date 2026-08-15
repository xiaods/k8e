# dsh-k8e-sandbox

Out-of-tree DeepSeek Harness plugin family (KIP-20). These packages let a dsh
profile route its filesystem and subprocess execution world into k8e-sandbox
via `dsh plugin --profile <name> add`.

## Packages

| Package | Role | `ctx` key |
|---|---|---|
| [`@k8e/dsh-k8e-sandbox`](packages/dsh-k8e-sandbox) | Sandbox owner service (session lifecycle + shared CLI/gRPC client) | `ctx.k8eSandbox` |
| [`@k8e/dsh-k8e-sandbox-client`](packages/dsh-k8e-sandbox-client) | Transport: `CliK8eClient` (Phase 1) + `GrpcK8eClient` (Phase 2) | library |
| [`@k8e/dsh-k8e-sandbox-fs`](packages/dsh-k8e-sandbox-fs) | Filesystem seam provider | `ctx.fs` |
| [`@k8e/dsh-k8e-sandbox-subprocess`](packages/dsh-k8e-sandbox-subprocess) | Subprocess seam provider (streaming exec + `spawnTerminal`) | `ctx.subprocess` |
| [`@k8e/dsh-k8e-sandbox-bundle`](packages/dsh-k8e-sandbox-bundle) | Installable bundle (`dsh.bundle.patch` → `cordis.patch.yml`) | — |

## Status

Phase 1 (CLI transport) and Phase 2 (direct gRPC: `spawnTerminal` + streaming
`spawn`) are implemented. The tree typechecks cleanly against the dsh checkout;
runtime e2e against a live K8E gateway is still pending — the procedure is in
[`e2e.md`](e2e.md).

## Test

`npm test` runs a fake-ctx runtime test (no harness, no gateway): it mounts
`K8eSubprocessRuntime` on a fake `ctx` with a fake owner + fake gRPC client and
asserts the `spawnTerminal` / `spawn` mapping (`tests/subprocess.test.mjs`,
bundled on the fly by `scripts/test.mjs`). It needs the same `@deepseek-ai/*`
symlinks + `esbuild` + `@grpc/*` deps as the Typecheck step below.

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
  '@k8e/dsh-k8e-sandbox': [os.path.join(K8E,'packages','dsh-k8e-sandbox','src')],
  '@k8e/dsh-k8e-sandbox-client': [os.path.join(K8E,'packages','dsh-k8e-sandbox-client','src')],
  '@k8e/dsh-k8e-sandbox-client/grpc': [os.path.join(K8E,'packages','dsh-k8e-sandbox-client','src','grpc.ts')],
  '@k8e/dsh-k8e-sandbox-fs': [os.path.join(K8E,'packages','dsh-k8e-sandbox-fs','src')],
  '@k8e/dsh-k8e-sandbox-subprocess': [os.path.join(K8E,'packages','dsh-k8e-sandbox-subprocess','src')],
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
