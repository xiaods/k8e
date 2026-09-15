# KIP-28: Trust anchor bootstrap for the remote sandbox gRPC gateway

| Author | Updated | Status |
|--------|---------|--------|
| @xiaods | 2026-09-15 | Proposed |

## Summary

A remote `k8e-sandbox-cli` has no trusted way to obtain the sandbox CA, so the
first `connect` against a public gateway fails with

```text
login (verify server trust with --ca-file): … x509: “ip-192-0-2-10”
certificate is not trusted
```

The only documented escape hatch is `--ca-file`, which requires the operator to
copy a file off the server by some other means. That does not scale to agents,
CI, or anyone who did not provision the host.

This KIP defines how a remote client establishes trust in the gateway:

| Milestone | Deliverable | Server change | Proto change | Local path touched |
|-----------|-------------|---------------|--------------|--------------------|
| **M1** | Client trust-resolution chain + `--reset-certs` / `--reset-trust` split + `--ca-fingerprint` | none (optional full-chain send) | none | no |
| **M2** | CA distributed over the **already-trusted** HTTPS listener: `/.well-known/k8e/sandbox-ca.crt` | Gateway route + static backend | none | no |
| **M3** | Publicly-trusted gRPC server certificate from cert-manager | cert-manager `Certificate` + second loopback listener | yes | yes |

M1 + M2 solve the reported problem without touching the local (loopback) path or
the wire protocol. M3 is the security end state and is explicitly phased last
because it is the only one that changes how local tooling dials the gateway.

## Motivation

Two different certificates are in play on a public gateway, and only one of them
is publicly trusted (verified on a live deployment).

> **Notation.** Every hostname and address in this KIP is illustrative and drawn
> from RFC-reserved ranges — `example.com` (RFC 2606) and `192.0.2.0/24`
> (RFC 5737, TEST-NET-1). Substitute your own gateway name and node addressing;
> nothing here should be read as a real endpoint.

| Listener | Subject | Issuer | SAN |
|----------|---------|--------|-----|
| `:443` HTTPS (Cilium/e2b Gateway) | `CN=sbx.example.com` | **Let's Encrypt** via `Issuer/letsencrypt-production` | `DNS:sbx.example.com` |
| `:50051` gRPC (sandbox gateway) | `O=K8E Sandbox, CN=ip-192-0-2-10` | **`O=K8E Sandbox, CN=K8E Sandbox CA`** (k8e self-signed) | interface IPs + `DNS:sbx.example.com` |

[KIP-27](kip-27-bundled-cert-manager.md) bundles cert-manager and
[KIP-22](kip-22-sandbox-advertise-hostname.md) puts the public name in the
gateway cert's SANs — but neither makes that certificate *trusted*. The gateway
always self-signs; there is no cert-manager branch:

```go
// pkg/sandboxmatrix/grpc/server.go:391
if err := ensureServerCert(s.caKey, s.caCert, s.serverCertFile, s.serverKeyFile, s.advertiseHostname); err != nil {
    return fmt.Errorf("sandbox server cert: %w", err)
}

serverTLS, err := tls.LoadX509KeyPair(s.serverCertFile, s.serverKeyFile)
// …
creds, err := buildMTLSCreds(s.caCert, serverTLS)
```

So the SAN is correct and the *trust* is missing. A client dialling
`sbx.example.com:50051` gets a name match and a chain-of-trust failure.

### Root-cause chain (verified in tree)

1. `ensureServerCert` unconditionally signs the gateway cert from the sandbox CA
   (`pkg/sandboxmatrix/grpc/cert.go`); the SAN set comes from
   `collectServerSANs(hostname, advertiseHostname)`.
2. On the remote client, `resolveTrustFile` returns the **empty string** when
   `--ca-file` is unset and the cache is unusable:

   ```go
   // pkg/sandbox/client/client.go:285
   func resolveTrustFile(opts ConnectOptions, state cacheState, caFile string) string {
       if opts.CAFile != "" || opts.ResetCerts || state.caMissing || state.stampErr != nil {
           return opts.CAFile
       }
       return caFile
   }
   ```
3. `loginTLSConfig` treats an empty `caFile` as **system roots** — which is the
   correct default — and therefore rejects the self-signed chain:

   ```go
   // pkg/sandbox/client/client.go:591
   func loginTLSConfig(endpoint, caFile string, insecure bool) (*tls.Config, error) {
       cfg := &tls.Config{MinVersion: tls.VersionTLS12}
       if caFile == "" {
           // System roots are the default. An explicit opt-in is required to send
           // the API key without authenticating the server during initial bootstrap.
           cfg.InsecureSkipVerify = insecure //nolint:gosec // explicit bootstrap opt-in
           return cfg, nil
       }
   ```
4. `authenticate` wraps that in
   `sandbox client: login (verify server trust with --ca-file)` — the message the
   user sees. The suggestion is right; there is just no *reachable* way to get the
   file.

**Two consequences worth stating explicitly**, because they shape the whole
design:

- **The bootstrap path already defaults to WebPKI.** Nothing in the client needs
  to change for a publicly-trusted gateway cert to work. It is purely a server
  property. (M3.)
- **`--reset-certs` is a trap.** Because `resolveTrustFile` short-circuits on
  `opts.ResetCerts`, rotating a client certificate also discards the trust
  anchor, turning a working client into a broken one. Operators reach for
  `--reset-certs` precisely when something is wrong, so the failure is
  self-inflicted and repeatable. (M1.)

## Design principles

1. **Never silently degrade.** No implicit fallback to `InsecureSkipVerify`.
   The API key is a bearer credential; sending it over an unauthenticated
   channel is equivalent to publishing it.
2. **Trust must arrive over an already-authenticated channel, or be explicitly
   asserted by the operator.** "First packet wins" is only acceptable when the
   operator typed the fingerprint.
3. **The local path is sacred.** Same-host clients already have a trusted route
   to `/var/lib/k8e/server/tls/sandbox-ca.crt`. Nothing here may regress it.
4. **A CA certificate is public data.** Publishing it discloses nothing; the
   private key never leaves `/var/lib/k8e/server/tls/`.

---

## M1 — Client trust resolution (no server change)

### 1.1 Trust-resolution order

`resolveTrustFile` becomes an explicit chain. First hit wins:

| # | Source | Requires operator action | Security |
|---|--------|--------------------------|----------|
| 1 | `--ca-file <path>` | copy file once | full verification |
| 2 | Cached `ca.crt` **for this endpoint** | none (steady state) | full verification |
| 3 | `--ca-fingerprint sha256:…` | publish fingerprint | pinning + full verification |
| 4 | Well-known HTTPS fetch (M2) | none | full verification, WebPKI-protected |
| 5 | Interactive confirmation on a TTY | yes | **TOFU** — last resort |
| 6 | Fail | — | — |

Steps 1–4 are non-interactive and therefore automatable. Step 5 exists only when
`stdin` is a TTY; in CI and agent contexts it is skipped and the command fails
with the actionable error from step 6.

Note the current order is effectively 1 → 2 → *fail*. M1 makes the failure mode
explicit and enumerable rather than emergent.

### 1.2 Split `--reset-certs` into identity and trust

Today one flag destroys both. Two flags:

| Flag | Drops | Keeps | Use when |
|------|-------|-------|----------|
| `--reset-certs` | client key + client cert | **pinned CA** | cert expired/renewing, key rotation |
| `--reset-trust` | client key + client cert **+ CA** | — | gateway reinstalled, CA rotated, wrong CA pinned |

`--reset-trust` implies `--reset-certs`. `resolveTrustFile` short-circuits only
on `ResetTrust`, never on `ResetCerts`.

This alone fixes the reported repro: `connect --reset-certs` against a host whose
CA was already cached now succeeds instead of falling back to system roots.

### 1.3 `--ca-fingerprint`

```bash
k8e-sandbox-cli --endpoint sbx.example.com:50051 \
  --apikey <key> --ca-fingerprint sha256:9f2a…c41d connect
```

The client performs a **verification-only handshake** (chain captured, not
trusted), computes the SPKI SHA-256 of the peer chain's trust anchor, and aborts
unless it matches. It never sends the API key before the pin matches.

Prerequisite: the anchor must be *visible* in the handshake. Today
`tls.LoadX509KeyPair(s.serverCertFile, s.serverKeyFile)` sends whatever the PEM
file holds; if `sandbox-server.crt` contains only the leaf, the self-signed root
is not on the wire. M1 therefore includes an optional, backwards-compatible
server change: **write and serve the full chain (leaf + CA)**. Appending a public
CA certificate to a serving chain leaks nothing and lets every client pin the
anchor without a file copy.

When the full chain is present, M2's fetch and M1's pin can both be validated
against the same anchor.

### 1.4 Actionable failure

Step 6 must not just restate the x509 error. It prints, per endpoint:

```text
TLS trust failed for sbx.example.com:50051
  server offered: O=K8E Sandbox, CN=ip-192-0-2-10
                  issued by O=K8E Sandbox, CN=K8E Sandbox CA
  no trusted CA available for this endpoint. Choose one:
    1. --ca-file <path>               (copy /var/lib/k8e/server/tls/sandbox-ca.crt from the server)
    2. --ca-fingerprint sha256:<hex>  (anchor SPKI, from the operator)
    3. https://sbx.example.com/.well-known/k8e/sandbox-ca.crt  (M2)
```

---

## M2 — CA distribution over the trusted HTTPS listener (recommended)

### 2.1 Idea

The gateway host **already terminates trusted HTTPS on `:443`** with a
cert-manager/Let's Encrypt certificate for exactly the hostname the client is
dialling. Serve the sandbox CA there. The client fetches it over WebPKI, then
pins it.

```http
GET https://<grpc-host>/.well-known/k8e/sandbox-ca.crt
→ 200, Content-Type: application/x-x509-ca-cert
→ PEM of /var/lib/k8e/server/tls/sandbox-ca.crt
```

Host is derived from the gRPC endpoint (`host:port` → `https://host`), default
port 443. A non-default gRPC port says nothing about the HTTPS port; the HTTPS
port defaults to 443 and is overridable with `--ca-url` for split-horizon setups.

### 2.2 Server side

- Add an HTTPRoute on the existing `https` listener, path prefix
  `/.well-known/k8e/`, routing to a static backend that serves the CA from the
  host filesystem (`/var/lib/k8e/server/tls/sandbox-ca.crt`, read-only mount).
- Cache-Control: short (e.g. `max-age=300`) — the CA is effectively immutable
  (10-year self-signed root), so this is just churn control.
- Serve it on the **HTTPS** listener only. An HTTP listener is not an
  authenticated channel and must not be used for this.

This is additive: it creates no Issuer, requests no certificate, and — per
KIP-27 — changes nothing about who can reach a sandbox.

### 2.3 Client side

1. `GET` with **system roots**, `InsecureSkipVerify: false`, 5s timeout.
2. Reject non-200, reject `Content-Type` other than
   `application/x-x509-ca-cert` / `text/plain` / `application/x-pem-file`.
3. Cap the body (e.g. 64 KiB) to bound a hostile or misconfigured server.
4. Parse strictly: exactly one PEM block of type `CERTIFICATE`, and it must have
   `IsCA` / `BasicConstraintsValid` set.
5. **Pin on first use**: store as `ca.crt` and record `trust_source: well-known`
   plus the anchor SPKI in the endpoint stamp.
6. On every later connect, require the anchor to be identical. A changed anchor
   is an error, not a silent re-pin — the operator must run `--reset-trust`.

### 2.4 Security analysis

- **What is published**: a CA *certificate*. Public by construction. The private
  key stays on the host.
- **Adversary model**: to serve a hostile CA at this URL, an attacker must serve
  valid HTTPS for the gateway's hostname — i.e. already control DNS or hold a
  certificate for it. That adversary can already impersonate the gateway to any
  relying party. **The new surface is nil.**
- **What is not protected**: this does not authenticate the *first* fetch beyond
  WebPKI. That is exactly the guarantee WebPKI provides, and it is strictly
  stronger than today's out-of-band `scp`.
- **Residual risk**: a CA pinned during a window in which the HTTPS channel was
  compromised stays pinned. Mitigated by the pin-on-first-use check (step 6),
  by `doctor` printing the pinned anchor fingerprint, and by `--reset-trust`.

### 2.5 Fallback

No trusted HTTPS (bare IP, internal name, ACME-unreachable) → skip to step 5/6
of the resolution chain. M2 is an *additional* rung, never a dependency.

---

## M3 — Publicly-trusted gRPC server certificate (end state)

### 3.1 Server

- New flag `--sandbox-server-cert-source=internal|external|auto`
  (default **`internal`** for the first release that ships it; `auto` only after
  the client base has rolled forward — see 3.4).
- `external`: when `--sandbox-advertise-hostname` is set, ensure a cert-manager
  `Certificate` for that hostname and watch the resulting Secret
  (`issuerRef` defaults to the operator's public Issuer, e.g.
  `letsencrypt-production`); write it to
  `/var/lib/k8e/server/tls/external-server.{crt,key}` and hot-reload.
  cert-manager handles renewal; k8e only reacts to Secret updates.
- `auto`: use `external` **only if** the advertise hostname is a DNS name (not an
  IP — public CAs do not issue IP certificates) *and* a usable external cert is
  present. Otherwise fall back to `internal`.
- **Client-certificate verification is unchanged.** `buildMTLSCreds(s.caCert, …)`
  keeps verifying client certs against the sandbox CA. Server identity and
  client identity legitimately live in different trust domains; nothing about
  mTLS requires a single CA.

With 3.1 in place the bootstrap needs **no client change at all** — the client
already defaults to system roots:

```bash
k8e-sandbox-cli --endpoint sbx.example.com:50051 --apikey <key> connect
```

### 3.2 The loopback problem (why M3 is not free)

A single TLS listener presents one certificate. A Let's Encrypt certificate for
`sbx.example.com` has no `127.0.0.1` SAN and cannot have one. Local clients
(`dialMTLSMaterial` → `loopbackTLSConfig`) verify the chain against the sandbox
CA pool and deliberately skip hostname verification; swapping the server cert
breaks both halves.

`0.0.0.0:50051` and `127.0.0.1:50051` cannot coexist, so M3 requires a
**second, loopback-bound listener on its own port** (e.g. `127.0.0.1:50052`)
serving the self-signed sandbox-CA cert, with local consumers repointed:

- the embedded E2B server's gateway dial (KIP-18 / KIP-24 expose chain),
- `k8e-sandbox-cli` local mode,
- any `LocalAuth` loopback path.

This is the single largest cost in the KIP and the reason M3 is last. It must be
solved in the same change as the external cert, not deferred.

### 3.3 Proto change: decouple trust from identity

Today the client conflates two roles of `ca_cert`: *issuer of my client cert*
and *anchor for verifying the server*. With 3.1 they diverge, and steady-state
dials break, because `dialMTLSMaterial` uses the cached CA as `RootCAs`:

```go
// pkg/sandbox/client/client.go:539
func dialMTLSMaterial(endpoint string, mat *mtlsMaterial) (*grpc.ClientConn, error) {
    var creds credentials.TransportCredentials
    if isLoopback(endpoint) {
        creds = credentials.NewTLS(loopbackTLSConfig(mat.pool, mat.clientCert))
    } else {
        creds = credentials.NewTLS(&tls.Config{
            Certificates: []tls.Certificate{mat.clientCert},
            RootCAs:      mat.pool,
            MinVersion:   tls.VersionTLS12,
        })
    }
```

Add a field to `proto/sandbox/v1/sandbox.proto`:

```proto
message LoginResponse {
  string cert    = 1;  // PEM-encoded signed client certificate
  string ca_cert = 2;  // PEM-encoded sandbox CA (issuer of `cert`; default server anchor)
  int64  valid_days = 3;

  // How the client must verify the *server* on subsequent dials.
  enum ServerTrust {
    SERVER_TRUST_UNSPECIFIED = 0;  // legacy server: verify with ca_cert
    SERVER_TRUST_SANDBOX_CA  = 1;  // server cert is self-signed: verify with ca_cert
    SERVER_TRUST_SYSTEM      = 2;  // server cert is publicly trusted: use system roots
  }
  ServerTrust server_trust = 4;
}
```

`UNSPECIFIED` preserves today's behaviour exactly. The client persists
`trust_mode` per endpoint next to the CA; `dialMTLSMaterial` selects
`RootCAs = mat.pool` (modes 0/1) or `RootCAs = nil` (mode 2, system roots).

### 3.4 Compatibility matrix

| Client | Server | server_trust | Result |
|--------|--------|--------------|--------|
| new | old | absent → `UNSPECIFIED` | uses `ca_cert` — today's behaviour |
| old | new | ignored (unknown field) | uses `ca_cert` to verify an LE cert → **fails** |

The second row is why `--sandbox-server-cert-source` must default to `internal`
and only be flipped to `auto` once the client fleet has rolled forward. Ship the
server capability and the client capability in separate releases, flip the
default in a third.

---

## Resulting operator UX

```bash
# Remote, trusted HTTPS available (M2) — no flags beyond the endpoint and key
k8e-sandbox-cli --endpoint sbx.example.com:50051 --apikey <key> connect

# Remote, no trusted HTTPS — operator-published anchor
k8e-sandbox-cli --endpoint sbx.example.com:50051 --apikey <key> \
  --ca-fingerprint sha256:9f2a…c41d connect

# Rotating a client cert keeps the pinned CA (M1)
k8e-sandbox-cli connect --reset-certs

# Gateway reinstalled / CA rotated
k8e-sandbox-cli connect --reset-trust --apikey <key>
```

## Acceptance criteria

### M1

1. `--reset-certs` against an endpoint with a valid cached CA succeeds without
   `--ca-file`.
2. `--reset-trust` drops the CA and re-enters the resolution chain.
3. `--ca-fingerprint` with a wrong value fails **before** any credential is sent.
4. A non-TTY client with no trust source fails with the enumerated error listing
   all three remedies.

### M2

1. `curl https://<host>/.well-known/k8e/sandbox-ca.crt` returns the sandbox CA
   over a WebPKI-verified connection.
2. Client pins on first use and **refuses** a changed anchor on later connects.
3. Body-size, Content-Type, and single-PEM-block checks are enforced.
4. Endpoint served over plain HTTP is rejected.

### M3

1. `external` mode serves a publicly-trusted cert on `:50051`; a stock client
   with zero cert flags verifies it against system roots.
2. Loopback consumers (E2B server, local CLI, `LocalAuth`) still work against
   the second listener.
3. `server_trust=SYSTEM` round-trips and drives `RootCAs` selection.
4. Old client + new server fails loudly, not silently (3.4), and the default
   remains `internal` until sign-off.

### Cross-cutting

1. Local mode and `/var/lib/k8e/server/tls/sandbox-ca.crt` discovery are
   unchanged in every milestone.
2. No code path sets `InsecureSkipVerify` except the existing explicit
   `--insecure-bootstrap` opt-in and the pre-existing loopback helper.

## Touch points

| Area | Path |
|------|-----|
| Trust resolution chain | `pkg/sandbox/client/client.go` (`resolveTrustFile`, `dialMTLSMaterial`) |
| Reset flags | `pkg/sandboxcli/connect.go`, `cmd/sandboxcli/main.go` |
| Gateway server cert | `pkg/sandboxmatrix/grpc/cert.go` (`ensureServerCert`, `collectServerSANs`) |
| Gateway startup / creds | `pkg/sandboxmatrix/grpc/server.go` |
| Wire protocol | `proto/sandbox/v1/sandbox.proto` (`LoginResponse`) |
| Well-known route | Gateway HTTPRoute + static backend (KIP-18 / KIP-27 listeners) |
| Docs | this KIP, embedded `SKILL.md`, README remote-connect section |

## Non-goals

- Replacing mTLS or changing client-cert issuance (KIP-14).
- Making `--insecure-bootstrap` the default, or any implicit insecure fallback.
- Auto-discovering the public hostname from cloud metadata — the operator sets it
  (KIP-22).
- Changing `advertiseIP()` / the cluster-internal bridge (KIP-21).
- Solving trust for endpoints that are bare IPs. Those need `--ca-file` or
  `--ca-fingerprint`; public CAs do not issue IP certificates, and the
  well-known fetch needs a hostname.

## Alternatives considered

| Option | Why not |
|--------|---------|
| Keep documenting `--ca-file` only | Does not work for agents, CI, or anyone who did not provision the host. This is the status quo being fixed. |
| Default `--insecure-bootstrap` | Sends a bearer API key over an unauthenticated channel. Violates principle 1. |
| TOFU with no pinning, non-interactive | Silently trusts whichever CA answered first. Strictly worse than the x509 error. |
| Embed the CA in the API key / out-of-band bundle | Couples credential rotation to CA rotation; bloats a secret that is already pasted into shells and env files. |
| Reuse the kube-apiserver CA | Different trust domain; would let any apiserver client mint sandbox identities. |
| Short-lived bootstrap token instead of trust bootstrap | Doesn't address the problem: the CA is still unknown after the token is redeemed. |
| M2 only, skipping M3 | Defensible and cheaper; kept as a valid stopping point. M3 stays in the KIP because it removes the out-of-band channel entirely. |

## References

- `pkg/sandbox/client/client.go` — `resolveTrustFile`, `loginTLSConfig`, `dialMTLSMaterial`, `loopbackTLSConfig`
- `pkg/sandboxmatrix/grpc/cert.go` — `ensureServerCert`, `collectServerSANs`
- `pkg/sandboxmatrix/grpc/server.go` — `ensureServerCert` / `buildMTLSCreds` wiring
- [KIP-14](kip-14-mtls-dynamic-cert-issuance.md) — mTLS dynamic client certs
- [KIP-17](kip-17-sandbox-cli-profiles-and-apikey-ttl.md) — profiles, cert dirs, API key TTL
- [KIP-22](kip-22-sandbox-advertise-hostname.md) — advertise hostname + SAN regen
- [KIP-24](kip-24-sandbox-service-exposure.md) — the loopback gateway consumer in 3.2
- [KIP-27](kip-27-bundled-cert-manager.md) — bundled cert-manager and the `sandbox-e2b` HTTPS cert
