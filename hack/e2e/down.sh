#!/usr/bin/env bash
# Stop and remove an E2E cluster.
#
#   hack/e2e/down.sh [profile]
#
# The state directory is kept for post-mortem inspection unless E2E_PURGE=1.
set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    e2e_usage
    exit 0
fi

e2e_set_profile "${1:-${E2E_PROFILE}}"
e2e_require_cmds docker

if e2e_container_exists; then
    e2e_log "removing container ${E2E_CONTAINER}"
    docker rm -f "${E2E_CONTAINER}" >/dev/null
else
    e2e_log "container ${E2E_CONTAINER} already gone"
fi

if [ "${E2E_PURGE}" = "1" ]; then
    e2e_log "purging ${E2E_STATE_DIR}"
    rm -rf "${E2E_STATE_DIR}"
    # Runtime storage lives on its own volume (see E2E_CONTAINERD_ROOT in lib.sh).
    if docker volume inspect "${E2E_CONTAINERD_ROOT_VOLUME}" >/dev/null 2>&1; then
        e2e_log "purging volume ${E2E_CONTAINERD_ROOT_VOLUME}"
        docker volume rm -f "${E2E_CONTAINERD_ROOT_VOLUME}" >/dev/null 2>&1 || true
    fi
fi
