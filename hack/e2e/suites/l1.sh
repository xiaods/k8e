#!/usr/bin/env bash
# L1 suite — control plane only.
#
# Validates that a `k8e server --disable-agent` cluster:
#   * serves a healthy, versioned API
#   * creates the core namespaces
#   * honours --disable-* flags when staging addon manifests
#   * ships working in-process subcommands (kubectl / crictl)
#   * survives a container restart without losing cluster state
#
# CNI, agent and sandbox orchestration are intentionally out of scope here.
set -euo pipefail

. "$(cd "$(dirname "$0")/.." && pwd)/lib.sh"

e2e_set_profile "${E2E_PROFILE}"
e2e_require_cmds kubectl
e2e_require_cluster

e2e_log "L1: control plane checks (container ${E2E_CONTAINER})"

# --- API health and identity --------------------------------------------

if e2e_kubectl get --raw=/readyz 2>/dev/null | grep -qi '^ok'; then
    e2e_ok "/readyz reports ok"
else
    e2e_bad "/readyz reports ok"
fi

if e2e_kubectl get --raw=/livez 2>/dev/null | grep -qi '^ok'; then
    e2e_ok "/livez reports ok"
else
    e2e_bad "/livez reports ok"
fi

version_json="$(e2e_kubectl get --raw=/version 2>/dev/null || true)"
if printf '%s' "${version_json}" | grep -q '"gitVersion"'; then
    e2e_ok "/version exposes gitVersion ($(printf '%s' "${version_json}" | jq -r '.gitVersion' 2>/dev/null || echo unknown))"
else
    e2e_bad "/version exposes gitVersion"
fi

# --- core namespaces -----------------------------------------------------

for ns in default kube-system kube-public kube-node-lease; do
    if e2e_kubectl get namespace "${ns}" >/dev/null 2>&1; then
        e2e_ok "namespace ${ns} exists"
    else
        e2e_bad "namespace ${ns} exists"
    fi
done

# --- agent is disabled, so no node is registered -------------------------

node_count="$(e2e_kubectl get nodes --no-headers 2>/dev/null | wc -l | tr -d ' ')"
if [ "${node_count}" = "0" ]; then
    e2e_ok "--disable-agent leaves the cluster without nodes"
else
    e2e_bad "--disable-agent leaves the cluster without nodes (found ${node_count})"
fi

# --- disable semantics ---------------------------------------------------

# --disable-sandbox-matrix keeps every manifests/sandbox-matrix/* addon out of
# the cluster: the sandbox-matrix namespace + crds.yaml CRDs, the sandbox
# RuntimeClasses, the default warm pool, the Cilium network policy and the e2b
# Gateway API bridge. The API types themselves (sandboxmatrixes.k8e.sh, ...)
# are registered by the CRD controller at start-up and are intentionally not
# gated by this flag.
if e2e_kubectl get namespace sandbox-matrix >/dev/null 2>&1; then
    e2e_bad "--disable-sandbox-matrix keeps the sandbox-matrix namespace out of the cluster"
else
    e2e_ok "--disable-sandbox-matrix keeps the sandbox-matrix namespace out of the cluster"
fi

for runtime in gvisor kata firecracker; do
    if e2e_kubectl get runtimeclass "${runtime}" >/dev/null 2>&1; then
        e2e_bad "--disable-sandbox-matrix keeps runtimeclass ${runtime} out of the cluster"
    else
        e2e_ok "--disable-sandbox-matrix keeps runtimeclass ${runtime} out of the cluster"
    fi
done

if e2e_kubectl get sandboxwarmpools -A --no-headers 2>/dev/null | grep -q .; then
    e2e_bad "--disable-sandbox-matrix keeps SandboxWarmPool objects out of the cluster"
else
    e2e_ok "--disable-sandbox-matrix keeps SandboxWarmPool objects out of the cluster"
fi

for service in sandbox-grpc-gateway e2b-server; do
    if e2e_kubectl get service "${service}" -A --no-headers 2>/dev/null | grep -q .; then
        e2e_bad "--disable-sandbox-matrix keeps the ${service} Service out of the cluster"
    else
        e2e_ok "--disable-sandbox-matrix keeps the ${service} Service out of the cluster"
    fi
done

if e2e_kubectl get crd gatewayclasses.gateway.networking.k8s.io >/dev/null 2>&1; then
    e2e_bad "--disable-sandbox-matrix keeps the Gateway API CRDs out of the cluster"
else
    e2e_ok "--disable-sandbox-matrix keeps the Gateway API CRDs out of the cluster"
fi

if e2e_kubectl -n kube-system get daemonset cilium >/dev/null 2>&1; then
    e2e_bad "--disable-cilium keeps the cilium agent out of the cluster"
else
    e2e_ok "--disable-cilium keeps the cilium agent out of the cluster"
fi

# --disable-cloud-controller stops the embedded cloud controller; its bundled
# RBAC (manifests/ccm.yaml, which creates the k8e-cloud-controller-manager
# ClusterRole) must not be staged or applied either.
if e2e_kubectl get clusterrole k8e-cloud-controller-manager >/dev/null 2>&1; then
    e2e_bad "--disable-cloud-controller keeps the CCM out of the cluster"
else
    e2e_ok "--disable-cloud-controller keeps the CCM out of the cluster"
fi

# --- staged addon manifests ---------------------------------------------

staged="${E2E_MANIFESTS_DIR}"
if [ -d "${staged}" ]; then
    e2e_ok "addon manifests staged at ${staged#${E2E_DATA_DIR}/}"
    for manifest in coredns.yaml local-storage.yaml runtimes.yaml rolebindings.yaml; do
        if [ -f "${staged}/${manifest}" ]; then
            e2e_ok "staged ${manifest}"
        else
            e2e_bad "staged ${manifest}"
        fi
    done
    for manifest in cilium.yaml ccm.yaml; do
        if [ -e "${staged}/${manifest}" ]; then
            e2e_bad "not staged ${manifest} (disabled)"
        else
            e2e_ok "not staged ${manifest} (disabled)"
        fi
    done
    if [ -e "${staged}/sandbox-matrix" ]; then
        e2e_bad "not staged sandbox-matrix/ (disabled)"
    else
        e2e_ok "not staged sandbox-matrix/ (disabled)"
    fi
else
    e2e_bad "addon manifests are staged under ${E2E_MANIFESTS_DIR}"
fi

# --- in-container subcommands and loopback API ---------------------------

if e2e_in_container /usr/local/bin/k8e kubectl version --client >/dev/null 2>&1; then
    e2e_ok "in-container 'k8e kubectl' works"
else
    e2e_bad "in-container 'k8e kubectl' works"
fi

if e2e_in_container /usr/local/bin/k8e crictl --version >/dev/null 2>&1; then
    e2e_ok "in-container 'k8e crictl' works"
else
    e2e_bad "in-container 'k8e crictl' works"
fi

if e2e_in_container /usr/local/bin/k8e kubectl --kubeconfig /test/kubeconfig.yaml \
    get --raw=/readyz >/dev/null 2>&1; then
    e2e_ok "API is reachable over loopback from inside the container"
else
    e2e_bad "API is reachable over loopback from inside the container"
fi

# --- restart recovery ----------------------------------------------------

if [ "${E2E_SKIP_GO_TESTS:-0}" = "1" ]; then
    e2e_log "skipping restart recovery test (E2E_SKIP_GO_TESTS=1)"
elif ! command -v go >/dev/null 2>&1; then
    e2e_warn "go toolchain not found; skipping restart recovery test"
else
    e2e_log "running TestKubernetesRestartRecovery against the cluster"
    # shellcheck disable=SC2086
    if (cd "${E2E_REPO}" &&
        MCP_TEST_KUBECONFIG="${E2E_KUBECONFIG}" \
            MCP_TEST_RESTART_CONTAINER="${E2E_CONTAINER}" \
            go test ${E2E_GO_TEST_ARGS} ./pkg/sandboxmcp -run '^TestKubernetesRestartRecovery$' -count=1 -v); then
        e2e_ok "cluster survives a container restart"
    else
        e2e_bad "cluster survives a container restart"
    fi
fi

e2e_summary
