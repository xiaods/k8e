#!/usr/bin/env bash
# Collect diagnostics from a running (or dead) E2E cluster.
#
#   hack/e2e/dump-logs.sh [profile]
#
# Best effort by design: every probe is allowed to fail. The script always
# exits 0 so that it can run from `if: failure()` CI steps.
set -uo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    e2e_usage
    exit 0
fi

e2e_set_profile "${1:-${E2E_PROFILE}}"
mkdir -p "${E2E_DIAG_DIR}"

capture() {
    local file="$1"
    shift
    {
        printf '### command: %s\n\n' "$*"
        "$@"
        printf '\n### exit: %d\n' "$?"
    } >"${E2E_DIAG_DIR}/${file}" 2>&1 || true
    return 0
}

e2e_log "collecting diagnostics into ${E2E_DIAG_DIR}"

# 01 — environment
{
    printf '### date\n'; date -u
    printf '\n### uname\n'; uname -a
    printf '\n### cgroup\n'; stat -fc %T /sys/fs/cgroup 2>/dev/null || true
    printf '\n### /dev/kvm\n'; ls -l /dev/kvm 2>/dev/null || echo "absent"
    printf '\n### /sys/fs/bpf\n'; ls -ld /sys/fs/bpf 2>/dev/null || echo "absent"
    printf '\n### docker\n'; docker version 2>/dev/null || true
    printf '\n### profile\n'; env | grep '^E2E_' | sort
    printf '\n### binary\n'; ls -l "${E2E_BINARY}" 2>/dev/null || echo "missing: ${E2E_BINARY}"
} >"${E2E_DIAG_DIR}/01-environment.txt" 2>&1 || true

# 02 — container state and logs
capture 02-container-inspect.txt docker inspect "${E2E_CONTAINER}"
capture 03-container-logs.txt docker logs --timestamps --tail 4000 "${E2E_CONTAINER}"

# 04 — cluster inventory
if [ -f "${E2E_KUBECONFIG}" ] && kubectl --kubeconfig "${E2E_KUBECONFIG}" --request-timeout=5s version >/dev/null 2>&1; then
    capture 04-nodes.txt e2e_kubectl get nodes -o wide
    capture 05-pods.txt e2e_kubectl get pods -A -o wide
    capture 06-events.txt e2e_kubectl get events -A --sort-by=.lastTimestamp
    capture 07-describe-nodes.txt e2e_kubectl describe nodes
    capture 08-crds.txt e2e_kubectl get crd
    capture 09-addons.txt e2e_kubectl get addons.k8e.sh -A -o yaml
    capture 10-endpointslices.txt e2e_kubectl get endpointslices -A
    capture 11-leases.txt e2e_kubectl get leases -A
    capture 12-sandbox-crs.txt e2e_kubectl get sandboxsessions,sandboxwarmpools -A
    if [ "${E2E_PROFILE}" = "l3" ] && e2e_container_running; then
        capture 13-cilium-status.txt docker exec "${E2E_CONTAINER}" /usr/local/bin/k8e kubectl \
            -n kube-system exec ds/cilium -- cilium status --verbose
    fi
else
    printf 'kubeconfig %s unusable; skipping cluster inventory\n' "${E2E_KUBECONFIG}" \
        >"${E2E_DIAG_DIR}/04-kubectl-unavailable.txt"
fi

# 14 — staged manifests and data directory (reveals addon/disable regressions)
#
# The server owns the data directory as root with mode 0700, so on a plain Linux
# host only the container can read it. Listing it from the outside reports an empty
# tree for a cluster that staged everything, which is how the addon-staging
# regression this file exists to explain stayed invisible.
if e2e_container_running; then
    {
        printf '### staged manifests (%s)\n' "${E2E_IN_CONTAINER_MANIFESTS_DIR}"
        docker exec "${E2E_CONTAINER}" sh -c 'ls -la "$1" 2>&1' e2e-diag \
            "${E2E_IN_CONTAINER_MANIFESTS_DIR}" || true
        printf '\n### data directory (depth 2)\n'
        docker exec "${E2E_CONTAINER}" sh -c 'find "$1" -maxdepth 2 2>&1 | head -200' e2e-diag \
            "${E2E_IN_CONTAINER_DATA_DIR}/data" || true
        printf '\n### log files\n'
        docker exec "${E2E_CONTAINER}" sh -c 'find "$1" -maxdepth 4 -name "*.log" -type f 2>&1 | head -40' e2e-diag \
            "${E2E_IN_CONTAINER_DATA_DIR}/data" || true
    } >"${E2E_DIAG_DIR}/14-data-dir.txt" 2>&1 || true
elif [ -d "${E2E_DATA_DIR}" ]; then
    # Container gone: fall back to whatever the host user is allowed to see.
    find "${E2E_DATA_DIR}" -maxdepth 3 2>&1 | head -200 >"${E2E_DIAG_DIR}/14-data-dir.txt" 2>&1 || true
fi

# 15 — in-container network state
if e2e_container_running; then
    {
        printf '### ip addr\n'; docker exec "${E2E_CONTAINER}" ip addr 2>&1 || true
        printf '\n### ip route\n'; docker exec "${E2E_CONTAINER}" ip route 2>&1 || true
        printf '\n### iptables\n'; docker exec "${E2E_CONTAINER}" iptables -L -n 2>&1 || true
        printf '\n### data tarball bin\n'; docker exec "${E2E_CONTAINER}" sh -c 'ls -l /test/data/data/*/bin 2>&1' || true
    } >"${E2E_DIAG_DIR}/15-in-container-network.txt" 2>&1 || true
fi

# 16 — runtime logs. containerd's log lives under its root directory, which is on
# the runtime volume and therefore invisible from the /test bind mount; it is the
# one file that explains a containerd that refuses to start, and `docker cp`
# reaches it even when the container is already dead.
if e2e_container_exists; then
    capture 16-containerd-log.txt e2e_containerd_log 400
    capture 17-runtime-layout.txt docker exec "${E2E_CONTAINER}" sh -c \
        'ls -la "${K8E_E2E_CONTAINERD_ROOT}"; echo "--- /test/data/agent"; ls -la /test/data/agent'
fi

e2e_log "diagnostics written:"
ls -1 "${E2E_DIAG_DIR}" | sed 's/^/  /'
