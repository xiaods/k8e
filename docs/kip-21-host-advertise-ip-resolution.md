# KIP-21: Loopback-proof host advertise-IP resolution for the sandbox ingress

| Author | Updated | Status |
|--------|---------|--------|
| @pi-agent-e2b-gateway | 2026-08-24 | Implemented (PR #550) — `advertiseIP()` is loopback-proof. EndpointSlice migration is [KIP-23](./kip-23-endpointslice-migration.md) (same change set, PR #551 folded into #550). |

## Summary

Kubernetes does not allow `Endpoints` to carry loopback addresses
(`127.0.0.1` / `::1`): they are not routable from pods, so any Service
backed by them is permanently unhealthy. The KIP-18 ingress manifest
(`manifests/sandbox-matrix/e2b-gateway.yaml`) bridges the two host-resident
services — the embedded E2B HTTP server (`:3676`) and the sandbox gRPC
gateway (`:50051`) — into the cluster through headless `Service` + `Endpoints`
objects whose address comes from the `%{ADVERTISE_IP}%` template. When the
server runs with **no flags at all** (the common default: no
`--advertise-address`, no non-loopback `--bind-address`), `advertiseIP()`
falls back to `cfg.Loopback(false)` and the staged file at
`/var/lib/k8e/server/manifests/sandbox-matrix/e2b-gateway.yaml` ends up with:

```yaml
subsets:
- addresses:
  - ip: 127.0.0.1
```

Kubernetes rejects/never serves these Endpoints, so the Gateway API has no
healthy backends and the **entire external sandbox door silently fails by
default**. This KIP makes advertise-IP resolution loopback-proof: the
resolved address must always be routable from pods, with a default-route
interface fallback and a hard, actionable failure instead of a silent
loopback write.

## Motivation

1. **Default install is broken.** A stock `k8e server` (no `--advertise-address`,
   `--bind-address` defaults to `0.0.0.0`) stages `Endpoints` pointing at
   `127.0.0.1`. The Cilium Gateway API `e2b` Gateway then reports no healthy
   backends for `:3676` and `:50051`, and both the E2B SDK and the sandbox
   gRPC client fail with opaque connection errors. The first-run experience
   (KIP-18, PR #541) is the one that breaks.
2. **Silent degradation.** Nothing in the server log explains why the
   Gateway backends are unhealthy; the manifest *looks* fine because the
   comment on the E2B listen default (`127.0.0.1:3676`) actively misleads
   (the embedded server actually listens on `0.0.0.0:3676` by default).
3. **The failure is structural.** Loopback in `Endpoints` is *never* usable —
   no pod can route to the host's loopback. There is no configuration in
   which it works, so the resolver must not be allowed to produce it.

## Root-cause chain (verified)

1. `pkg/server/server.go` → `advertiseIP(cfg)`:
   - `cfg.AdvertiseIP` (`--advertise-address`) — used when set;
   - else `cfg.APIServerBindAddress` when non-empty and not `0.0.0.0`/`::`;
   - else **`cfg.Loopback(false)` → `127.0.0.1`** (or `::1` for IPv6-only).
2. `stageFiles()` templates `%{ADVERTISE_IP}%` (server.go:307) into every
   staged asset, including `sandbox-matrix/e2b-gateway.yaml`, which is
   applied via `deploy.WatchFiles` and dropped at
   `/var/lib/k8e/server/manifests/…`.
3. Both `Endpoints` objects (`sandbox-grpc-gateway`, `e2b-server`) receive
   `ip: 127.0.0.1`. Kubernetes validation rejects loopback Endpoint
   addresses; even where accepted, kube-proxy/Cilium can never route pod
   traffic to the host loopback → Gateway API backends are unhealthy.
4. Note: `pkg/cli/server/server.go:250-256` already folds `--node-external-ip`
   / `--node-ip` into `AdvertiseIP`, so the remaining hole is exactly the
   pure-default case — which is also the most common one.

## Options

### Option A — loopback-proof resolution (recommended)

Make `advertiseIP()` never return a loopback (or any non-routable) address:

1. explicit `--advertise-address` (validated routable);
2. non-loopback `--bind-address`;
3. **default-route interface address** via `utilnet.ChooseHostInterface()`
   — the same mechanism `BindAddressOrLoopback(chooseHostInterface=true)`
   already uses, and what k3s uses to derive its default advertise IP;
4. if still nothing routable: return empty, and `stageFiles` **skips
   staging `e2b-gateway.yaml`** with a clear, actionable log
   (`set --advertise-address`), instead of writing an invalid manifest.

- **Pros**: fixes the default install; uses battle-tested k3s machinery;
  fail-loud instead of fail-silent; no architecture change; small diff.
- **Cons**: single-node hosts without a default route (rare) lose the
  e2b/gRPC ingress until `--advertise-address` is set — by design.

### Option B — validation + warning only

Keep the loopback fallback but log a warning and validate at startup.

- **Pros**: trivial.
- **Cons**: the broken manifest is still written; the ingress still fails
  by default. Does not solve the problem.

### Option C — hostNetwork pods / nodePort exposure

Move the e2b + gRPC surfaces into `hostNetwork` pods or expose them via
`NodePort` Services instead of Endpoints.

- **Pros**: no Endpoints involvement.
- **Cons**: contradicts the KIP-18 final architecture (both servers embed in
  the k8e-server host process); adds scheduling/port-conflict complexity;
  NodePort has an arbitrary port range and loses the stable
  `sandbox-matrix.svc` DNS names the Gateway routes reference. Rejected.

## Design

### `advertiseIP(cfg) string`

```go
func advertiseIP(cfg *config.Control) string {
    if cfg.AdvertiseIP != "" && isRoutableAdvertiseIP(cfg.AdvertiseIP) {
        return cfg.AdvertiseIP
    }
    if ip := cfg.APIServerBindAddress; ip != "" && isRoutableAdvertiseIP(ip) {
        return ip
    }
    if hostIP, err := utilnet.ChooseHostInterface(); err == nil && isRoutableAdvertiseIP(hostIP.String()) {
        return hostIP.String()
    }
    return ""
}
```

### `isRoutableAdvertiseIP(ip string) bool`

Rejects address classes that pods can never route to or that are invalid as
Endpoints addresses:

- loopback (`127.0.0.0/8`, `::1`)
- link-local (`169.254.0.0/16`, `fe80::/10`)
- unspecified (`0.0.0.0`, `::`)
- multicast (`224.0.0.0/4`, `ff00::/8`)
- non-IP / malformed input

### `stageFiles` behavior on unresolvable IP

When `advertiseIP(controlConfig) == ""`:

- log a `Warn`/`Error` explaining the consequence and the fix
  (`--advertise-address <node-ip>`);
- add `sandbox-matrix/e2b-gateway.yaml` to a **copy** of the `skips` map so
  the invalid Endpoints manifest is never staged (all other manifests are
  unaffected). `deploy.Stage` already supports per-asset skips;
- **fail closed across restarts**: a skip-listed manifest is also **removed
  from disk** if an earlier successful run left a copy behind
  (`deploy.Stage` skip branch → `removeStagedCopy`). This matters because
  the manifest watcher applies whatever it finds on disk every 15s — a
  stale `e2b-gateway.yaml` would otherwise be re-applied with its obsolete
  (possibly loopback) endpoint addresses. If the stale copy **cannot be
  removed** (permissions, read-only filesystem), `deploy.Stage` **fails
  loudly** — silently proceeding would let the watcher re-apply the stale
  manifest while the operator believes it is skip-listed.

Residual (documented limitation): objects already applied to the cluster
from a previous successful run (the `e2b-gateway.yaml` Addon and its owned
Gateway/Route/Service/Endpoints) are owned by the `k8e.sh/v1` Addon CR and
are not pruned by this stage-time removal — the watcher only acts on files
present on disk. In practice the unresolvable-IP state requires no
`--advertise-address`, a loopback/unspecified bind, and no default-route
interface, so the previous successful staging's address is still the
last-known-good door; the operator restoring `--advertise-address`
re-stages and re-applies. Deleting the Addon to prune its objects is
intentionally out of scope for the stage-time path (no cluster client at
that point in the lifecycle).

### Manifest comment fix

`manifests/sandbox-matrix/e2b-gateway.yaml` line ~78 says
`default 127.0.0.1:3676 — the Gateway fronts it`; the embedded E2B server
defaults to `0.0.0.0:3676` (`pkg/server/e2b_embedded.go`,
`--e2b-listen` flag default). Update the comment to state the embedded
listen default accurately and note that the Endpoints address comes from
the host advertise IP, never loopback.

## Acceptance criteria

1. A `k8e server` run with **no networking flags** on a host with a default
   route stages `e2b-gateway.yaml` whose `Endpoints` contain a non-loopback
   routable IP (the default-route interface address).
2. `advertiseIP()` never returns `127.0.0.1`/`::1` for any input
   combination (unit-test matrix).
3. `isRoutableAdvertiseIP` rejects loopback/link-local/unspecified/multicast
   and accepts private + public unicast (unit-test table).
4. On a host with no resolvable routable IP, `e2b-gateway.yaml` is **not**
   staged, any previously staged copy is **removed from disk** (so the
   manifest watcher cannot re-apply it), and the log contains an actionable
   `--advertise-address` hint.
5. `pkg/deploy` bindata test asserts the manifest Endpoints use the
   `%{ADVERTISE_IP}%` template and contain no literal loopback.
6. `go build ./pkg/...`, `go vet ./pkg/server/...`, and
   `go test ./pkg/server/... ./pkg/deploy/...` pass.

## Implementation plan

| # | Change | Files |
|---|--------|-------|
| 1 | `isRoutableAdvertiseIP` + loopback-proof `advertiseIP` | `pkg/server/server.go` |
| 2 | `stageFiles` skip + actionable log on unresolvable IP | `pkg/server/server.go` |
| 3 | Unit tests: resolution matrix + validator table | `pkg/server/server_test.go` (new) |
| 4 | Bindata test: template + no-loopback assertion | `pkg/deploy/zz_bindata_e2b_check_test.go` |
| 5 | Comment fix + docs alignment | `manifests/sandbox-matrix/e2b-gateway.yaml`, `docs/kip-18-sandbox-e2b-compat.md` |
| 6 | Validation | build/vet/test, bindata regen if manifest touched |

## Non-goals

- Changing the KIP-18 architecture (embedded servers behind the Gateway API).
- Allowing `--advertise-address` to be a loopback (we reject it loudly).
- Dual-stack / multiple advertise addresses (single address per Endpoints
  subset remains).
- IPv6 default-route interface selection beyond `ChooseHostInterface`.

## Related consideration: Endpoints vs EndpointSlice

The KIP-18 manifest bridges the host services with core `v1` `Endpoints`
objects (manually managed, headless-Service pattern). Two facts bound this
decision:

1. **The loopback fix is resource-agnostic.** `advertiseIP()`
   loopback-proof resolution applies identically whether the bridge is
   `Endpoints` or `discovery.k8s.io/v1 EndpointSlice` — EndpointSlice
   validation is *stricter* about address classes, so the same
   `%{ADVERTISE_IP}%` substitution would need the same non-loopback
   guarantee.
2. **`v1 Endpoints` is the legacy path.** It remains supported in this
   tree (k8s v1.35.5: no deprecation annotation on the `Endpoints` type),
   but `EndpointSlice` is the direction of travel and the recommended
   resource for new/manual endpoint management.

**Decision:** this KIP keeps `v1 Endpoints` — the loopback bug is a
correctness issue that must not be coupled to a resource migration, and the
bindata test (`kind: Endpoints` assertions, `zz_bindata_e2b_check_test.go`)
pins the current contract. Migrating the bridge to `EndpointSlice`
(`addressType: IPv4`, `kubernetes.io/service-name` label, per-address
`%{ADVERTISE_IP}%` template) is tracked as a separate follow-up so Cilium
Gateway API backend resolution can be verified independently.

## References

- `pkg/server/server.go` — `advertiseIP`, `stageFiles`, `%{ADVERTISE_IP}%`
- `pkg/daemons/config/types.go` — `Loopback`, `BindAddressOrLoopback`,
  `utilnet.ChooseHostInterface` usage
- `manifests/sandbox-matrix/e2b-gateway.yaml` — headless Service + Endpoints
  bridge (KIP-18)
- `docs/kip-18-sandbox-e2b-compat.md` — the ingress architecture this fixes
- `pkg/cli/server/server.go:250-256` — existing node-ip fold-in at CLI layer
- Kubernetes Endpoints semantics — loopback addresses are not valid pod-
  routable Endpoint targets
