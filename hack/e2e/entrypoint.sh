#!/bin/sh
# k8e E2E container entrypoint.
#
# kubelet validates that the cgroup subtree named by --cgroup-root exists and
# exposes cgroup.controllers, and refuses to start otherwise. Creating it here
# instead of from the host after `docker run` keeps the setup race-free.
set -e

if [ -n "${K8E_E2E_CGROUP_ROOT:-}" ]; then
    mkdir -p "${K8E_E2E_CGROUP_ROOT}" 2>/dev/null || true
fi

# containerd uses its root directory as the upper layer of the overlayfs it
# mounts for every container rootfs. The E2E data directory is a host bind mount
# (virtiofs under Docker Desktop/OrbStack), and that cannot back an overlay upper
# layer: the snapshot mounts come up read-only and every pod dies with
#   mkdirat(.../rootfs, "proc", 0o755): Read-only file system
# The runtime volume is therefore mounted outside the bind mount and linked in
# from here, so the path containerd was configured with keeps working.
if [ -n "${K8E_E2E_CONTAINERD_ROOT:-}" ] && [ -n "${K8E_E2E_CONTAINERD_VOLUME:-}" ]; then
    mkdir -p "$(dirname "${K8E_E2E_CONTAINERD_ROOT}")" "${K8E_E2E_CONTAINERD_VOLUME}"
    rm -rf "${K8E_E2E_CONTAINERD_ROOT}"
    ln -sfn "${K8E_E2E_CONTAINERD_VOLUME}" "${K8E_E2E_CONTAINERD_ROOT}"
fi

exec "$@"
