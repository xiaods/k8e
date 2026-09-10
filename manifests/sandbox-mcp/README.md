# Deploy MCP with K8E API keys

K8E server supplies the Kubernetes cluster, sandbox gRPC gateway and Cilium
Gateway API entry. MCP runs as an independent deployment. The existing
`sandbox-matrix/e2b` Gateway terminates public TLS and routes `/mcp` to the
MCP Service over cluster-internal HTTP. No OAuth issuer, introspection service,
resource configuration or OAuth client secret is required.

Create a named key using the existing K8E server command:

```sh
k8e sandbox-apikey create my-agent
```

Use the bare `key` from its output, not the `e2b_key` variant. Keep it private.
Existing unexpired keys in `sandbox-matrix/sandbox-apikeys` also work. Each client
should have its own named record; names/created_at determine the stable identity.
Preserve created_at when rotating credentials. Do not reuse legacy key names for
new users. Changing the Secret UID requires explicit ownership migration.

Build and import the image into K8E containerd (OrbStack Docker images are not
automatically available there):

```sh
docker build -f manifests/sandbox-mcp/Dockerfile -t k8e-mcp:local .
# Import into the target node's containerd (k8s.io namespace):
docker save k8e-mcp:local | docker exec -i <k8e-node-container> ctr -n k8s.io images import -
```

The runtime image is a non-root `scratch` image containing the static CLI and
the CA bundle required for gateway TLS verification. Importing an image only
matters on an agent-enabled K8E (the control-plane-only OrbStack recovery
harness has no node, so MCP pods cannot schedule there).

For a registry deployment, replace the image with your published immutable digest.

### Gateway mTLS material

`mcp-serve` reaches the sandbox gRPC gateway with a client certificate signed by
the sandbox CA. Bootstrap that pair from the same API key you created above —
the CLI performs the gateway `Login` handshake and writes `ca.crt`, `client.crt`
and `client.key`:

```sh
K8E_SANDBOX_CERT_DIR=/path/to/gateway-mtls \
  k8e-sandbox-cli connect --endpoint <gateway-host>:50051 \
  --apikey <bare-key-from-sandbox-apikey-create> --skip-skill
```

Pass those three files as `--gateway-ca/--gateway-cert/--gateway-key` below. The
files are the adapter's own backend identity: keep them out of source control and
treat the directory as secret material. The gateway only verifies that a client
certificate is present and unrevoked; the MCP adapter, not gRPC mTLS, is what
enforces per-resource ownership (see `pkg/sandboxmcp/README.md`).

Initialize using authorized gateway mTLS files. If the API Gateway already has
the `sandbox-matrix/sandbox-e2b` TLS Secret, omit `--public-cert` and
`--public-key`. Supply both options to install or deliberately update that
shared Gateway certificate:

```sh
bash hack/init-sandbox-mcp.sh --kubeconfig /path/to/k8e.yaml \
  --gateway-ca /path/to/gateway/ca.crt \
  --gateway-cert /path/to/gateway/tls.crt --gateway-key /path/to/gateway/tls.key \
  --public-cert /path/to/public/tls.crt --public-key /path/to/public/tls.key \
  --hostname mcp.example.com \
  --apply
```

Without `--apply`, only local input checks run. With it, the script verifies the
existing API key Secret and the K8E API Gateway, creates the namespace, verifies
or explicitly updates `sandbox-matrix/sandbox-e2b`, creates the gateway mTLS
client Secret, applies the Deployment, Service, ReferenceGrant and HTTPRoute,
then waits for the Deployment, the Gateway's `https` listener and route
references. If the Deployment does not become ready it prints pod status,
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
`sandbox-matrix/sandbox-apikeys`. The deployment uses two replicas and a disruption
budget, non-root execution and a read-only filesystem. `/healthz` reports process
health only; `/readyz` additionally verifies the ConfigMap state store and the
sandbox gateway, so a replica that cannot serve tool calls leaves Service
endpoints instead of accepting traffic. The HTTPRoute lives in `sandbox-matrix` so it can attach to the
Gateway's namespace-local HTTPS listener. A narrow ReferenceGrant in `k8e-mcp`
allows only that HTTPRoute kind and namespace to reference Service `k8e-mcp`.
Authorization headers and MCP metadata pass through unchanged. Public DNS still
points to the Gateway address and remains operator-managed.

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
