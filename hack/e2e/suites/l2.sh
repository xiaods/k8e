#!/usr/bin/env bash
# L2 suite — control plane + agent, no CNI.
#
# Validates that a `k8e server` cluster starts a working node:
#   * serves a healthy, versioned API
#   * registers a node running the in-process kubelet
#   * keeps the node NotReady *only* because no CNI is installed
#   * brings up containerd (CRI plugin) with runc and its shims
#   * runs a pod end to end through kubelet -> containerd -> runc
#
# The node is deliberately not expected to reach Ready here: l2 runs without a
# CNI so that runtime failures cannot hide behind networking, and the pod used
# below is hostNetwork so that it never needs one.
set -euo pipefail

. "$(cd "$(dirname "$0")/.." && pwd)/lib.sh"

e2e_set_profile "${E2E_PROFILE}"
e2e_require_cmds kubectl
e2e_require_cluster

e2e_log "L2: control plane + agent checks (container ${E2E_CONTAINER})"

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

server_version="$(e2e_kubectl get --raw=/version 2>/dev/null | jq -r '.gitVersion' 2>/dev/null || true)"
if [ -n "${server_version}" ] && [ "${server_version}" != "null" ]; then
    e2e_ok "/version exposes gitVersion (${server_version})"
else
    e2e_bad "/version exposes gitVersion"
fi

# --- node registration ---------------------------------------------------

node="$(e2e_kubectl get nodes -o jsonpath='{.items[0].metadata.name}' 2>/dev/null || true)"
if [ -n "${node}" ]; then
    e2e_ok "node ${node} registered"
else
    e2e_bad "node registered"
    e2e_summary
fi

node_version="$(e2e_kubectl get node "${node}" -o jsonpath='{.status.nodeInfo.kubeletVersion}' 2>/dev/null || true)"
if [ -n "${node_version}" ]; then
    e2e_ok "kubelet reports a version (${node_version})"
else
    e2e_bad "kubelet reports a version"
fi

if [ -n "${server_version}" ] && [ "${node_version}" = "${server_version}" ]; then
    e2e_ok "kubelet runs in-process at the server version (${node_version})"
else
    e2e_bad "kubelet runs in-process at the server version (node=${node_version} server=${server_version})"
fi

# --- the node is only held back by the missing CNI -----------------------

ready_status="$(e2e_kubectl get node "${node}" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.status}{end}' 2>/dev/null || true)"
ready_message="$(e2e_kubectl get node "${node}" -o jsonpath='{range .status.conditions[?(@.type=="Ready")]}{.message}{end}' 2>/dev/null || true)"

if [ "${ready_status}" = "False" ]; then
    e2e_ok "node is NotReady (no CNI in this profile)"
else
    e2e_bad "node is NotReady (no CNI in this profile), status=${ready_status}"
fi

if printf '%s' "${ready_message}" | grep -qi 'cni'; then
    e2e_ok "NotReady is caused by the absent CNI plugin, not by a failed runtime"
else
    e2e_bad "NotReady is caused by the absent CNI plugin, not by a failed runtime (${ready_message})"
fi

for condition in MemoryPressure DiskPressure PIDPressure; do
    condition_status="$(e2e_kubectl get node "${node}" -o jsonpath="{range .status.conditions[?(@.type==\"${condition}\")]}{.status}{end}" 2>/dev/null || true)"
    if [ "${condition_status}" = "False" ]; then
        e2e_ok "node has no ${condition}"
    else
        e2e_bad "node has no ${condition} (${condition_status})"
    fi
done

# --- kubelet is reachable and healthy through the API server -------------

if e2e_kubectl get --raw "/api/v1/nodes/${node}/proxy/healthz" 2>/dev/null | grep -qi '^ok'; then
    e2e_ok "kubelet answers /healthz through the API server proxy"
else
    e2e_bad "kubelet answers /healthz through the API server proxy"
fi

# --- container runtime stack ---------------------------------------------

if e2e_in_container /usr/local/bin/runc --version >/dev/null 2>&1; then
    e2e_ok "runc is available to the node"
else
    e2e_bad "runc is available to the node"
fi

if e2e_in_container sh -c 'command -v containerd-shim-runc-v2' >/dev/null 2>&1; then
    e2e_ok "containerd-shim-runc-v2 is available to the node"
else
    e2e_bad "containerd-shim-runc-v2 is available to the node"
fi

cri_plugins="$(e2e_in_container ctr --address "${E2E_CONTAINERD_SOCKET}" plugins ls 2>/dev/null || true)"
if printf '%s' "${cri_plugins}" | grep -qE 'grpc\.v1[[:space:]]+cri[[:space:]].*ok'; then
    e2e_ok "containerd's CRI plugin is running"
else
    e2e_bad "containerd's CRI plugin is running"
fi

# --- a pod really runs through kubelet -> containerd -> runc ------------

pod="e2e-l2-hostnet"
pod_manifest="${E2E_STATE_DIR}/l2-hostnet-pod.yaml"
e2e_kubectl delete pod "${pod}" --ignore-not-found --wait=true >/dev/null 2>&1 || true

# hostNetwork keeps the pod off the (absent) CNI plugin, and the image is
# preloaded from the host so the node needs no registry access.
cat > "${pod_manifest}" <<EOF
apiVersion: v1
kind: Pod
metadata:
  name: ${pod}
  labels:
    k8e-e2e/test: l2
spec:
  nodeName: ${node}
  hostNetwork: true
  restartPolicy: Never
  containers:
    - name: app
      image: ${E2E_TEST_IMAGE}
      imagePullPolicy: IfNotPresent
      command: ["/bin/sh", "-c", "echo k8e-e2e-hostnet-ok; sleep 3600"]
EOF

# Applying a manifest file rather than piping into `kubectl apply -f -` keeps
# the check free of stdin/pipefail races and leaves the exact spec in the state
# directory for diagnostics.
if apply_output="$(e2e_kubectl apply -f "${pod_manifest}" 2>&1)"; then
    e2e_ok "hostNetwork pod accepted by the API server"
else
    e2e_bad "hostNetwork pod accepted by the API server (${apply_output})"
fi

if e2e_wait_pod_running "${pod}" "${E2E_POD_TIMEOUT:-180}"; then
    e2e_ok "pod runs through kubelet, containerd and runc"
else
    e2e_bad "pod runs through kubelet, containerd and runc"
    e2e_kubectl describe pod "${pod}" 2>&1 | tail -15 || true
fi

pod_logs="$(e2e_kubectl logs "${pod}" 2>/dev/null || true)"
if printf '%s' "${pod_logs}" | grep -q 'k8e-e2e-hostnet-ok'; then
    e2e_ok "pod output is readable through the kubelet log endpoint"
else
    e2e_bad "pod output is readable through the kubelet log endpoint"
fi

pod_ip="$(e2e_kubectl get pod "${pod}" -o jsonpath='{.status.podIP}' 2>/dev/null || true)"
if [ -n "${pod_ip}" ]; then
    e2e_ok "pod reports an IP (${pod_ip})"
else
    e2e_bad "pod reports an IP"
fi

e2e_kubectl delete pod "${pod}" --ignore-not-found --wait=false >/dev/null 2>&1 || true

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
