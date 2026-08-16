# KIP-21 follow-up: migrating the sandbox ingress bridge to EndpointSlice

| Author | Updated | Status |
|--------|---------|--------|
| @pi-agent-e2b-gateway | 2026-08-16 | Design consideration — deferred (P2), pending KIP-21 PR #550 merge |

## Summary

`manifests/sandbox-matrix/e2b-gateway.yaml` bridges the two host-resident
services (embedded e2b HTTP `:3676`, sandbox gRPC `:50051`) into the cluster
with core `v1` `Endpoints` objects (manual, headless-Service pattern). This
note evaluates migrating that bridge to `discovery.k8s.io/v1 EndpointSlice`
— the modern, non-deprecated replacement — and documents the exact design so
the switch can be made independently of the KIP-21 loopback fix.

**Decision: defer.** The KIP-21 correctness fix (loopback-proof
`advertiseIP()`) is resource-agnostic and must land first (PR #550). The
`v1 Endpoints` API carries no deprecation annotation in the vendored
k8s v1.35.5-k3s1 tree, so there is no removal pressure. The migration below
is low-risk but touches the same manifest, and doing both in one change set
would couple a correctness fix to a resource migration.

## Why the migration is safe and feasible

Facts verified against the vendored k8s 1.35.5 tree
(`staging/src/k8s.io/endpointslice@v1.35.5-k3s1`):

1. **Service association** — an EndpointSlice is bound to a Service by the
   `kubernetes.io/service-name: <service>` label
   (`util/utils.go:213-214`). Same namespace required.
2. **Controller coexistence** — the EndpointSlice controller only manages
   slices it created (`endpointslice.kubernetes.io/managed-by:
   endpointslice-controller.k8s.io`); manual slices with a different
   `managed-by` value are left untouched (`reconciler.go:664-667`).
3. **Selector-less headless Services generate nothing** — both bridge
   Services have no `spec.selector`, so the controller derives no
   pod-backed endpoints for them. The manual EndpointSlice is therefore the
   sole backend source — exactly the role the manual `Endpoints` objects
   play today. (This is also why the current manual-`Endpoints` pattern
   survives reconciliation.)
4. **Address rules are the same or stricter** — `addressType` is immutable
   (`IPv4` for our single-stack advertise IP); addresses must be canonical
   unicast IPs. Loopback remains invalid, which is precisely the invariant
   KIP-21's `isRoutableAdvertiseIP()` already guarantees for
   `%{ADVERTISE_IP}%`.
5. **Cilium backend resolution** — Cilium (chart 1.20.0 in this repo) builds
   its service map from EndpointSlices (kube-proxy replacement mode, default
   since 1.12; the Gateway API controller uses the same service resolution),
   so Gateway → Service → EndpointSlice is the path already in use.

## Target YAML (replaces the two `Endpoints` objects)

```yaml
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: sandbox-grpc-gateway
  namespace: sandbox-matrix
  labels:
    kubernetes.io/service-name: sandbox-grpc-gateway
    endpointslice.kubernetes.io/managed-by: k8e
    app.kubernetes.io/managed-by: k8e
addressType: IPv4
endpoints:
- addresses:
  - "%{ADVERTISE_IP}%"
  conditions:
    ready: true
ports:
- name: grpc
  port: 50051
  protocol: TCP
---
apiVersion: discovery.k8s.io/v1
kind: EndpointSlice
metadata:
  name: e2b-server
  namespace: sandbox-matrix
  labels:
    kubernetes.io/service-name: e2b-server
    endpointslice.kubernetes.io/managed-by: k8e
    app.kubernetes.io/managed-by: k8e
addressType: IPv4
endpoints:
- addresses:
  - "%{ADVERTISE_IP}%"
  conditions:
    ready: true
ports:
- name: http
  port: 3676
  protocol: TCP
```

Notes:
- One slice per `addressType`; single-stack `IPv4` matches the single
  advertise IP. IPv6-only clusters would need `addressType: IPv6` with
  `::1`-free resolution (the KIP-21 resolver already handles this).
- The `%{ADVERTISE_IP}%` template and the loopback-proof `advertiseIP()`
  resolver are reused unchanged; the skip-when-unresolvable path in
  `stageFiles` already keys on the manifest file name, so it applies to the
  whole file (both slices staged or neither).

## Test impact

- `pkg/deploy/zz_bindata_e2b_check_test.go` — the `kind: Endpoints` /
  `name: <service>` assertions must become EndpointSlice-aware:
  assert both slices carry `kubernetes.io/service-name`, `addressType: IPv4`,
  the `%{ADVERTISE_IP}%` template, and no literal loopback (the KIP-21
  assertions already check template count + no-loopback and stay valid).
- `pkg/deploy/stage_test.go` — substitution + skip tests remain valid
  unchanged (they operate on the staged file regardless of kind).

## Verification checklist (after switching)

1. `kubectl get endpointslice -n sandbox-matrix` — both slices present,
   `ENDPOINTS` shows the node advertise IP (non-loopback).
2. `kubectl get endpoints -n sandbox-matrix` — controller should NOT
   recreate Endpoints (selector-less Service + no Endpoints object in
   manifest).
3. `kubectl exec -n kube-system <cilium-agent> -- cilium service list` —
   `sandbox-grpc-gateway` / `e2b-server` backends resolve to the host IP.
4. Gateway health: `kubectl get gateway e2b -n sandbox-matrix` and
   `kubectl get httptroute,tcproute` — backends ready; e2b SDK
   `apiUrl` and gRPC client handshake succeed.
5. Confirm no `Failed to reconcile EndpointSlice` / ownership warnings in
   the endpointslice controller logs.

## Rollback

Revert the manifest change (bindata regen via `zig build generate`); the
bridge falls back to the previous `v1 Endpoints` objects. No data migration.

## Non-goals

- Dual-stack slices (single advertise IP today).
- Automating slice generation (manual, template-substituted, like today).

## References

- `manifests/sandbox-matrix/e2b-gateway.yaml` — current bridge
- `pkg/server/server.go` — `advertiseIP()`, `isRoutableAdvertiseIP()`,
  `stageFiles` skip (KIP-21, PR #550)
- `pkg/deploy/zz_bindata_e2b_check_test.go`, `pkg/deploy/stage_test.go`
- Vendored `staging/src/k8s.io/endpointslice@v1.35.5-k3s1` —
  `util/utils.go`, `reconciler.go` (labels, managed-by semantics)
- Vendored `staging/src/k8s.io/api@v1.35.5-k3s1/discovery/v1/types.go`
  (addressType, endpoints, ports)
- `docs/kip-21-host-advertise-ip-resolution.md` — "Related consideration"
  section (decision to keep `v1 Endpoints` for the correctness fix)
