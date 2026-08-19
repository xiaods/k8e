# KIP-22: First-class advertise hostname for the sandbox gRPC gateway

| Author | Updated | Status |
|--------|---------|--------|
| @pi-agent | 2026-08-18 | Accepted — implemented |

## Summary

Remote deployments (AWS EC2, GCP, bare-metal behind NAT) commonly expose a
**public** IP/domain while the host's network interfaces only carry **private**
IPs. The sandbox gRPC gateway (`:50051`, mTLS) and the embedded E2B server
(`:3676`) run in the k8e-server host process; remote clients dial them through
the public name. For the mTLS handshake to succeed, the gateway's **server
certificate** must carry that public name as a SAN.

Today there is no first-class way to configure that name:

- `tls-san` (in `/etc/k8e/config.yaml`) only feeds the **kube-apiserver**
  serving cert (`ControlConfig.SANs`), never the sandbox gateway cert — so it is
  the wrong knob.
- `K8E_SANDBOX_ADVERTISED_HOSTNAME` does add a DNS SAN to the gateway cert, but
  it is a fragile side-channel: the cert is **cached** (reused while >30 days of
  validity) and is never regenerated when the value changes; the env var must be
  injected into the k8e-server **process** environment (systemd), not a login
  shell; and there is no validation (an IP is naively treated as a DNS name).

This KIP introduces a first-class, validated `--sandbox-advertise-hostname`
flag (also settable via the config file) and makes the gateway regenerate its
server cert when the configured SAN set changes.

## Root-cause chain (verified)

1. `pkg/cli/cmds/server.go` — `--tls-san` → `ServerConfig.TLSSan`.
2. `pkg/cli/server/server.go` — `ControlConfig.SANs = SplitStringSlice(cfg.TLSSan)`.
3. `pkg/daemons/control/` — `SANs` only shapes the apiserver/supervisor/etcd
   serving certs. The sandbox gateway cert is generated separately in
   `pkg/sandboxmatrix/grpc/cert.go` → `collectServerSANs`.
4. `collectServerSANs(hostname)` collects `os.Hostname()` + non-loopback
   interface IPs (private VPC IPs in AWS) + `K8E_SANDBOX_ADVERTISED_HOSTNAME`.
5. `ensureServerCert` reuses the on-disk cert when it is still valid >30 days,
   **regardless of SAN changes** — so setting the env var after first boot has
   no effect until the cert expires or is deleted by hand.

## Design

### Config field + flag

- `config.SandboxConfig.AdvertiseHostname` (new field).
- `--sandbox-advertise-hostname <name-or-ip>` flag, env
  `K8E_SANDBOX_ADVERTISED_HOSTNAME` (kept for backward compat, merged as a
  fallback). Settable via `/etc/k8e/config.yaml` like any other flag
  (`sandbox-advertise-hostname: sandbox.example.com`).

### SAN collection

```go
func collectServerSANs(hostname, advertiseHostname string) serverSANs
```

- Always: machine `hostname` (DNS) + non-loopback interface IPs.
- `advertiseHostname` (flag) and the legacy env var are merged; a value that
  parses as an IP is added to the **IP** SANs, anything else is added as a
  **DNS** SAN. Loopback values are dropped. Duplicates are deduped.
- **Validation (hard fail)**: a non-IP value must be a bare RFC 1123 DNS name.
  URL schemes (`https://…`), `host:port`, paths, whitespace, and invalid labels
  are **rejected at startup** — `k8e server` refuses to start with an actionable
  error (`invalid --sandbox-advertise-hostname: …`), and the gateway's
  `collectServerSANs` enforces the same check as a last line of defense. A
  malformed value can never be silently omitted, which would otherwise leave an
  operator with a gateway whose certificate cannot authenticate the configured
  endpoint.

### Certificate regeneration on SAN change

`ensureServerCert` now reuses the on-disk cert only when **both** hold:

1. still valid >30 days, **and**
2. `sansCoveredBy(cert, want)` — every desired DNS name and IP is already
   present in the cert.

Changing `--sandbox-advertise-hostname` (or the env var) and restarting the
server therefore regenerates the cert automatically — no manual deletion of
`/var/lib/k8e/server/tls/sandbox-server.{crt,key}` required.

Because SAN-driven rotations can now fire on any restart, cert/key files are
replaced **atomically** (sibling temp file + fsync + rename), so a crash
mid-rotation cannot leave a truncated cert or key behind.

## Scope vs. the cluster-internal door

The internal `advertiseIP()` → `%{ADVERTISE_IP}%` path (KIP-21, the headless
Service/Endpoints bridge) is **unchanged**: pods reach the host process via the
private IP, which is correct inside the VPC. This KIP only adds the *external*
name to the cert SANs so remote clients can complete the mTLS handshake through
the public domain/IP.

## Acceptance criteria

1. `--sandbox-advertise-hostname sandbox.example.com` yields a gateway server
   cert whose `DNSNames` include `sandbox.example.com`.
2. The same flag with a literal IP yields an `IPAddresses` entry (not a DNS
   entry).
3. `K8E_SANDBOX_ADVERTISED_HOSTNAME` alone still works (backward compat).
4. Changing the value and restarting regenerates the cert (SAN-coverage check).
5. `go build ./pkg/...`, `go vet ./pkg/sandboxmatrix/... ./pkg/cli/...`, and
   `go test ./pkg/sandboxmatrix/... ./pkg/server/...` pass.

## Implementation

| # | Change | Files |
|---|--------|-------|
| 1 | `AdvertiseHostname` config + flag (env `K8E_SANDBOX_ADVERTISED_HOSTNAME`) | `pkg/daemons/config/types.go`, `pkg/cli/cmds/server.go`, `pkg/cli/server/server.go` |
| 2 | Thread the value into the gRPC server | `pkg/sandboxmatrix/controller.go`, `pkg/sandboxmatrix/grpc/server.go` |
| 3 | SAN merge + IP-vs-DNS + regen-on-change | `pkg/sandboxmatrix/grpc/cert.go` |
| 4 | Unit tests | `pkg/sandboxmatrix/grpc/cert_test.go` (new) |

## Non-goals

- TLS termination for the Cilium Gateway API HTTPRoute (`:443`) — that is the
  Gateway's own certificate, configured separately.
- Changing `advertiseIP()` / the Endpoints bridge (KIP-21).
- Auto-discovery of the public IP via cloud metadata (AWS IMDS / EIP query) —
  the operator supplies the name explicitly; auto-discovery can be a follow-up.

## References

- `pkg/sandboxmatrix/grpc/cert.go` — `ensureServerCert`, `collectServerSANs`
- `pkg/cli/cmds/server.go` — `--tls-san` (apiserver-only), sandbox flags
- `docs/kip-21-host-advertise-ip-resolution.md` — the internal advertise-IP path
- `docs/kip-18-sandbox-e2b-compat.md` — the ingress architecture
