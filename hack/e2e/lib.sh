#!/usr/bin/env bash
# K8E end-to-end harness — shared helpers.
#
# Source this file from harness scripts; do not execute it directly.
# Bash 3.2 compatible (macOS still ships 3.2): avoid bash 4+ builtins.

set -euo pipefail

E2E_DIR="$(cd "$(dirname "${BASH_SOURCE[0]:-$0}")" && pwd)"
E2E_REPO="$(cd "${E2E_DIR}/../.." && pwd)"

E2E_PROFILE="${E2E_PROFILE:-l1}"
E2E_API_TIMEOUT="${E2E_API_TIMEOUT:-300}"
E2E_NODE_TIMEOUT="${E2E_NODE_TIMEOUT:-420}"
E2E_STAGING_TIMEOUT="${E2E_STAGING_TIMEOUT:-120}"
E2E_KEEP="${E2E_KEEP:-0}"
E2E_PURGE="${E2E_PURGE:-0}"
E2E_SKIP_IMAGE_BUILD="${E2E_SKIP_IMAGE_BUILD:-0}"
E2E_IN_CONTAINER_API_PORT="${E2E_IN_CONTAINER_API_PORT:-6443}"
# Where up.sh mounts ${E2E_DATA_DIR} inside the container, and the kubeconfig the
# server writes there. Defined once so the mount and its readers cannot drift.
E2E_IN_CONTAINER_DATA_DIR="${E2E_IN_CONTAINER_DATA_DIR:-/test}"
E2E_IN_CONTAINER_KUBECONFIG="${E2E_IN_CONTAINER_KUBECONFIG:-${E2E_IN_CONTAINER_DATA_DIR}/kubeconfig.yaml}"
E2E_IMAGE="${E2E_IMAGE:-k8e-e2e:local}"
E2E_EXTRA_SERVER_ARGS="${E2E_EXTRA_SERVER_ARGS:-}"
E2E_GO_TEST_ARGS="${E2E_GO_TEST_ARGS:-}"
E2E_SKIP_GO_TESTS="${E2E_SKIP_GO_TESTS:-0}"

E2E_CHECKS=0
E2E_FAILURES=0

# e2e_set_profile <profile>
# Resolve profile-derived paths. Anything already exported by the caller wins.
e2e_set_profile() {
    E2E_PROFILE="$1"
    : "${E2E_CONTAINER:=k8e-e2e-${E2E_PROFILE}}"
    : "${E2E_STATE_DIR:=/tmp/k8e-e2e/${E2E_PROFILE}}"
    : "${E2E_DATA_DIR:=${E2E_STATE_DIR}/var}"
    : "${E2E_DIAG_DIR:=${E2E_STATE_DIR}/diagnostics}"
    : "${E2E_BINARY:=${E2E_REPO}/bin/k8e}"
    : "${E2E_KUBECONFIG:=${E2E_STATE_DIR}/kubeconfig}"
    # up.sh runs the server with
    # --data-dir ${E2E_IN_CONTAINER_DATA_DIR}/data and mounts ${E2E_DATA_DIR}
    # there, so the staged addon manifests land here on the host.
    : "${E2E_MANIFESTS_DIR:=${E2E_DATA_DIR}/data/server/manifests}"
    # Each profile publishes its API on its own host port so that clusters kept
    # alive with E2E_KEEP=1 do not fight over a single port. An unknown profile is
    # rejected by the case further down, so no port default is needed here.
    case "${E2E_PROFILE}" in
    l1) : "${E2E_API_PORT:=16443}" ;;
    l2) : "${E2E_API_PORT:=16444}" ;;
    l3) : "${E2E_API_PORT:=16445}" ;;
    *) ;;
    esac
    # Dedicated cgroup subtree for kubelet. The container entrypoint creates it
    # before k8e starts; keeping the host's cgroup tree untouched.
    : "${E2E_CGROUP_ROOT:=/k8e-e2e}"
    # containerd socket created by the agent inside the container.
    : "${E2E_CONTAINERD_SOCKET:=/run/k8e/containerd/containerd.sock}"
    : "${E2E_TEST_IMAGE:=busybox:1.36}"
    # containerd keeps its snapshots under <data-dir>/agent/containerd and uses
    # that directory as the upper layer of the overlayfs it mounts for every
    # container rootfs. The data directory is a host bind mount, which on Docker
    # Desktop/OrbStack is virtiofs and cannot back an overlay upper layer: the
    # snapshot mounts come up read-only and every pod sandbox dies with
    #   mkdirat(.../rootfs, "proc", 0o755): Read-only file system
    # So the runtime lives on a dedicated volume, mounted OUTSIDE the bind mount
    # (a volume nested under a virtiofs share is silently not applied at all --
    # `docker inspect` lists it, /proc/mounts does not; the entrypoint links the
    # volume in at E2E_CONTAINERD_ROOT).
    : "${E2E_CONTAINERD_ROOT:=/test/data/agent/containerd}"
    : "${E2E_CONTAINERD_ROOT_VOLUME:=k8e-e2e-${E2E_PROFILE}-containerd}"
    : "${E2E_CONTAINERD_VOLUME_PATH:=/var/lib/k8e-containerd}"

    case "${E2E_PROFILE}" in
    l1)
        # No kubelet at all: nothing to wait for, nothing to run.
        : "${E2E_EXPECT_NODE_READY:=0}"
        : "${E2E_PRELOAD_IMAGES:=}"
        ;;
    l2)
        # No CNI, so kubelet stays NetworkReady=false and the node never turns
        # Ready; registration is the strongest signal this profile can give.
        : "${E2E_EXPECT_NODE_READY:=0}"
        : "${E2E_PRELOAD_IMAGES:=rancher/mirrored-pause:3.6 ${E2E_TEST_IMAGE}}"
        ;;
    l3)
        : "${E2E_EXPECT_NODE_READY:=1}"
        : "${E2E_PRELOAD_IMAGES:=rancher/mirrored-pause:3.6 ${E2E_TEST_IMAGE}}"
        ;;
    *)
        e2e_die "unknown E2E profile '${E2E_PROFILE}' (expected l1, l2 or l3)"
        ;;
    esac

    export E2E_PROFILE E2E_CONTAINER E2E_STATE_DIR E2E_DATA_DIR E2E_DIAG_DIR
    export E2E_BINARY E2E_KUBECONFIG E2E_IMAGE E2E_API_PORT E2E_MANIFESTS_DIR
    export E2E_CGROUP_ROOT E2E_CONTAINERD_SOCKET E2E_TEST_IMAGE
    export E2E_CONTAINERD_ROOT E2E_CONTAINERD_ROOT_VOLUME E2E_CONTAINERD_VOLUME_PATH
    export E2E_EXPECT_NODE_READY E2E_PRELOAD_IMAGES
    export E2E_API_TIMEOUT E2E_NODE_TIMEOUT E2E_STAGING_TIMEOUT E2E_KEEP E2E_PURGE
    export E2E_SKIP_IMAGE_BUILD E2E_IN_CONTAINER_API_PORT E2E_EXTRA_SERVER_ARGS
    export E2E_IN_CONTAINER_DATA_DIR E2E_IN_CONTAINER_KUBECONFIG
    export E2E_GO_TEST_ARGS E2E_SKIP_GO_TESTS
}

e2e_log() { printf '[e2e:%s] %s\n' "${E2E_PROFILE}" "$*"; }
e2e_warn() { printf '[e2e:%s] WARNING: %s\n' "${E2E_PROFILE}" "$*" >&2; }
e2e_die() {
    printf '[e2e:%s] ERROR: %s\n' "${E2E_PROFILE}" "$*" >&2
    exit 1
}

e2e_ok() {
    E2E_CHECKS=$((E2E_CHECKS + 1))
    printf '  ok   %s\n' "$1"
}

e2e_bad() {
    E2E_CHECKS=$((E2E_CHECKS + 1))
    E2E_FAILURES=$((E2E_FAILURES + 1))
    printf '  FAIL %s\n' "$1"
}

# e2e_check <description> <command...>
e2e_check() {
    local description="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        e2e_ok "$description"
    else
        e2e_bad "$description"
    fi
}

# e2e_check_absent <description> <command...>
e2e_check_absent() {
    local description="$1"
    shift
    if "$@" >/dev/null 2>&1; then
        e2e_bad "$description"
    else
        e2e_ok "$description"
    fi
}

e2e_summary() {
    printf '\n[e2e:%s] %d checks, %d failures\n' "${E2E_PROFILE}" "${E2E_CHECKS}" "${E2E_FAILURES}"
    if [ "${E2E_FAILURES}" -ne 0 ]; then
        return 1
    fi
    return 0
}

e2e_require_cmds() {
    local missing=""
    local cmd
    for cmd in "$@"; do
        if ! command -v "$cmd" >/dev/null 2>&1; then
            missing="${missing} ${cmd}"
        fi
    done
    if [ -n "${missing}" ]; then
        e2e_die "missing required commands:${missing}"
    fi
}

# --- profiles -------------------------------------------------------------

# l1: control plane only (no agent, no CNI) — validates API, addon staging,
#     disable semantics and restart recovery.
# l2: control plane + agent, no CNI — validates kubelet/containerd bring-up.
# l3: full stack including the bundled Cilium CNI/Gateway.
e2e_profile_flags() {
    case "${E2E_PROFILE}" in
    l1)
        printf '%s\n' \
            --disable-agent \
            --disable-sandbox-matrix \
            --disable-e2b \
            --disable-cilium \
            --disable-cloud-controller \
            --egress-selector-mode disabled
        ;;
    l2)
        printf '%s\n' \
            --disable-sandbox-matrix \
            --disable-e2b \
            --disable-cilium \
            --disable-cloud-controller \
            --egress-selector-mode disabled \
            "--kubelet-arg=cgroup-root=${E2E_CGROUP_ROOT}"
        ;;
    l3)
        printf '%s\n' \
            --disable-cloud-controller \
            --egress-selector-mode disabled \
            "--kubelet-arg=cgroup-root=${E2E_CGROUP_ROOT}"
        ;;
    *) ;;
    esac
    if [ -n "${E2E_EXTRA_SERVER_ARGS}" ]; then
        # Intentionally word-split: the value is a flag list.
        # shellcheck disable=SC2086
        printf '%s\n' ${E2E_EXTRA_SERVER_ARGS}
    fi
}

e2e_profile_needs_agent() {
    [ "${E2E_PROFILE}" != "l1" ]
}

# Extra docker arguments required by the profile.
e2e_profile_docker_args() {
    if e2e_profile_needs_agent; then
        printf '%s\n' --privileged --cgroupns=host
        if [ -d /lib/modules ]; then
            printf '%s\n' -v /lib/modules:/lib/modules:ro
        fi
    fi
    if [ "${E2E_PROFILE}" = "l3" ] && [ -d /sys/fs/bpf ]; then
        printf '%s\n' -v /sys/fs/bpf:/sys/fs/bpf
    fi
}

# --- docker / cluster state ----------------------------------------------

e2e_container_exists() {
    docker inspect "$E2E_CONTAINER" >/dev/null 2>&1
}

e2e_container_running() {
    [ "$(docker inspect -f '{{.State.Running}}' "$E2E_CONTAINER" 2>/dev/null)" = "true" ]
}

e2e_build_image() {
    if [ "${E2E_SKIP_IMAGE_BUILD}" = "1" ]; then
        e2e_log "skipping image build (E2E_SKIP_IMAGE_BUILD=1)"
        return 0
    fi
    if docker image inspect "${E2E_IMAGE}" >/dev/null 2>&1; then
        e2e_log "reusing existing image ${E2E_IMAGE}"
        return 0
    fi
    e2e_log "building ${E2E_IMAGE} from hack/e2e/Dockerfile"
    docker build -q -f "${E2E_DIR}/Dockerfile" -t "${E2E_IMAGE}" "${E2E_DIR}" >/dev/null
}

# --- kubectl helpers ------------------------------------------------------

e2e_kubectl() {
    kubectl --kubeconfig "${E2E_KUBECONFIG}" --request-timeout=20s "$@"
}

e2e_api_ready() {
    kubectl --kubeconfig "${E2E_KUBECONFIG}" --request-timeout=3s get --raw=/readyz >/dev/null 2>&1
}

# Copy the in-container kubeconfig out and point it at the published port.
#
# The server writes it as root with mode 0600, so the bind-mounted copy is only
# readable by its owner: on Docker Desktop/OrbStack the mount is mapped to the
# caller and `cp` works, but on a plain Linux host (CI) the unprivileged user
# gets "Permission denied" instead. Reading it through the container works
# everywhere, and the temp file keeps a half-written config from being used.
e2e_rewrite_kubeconfig() {
    local tmp="${E2E_KUBECONFIG}.tmp"
    e2e_in_container test -s "${E2E_IN_CONTAINER_KUBECONFIG}" || return 1
    e2e_in_container cat "${E2E_IN_CONTAINER_KUBECONFIG}" >"${tmp}" || return 1
    mv "${tmp}" "${E2E_KUBECONFIG}"
    kubectl --kubeconfig "${E2E_KUBECONFIG}" config set-cluster default \
        --server="https://127.0.0.1:${E2E_API_PORT}" >/dev/null
    return 0
}

e2e_wait_api() {
    local deadline log
    deadline=$(($(date +%s) + E2E_API_TIMEOUT))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! e2e_container_running; then
            log="$(docker logs --tail 200 "${E2E_CONTAINER}" 2>&1 || true)"
            printf '\n--- container logs ---\n%s\n' "${log}" >&2
            if e2e_profile_needs_agent; then
                # containerd logs to a file under its own root, not to stdout, so
                # the dump above never shows why it refused to start (a config it
                # rejects kills it before k8e can print anything useful).
                printf '\n--- containerd log (last 50 lines) ---\n' >&2
                e2e_containerd_log 50 >&2
            fi
            # `zig build k8e` produces cmd/server, which -- unlike the release
            # cmd/k8e multicall binary -- embeds no runtime binaries. Profiles
            # with an agent therefore die trying to exec containerd from PATH.
            if e2e_profile_needs_agent && printf '%s' "${log}" | grep -q 'executable file not found in \$PATH'; then
                e2e_die "profile ${E2E_PROFILE} needs an agent, but ${E2E_BINARY} bundles no container runtime. Build a release binary (cmd/k8e, which stages containerd/runc) or use an image that provides them in PATH; the l1 profile works with the cmd/server build."
            fi
            e2e_die "container ${E2E_CONTAINER} exited before the API became ready (see logs above); E2E_BINARY=${E2E_BINARY} must be a linux/amd64 build of k8e"
        fi
        if e2e_rewrite_kubeconfig && e2e_api_ready; then
            return 0
        fi
        sleep 2
    done
    return 1
}

# e2e_wait_manifests waits until the control plane has written its staged addon
# manifests. Controllers (and therefore staging) only start once the API server
# reports ready, so waiting on /readyz alone races with the staging step.
e2e_wait_manifests() {
    local deadline
    deadline=$(($(date +%s) + E2E_STAGING_TIMEOUT))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! e2e_container_running; then
            e2e_die "container ${E2E_CONTAINER} exited while waiting for addon staging"
        fi
        if [ -d "${E2E_MANIFESTS_DIR}" ]; then
            return 0
        fi
        sleep 2
    done
    e2e_warn "addon manifests were not staged within ${E2E_STAGING_TIMEOUT}s"
    return 1
}

e2e_wait_node_ready() {
    local deadline
    deadline=$(($(date +%s) + E2E_NODE_TIMEOUT))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! e2e_container_running; then
            e2e_die "container ${E2E_CONTAINER} exited while waiting for the node"
        fi
        if e2e_kubectl wait --for=condition=Ready node --all --timeout=5s >/dev/null 2>&1; then
            return 0
        fi
        sleep 5
    done
    e2e_warn "node did not become Ready within ${E2E_NODE_TIMEOUT}s"
    return 1
}

# e2e_wait_node_registered waits until kubelet has registered the node and is
# posting its status. Profiles without a CNI never report Ready, so this is the
# strongest signal they can give.
e2e_wait_node_registered() {
    local deadline
    deadline=$(($(date +%s) + E2E_NODE_TIMEOUT))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! e2e_container_running; then
            e2e_die "container ${E2E_CONTAINER} exited while waiting for a node"
        fi
        if e2e_kubectl get nodes -o jsonpath='{range .items[*]}{.status.nodeInfo.kubeletVersion}{"\n"}{end}' 2>/dev/null | grep -q .; then
            return 0
        fi
        sleep 2
    done
    e2e_warn "no node was registered within ${E2E_NODE_TIMEOUT}s"
    return 1
}

# e2e_wait_namespace_ready [namespace]
#
# The API server serves traffic before kube-controller-manager has populated the
# per-namespace bootstrap objects. Every pod needs them, and without this wait
# the first attempts fail with confusing, non-obvious errors:
#   * `default` ServiceAccount missing  -> "error looking up service account
#     default/default: serviceaccount \"default\" not found" (Forbidden)
#   * `kube-root-ca.crt` ConfigMap missing -> the kube-api-access projected
#     volume never becomes mountable ("configmap \"kube-root-ca.crt\" not found")
e2e_wait_namespace_ready() {
    local namespace="${1:-default}"
    local deadline
    deadline=$(($(date +%s) + ${E2E_NAMESPACE_TIMEOUT:-120}))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! e2e_container_running; then
            e2e_die "container ${E2E_CONTAINER} exited while waiting for namespace ${namespace}"
        fi
        if e2e_kubectl get serviceaccount default -n "${namespace}" >/dev/null 2>&1 &&
            e2e_kubectl get configmap kube-root-ca.crt -n "${namespace}" >/dev/null 2>&1; then
            return 0
        fi
        sleep 2
    done
    e2e_warn "namespace ${namespace} was not fully bootstrapped within ${E2E_NAMESPACE_TIMEOUT:-120}s"
    return 1
}

# e2e_wait_pod_running <pod> [timeout]
# Waits for a pod to reach Running; returns 1 on Failed/unknown-phase timeout.
e2e_wait_pod_running() {
    local pod="$1"
    local timeout="${2:-120}"
    local deadline phase
    deadline=$(($(date +%s) + timeout))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        phase="$(e2e_kubectl get pod "${pod}" -o jsonpath='{.status.phase}' 2>/dev/null || true)"
        case "${phase}" in
        Running) return 0 ;;
        Failed | Succeeded) return 1 ;;
        *) ;;
        esac
        sleep 3
    done
    return 1
}

# e2e_node_platform maps the container's machine type to an OCI platform string.
# Empty output means "unknown, do not filter".
e2e_node_platform() {
    case "$(e2e_in_container uname -m 2>/dev/null || true)" in
    x86_64 | amd64) printf 'linux/amd64\n' ;;
    aarch64 | arm64) printf 'linux/arm64\n' ;;
    armv7l) printf 'linux/arm/v7\n' ;;
    s390x) printf 'linux/s390x\n' ;;
    ppc64le) printf 'linux/ppc64le\n' ;;
    riscv64) printf 'linux/riscv64\n' ;;
    *) ;;
    esac
}

# e2e_verify_containerd_root
#
# Proves that containerd's root directory can actually back an overlayfs upper
# layer by mounting one there. Without this, a root directory that landed on a
# filesystem which cannot do so (a virtiofs host bind mount is the usual
# suspect) stays silent: the cluster comes up healthy, and every pod then fails
# with `mkdirat(.../rootfs, "proc", 0o755): Read-only file system`.
e2e_verify_containerd_root() {
    e2e_in_container sh -c '
        set -e
        probe="$1/.overlay-probe"
        rm -rf "$probe"
        mkdir -p "$probe/lower" "$probe/upper" "$probe/work" "$probe/merged"
        mount -t overlay overlay \
            -o "lowerdir=$probe/lower,upperdir=$probe/upper,workdir=$probe/work" \
            "$probe/merged"
        mkdir "$probe/merged/probe-dir"
        umount "$probe/merged"
        rm -rf "$probe"
    ' e2e-overlay-probe "${E2E_CONTAINERD_ROOT}" >/dev/null 2>&1
}

# e2e_preload_images imports ${E2E_PRELOAD_IMAGES} into the cluster's containerd.
#
# The node gets no reliable registry access (and CI should not depend on one),
# so images are pulled on the host and pushed in through `ctr images import`.
# Importing into the k8s.io namespace is what makes CRI see them.
#
# `docker save` has to be told which platform to export. When Docker uses the
# containerd image store it writes an OCI layout whose top-level index.json lists
# every platform of the image, but only the blobs of the local one are included;
# `ctr images import` then fails with `content digest sha256:...: not found`
# while walking the index. `--platform` trims the index down to the node's
# architecture, which is what the node can execute anyway.
e2e_preload_images() {
    [ -n "${E2E_PRELOAD_IMAGES:-}" ] || return 0

    local deadline image platform save_flags
    deadline=$(($(date +%s) + ${E2E_PRELOAD_TIMEOUT:-300}))
    while [ "$(date +%s)" -lt "${deadline}" ]; do
        if ! e2e_container_running; then
            e2e_die "container ${E2E_CONTAINER} exited while waiting for containerd"
        fi
        if e2e_in_container test -S "${E2E_CONTAINERD_SOCKET}" 2>/dev/null; then
            break
        fi
        sleep 2
    done
    if ! e2e_in_container test -S "${E2E_CONTAINERD_SOCKET}" 2>/dev/null; then
        e2e_warn "containerd socket ${E2E_CONTAINERD_SOCKET} never appeared"
        return 1
    fi

    for image in ${E2E_PRELOAD_IMAGES}; do
        # Reuse a local copy when there is one, otherwise pull it.
        if ! docker image inspect "${image}" >/dev/null 2>&1 &&
            ! docker pull -q "${image}" >/dev/null; then
            e2e_warn "could not pull ${image} on the host"
            return 1
        fi
    done

    # `docker save --platform` only exists on newer clients; older ones write a
    # plain docker-archive that imports as-is, so the flag is simply optional.
    save_flags=()
    platform="$(e2e_node_platform)"
    if [ -n "${platform}" ] && docker save --help 2>&1 | grep -q -- '--platform'; then
        save_flags+=(--platform "${platform}")
    else
        e2e_warn "docker save does not support --platform; importing whatever the daemon exports"
    fi

    # shellcheck disable=SC2086
    if ! docker save "${save_flags[@]+"${save_flags[@]}"}" ${E2E_PRELOAD_IMAGES} |
        e2e_in_container_stdin ctr --address "${E2E_CONTAINERD_SOCKET}" --namespace k8s.io images import - >/dev/null; then
        e2e_warn "could not import ${E2E_PRELOAD_IMAGES} into containerd"
        return 1
    fi
    e2e_log "preloaded images: ${E2E_PRELOAD_IMAGES}"
}

e2e_require_cluster() {
    e2e_container_running || e2e_die "container ${E2E_CONTAINER} is not running (run hack/e2e/up.sh first)"
    e2e_api_ready || e2e_die "K8E API is not ready"
    [ -f "${E2E_KUBECONFIG}" ] || e2e_die "missing kubeconfig ${E2E_KUBECONFIG}"
}

e2e_in_container() {
    docker exec "${E2E_CONTAINER}" "$@"
}

# e2e_in_container_stdin <command...>
# Like e2e_in_container, but keeps stdin attached so the command can read piped
# input (e.g. `ctr images import -`). Plain `docker exec` closes stdin, which
# makes such commands fail with "unrecognized image format".
e2e_in_container_stdin() {
    docker exec -i "${E2E_CONTAINER}" "$@"
}

# e2e_containerd_log [lines]
# Print the tail of containerd's own log file.
#
# containerd writes it under its root directory, which lives on the runtime
# volume and is therefore invisible from the /test bind mount: `docker cp` is the
# only way to reach it, and unlike `docker exec` it also works on a stopped
# container -- which is exactly when the log matters most.
e2e_containerd_log() {
    local lines="${1:-400}"
    docker cp "${E2E_CONTAINER}:${E2E_CONTAINERD_ROOT}/containerd.log" - 2>/dev/null |
        tar -xO 2>/dev/null |
        tail -n "${lines}" || true
    return 0
}

e2e_usage() {
    cat <<EOF
K8E end-to-end harness

  hack/e2e/up.sh <profile>     start a single-node cluster in a container
  hack/e2e/dump-logs.sh        collect diagnostics into \${E2E_DIAG_DIR}
  hack/e2e/down.sh             stop and remove the cluster container
  hack/e2e/run.sh <profile> [suite...]
                               up -> suites -> diagnostics on failure -> down

Profiles:
  l1  control plane only (no agent, no CNI) — implemented and runs in CI
  l2  control plane + agent, no CNI — implemented: Docker pulls the pause and
      busybox images on the host and preloads them into the node
  l3  full stack (containerd + Cilium CNI) — cluster can be started, no suite
      yet

Only suites/l1.sh and suites/l2.sh exist today. The k8e binary built by
`zig build k8e` (cmd/server) embeds no runtime binaries, so the image supplies
containerd/runc itself (see Dockerfile); container images come from the host
through `ctr images import` because the node has no registry access.

Agent profiles keep containerd's storage on a Docker volume (mounted at
/var/lib/k8e-containerd and linked into the data directory): the virtiofs-backed
/test bind mount cannot host containerd's overlayfs snapshots, which would
otherwise come up read-only. up.sh verifies this before running a suite.

Environment:
  E2E_BINARY      k8e binary to run (default: <repo>/bin/k8e), must be a linux
                  build for the Docker daemon's architecture
  E2E_IMAGE       container image (default: k8e-e2e:local)
  E2E_API_PORT    published API port on the host (default: 16443, 16444, 16445
                  for l1, l2, l3 so profiles can coexist)
  E2E_KEEP=1      leave the container running after run.sh
  E2E_REUSE=1     reuse the existing data directory and runtime volume in up.sh
  E2E_PURGE=1     remove the state directory and runtime volume in down.sh
EOF
}
