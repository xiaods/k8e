# MCP API Key entry

The standalone sandbox CLI exposes `mcp-serve`, an HTTPS MCP 2026-07-28 entry
using K8E API keys. Configure a client with the `/mcp` URL and
`Authorization: Bearer <bare K8E key>`. OAuth login/discovery is not implemented;
clients must support explicit Authorization headers and this protocol version.

The service reads `sandbox-matrix/sandbox-apikeys` on every authenticated request,
using the shared K8E `keys.json` codec and expiry checks. Revocation applies to
subsequent requests; already admitted work is not cancelled. Kubernetes failures
fail closed with 503, and invalid credentials receive 401. Duplicate keys assigned
to multiple identities are rejected. API keys are never sent to the gateway.

Ownership uses Secret UID, record name and created_at, not the credential bytes.
Rotate a v2 record by changing its key while preserving name and created_at.
Deleting/recreating a record with a new created_at gives it a new identity.
Legacy records lack created_at: never recycle their names for different users.
Recreating the entire Secret changes its UID and requires deliberate ownership
migration. No automatic adoption of previous OAuth-owned or legacy sessions occurs.

The nine tools validate ownership for sessions/runs and persist atomic operation
admission in ConfigMaps. Lost replies and uncertain completion writes remain
unknown and never trigger automatic resubmission. State records must be retained;
there is no automatic GC or unknown-outcome reconciler.

Only discover/list/call are advertised; there are no protocol sessions, initialize
handshake, SSE streams or Tasks extensions. Browser CORS preflight is not supported.
The `/healthz` endpoint reports process health, not gateway or Kubernetes readiness.

Deployment and initialization: `manifests/sandbox-mcp/README.md`.

```sh
go test -race ./pkg/sandboxmcp ./cmd/sandboxcli -count=1
MCP_RUN_INTEROP=1 go test ./pkg/sandboxmcp -run TestIndependentPythonClient -v
MCP_TEST_KUBECONFIG=/path/to/k8e.yaml go test ./pkg/sandboxmcp -run TestKubernetesRestartRecovery -v
# OrbStack: native K8E control plane with managed embedded etcd.
K8E_BINARY=/tmp/k8e-mcp-server hack/test-sandbox-mcp-orbstack.sh
```

The Python test uses real HTTPS with a simulated backend. The cluster test uses
a real Kubernetes API with a simulated backend; full sandbox end-to-end and
official client interoperability remain separate acceptance work.
