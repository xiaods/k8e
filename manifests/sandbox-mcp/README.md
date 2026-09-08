# Deploy MCP with K8E API keys

K8E server supplies the Kubernetes cluster and sandbox gRPC gateway. MCP runs as
an independent HTTPS deployment. No OAuth issuer, introspection service, resource
configuration or OAuth client secret is required.

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
```

For a registry deployment, replace the image with your published immutable digest.
Initialize using operator-provisioned public TLS and authorized gateway mTLS files:

```sh
bash hack/init-sandbox-mcp.sh --kubeconfig /path/to/k8e.yaml \
  --public-cert /path/to/public/tls.crt --public-key /path/to/public/tls.key \
  --gateway-ca /path/to/gateway/ca.crt \
  --gateway-cert /path/to/gateway/tls.crt --gateway-key /path/to/gateway/tls.key \
  --apply
```

Without `--apply`, only local input checks run. With it, the script verifies the
existing API key Secret, creates the namespace and TLS Secrets, applies the
deployment, and rolls it to reload certificates. It does not print credential
contents or copy API keys. No ConfigMap for authentication settings is needed.

RBAC grants ConfigMap get/create/update in `k8e-mcp`, plus get on exactly
`sandbox-matrix/sandbox-apikeys`. The deployment uses two replicas and a disruption
budget, non-root execution and a read-only filesystem. `/healthz` reports process
health only. Route HTTPS through your existing gateway to Service `k8e-mcp:443`;
this manifest does not create public DNS, certificates or ingress automatically.

Clients connect to `https://YOUR_HOST/mcp` with `Authorization: Bearer <key>`.
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
