# Deploy MCP with K8E API keys

K8E server supplies the Kubernetes cluster, sandbox gRPC gateway and Cilium
Gateway API entry. MCP runs as an independent deployment. The existing
`sandbox-matrix/e2b` Gateway terminates public TLS and routes `/mcp` to the
MCP Service over cluster-internal HTTP. No OAuth issuer, introspection service,
resource configuration or OAuth client secret is required.

The deployment is **opt-in**: the K8E server does not stage this manifest by
default (see `pkg/server` `stageFiles`). Every default install ran a
`k8e-mcp` Deployment whose pods could never become ready — the
`mcp-bootstrap` Secret and the `k8e-mcp:local` image below do not exist
until an operator provides them — so the manifest is only applied by
`hack/init-sandbox-mcp.sh` once its prerequisites are in place. Skipping is
fail-closed: upgrading a server removes a previously staged copy, and a
one-time cleanup of the old Deployment (`kubectl -n k8e-mcp delete
deployment k8e-mcp`) finishes the transition.

Create a named key using the existing K8E server command:

```sh
k8e sandbox-apikey create my-agent
```

Use the bare `key` from its output, not the `e2b_key` variant. Keep it private.
Existing unexpired keys in `sandbox-matrix/sandbox-apikeys` also work. Each client
should have its own named record; names/created_at determine the stable identity.
Preserve created_at when rotating credentials. Do not reuse legacy key names for
new users. Changing the Secret UID requires explicit ownership migration.

### Image

The Deployment pulls `ghcr.io/xiaods/k8e-mcp:latest`, which the release
workflow publishes (`.github/workflows/release.yml`, `build-mcp-image`) for
linux/amd64 and linux/arm64. A connected install needs no build or import step.

For a source build or an air-gapped node, build locally, import into the target
node's containerd (OrbStack Docker images are not automatically available
there), and pass `--image` so the Deployment uses the local tag:

```sh
docker build -f manifests/sandbox-mcp/Dockerfile -t k8e-mcp:local .
# Import into the target node's containerd (k8s.io namespace):
docker save k8e-mcp:local | docker exec -i <k8e-node-container> ctr -n k8s.io images import -
bash hack/init-sandbox-mcp.sh --kubeconfig /path/to/k8e.yaml --image k8e-mcp:local --apply
```

The runtime image is a non-root `scratch` image containing the static CLI and
the CA bundle required for gateway TLS verification. Importing an image only
matters on an agent-enabled K8E (the control-plane-only OrbStack recovery
harness has no node, so MCP pods cannot schedule there). A registry deployment
should pin an immutable digest instead: `--image
ghcr.io/xiaods/k8e-mcp@sha256:<digest>`.

### Gateway identity (KIP-30 M2)

`mcp-serve` reaches the sandbox gRPC gateway with a client certificate signed
by the sandbox CA. Since M2 the adapter **enrolls its own identity at
startup**: `hack/init-sandbox-mcp.sh` ships the sandbox CA
(`/var/lib/k8e/server/tls/sandbox-ca.crt` by default; non-secret — a CA exists
to be distributed) and one API key in Secret `k8e-mcp/mcp-bootstrap`; the
adapter performs the gateway `Login` handshake itself and caches the resulting
identity in ConfigMap `k8e-mcp/mcp-client-cert` (`ca.crt` / `tls.crt` /
`tls.key`).

The cache is what makes restarts, rollouts and upgrades cheap: a warm cache
serves the identity with **zero Login calls and no API key needed**, so
`K8E_MCP_BOOTSTRAP_API_KEY` can be unset after the first enrollment. The
client certificate is issued with the standard 90-day TTL; a restart within
30 days of expiry re-enrolls, **reusing the cached private key** so the
identity (and the server's ledger entry) stays stable. First-writer-wins on
the ConfigMap means N replicas of one Deployment share one identity. To force
a brand-new identity, delete `k8e-mcp/mcp-client-cert`.

The API key is only consumed on cold starts and renewals. Rotate it in
`sandbox-matrix/sandbox-apikeys` and update the Secret; in-flight enrolled
certificates remain valid until their own TTL.

```sh
bash hack/init-sandbox-mcp.sh --kubeconfig /path/to/k8e.yaml --apply
# or choose the key explicitly (environment, never argv):
MCP_BOOTSTRAP_API_KEY=<bare-key> \
  bash hack/init-sandbox-mcp.sh --kubeconfig /path/to/k8e.yaml --apply
```

The trust model is fail-closed: with no CA and no cached identity, the adapter
**refuses to send the API key** unless `--insecure-bootstrap` /
`K8E_MCP_INSECURE_BOOTSTRAP` is set explicitly (trust-on-first-use, pinned to
the CA the server presents on first contact).

Operators who pre-generate pairs keep the pre-M2 path: `--gateway-ca`,
`--gateway-cert` and `--gateway-key` (all three, files, mounted by the
operator) skip enrollment entirely. The two modes are mutually exclusive.

### Tenant scoping

`K8E_MCP_TENANT` scopes every session the adapter creates to one tenant
(tenants are the isolation unit; API keys are only credentials). Empty keeps
the pre-M2 behavior — sessions land in the server-side shared `default` bucket,
which is single-tenant only. Run one Deployment per tenant for more, each with
its own `mcp-bootstrap` Secret namespace copy and its own state namespace.

Without `--apply`, only local input checks run. With it, the script verifies the
existing API key Secret and the K8E API Gateway, creates the namespace, verifies
or explicitly updates `sandbox-matrix/sandbox-e2b`, picks an enrollment key
(`MCP_BOOTSTRAP_API_KEY` or the first active key in `keys.json`), creates
`k8e-mcp/mcp-bootstrap` (CA + key), applies the Deployment, Service,
ReferenceGrant and HTTPRoute, then waits for the Deployment, the Gateway's
`https` listener and route references. `--image` overrides the Deployment image
(default `ghcr.io/xiaods/k8e-mcp:latest`) for locally built or air-gap-imported
images.
If the Deployment does not become ready it prints pod status,
the deployment description and recent logs, then exits non-zero. Before
reporting success, it makes an unauthenticated HTTPS request to `/mcp` and
requires the MCP backend's expected `405` response; this verifies certificate
trust and hostname matching without exposing an API Key. Export
`MCP_PROBE_API_KEY` (environment, never argv) to additionally call
`server/discover` with `Authorization: Bearer` and require `200`, which proves
the full authenticated path end to end. It does not print credential contents or
copy API keys. No ConfigMap for authentication settings is needed. A replacement
shared certificate must cover every hostname already served by the HTTPS
listener.

The script prints the final client URL. `--hostname` should match the public
certificate and DNS record. When omitted, the script uses the first address from
`Gateway/e2b.status.addresses`; this works directly when the certificate
contains that IP.

RBAC grants ConfigMap get/create/update in `k8e-mcp`, plus get on exactly
`sandbox-matrix/sandbox-apikeys`. The identity ConfigMap `mcp-client-cert` is
covered by an explicit resourceName rule. The deployment uses two replicas and a disruption
budget, non-root execution and a read-only filesystem. `/healthz` reports process
health only; `/readyz` additionally verifies the ConfigMap state store and the
sandbox gateway, so a replica that cannot serve tool calls leaves Service
endpoints instead of accepting traffic. The startup probe is a plain TCP check —
it passes once the listener is up, before enrollment completes; readiness is
what fails a replica whose identity cannot be established. The HTTPRoute lives in `sandbox-matrix` so it can attach to the
Gateway's namespace-local HTTPS listener. A narrow ReferenceGrant in `k8e-mcp`
allows only that HTTPRoute kind and namespace to reference Service `k8e-mcp`.
Authorization headers and MCP metadata pass through unchanged. Public DNS still
points to the Gateway address and remains operator-managed.

The HTTPRoute attaches to both the `https` and the `http` listener of the
Gateway. The `http` attachment exists so a plain-HTTP install — one where the
`sandbox-e2b` Secret does not exist yet and the `https` listener reports
Invalid — still serves `/mcp` instead of the route being
`NotAllowedByListeners`. Prefer `https` for real clients: the `http` path
carries the `Authorization` header in plaintext. The Deployment also tolerates
the `node-role.kubernetes.io/control-plane` (and legacy `master`) taint, so a
single-node install — every node a control-plane node — schedules MCP without
extra flags.

Clients connect to the printed `https://YOUR_HOST/mcp` URL with
`Authorization: Bearer <key>`.
Configuration syntax varies by client. Only clients supporting custom headers
and MCP 2026-07-28 are in scope; automatic OAuth login is deferred.

## Verification

```sh
MCP_RUN_INTEROP=1 go test -race ./pkg/sandboxmcp -run TestIndependentPythonClient -v
MCP_TEST_KUBECONFIG=/path/to/k8e.yaml go test -race ./pkg/sandboxmcp -run TestKubernetesRestartRecovery -v
```

The first uses an independent Python HTTPS client with a simulated backend. The
second creates a temporary namespace and verifies real ConfigMap CAS and recovery
in separate processes. On a dedicated local K8E container, additionally set
`MCP_TEST_RESTART_CONTAINER` to restart that control plane between phases.

For an OrbStack-hosted K8E control plane, use the repository helper. It starts
K8E with `--cluster-init` (required for the managed embedded datastore), waits
for `/readyz`, then runs the restart recovery test against the generated
kubeconfig:

```sh
K8E_BINARY=/tmp/k8e-mcp-server hack/test-sandbox-mcp-orbstack.sh
```

The helper leaves its container and data available for inspection and refuses
to replace an existing container. Choose another `K8E_MCP_CONTAINER`,
`K8E_MCP_DATA_DIR` and `K8E_MCP_API_PORT` for an independent run.

Validated on OrbStack Linux on 2026-09-08: the native K8E control plane
started with `--cluster-init`, passed `/readyz`, and passed the race-enabled
`TestKubernetesRestartRecovery` including an actual container restart (8.731s).
Completed results, unknown-outcome non-replay and ownership survived restart.
The race-enabled `TestGatewayWithKubernetesMock` also passed inside Linux
(1.082s); it exercises production gRPC handlers with Kubernetes and sandboxd
fixtures, without gateway mTLS or real workload pods:

```sh
go test -race -tags mcp_integration ./pkg/sandboxmcp -run '^TestGatewayWithKubernetesMock$' -v -count=1
```

This validates Kubernetes API persistence and restart recovery with the sandbox
backend simulated by the test. Public gateway acceptance additionally requires
a running Cilium Gateway controller, a DNS name or reachable Gateway address,
and a certificate valid for that client-visible host.

For deployed gateway acceptance, set `MCP_TEST_API_KEY` in the environment, then:

```sh
python3 tests/mcp/client_smoke.py --endpoint https://YOUR_HOST/mcp --state /tmp/mcp-smoke.json --phase before
kubectl --kubeconfig /path/to/k8e.yaml -n k8e-mcp rollout restart deployment/k8e-mcp
kubectl --kubeconfig /path/to/k8e.yaml -n k8e-mcp rollout status deployment/k8e-mcp
python3 tests/mcp/client_smoke.py --endpoint https://YOUR_HOST/mcp --state /tmp/mcp-smoke.json --phase after
python3 tests/mcp/client_smoke.py --endpoint https://YOUR_HOST/mcp --state /tmp/mcp-smoke.json --phase cleanup
```

Use `--ca /path/to/ca.pem` for a private CA. Retain state records and uncertain
operation IDs; do not delete them to force retries. Establish state backup and
quota policies before production use. Full gateway acceptance is still pending.
