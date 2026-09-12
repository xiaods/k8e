#!/usr/bin/env bash
# Start a single-node K8E cluster inside a container for end-to-end tests.
#
#   hack/e2e/up.sh [profile]
#
# The cluster state lives on the host under ${E2E_DATA_DIR} so that it survives
# a container restart (which is how the recovery tests exercise persistence).
set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    e2e_usage
    exit 0
fi

e2e_set_profile "${1:-${E2E_PROFILE}}"

e2e_require_cmds docker kubectl

[ -n "${E2E_BINARY}" ] || e2e_die "E2E_BINARY must be set"
[ -x "${E2E_BINARY}" ] || e2e_die "missing executable E2E_BINARY=${E2E_BINARY} (run 'make k8e' first)"

e2e_build_image

# The container has to go before its data directory is recycled: deleting a
# directory that a still-running container has bind mounted leaves the next
# container with a dead mount handle, and every write under /test then fails
# with ENOENT.
if e2e_container_exists; then
    e2e_log "removing stale container ${E2E_CONTAINER}"
    docker rm -f "${E2E_CONTAINER}" >/dev/null
fi

# A fresh cluster by default: stale state is a common source of confusing
# failures. E2E_REUSE=1 keeps an existing data directory.
if [ "${E2E_REUSE:-0}" != "1" ]; then
    rm -rf "${E2E_DATA_DIR}"
fi
mkdir -p "${E2E_DATA_DIR}" "${E2E_DIAG_DIR}" "${E2E_STATE_DIR}"

# Runtime storage goes on its own volume (see E2E_CONTAINERD_ROOT in lib.sh), so
# it has to be recycled here rather than with the data directory. The container
# is already gone at this point, otherwise the volume would still be in use.
#
# The volume is mounted outside the ${E2E_DATA_DIR} bind mount: a nested mount
# under the virtiofs share that Docker Desktop/OrbStack uses is not reliably
# applied, and containerd's root has to end up on a real Linux filesystem. The
# entrypoint links it back to the path containerd was configured with.
storage_args=()
if e2e_profile_needs_agent; then
    if [ "${E2E_REUSE:-0}" != "1" ]; then
        docker volume rm -f "${E2E_CONTAINERD_ROOT_VOLUME}" >/dev/null 2>&1 || true
    fi
    storage_args+=(-v "${E2E_CONTAINERD_ROOT_VOLUME}:${E2E_CONTAINERD_VOLUME_PATH}")
fi

flags=()
while IFS= read -r line; do
    [ -n "${line}" ] || continue
    flags+=("${line}")
done < <(e2e_profile_flags)

docker_args=()
while IFS= read -r line; do
    [ -n "${line}" ] || continue
    docker_args+=("${line}")
done < <(e2e_profile_docker_args)

env_args=()
if e2e_profile_needs_agent; then
    env_args+=(-e "K8E_E2E_CGROUP_ROOT=${E2E_CGROUP_ROOT}")
    env_args+=(-e "K8E_E2E_CONTAINERD_ROOT=${E2E_CONTAINERD_ROOT}")
    env_args+=(-e "K8E_E2E_CONTAINERD_VOLUME=${E2E_CONTAINERD_VOLUME_PATH}")
fi

e2e_log "starting ${E2E_CONTAINER} (profile=${E2E_PROFILE}, image=${E2E_IMAGE})"
e2e_log "server flags: ${flags[*]:-<none>}"
if [ "${#docker_args[@]}" -gt 0 ]; then
    e2e_log "docker flags: ${docker_args[*]}"
fi
if [ "${#storage_args[@]}" -gt 0 ]; then
    e2e_log "runtime storage: ${storage_args[*]}"
fi

docker run -d --name "${E2E_CONTAINER}" \
    -p "127.0.0.1:${E2E_API_PORT}:${E2E_IN_CONTAINER_API_PORT}" \
    -v "${E2E_DATA_DIR}:/test" \
    -v "${E2E_BINARY}:/usr/local/bin/k8e:ro" \
    "${storage_args[@]+"${storage_args[@]}"}" \
    "${docker_args[@]+"${docker_args[@]}"}" \
    "${env_args[@]+"${env_args[@]}"}" \
    "${E2E_IMAGE}" \
    /usr/local/bin/k8e server \
    --cluster-init \
    --data-dir /test/data \
    --write-kubeconfig /test/kubeconfig.yaml \
    --https-listen-port "${E2E_IN_CONTAINER_API_PORT}" \
    --tls-san 127.0.0.1 \
    "${flags[@]+"${flags[@]}"}" >/dev/null

if ! e2e_wait_api; then
    docker logs --tail 100 "${E2E_CONTAINER}" >&2 2>&1 || true
    e2e_die "K8E API did not become ready within ${E2E_API_TIMEOUT}s"
fi
e2e_log "API ready at https://127.0.0.1:${E2E_API_PORT} (kubeconfig ${E2E_KUBECONFIG})"

if e2e_wait_manifests; then
    e2e_log "addon manifests staged at ${E2E_MANIFESTS_DIR}"
else
    e2e_warn "continuing without staged manifests; the suite will report the failure"
fi

if e2e_profile_needs_agent; then
    if [ "${E2E_EXPECT_NODE_READY}" = "1" ]; then
        if e2e_wait_node_ready; then
            e2e_log "node is Ready"
        else
            e2e_warn "continuing without a Ready node; the suite will report the failure"
        fi
    elif e2e_wait_node_registered; then
        e2e_log "node registered (no CNI in this profile: Ready is not expected)"
    else
        e2e_warn "continuing without a registered node; the suite will report the failure"
    fi

    # Fail fast when the runtime storage cannot back containerd's snapshots:
    # the cluster would look healthy and every pod would then fail with a
    # read-only rootfs, which is a miserable failure to debug from the outside.
    if ! e2e_verify_containerd_root; then
        e2e_die "${E2E_CONTAINERD_ROOT} cannot back an overlayfs mount; containerd snapshots would come up read-only (runtime volume missing or shadowed?)"
    fi

    if ! e2e_preload_images; then
        e2e_warn "continuing without preloaded images; pod-level checks will fail"
    fi

    # Pods cannot be admitted or mounted until the controller manager has
    # bootstrapped the namespace (default ServiceAccount, API CA configmap).
    if e2e_wait_namespace_ready default; then
        e2e_log "default namespace is bootstrapped"
    else
        e2e_warn "continuing without a bootstrapped default namespace; pod-level checks may fail"
    fi
fi

e2e_log "cluster is up"
