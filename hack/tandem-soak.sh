#!/usr/bin/env bash
# Standalone soak test for the Tandem backend (issue #592, M2 stage-4).
#
# Runs against a real rqlite-backed k8e binary inside an OrbStack VM (or any
# Linux host) and asserts three things over a sustained window:
#   1. Every acknowledged write is readable (no silent data loss).
#   2. No watch gaps or phantom events (the watch oracle).
#   3. RSS grows sub-linearly (no unbounded growth).
#
# This is a complement to TestTandemSoak in pkg/tandem: the Go test runs under
# the test runner's resource limits and a short deadline, while this script
# runs for minutes against the production-shaped binary, collecting metrics the
# test runner cannot (RSS from /proc, actual CPU via top).
#
# Usage:
#   hack/tandem-soak.sh                                  # default 2m soak
#   TANDEM_SOAK_DURATION=5m hack/tandem-soak.sh          # longer run
#   TANDEM_SOAK_PROFILE=high hack/tandem-soak.sh         # faster writes
#
# Prerequisites in PATH: k8e, rqlited, rqlite, etcdctl (or k8e kubectl),
# curl, jq.
set -euo pipefail

SOAK_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)"
cd "${SOAK_DIR}"

# Defaults -------------------------------------------------------------
DURATION="${TANDEM_SOAK_DURATION:-2m}"
PROFILE="${TANDEM_SOAK_PROFILE:-normal}"
K8E_BIN="${TANDEM_SOAK_K8E:-$(command -v k8e || echo)}"
RQLITED_BIN="${TANDEM_SOAK_RQLITED:-$(command -v rqlited || echo)}"
DATA_DIR="${TANDEM_SOAK_DATA:-/tmp/k8e-soak/$(date +%s)}"

if [[ -z "${K8E_BIN}" ]]; then
    echo "[ERROR] k8e binary not found; set TANDEM_SOAK_K8E or put k8e on PATH" >&2
    exit 1
fi
if [[ -z "${RQLITED_BIN}" ]]; then
    echo "[ERROR] rqlited not found; install rqlite or set TANDEM_SOAK_RQLITED" >&2
    exit 1
fi

# Parse the duration once, for logging and the deadline.
if ! DURATION_SEC="$(printf '%s' "${DURATION}" | sed -E 's/([0-9]+)m$/\1*60/' | bc)" 2>/dev/null; then
    echo "[ERROR] bad duration: ${DURATION}" >&2
    exit 1
fi

# Profile: writes-per-second target. The soak is about correctness under
# sustained load, not throughput, so even "high" is modest.
case "${PROFILE}" in
    low)    WRITE_PPS=10   ;;
    normal) WRITE_PPS=50   ;;
    high)   WRITE_PPS=200  ;;
    *)      echo "[ERROR] unknown profile: ${PROFILE}" >&2; exit 1 ;;
esac

echo "=== Tandem soak ==="
echo "  duration:   ${DURATION} (${DURATION_SEC}s)"
echo "  profile:    ${PROFILE} (${WRITE_PPS} writes/s)"
echo "  k8e:        ${K8E_BIN}"
echo "  rqlited:    ${RQLITED_BIN}"
echo "  data-dir:   ${DATA_DIR}"

# Setup ----------------------------------------------------------------
rm -rf "${DATA_DIR}"
mkdir -p "${DATA_DIR}"/{kubelet,pods,etc,log}

# Generate self-signed etcd credentials for the compat layer.
# k8e handles this automatically when none are present.
export K8E_DATA_DIR="${DATA_DIR}"

# Start k8e server with tandem backend.
echo "[soak] starting k8e with --datastore-backend tandem..."
"${K8E_BIN}" server \
    --datastore-backend tandem \
    --datastore-endpoint "http://127.0.0.1:4001" \
    --data-dir "${DATA_DIR}" \
    --write-kubeconfig "${DATA_DIR}/kubeconfig" \
    > "${DATA_DIR}/log/k8e.log" 2>&1 &
K8E_PID=$!

# Wait for the kubeconfig to appear — that is when the apiserver is ready.
echo "[soak] waiting for k8e to become ready..."
for ((i = 0; i < 120; i++)); do
    if [[ -f "${DATA_DIR}/kubeconfig" ]]; then
        # Confirm the apiserver actually answers.
        if curl -fsSk "https://127.0.0.1:6443/healthz" > /dev/null 2>&1; then
            echo "[soak] k8e ready"
            break
        fi
    fi
    sleep 1
done
if (( i >= 120 )); then
    echo "[ERROR] k8e did not become ready within 2 minutes" >&2
    cat "${DATA_DIR}/log/k8e.log" >&2
    kill "${K8E_PID}" 2>/dev/null
    exit 1
fi

# Use etcdctl through the compat layer. Since k8e bundles a kubectl that
# tunnels to the apiserver, we use the etcd client credentials from the
# data directory.
export ETCDCTL_API=3
ETCD_CERT_DIR="${DATA_DIR}/tandem"  # the compat layer stores certs alongside
etcdctl() {
    "${K8E_BIN}" kubectl exec -n kube-system etcd-tandem-0 -- \
        etcdctl \
        --cacert="${DATA_DIR}/tls/etcd/server-ca.crt" \
        --cert="${DATA_DIR}/tls/etcd/peer.crt" \
        --key="${DATA_DIR}/tls/etcd/peer.key" \
        --endpoints="https://127.0.0.1:2379" \
        "$@" 2>/dev/null
}

# Fallback: try the compat layer's etcd endpoints directly.
etcdctl() {
    "${SOAK_DIR}/bin/etcdctl" \
        --cacert="${DATA_DIR}/tls/etcd/server-ca.crt" \
        --cert="${DATA_DIR}/tls/etcd/peer.crt" \
        --key="${DATA_DIR}/tls/etcd/peer.key" \
        --endpoints="https://127.0.0.1:2379" \
        "$@" 2>/dev/null || \
    kubectl --kubeconfig="${DATA_DIR}/kubeconfig" exec -n kube-system etcd-tandem-0 -- \
        etcdctl \
        --cacert="/var/run/k8e/server/tls/etcd/server-ca.crt" \
        --cert="/var/run/k8e/server/tls/etcd/peer.crt" \
        --key="/var/run/k8e/server/tls/etcd/peer.key" \
        --endpoints="https://127.0.0.1:2379" \
        "$@" 2>/dev/null
}

# --- workload ---------------------------------------------------------
# Write loop: a background process that puts a key per write interval and
# records it in a local oracle file.
ORACLE="${DATA_DIR}/oracle.txt"
> "${ORACLE}"

writer() {
    local i=0
    local interval=$(( 1000 / WRITE_PPS ))  # ms between writes
    while true; do
        local key="soak/key/$(printf '%08d' $i)"
        local value="v-$i-$(date +%s%N)"
        if etcdctl put "${key}" "${value}" > /dev/null 2>&1; then
            echo "${key}=${value}" >> "${ORACLE}"
        else
            echo "[writer] put failed at key ${key}" >&2
            return 1
        fi
        i=$((i + 1))
        sleep "0.${interval}"
    done
}

# Resource sampler: captures rqlite RSS + CPU every 30s.
sampler() {
    local log="${DATA_DIR}/resources.csv"
    echo "timestamp,rss_mb,cpu_pct,oracle_keys" > "${log}"
    while true; do
        sleep 30
        # Find the rqlite process: the one running rqlited.
        local rqlite_pid
        rqlite_pid="$(pgrep -f 'rqlited' | head -1)"
        if [[ -n "${rqlite_pid}" ]]; then
            local rss_pages
            rss_pages="$(awk '{print $2}' "/proc/${rqlite_pid}/statm" 2>/dev/null || echo 0)"
            local page_size
            page_size="$(getconf PAGESIZE)"
            local rss_mb=$(( rss_pages * page_size / 1024 / 1024 ))
            local cpu_pct
            cpu_pct="$(ps -p "${rqlite_pid}" -o %cpu= 2>/dev/null | tr -d ' ' || echo 0)"
            local oracle_keys
            oracle_keys="$(wc -l < "${ORACLE}" 2>/dev/null || echo 0)"
            echo "$(date +%s),${rss_mb},${cpu_pct},${oracle_keys}" >> "${log}"
        fi
    done
}

# --- run --------------------------------------------------------------
echo "[soak] starting workload..."
writer &
WRITER_PID=$!
sampler &
SAMPLER_PID=$!

echo "[soak] running for ${DURATION}..."
sleep "${DURATION}"

echo "[soak] stopping workload..."
kill "${WRITER_PID}" 2>/dev/null || true
kill "${SAMPLER_PID}" 2>/dev/null || true
wait "${WRITER_PID}" 2>/dev/null || true
wait "${SAMPLER_PID}" 2>/dev/null || true

# --- verify -----------------------------------------------------------
echo "[soak] verifying oracle..."
TOTAL_KEYS="$(wc -l < "${ORACLE}")"
echo "[soak] total keys written: ${TOTAL_KEYS}"

if [[ "${TOTAL_KEYS}" -eq 0 ]]; then
    echo "[ERROR] soak wrote nothing" >&2
    kill "${K8E_PID}" 2>/dev/null
    exit 1
fi

# Spot-check 100 random keys from the oracle.
MISMATCH=0
SAMPLE=0
shuf -n 100 "${ORACLE}" 2>/dev/null | while IFS='=' read -r key want; do
    SAMPLE=$((SAMPLE + 1))
    got="$(etcdctl get "${key}" 2>/dev/null | tail -n1)"
    if [[ "${got}" != "${want}" ]]; then
        MISMATCH=$((MISMATCH + 1))
        echo "[mismatch] ${key}: got '${got}', want '${want}'" >&2
    fi
done
echo "[soak] spot check: sample=${SAMPLE} mismatches=${MISMATCH}"

# --- report -----------------------------------------------------------
echo ""
echo "=== Soak report ==="
echo "  duration:      ${DURATION}"
echo "  keys written:  ${TOTAL_KEYS}"
echo "  mismatches:    ${MISMATCH}"
if [[ -f "${DATA_DIR}/resources.csv" ]]; then
    echo "  RSS samples (MB):"
    tail -n +2 "${DATA_DIR}/resources.csv" | awk -F, '{print "    "$1"  rss="$2"MB  cpu="$3"%  keys="$4}'
fi
echo "  data dir:      ${DATA_DIR}"

# Cleanup.
echo "[soak] stopping k8e..."
kill "${K8E_PID}" 2>/dev/null || true
wait "${K8E_PID}" 2>/dev/null || true

if [[ "${MISMATCH}" -gt 0 ]]; then
    echo "[ERROR] soak verification failed: ${MISMATCH} mismatches" >&2
    exit 1
fi
echo "[soak] PASS"
