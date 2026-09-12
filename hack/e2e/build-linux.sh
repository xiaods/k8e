#!/usr/bin/env bash
# Build a linux k8e binary from a non-Linux workstation (convenience).
#
#   hack/e2e/build-linux.sh
#   E2E_BINARY=$PWD/bin/k8e-linux-arm64 hack/e2e/run.sh l1
#
# CI uses `make k8e` on a Linux runner instead; this script only exists so that
# macOS contributors can run the harness. It mirrors the flags used by
# `zig build k8e` (same build tags, CGO on, statically linked) so the resulting
# binary runs inside the stock E2E image.
set -euo pipefail

. "$(cd "$(dirname "$0")" && pwd)/lib.sh"

if [ "${1:-}" = "-h" ] || [ "${1:-}" = "--help" ]; then
    e2e_usage
    exit 0
fi

e2e_require_cmds docker

image="${E2E_GOLANG_IMAGE:-golang:1.26.7}"
# Match the host architecture by default: emulating a foreign arch would make
# the k8s build painfully slow. CI builds amd64 through `make k8e` instead.
host_arch="$(docker info --format '{{.Architecture}}' 2>/dev/null || echo amd64)"
case "${host_arch}" in
aarch64 | arm64) default_arch="arm64" ;;
*) default_arch="amd64" ;;
esac
goarch="${E2E_GOARCH:-${default_arch}}"
out="bin/k8e-linux-${goarch}"
tags="ctrd netcgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp"
ldflags="-w -s -extldflags '-static -lm -ldl -lz -lpthread'"
go_cache="${E2E_STATE_DIR:-/tmp/k8e-e2e}/gocache"
mkdir -p "${go_cache}"

# Reuse the host module cache and GOPROXY when a matching Go toolchain exists;
# otherwise the build would re-download the whole k8s dependency tree.
extra_docker_args=()
docker_env=(-e GOCACHE=/gocache)
if command -v go >/dev/null 2>&1; then
    host_cache="$(go env GOMODCACHE 2>/dev/null || true)"
    if [ -n "${host_cache}" ] && [ -d "${host_cache}" ]; then
        extra_docker_args+=(-v "${host_cache}":/go/pkg/mod)
        e2e_log "reusing host module cache ${host_cache}"
    fi
    docker_env+=(-e "GOPROXY=$(go env GOPROXY)")
fi

# gcc ships with the golang image; the static extldflags additionally need the
# zlib and seccomp development packages.
e2e_log "building ./cmd/server for linux/${goarch} with ${image} (output ${out})"
docker run --rm \
    -v "${E2E_REPO}":/src \
    -v "${go_cache}":/gocache \
    "${extra_docker_args[@]+"${extra_docker_args[@]}"}" \
    -w /src \
    -e DEBIAN_FRONTEND=noninteractive \
    -e GOOS=linux -e GOARCH="${goarch}" -e CGO_ENABLED=1 \
    "${docker_env[@]}" \
    "${image}" \
    sh -c "apt-get update -qq \
        && apt-get install -y -qq --no-install-recommends zlib1g-dev libseccomp-dev >/dev/null \
        && go build -tags '${tags}' -buildvcs=false -ldflags \"${ldflags}\" -o ${out} ./cmd/server"

e2e_log "built ${E2E_REPO}/${out}"
e2e_log "run: E2E_BINARY=${E2E_REPO}/${out} hack/e2e/run.sh l1"
e2e_log "override the target architecture with E2E_GOARCH=amd64|arm64"
