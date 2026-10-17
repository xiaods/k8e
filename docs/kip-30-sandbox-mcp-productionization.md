# KIP-30: Sandbox MCP productionization — healthy default install, multi-node readiness, IP-only TLS

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-10-17 | Implemented — M0 (default-skip + manual TLS), M1 (published image + HTTP fallback + tolerations), M2 (self-bootstrap + tenant-scoped sessions), M3.1 (leader-gated EndpointSlice), M3.2 (TLS mirror), M3.3 (warm-pool node/runtime awareness), M3.4 (CLI config block + tls export-ca) |

**Relates to**: [KIP-8](kip-8-skill-cli-replace-mcp.md) (MCP over API key — this
KIP makes its Deployment actually runnable), [KIP-21](kip-21-host-advertise-ip-resolution.md)
/ [KIP-23](kip-23-endpointslice-migration.md) (ingress bridge this KIP extends
to multi-endpoint), [KIP-22](kip-22-sandbox-advertise-hostname.md) (public name
in the gateway SAN), [KIP-28](kip-28-sandbox-gateway-trust-bootstrap.md) (remote
client trust — this KIP defines the IP-only deployment variant), [KIP-14](kip-14-mtls-dynamic-cert-issuance.md)
(Login RPC + client cert ledger reused by M2)

## Summary

The MCP adapter (`pkg/sandboxmcp/`, `manifests/sandbox-mcp/`) is the agent-facing
HTTP door, but a default K8E install has never been able to serve it. Three
independent defects compound:

1. **The Deployment is staged but can never run.** `manifests/sandbox-mcp/deployment.yaml`
   is staged by `deploy.Stage` and applied by the deploy watcher on every
   install, but it needs `mcp-gateway-tls` (created only by
   `hack/init-sandbox-mcp.sh`) and the `k8e-mcp:local` image (built from the
   repo, shipped in no release asset). Every default install spends forever in
   `FailedMount` / `ImagePullBackOff`.
2. **The HTTPRoute is rejected on a default install.** `k8e-mcp` attaches only
   to `sectionName: https` (deployment.yaml:184), and the `https` listener stays
   Invalid until the optional `sandbox-e2b` TLS Secret exists
   (e2b-gateway.yaml:166-176) — so on an HTTP-only install the route is
   `NotAllowedByListeners` and `/mcp` is unreachable even with the Deployment up.
3. **Nothing above is multi-node-safe.** Every server runs `deploy.WatchFiles`
   with no leader election; the `sandbox-grpc-gateway` EndpointSlice pins a
   single `%{ADVERTISE_IP}%`; the image must be imported on every node;
   control-plane taints block scheduling; and the warm pool has no node/runtime
   awareness, so a node without gVisor churns Pending pods forever.

This KIP makes MCP a **first-class opt-in citizen** instead of a permanently
broken default:

| Milestone | Deliverable | Default install | Multi-node | Code surface |
|-----------|-------------|-----------------|------------|--------------|
| **M0** | Default-skip `sandbox-mcp` manifests; init script is the single apply path | no broken pods | same | `pkg/server`, `hack/init-sandbox-mcp.sh`, docs |
| **M1** | MCP image in the release pipeline + `sectionName: http` fallback + tolerations + one-time cleanup of stale objects | unchanged | image works on every node | release workflow, `manifests/sandbox-mcp/deployment.yaml` |
| **M2** | `mcp-serve` self-bootstrap from an API key (`Login` → CSR → signed client cert), cert cache in a ConfigMap, **tenant-scoped sessions** | init script optional | same | `pkg/sandboxmcp`, deployment manifest |
| **M3** | Leader-gated ingress: EndpointSlice maintained by the sandboxmatrix controller with one endpoint per server; TLS mirror loop copying `sandbox-server` pair → `sandbox-e2b` Secret; warm-pool node/runtime awareness | unchanged | gateway + warm pool survive node loss and heterogeneous nodes | `pkg/sandboxmatrix`, `pkg/server`, `pkg/sandboxcli` (config-block + `tls export-ca`) |

M0 landed in `817e98dd` (verified end-to-end on v1.37.0-20261015-nightly+k8e3:
staged `deployment.yaml` removed, deleted Deployment not re-created, Gateway
`https` listener `Programmed/True` with a mirrored cert, external client
verifies the chain with the sandbox CA). M1 landed in this change: the release
workflow publishes `ghcr.io/xiaods/k8e-mcp` (tagged and `:latest`, both
arches), the Deployment pulls it by default with `--image` as the override, the
HTTPRoute attaches to `http` too, and the Deployment tolerates the
control-plane taint. M2–M3 are what remains.

> **Notation.** Hostnames and addresses are illustrative (RFC 2606/5737):
> `sbx.example.com`, `192.0.2.10`, `203.0.113.5`. Substitute your own.

## Motivation

### Verified root cause: four inputs the server cannot self-provide

`pkg/server/server.go` `stageFiles()` stages every bindata manifest
(`deploy.Stage`) and the deploy watcher applies whatever it finds on disk
(`pkg/deploy/controller.go` `WatchFiles`). That includes
`sandbox-mcp/deployment.yaml`, which mounts `mcp-gateway-tls` (deployment.yaml:139)
and pulls `k8e-mcp:local` (deployment.yaml:87) — neither exists on a default
install:

- `mcp-gateway-tls` is the MCP pod's **client** identity against the sandbox
  gRPC gateway: `ca.crt` (sandbox CA) + `tls.crt`/`tls.key` (a client cert the
  CA signed). `hack/init-sandbox-mcp.sh` bootstraps it from an API key through
  the same `Login` → CSR → `signClientCert` path every other client uses
  (`pkg/sandboxmatrix/grpc/server.go:1054`, `cert.go:280`; 90-day TTL, recorded
  in the `issuedCertStore` ledger).
- The server **cannot** hold that client private key: its trust model is that
  clients generate their own key pair and the server only signs the CSR
  (KIP-14). A server-side bootstrap would mint a backdoor identity. So this
  input is structurally operator-side, not a bug.
- `k8e-mcp:local` is built from `manifests/sandbox-mcp/Dockerfile`
  (`golang:1.26.7` → scratch, non-root) and carried in no release asset; the
  image must be built and imported per node.
- `sandbox-e2b`, the optional public TLS Secret for the Gateway's `https`
  listener, is likewise operator-provided (KIP-22/KIP-24).

Therefore a default install **necessarily** produces unrunnable pods. This is
what M0 fixes by not staging the manifest at all.

### Verified multi-node hazards

1. **EndpointSlice drift (`WatchFiles`, no leader election).** The controller
   `reconciler` is leader-gated (`pkg/sandboxmatrix/controller.go:75-110`), but
   `deploy.WatchFiles` runs on **every** server and owns the
   `sandbox-grpc-gateway` EndpointSlice, whose single endpoint is the staging
   server's `%{ADVERTISE_IP}%`. On a 3-node cluster each server overwrites the
   same object — last writer wins, so the gateway only ever fronts one server
   and flips between them on every restart.
2. **Image fan-out.** `k8e-mcp:local` must be imported into each node's
   containerd (`ctr -n k8s.io images import`) — acceptable for a single node,
   error-prone across three.
3. **Control-plane taint.** `k8e server` taints the control plane
   (`--node-taint`); the MCP Deployment sets no toleration, so on a
   dedicated-control-plane cluster it is `Unschedulable`.
4. **Warm pool has no node awareness.** `SandboxWarmPool` reconciliation has no
   nodeSelector or RuntimeClass awareness; a node without gVisor (or without the
   sandbox image) leaves warm pods `Pending`, which are reaped by
   `recycleUnschedulableWarmPodAfter` (`controller.go:201`) and recreated — a
   permanent Pending/recycle churn with zero usable sandboxes.
5. **Stale objects survive the fix.** M0 stops *staging* the manifest; the
   AddOn/Deployment objects from an older install remain in the cluster. On the
   verification cluster the k8e-mcp Deployment kept recreating
   `ContainerCreating` pods until it was deleted manually (`kubectl -n k8e-mcp
   delete deployment k8e-mcp`). An upgrade needs this cleanup once.

### Verified IP-only TLS story

A single-node install reached by bare public IP (no DNS, `203.0.113.5`) can
already serve a *trusted* HTTPS `/mcp` — no new CA, no new code:

1. `--sandbox-advertise-hostname 203.0.113.5` (env
   `K8E_SANDBOX_ADVERTISED_HOSTNAME`) lands the public IP in the
   `sandbox-server` certificate's IP SANs (`collectServerSANs`, regenerating the
   pair when the SAN set changes — KIP-22).
2. The sandbox CA (`/var/lib/k8e/server/tls/sandbox-ca.crt`, 10-year TTL,
   auto-generated by `ensureCA`/`generateCA`, `grpc/cert.go:63`) signs it.
3. Mirror the pair into `sandbox-matrix/sandbox-e2b` (server-side apply with a
   dedicated field manager) — the Gateway `https` listener goes `Programmed/True`
   and the `k8e-mcp` HTTPRoute is accepted.
4. The client sets `NODE_EXTRA_CA_CERTS` (undici-family clients) or
   `--cacert`/`--ca-file` to the same CA, and authenticates with
   `Authorization: Bearer e2b_<key>` (`pkg/sandboxmcp/auth.go:50` — the only
   accepted credential channel).

All of the above was verified on the live cluster. What is missing is
**automation**: the mirror is manual, and nothing reconciles it across restarts
or cert rotations (the server pair rotates on San change and every 90 days).
That is M3.

## Design principles

1. **Opt-in, but complete.** MCP is off by default (M0), but turning it on must
   be one command that works on one node *and* three — not a scavenger hunt for
   four missing inputs.
2. **Never ship a broken pod.** Anything the server cannot self-provision is
   not staged by default; fail closed and let the operator opt in explicitly
   (same fail-closed removal semantics as KIP-21's advertise-IP skip).
3. **The trust model is not negotiable.** The server signs client CSRs; it never
   holds a client private key (KIP-14). M2 keeps that invariant *inside* the
   cluster by generating the pair in the MCP pod and only persisting the
   certificate, never the private key, beyond the pod.
4. **Leader-gated writes for shared objects.** The EndpointSlice and the
   `sandbox-e2b` mirror are cluster-global; only the lease holder writes them.
5. **No invented PKI.** IP-only deployments reuse the existing sandbox CA and
   its rotation semantics; we do not add a second self-signed hierarchy.
6. **The tenant is the isolation unit; the API key is only a credential.** An
   API key `Record` (`pkg/sandbox/apikey/store.go`) carries no tenant field —
   it is a shared, rotatable credential. `tenantID` is a request field, set
   client-side by `--tenant` / `K8E_SANDBOX_TENANT` (default `default`) and it
   is what actually keys sessions (`pkg/sandboxmatrix/grpc/orchestrator.go:384`).
   Isolation is never re-derived from the key.

## M0 — Default-skip sandbox-mcp manifests (implemented)

`pkg/server/server.go` `stageFiles()` now default-skips `sandbox-mcp` (directory
key stops every asset under it from staging; the file-level key additionally
triggers `Stage`'s fail-closed removal of a previously staged
`deployment.yaml`):

```go
if _, ok := controlConfig.Skips["sandbox-mcp"]; !ok {
    controlConfig.Skips["sandbox-mcp"] = true
    controlConfig.Skips["sandbox-mcp/deployment.yaml"] = true
}
```

The skip is **not** a `disable`: `shouldDisableFile` prunes objects and deletes
the file on *every* restart (`controller.go:166`/`493`), which would fight the init
script's `kubectl apply --server-side --field-manager=k8e-mcp-init`. Skip only
ignores the file. `hack/init-sandbox-mcp.sh` remains the single apply path —
its `kubectl apply -f manifests/sandbox-mcp/deployment.yaml` is annotated as
load-bearing, and `manifests/sandbox-mcp/README.md` documents the opt-in and the
one-time `kubectl -n k8e-mcp delete deployment k8e-mcp` cleanup for upgrades
from pre-M0 installs.

Covered by `TestStageSkipsSandboxMCPDefault` (skip + stale-copy removal + peer
manifests still stage). Landed in `817e98dd`; verified on the live cluster
(§Verification).

## M1 — Publishable image + HTTP fallback + tolerations (implemented)

Three small changes remove the manual build/import and the route rejection:

1. **Release pipeline builds and pushes `k8e-mcp`.** Add the image to the
   release workflow alongside the sandbox image (GHCR, `${{ secrets.GITHUB_TOKEN }}`):
   tag it `ghcr.io/xiaods/k8e-mcp:<sanitized-tag>` and `:latest`. The
   Deployment defaults to `ghcr.io/xiaods/k8e-mcp:latest` and the init script
   gains `--image` for the override — a concrete default rather than
   `%{SYSTEM_DEFAULT_REGISTRY}%` templating, because the Deployment is applied
   by `hack/init-sandbox-mcp.sh` (raw `kubectl apply`), which performs no
   server-side substitution; templated manifests would still need an explicit
   flag there anyway. `imagePullPolicy` is left unset so kubelet's tag-based
   default applies (`Always` for `:latest`, `IfNotPresent` for a versioned or
   locally imported tag — air-gap uses `--image k8e-mcp:local`).
2. **HTTPRoute attaches to `http` as well.** Add a second parentRef with
   `sectionName: http`. On a default (HTTP-only) install the route is accepted
   immediately; an operator that terminates TLS at the Gateway gets the `https`
   listener as before. `ResolvedRefs` semantics are unchanged; the Gateway
   simply has two parents, matching how `e2b-control-plane`/`e2b-envd` attach.
3. **Tolerations for the control-plane taint** (`key:
   node-role.kubernetes.io/control-plane`, `effect: NoSchedule`), mirroring
   other system components, so a single-node-all-control-plane cluster (the
   common small install) can schedule MCP without extra flags.

Upgrade cleanup (from pre-M0 installs) is documented in the README: delete the
stale Deployment once; the watcher no longer re-applies it.

## M2 — mcp-serve self-bootstrap from the API key

The init script's four-step dance (namespace, secrets, apply, rollout) shrinks
to: point `mcp-serve` at an API key and let it enroll itself. `mcp-serve`'s flags
already accept environment variables (`pkg/sandboxmcp/command.go:45-57`), so the
Deployment can drive the whole flow from env:

1. **Bootstrap identity in the pod.** On start, `mcp-serve` generates a private
   key, calls `Login` with the API key, submits a CSR, and receives a signed
   client cert (same `grpc/server.go:1054` path as every CLI client). The
   private key stays in the pod (emptyDir or generated in-process); only the
   certificate is cached.
2. **Cache the certificate in a ConfigMap** (`k8e-mcp/mcp-client-cert`:
   `tls.crt` + the CA) so rollouts and restarts reuse the identity until the
   90-day TTL lapses, then re-enroll. The existing `sandboxmcp` RBAC (`mcp-state`
   Role) is extended with ConfigMap read/write on that single object.
3. **Consent model.** Enrolling with an arbitrary API key from inside the
   cluster is automatic when the operator sets the bootstrap env on the
   Deployment (recommended default in the README), and **never** enabled by
   M0/M1 staging — the manifest stays skipped, so the Enrollment is still an
   explicit operator action, preserving "MCP is opt-in".
4. **Tenant-scoped sessions.** Today `pkg/sandboxmcp/tools.go:269` calls
   `CreateSession` with only `SessionId` and `RuntimeClass` — no `TenantId` —
   so every MCP-created session lands in the shared `default` bucket and all MCP
   clients share one scope. M2 threads the tenant through `mcp-serve`
   (`K8E_MCP_TENANT` env, forwarded on every session RPC), making one
   Deployment the boundary of one tenant and multi-tenancy a matter of
   deploying several MCP instances. Keys stay what they are — a shared
   credential *within* a tenant — so rotation or a CI key never changes scope.

   **Update (supersedes the earlier "keep ephemeral" decision):** the
   sandbox model is now unified — an empty tenant is normalized to the shared
   `default` bucket server-side (`defaultTenantID`), and **every** session is
   persistent regardless of tenant. The warm pool provisions one workspace PVC
   per warm pod at boot (`workspace-warm-*`, 1Gi local-path), so a warm claim
   yields a persistent VM (fast start *and* a PVC workspace); the claim is
   retired with the pod on trim/reap/recycle, returned to the pool unlabeled on
   release, and deleted on destroy only when it still carries the session's
   label (paused/crashed sessions no longer leak; sub-agent children never
   delete the parent's shared claim). Legacy EmptyDir warm pods are skipped by
   the claim path and age out via the idle reaper; the only remaining
   pause refusal is that legacy shape. Session GC now reclaims Paused sessions
   too, whose expiry timer and 1Gi claim survive pause.

   This *raises* the stakes of M2's tenant threading: with normalization, an
   MCP instance that does not send `TenantId` puts every client in the shared
   `default` bucket — persistence is no longer the thing that distinguishes
   them, isolation is, and only the tenant provides it. Storage footprint also
   changes scope: every anonymous session now costs a 1Gi claim, so warm-pool
   sizing (target × 1Gi) and the idle-reap TTL become the dials that bound disk
   usage.

The `sandbox-e2b` TLS Secret and `mcp-gateway-tls` mount remain
operator-provided; M2 only removes the image and identity scavenger hunts.

## M3 — Leader-gated ingress, TLS mirror loop, warm-pool node awareness

### 3.1 EndpointSlice with one endpoint per server

Move the `sandbox-grpc-gateway` EndpointSlice out of the staged manifest and
into the **leader-gated** sandboxmatrix controller (`controller.go:75-110`
already holds the `sandboxmatrix-controller` Lease used by warm-pool
reconciliation and session GC):

- Each server, when leader, owns the EndpointSlice and writes **one endpoint
  per advertised server address** (advertise IP + advertise hostname, port
  50051): `endpoints: [{addresses: [192.0.2.10, 203.0.113.5], conditions:
  {ready: true}}]`.
- Membership comes from the Leases/`k8e.io` endpoints the servers already write
  (the same source the HA control plane uses), not from the staging-time
  template — so a node joining or leaving updates the slice in reconcile time,
  and there is no last-writer-wins drift because only the lease holder writes.
- The staged `e2b-gateway.yaml` keeps the Service/Route objects but drops the
  static single-address EndpointSlice; the controller reconciles the object
  keyed on the `app.kubernetes.io/managed-by: k8e-sandboxmatrix` label, using
  optimistic-concurrency update (retry on conflict) rather than server-side
  apply, so a hand-edited slice is corrected, not silently overwritten.
- `K8E_SKIP_ENDPOINTSLICE_CONTROLLER` (default false) keeps an escape hatch for
  operators who manage the object externally (e.g. MetalLB-style VIP fronting).

### 3.2 TLS mirror loop → `sandbox-e2b`

A leader-gated controller loop (same lease as 3.1) mirrors
`/var/lib/k8e/server/tls/sandbox-server.{crt,key}` into
`sandbox-matrix/sandbox-e2b` so the Gateway's `https` listener is valid on any
install that has a routable public IP/hostname — already the KIP-21/KIP-22
prerequisites:

- On each reconcile (and on cert-file change via the existing fsnotify-style
  deploy watcher event source, or a 10-minute sync): read the pair, check the
  fingerprint against the Secret, and update with optimistic concurrency
  (retry on conflict) when the Secret carries the
  `app.kubernetes.io/managed-by: k8e-sandbox-tls-mirror` label (or is absent),
  leaving hand-created Secrets untouched.
- **Ownership is opt-in by absence:** the loop takes over only when the Secret
  is missing **or** already carries our label. A hand-created
  `sandbox-e2b` (cert-manager `Certificate`, a purchased cert, an operator's
  file) is never touched — exactly the conflict semantics `hack/init-sandbox-mcp.sh`
  already practice. This is what the manual M0 verification exercised end to end.
- On IP-only deployments nothing changes for the client: the CA is the sandbox
  CA (KIP-28 M2's `/.well-known/k8e/sandbox-ca.crt` distribution, once landed,
  removes the manual `NODE_EXTRA_CA_CERTS` step as well).

### 3.3 Warm-pool node/runtime awareness

`SandboxWarmPool` reconciliation gains:

- `nodeSelector`-likeconstraints honoring a per-node annotation
  (`sandbox.k8e.io/runtime-classes: gvisor,kata`) or the presence of the
  `RuntimeClass` node selector already in the CRD; warm pods land only on nodes
  that can run them.
- Reaping refinement: a warm pod Pending because of an unschedulable node
  condition is *not* recreated against the same node (avoids the
  Pending/recycle churn); the pool reports `NotReady` with a reason instead of
  silently burning the reconcile loop.

### 3.4 CLI: print the ready MCP config block + `tls export-ca`

Closing the UX gap of a header-based credential channel:

- `k8e-sandbox-cli apikey create` (and `apikey list`) prints a copy-pasteable
  MCP config block after creating a key:

  ```json
  {
    "url": "https://sbx.example.com/mcp",
    "headers": { "Authorization": "Bearer e2b_<key>" }
  }
  ```

  computed from the active profile's gateway URL (KIP-17 profiles), the
  advertise hostname (KIP-22), and whether `sandbox-e2b` exists (https vs http).
- `k8e-sandbox-cli tls export-ca` writes the sandbox CA to
  `~/.k8e/sandbox-ca.crt` (or stdout), reducing the IP-only bootstrap to two
  CLI commands (M3's mirror + this export), and giving KIP-28 M2 a
  non-`/.well-known` fallback.

## Verification (M0, live cluster)

Single node `203.0.113.5`-style public IP install, upgraded
`k8e2 (4577443d) → k8e3 (262e39ab)`:

| Check | Result |
|---|---|
| Staged manifest after upgrade | `/var/lib/k8e/server/manifests/sandbox-mcp/` only contains inert `Dockerfile`/`README.md`; `deployment.yaml` fail-closed-removed |
| Deploy watcher behavior | after `kubectl -n k8e-mcp delete deployment k8e-mcp`, **not re-created** (no AddOn, no manifest) — 75 s observation |
| Gateway TLS | `sandbox-e2b` created by hand (field manager `sandbox-tls-manual`); `https` listener `Programmed/True`, `k8e-mcp` HTTPRoute `Accepted/True` (was NotAllowedByListeners before the Secret) |
| External client | `curl --cacert sandbox-ca.crt https://<public-ip>/mcp` → `ssl_verify_result=0`, HTTP 503 (upstream missing — pre-M1 image) — chain + IP SAN verified end to end |
| Cluster health after upgrade | node `Ready`, `sandbox-warm-*` recreated `1/1 Running` |

## Risks / open questions

1. **Where does the MCP image live in air-gap?** GHCR digest pinning plus the
   `%{SYSTEM_DEFAULT_REGISTRY}%` template covers connected installs; air-gap
   needs the image in the air-gap tarball (M1 must extend `hack/package-airgap.sh`).
   Open: include by default or as an optional tarball?
2. **EndpointSlice membership source.** Server Lease endpoints are the natural
   source; an alternative is the `sandboxmatrix` CR's own node list. Prefer
   Leases (no new state), but this needs the reconciler to filter
   `k8e.io/server`-style leases only.
3. **TLS mirror on multi-server: which pair?** Each server signs its own
   `sandbox-server` pair from the shared CA, so any pair validates against the
   same CA. The leader mirrors *its own* pair; clients trust the CA, not the
   leaf, so this is correct but worth an explicit note in the docs (a
   leaf-fingerprint drift between leaders is the observable symptom).
4. **Bootstrap API key in a Deployment env** (M2) is a Secret ref, never a
   literal; still, enrollment grants a 90-day client identity. Mitigation: the
   `issuedCertStore` ledger already names the identity, so revocation/rotation
   is auditable. Open: should the cert TTL for internal bootstrap be shorter
   than the CLI's 90 days?
5. **Warm-pool annotation vs runtime probe.** Node annotation is explicit and
   cheap; a probe (actually scheduling a probe pod) is more accurate but costs
   a sandbox. Prefer annotation + `NotReady` reason.

## Non-goals

- Publicly trusted certificates for the gRPC gateway (KIP-28 M3's job).
- Moving MCP into the server binary (it stays a separate Deployment; KIP-8's
  design is unchanged).
- Per-API-key isolation. The key is a credential, not a scope; tenants are the
  isolation boundary (design principle 6).

## References

- `pkg/server/server.go` — `stageFiles()` M0 skip
- `pkg/deploy/stage.go`, `pkg/deploy/controller.go:48` — fail-closed skip vs
  repeated pruning disable semantics
- `pkg/deploy/stage_test.go` — `TestStageSkipsSandboxMCPDefault`
- `hack/init-sandbox-mcp.sh` — the single apply path (server-side, field
  manager `k8e-mcp-init`)
- `manifests/sandbox-mcp/deployment.yaml:87/139/182` — image, secret mount,
  `sectionName: https`
- `manifests/sandbox-matrix/e2b-gateway.yaml:158-176` — listeners,
  `sandbox-e2b` TLS
- `pkg/sandboxmatrix/grpc/cert.go:63/111/280`, `grpc/server.go:1054` — CA,
  server pair, `Login` CSR signing
- `pkg/sandboxmatrix/controller.go:75-110` — leader-gated reconciler (M3 host)
- `pkg/sandboxmcp/command.go:46-53`, `pkg/sandboxmcp/auth.go:50` — env-driven
  flags, Bearer-only auth
- `pkg/sandboxmcp/tools.go:269` — MCP session creation omitting `TenantId`
- `pkg/sandbox/apikey/store.go` — key `Record` (no tenant field);
  `pkg/sandboxmatrix/grpc/orchestrator.go:384` — `TenantID` keys the session
