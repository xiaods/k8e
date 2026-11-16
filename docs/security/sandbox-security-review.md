# K8E Sandbox Security Review

Source-reviewed at commit `d06b5f0b` (worktree clean). Scope: the sandbox
control plane and data plane — `pkg/sandboxmatrix`, `pkg/sandbox`,
`pkg/sandboxcli`, `pkg/sandboxmcp`, `pkg/sandboxlayer`, `sandboxd`, and
`manifests/sandbox-matrix`.

Method: static source review only (no live probing, no deployed endpoints).
Each finding names the lower-trust principal, the accepted input, the intended
control, the boundary crossed, and the concrete result. Findings that depend on
a deployment choice are marked as such.

## Summary

| ID | Severity | Area | Status |
|----|----------|------|--------|
| F1 | High | gRPC gateway / E2B control plane: no tenant or ownership authorization | Fixed (opt-in tenant binding) |
| F2 | High | Certificate revocation was in-memory only and skipped on first load | Fixed |
| F3 | High | `Login` bypassed certificate revocation (re-enroll with a revoked cert) | Fixed |
| F4 | Medium | Rate limiter trusted a client-supplied tenant header | Fixed |
| F5 | Medium | Warm pods accepted ingress from any pod (policy had no ingress rules) | Fixed |
| F6 | Medium | Session / sub-agent / run identifiers were predictable timestamps | Fixed |
| F7 | Medium | `secret_refs` could reference any non-reserved Secret in the namespace | Fixed (tenant label scope) |
| F8 | Medium | `localAuth` trusts every loopback peer without a client cert | Open (by design) |
| F9 | Low | Sandbox pod security context lacks seccomp / capability drop | Fixed (no SA token) |
| F10 | Low | `PipInstall` allowed pip option injection | Open (not a privilege gain) |
| F11 | Low | E2B signing secret falls back to the CA private key / per-process random | Open |

## F1 — No tenant or ownership authorization on session RPCs (High, fixed)

**Before.** Any authenticated caller (any API key, any client certificate signed
by the sandbox CA) could present any `session_id`. `checkMTLSAuth`
(`pkg/sandboxmatrix/grpc/server.go`) only checked that a valid, non-revoked
client certificate existed, and `getSession` / `listSessions` performed no
filtering. The E2B control plane had the same gap.

**Decision (user-approved).** Add an optional `tenant` field to
`apikey.Record`; when set, the gateway derives the caller's tenant from the
certificate CN (the API key name) and enforces it on every session-scoped RPC.
Keys without a tenant keep the legacy shared-credential behaviour, so
single-tenant installs are unaffected.

**Implementation.**

- `apikey.Record.Tenant` (`pkg/sandbox/apikey/store.go`) round-trips through
  the v2 JSON; an empty tenant is omitted, so the wire format stays compatible
  with pre-tenant Secret payloads. `NewRecordWithTenant` is the constructor.
- The gateway keeps a name→tenant map (`Server.apiKeyTenants`), reloaded from
  the Secret with the keys, and swapped atomically with them.
- The auth interceptor stashes the caller's tenant in the request context
  (`withCallerTenant`), so handlers and the orchestrator can enforce it
  without re-reading the Secret.
- `Orchestrator.getSession` denies a session whose `spec.tenantID` differs from
  the caller's tenant, returning **NotFound** (not PermissionDenied) so a
  cross-tenant caller cannot learn that the session exists.
  `getSessionRaw` remains for internal cleanup/pruning.
- `Orchestrator.listSessions` filters to the caller's tenant.
- `Orchestrator.createSessionWithTTL` applies the bound tenant when the request
  omits one and rejects an explicit mismatch with PermissionDenied.
- `Server.getPodIP` enforces the same check, which covers every file, exec,
  transcript, events, process and terminal RPC (they all route through it).
- The expose registry (`ExposeService` fast path, `UnexposeService`,
  `ListExposed`) enforces the tenant before reading or mutating it.
- `ConfirmAction` only opens/confirms approvals for sessions the caller may see,
  and approval IDs are now random (`approval-<hex>`) instead of
  `approval-<session>-<UnixNano>`.
- The E2B control plane (`pkg/sandbox/e2b`) threads the same binding:
  `SecretKeySet.Tenants` feeds `ReplaceAPIKeysWithTenants`, the auth wrapper
  puts the tenant in the request context, `handleCreate` stamps it,
  `findLive`/`handleConnect`/`listLiveSessions` scope to it, and
  `reacquireByKey` refuses a `metadata.name` owned by another tenant.
- `k8e sandbox-apikey create --tenant <id>` sets the binding; `list` shows it.

**Why NotFound rather than PermissionDenied.** A caller from another tenant must
not learn that a session exists. NotFound also matches the existing handlers,
which already mask lookup errors.

**Backward compatibility.** A key with no tenant is unbound: no request is
filtered, no session is rejected, and the JSON omits the field. This is the
only mode existing installs get until they opt in.

**Tests.** `TestCallerTenantFromCertificate`, `TestGetSessionTenantIsolation`,
`TestListSessionsTenantFilter`, `TestCreateSessionTenantBinding`,
`TestExposeServiceTenantIsolation`, `TestConfirmActionApprovalIDIsRandom`
(`pkg/sandboxmatrix/grpc/authz_test.go`); `TestTenantRoundTripsThroughV2`,
`TestTenantOmittedWhenEmpty` (`pkg/sandbox/apikey/store_test.go`);
`TestControlPlaneTenantIsolation`, `TestControlPlaneTenantNameConflict`
(`pkg/sandbox/e2b/authz_test.go`).

## F2 — Certificate revocation was not durable (High, fixed)

**Before.** `RevocationList` was created empty on every start
(`newRevocationList()`), and revocations were only computed from the previous
in-memory API-key snapshot:

```go
if s.revocList != nil && s.issuedStore != nil && prev != nil { ... }
```

Two consequences:

- After a gateway restart, every previously revoked certificate became valid
  again until its 90-day expiry.
- A key deleted while the gateway was down was never revoked (`prev` is nil on
  the first load after restart).

**Fix.**

- `RevocationList` now persists to `<tlsDir>/sandbox-revoked.json`
  (`pkg/sandboxmatrix/grpc/cert.go:436`) and loads it on start.
- The issued-cert ledger and revocation list are initialised before the first
  `loadAPIKeys` call (`pkg/sandboxmatrix/grpc/server.go:409`).
- `revokeRemovedKeys` (`server.go:311`) reconciles every key name in the issued
  ledger against the active set on each load, not only against the previous
  snapshot.
- A genuinely missing Secret (`apierrors.IsNotFound`) revokes all issued certs;
  a transient read error keeps the last known key set instead of locking every
  client out.

**Tests.** `TestRevocationListPersistsAcrossRestart`,
`TestRevokeRemovedKeysReconcilesInactiveKeys`
(`pkg/sandboxmatrix/grpc/cert_store_test.go`).

## F3 — `Login` bypassed revocation (High, fixed)

**Before.** The mTLS interceptor exempts `Login` (it is how a client obtains its
first certificate). `Login` derived the key name from the client certificate but
never checked the revocation list, so a holder of a revoked certificate could
call `Login` again and receive a fresh 90-day certificate for the same API key.

**Fix.** `Login` now rejects a revoked certificate before issuing a new one
(`pkg/sandboxmatrix/grpc/server.go:1144`). API-key callers (no certificate) are
unaffected, because the key itself is the root credential.

**Test.** `TestLoginRejectsRevokedCertificate` (`cert_test.go`).

## F4 — Rate limiter trusted a client header (Medium, fixed)

**Before.** `extractTenant` preferred the `x-sandbox-tenant` metadata header:

```go
if vals := md.Get("x-sandbox-tenant"); len(vals) > 0 { return vals[0] }
```

A client could rotate that header to get a fresh bucket per request (evading
the limit) or set it to another tenant's name to exhaust that tenant's bucket.
In the common case (mTLS clients send neither the header nor `Authorization`)
every remote client hashed to `anon` and shared one global bucket, so one noisy
tenant could starve all others.

**Fix.** `extractTenant` (`pkg/sandboxmatrix/ratelimit/ratelimit.go:201`) now
derives the bucket key from the authenticated identity only:

1. mTLS client certificate CommonName (the API key name), else
2. SHA-256 of the bearer token presented to `Login`, else
3. `local` for loopback peers (the embedded e2b server), else
4. `anon`.

Loopback gets its own bucket so a remote unauthenticated client cannot drain the
budget the in-process e2b data plane depends on.

**Test.** `TestExtractTenant` asserts the certificate CN wins, that two
different tenant headers map to the same bucket, that loopback is isolated from
remote peers, and that distinct tokens do not collide.

## F5 — Warm pods accepted ingress from any pod (Medium, fixed)

**Before.** `manifests/sandbox-matrix/network-policy.yaml` selected warm pods and
declared only `egress`. A Cilium policy with egress but no ingress leaves ingress
unrestricted, so any pod in the cluster could reach the unauthenticated
`sandboxd` on `:2024` of a warm (not yet claimed) sandbox.

**Fix.** The policy now allows ingress to `:2024` only from the `host` entity
(the kubelet TCP readiness/startup probe) and otherwise denies ingress.

## F6 — Predictable identifiers (Medium, fixed)

**Before.** `sess-<UnixNano>`, `<parent>-sub-<UnixNano>` and
`<session>-bg-<UnixNano>` were derived from the wall clock. Combined with F1,
an attacker could guess session IDs.

**Fix.** `randomHex` (`pkg/sandboxmatrix/grpc/orchestrator.go:117`) supplies
16 random bytes for session IDs and 8 for sub-agent/run IDs. The `sess-` and
`-sub-`/`-bg-` prefixes are kept, so identifiers remain readable and existing
parsing (which never depended on the numeric suffix) is unaffected.

**Test.** `TestCreateSession_GeneratesID` asserts the `sess-` prefix, a 32-char
hex suffix, and uniqueness.

## F7 — `secret_refs` can read any namespace Secret (Medium, fixed)

**Before.** An authenticated caller could create a session with `secret_refs`
pointing at any Secret in the sandbox namespace except `sandbox-apikeys`.
`reservedSessionSecretNames` (`pkg/sandboxmatrix/grpc/env.go`) was a two-entry
deny-list and `isSessionSecretRefAllowed` rejected only those; `readSecretKey`
read the Secret with the gateway's own RBAC and injected it into the session
environment, where `Exec` could print it. This was independent of F1: even a
caller bound to its own tenant could name any non-reserved Secret.

**Fix.** `secret_refs` is now tenant-scoped at exec resolution:

- A Secret may carry the label `sandbox.k8e.io/tenant: <tenant>`. A labelled Secret is readable
  only by a session whose `spec.tenantID` equals that label.
- An unlabelled Secret is legacy-compatible for the shared `default` tenant only;
  a non-default tenant cannot reference it (otherwise the label would be
  advisory).
- `checkSecretTenantScope` (`pkg/sandboxmatrix/grpc/server.go`) enforces this;
  `readSecretKey` takes the session's tenant and calls it before returning the
  value.
- The gateway-internal deny-list is unchanged and still applies (defence in
  depth), and the error never includes the Secret value.

**Operator action.** Label tenant-owned Secrets:

```bash
kubectl -n sandbox-matrix label secret my-llm-secret sandbox.k8e.io/tenant=acme
```

Existing unlabelled Secrets keep working for the `default` tenant; label them
before moving a tenant off `default`.

**Tests.** `TestCheckSecretTenantScope`, `TestReadSecretKey_TenantScope`,
`TestResolveSessionEnv_RejectsCrossTenantSecret`
(`pkg/sandboxmatrix/grpc/secret_scope_test.go`).

## F8 — `localAuth` trusts every loopback peer (Medium, open, by design)

**Evidence.** The gateway listens on `0.0.0.0:50051`
(`pkg/sandboxmatrix/grpc/server.go`), `peerIdentity` returns `isLocal=true` for
any peer whose address is loopback, and the controller hard-codes
`LocalAuth: true` (`pkg/sandboxmatrix/controller.go:84`). With `LocalAuth`,
`checkMTLSAuth` returns success for loopback peers without any client
certificate.

**Result.** Any process able to connect to `127.0.0.1:50051` on the host — for
example a sidecar in the gateway pod, or any local user on a single-node
install — can drive the full sandbox API (create/exec/read/write/destroy any
session) without a certificate.

**Assessment.** This is documented as intentional ("LocalAuth permits exactly
those loopback connections"), and the embedded e2b server relies on it. It is
only a real escalation on a multi-tenant host or a host with untrusted local
users. Hardening would require the embedded e2b client to present a credential
on loopback (for example the API key) and `localAuth` to require it.

## F9 — Sandbox pod security context (Low, fixed for the SA token)

**Before.** `SandboxPodSpec` (`pkg/sandboxmatrix/grpc/orchestrator.go`) set only
`ReadOnlyRootFilesystem: true`; the pod mounted the default ServiceAccount token.

**Fix.** `AutomountServiceAccountToken: false` — sandboxd is a static Zig binary
that never talks to the Kubernetes API, so the default ServiceAccount token is an
unnecessary credential inside an untrusted, agent-controlled workload. The
kubelet TCP probes come from the node and do not need it.

**Still open (deliberately not changed here).** The sandbox image runs as root by
design (`sandbox/Dockerfile`: "Root inside the sandbox is safe — isolation is
enforced by the RuntimeClass boundary"), and agents need to install packages.
`SeccompProfile: RuntimeDefault`, `AllowPrivilegeEscalation: false` and dropping
capabilities are likely to break real agent workloads (`sudo`, `mount`, package
managers) and should be a separate, opt-in decision validated against those
workloads.

**Test.** `TestSandboxPodSpec_DoesNotMountServiceAccountToken`
(`pkg/sandboxmatrix/grpc/orchestrator_test.go`).

## F10 — `PipInstall` option injection (Low, open)

`sanitizePipPackage` validates the base package name and rejects
`; & | ` $ ( ) \n \r`, but the raw specifier is joined into
`pip install --no-cache-dir <spec>`. Characters such as space, `-`, `>`, `<`,
quotes and `#` are not rejected, so a specifier like `pkg>=1.0 --index-url=…`
reaches pip as additional options (argument injection, not shell command
injection).

This is **not** a privilege boundary in practice: any caller who can call
`PipInstall` can also call `Exec` with an arbitrary command in the same sandbox.
It is worth tightening (reject whitespace and a leading `-`, or shell-quote each
specifier) as defence in depth, but it does not change the trust model.

## F11 — E2B signing secret fallback (Low, open)

`resolveSigningSecret` (`pkg/sandbox/e2b/server.go`) uses
`K8E_E2B_SIGNING_SECRET`, else reads the sandbox CA private key
(`/var/lib/k8e/server/tls/sandbox-ca.key`), else generates a random per-process
key. Using the CA private key as an HMAC key couples two secrets, and the
per-process fallback makes envd tokens and signed file URLs invalid after a
restart (availability, not confidentiality). Consider documenting a required
`K8E_E2B_SIGNING_SECRET` for production and failing closed when it is absent.

## Hardening implemented in this change

| Area | Change | Tests |
|------|--------|-------|
| Tenant isolation | optional `apikey.Record.Tenant`; gateway + E2B control plane enforce it on every session-scoped RPC (NotFound on mismatch), create stamps/rejects it, list filters it | `TestCallerTenantFromCertificate`, `TestGetSessionTenantIsolation`, `TestListSessionsTenantFilter`, `TestCreateSessionTenantBinding`, `TestExposeServiceTenantIsolation`, `TestControlPlaneTenantIsolation`, `TestTenantRoundTripsThroughV2` |
| Secret refs | `secret_refs` are tenant-scoped: a Secret labelled `sandbox.k8e.io/tenant=<tenant>` is readable only by that tenant; unlabelled Secrets remain legacy-compatible for `default` | `TestCheckSecretTenantScope`, `TestReadSecretKey_TenantScope`, `TestResolveSessionEnv_RejectsCrossTenantSecret` |
| Rate limiting | bucket key from authenticated identity (cert CN / token hash / loopback), never the client header | `TestExtractTenant` |
| Certificate revocation | persisted to disk, loaded on start, reconciled on every key load, checked at `Login` | `TestRevocationListPersistsAcrossRestart`, `TestRevokeRemovedKeysReconcilesInactiveKeys`, `TestLoginRejectsRevokedCertificate` |
| Identifiers | crypto-random session / sub-agent / run / approval IDs | `TestCreateSession_GeneratesID`, `TestConfirmActionApprovalIDIsRandom` |
| Network policy | warm pods deny ingress except kubelet probes | manifest only |
| Pod hardening | sandbox pods set `AutomountServiceAccountToken: false` (no Kubernetes credential inside the untrusted workload) | `TestSandboxPodSpec_DoesNotMountServiceAccountToken` |

Validation run:

```bash
go build ./pkg/...
go vet   ./pkg/sandboxmatrix/... ./pkg/sandbox/... ./pkg/server/...
go test  ./pkg/sandboxmatrix/... ./pkg/sandbox/... ./pkg/sandboxmcp/... \
         ./pkg/sandboxcli/... ./pkg/server/... -count=1
```

All pass.

## Recommended follow-ups (in priority order)

1. F7: turn `secret_refs` into a tenant-labelled allowlist. (With F1 now
   enforced, this is the remaining cross-tenant data path: a session can still
   reference any non-reserved Secret in the namespace.)
2. F8: require a credential on loopback if the gateway host is shared.
3. F9: add `AutomountServiceAccountToken: false` to `SandboxPodSpec`.
4. F10: tighten `sanitizePipPackage` and shell-quote each specifier.
5. F11: require an explicit `K8E_E2B_SIGNING_SECRET` in production.
6. Snapshots (`SnapshotPut/Get/List`) are still cluster-global by name; scope
   them per tenant if snapshot content is sensitive.
7. Optionally add `SeccompProfile: RuntimeDefault` once validated against real
   agent workloads (see F9).

## Operator note

F1 is opt-in. Existing keys are unbound and keep the legacy shared-credential
behaviour. To isolate tenants, recreate keys with a binding:

```bash
k8e sandbox-apikey create acme-agent --tenant acme
k8e sandbox-apikey list          # shows the tenant
```

Clients then pass `--tenant acme` (or `K8E_SANDBOX_TENANT=acme`); a mismatch is
rejected with PermissionDenied. The binding is enforced by the sandbox gRPC
gateway and the E2B control plane, not by the CLI.