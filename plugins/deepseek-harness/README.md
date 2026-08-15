# dsh-k8e-sandbox

Out-of-tree DeepSeek Harness plugin family (KIP-20). These packages let a dsh
profile route its filesystem and subprocess execution world into k8e-sandbox
via `dsh plugin --profile <name> add`.

## Packages

| Package | Role | `ctx` key |
|---|---|---|
| [`@k8e/dsh-k8e-sandbox`](packages/dsh-k8e-sandbox) | Sandbox owner service (session lifecycle + shared client) | `ctx.k8eSandbox` |
| [`@k8e/dsh-k8e-sandbox-client`](packages/dsh-k8e-sandbox-client) | Transport abstraction: `CliK8eClient` (Phase 1) | library |
| [`@k8e/dsh-k8e-sandbox-fs`](packages/dsh-k8e-sandbox-fs) | Filesystem seam provider | `ctx.fs` |
| [`@k8e/dsh-k8e-sandbox-subprocess`](packages/dsh-k8e-sandbox-subprocess) | Subprocess seam provider | `ctx.subprocess` |
| [`@k8e/dsh-k8e-sandbox-bundle`](packages/dsh-k8e-sandbox-bundle) | Installable bundle (`dsh.bundle.patch` → `cordis.patch.yml`) | — |

## Status

Phase 1 (CLI transport) under construction. The TypeScript packages target
dsh's in-box types (`@deepseek-ai/dsh-fs`, `@deepseek-ai/dsh-subprocess`,
`@deepseek-ai/cordis`), which resolve from the dsh installation at profile boot
time rather than from this workspace's `node_modules`.
