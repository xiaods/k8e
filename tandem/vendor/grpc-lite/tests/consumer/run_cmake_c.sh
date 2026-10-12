#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)

run_isolated() {
    local work_dir=$1
    local package_source=$2
    local consumer_source=$3
    local bundle_dir=$4
    local mismatch_bundle=$5
    local nghttp2_archive=$6
    local cares_archive=$7
    local version=$8

    if command -v zig >/dev/null 2>&1; then
        printf '%s\n' 'Zig must not be available in the transpiled C bundle consumer' >&2
        exit 1
    fi

    export CC=${CC:-clang}
    export CXX=${CXX:-clang++}
    local common=(
        -G Ninja
        -DCMAKE_BUILD_TYPE=Release
        -DGRPC_LITE_NO_NETWORK=ON
        -DGRPC_LITE_NGHTTP2_URL="file://$nghttp2_archive"
        -DGRPC_LITE_NGHTTP2_URL_HASH=SHA256=3f042e5284ad349f837b65dd5be953b34c007a05259119092dbe6ebeaa73d306
        -DGRPC_LITE_CARES_URL="file://$cares_archive"
        -DGRPC_LITE_CARES_URL_HASH=SHA256=c222b6d681096f9444d2c4863d2c1174019e27cacca0a4a5c114d36dd7d7bf78)

    if cmake -S "$package_source" -B "$work_dir/negative-missing" \
        "${common[@]}" >"$work_dir/negative-missing.log" 2>&1; then
        printf '%s\n' 'CMake unexpectedly accepted a missing bundle path' >&2
        exit 1
    fi
    grep -q 'GRPC_LITE_TRANSPILED_C_DIR is required' "$work_dir/negative-missing.log"

    if cmake -S "$package_source" -B "$work_dir/negative-mismatch" \
        "${common[@]}" -DGRPC_LITE_TRANSPILED_C_DIR="$mismatch_bundle" \
        >"$work_dir/negative-mismatch.log" 2>&1; then
        printf '%s\n' 'CMake unexpectedly accepted a mismatched bundle manifest' >&2
        exit 1
    fi
    grep -q 'bundle version mismatch' "$work_dir/negative-mismatch.log"

    if cmake -S "$package_source" -B "$work_dir/negative-network" \
        -G Ninja -DGRPC_LITE_NO_NETWORK=ON \
        -DGRPC_LITE_TRANSPILED_C_DIR="$bundle_dir" \
        >"$work_dir/negative-network.log" 2>&1; then
        printf '%s\n' 'CMake unexpectedly selected remote dependencies with NO_NETWORK enabled' >&2
        exit 1
    fi
    grep -q 'requires nghttp2' "$work_dir/negative-network.log"

    mkdir -p "$package_source/third_party/nghttp2"
    : >"$package_source/third_party/nghttp2/CMakeLists.txt"
    if cmake -S "$package_source" -B "$work_dir/negative-remote-override" \
        -G Ninja -DGRPC_LITE_NO_NETWORK=ON \
        -DGRPC_LITE_TRANSPILED_C_DIR="$bundle_dir" \
        -DGRPC_LITE_NGHTTP2_URL=https://example.invalid/nghttp2.tar.gz \
        -DGRPC_LITE_CARES_URL="file://$cares_archive" \
        -DGRPC_LITE_CARES_URL_HASH=SHA256=c222b6d681096f9444d2c4863d2c1174019e27cacca0a4a5c114d36dd7d7bf78 \
        >"$work_dir/negative-remote-override.log" 2>&1; then
        printf '%s\n' 'CMake unexpectedly preferred a remote nghttp2 URL in NO_NETWORK mode' >&2
        exit 1
    fi
    grep -q 'configured URL is' "$work_dir/negative-remote-override.log"
    grep -q 'https://example.invalid/nghttp2.tar.gz' \
        "$work_dir/negative-remote-override.log"
    rm -rf "$package_source/third_party/nghttp2"

    cmake -S "$consumer_source" -B "$work_dir/build" \
        "${common[@]}" \
        -DGRPC_LITE_SOURCE_DIR="$package_source" \
        -DGRPC_LITE_TRANSPILED_C_DIR="$bundle_dir" \
        -DGRPC_LITE_EXPECTED_VERSION="$version"
    cmake --build "$work_dir/build" --target \
        c_consumer_shared c_consumer_static grpc_lite_protoc_gen \
        grpcpp_generated_from_bundle
    ctest --test-dir "$work_dir/build" --output-on-failure
}

if [[ ${1:-} == --isolated ]]; then
    shift
    if [[ $# -ne 8 ]]; then
        printf '%s\n' 'internal error: invalid isolated consumer arguments' >&2
        exit 2
    fi
    run_isolated "$@"
    exit
fi

if [[ $# -ne 2 ]]; then
    printf 'usage: %s <transpiled-c.tar.gz> <transpiled-c.tar.gz.sha256>\n' "$0" >&2
    exit 2
fi
archive=$(cd "$(dirname "$1")" && pwd)/$(basename "$1")
sidecar=$(cd "$(dirname "$2")" && pwd)/$(basename "$2")
if [[ ! -f "$archive" || ! -f "$sidecar" ]]; then
    printf '%s\n' 'The bundle archive and SHA-256 sidecar must both exist' >&2
    exit 1
fi

work_dir=$(mktemp -d "${TMPDIR:-/tmp}/grpc-lite-cmake-c-consumer.XXXXXX")
cleanup() {
    local status=$?
    if [[ -n ${GRPC_LITE_KEEP_E2E:-} ]]; then
        printf 'Kept CMake C consumer work directory: %s\n' "$work_dir" >&2
    else
        chmod -R u+w "$work_dir" 2>/dev/null || true
        rm -rf "$work_dir"
    fi
    return "$status"
}
trap cleanup EXIT

archive_name=$(basename "$archive")
if [[ $(basename "$sidecar") != "$archive_name.sha256" ]]; then
    printf '%s\n' 'The checksum sidecar name must be the archive name plus .sha256' >&2
    exit 1
fi
expected_sidecar="$(sha256sum "$archive" | cut -d ' ' -f 1)  $archive_name"
if [[ $(cat "$sidecar") != "$expected_sidecar" ]]; then
    printf '%s\n' 'The checksum sidecar is not canonical lowercase sha256sum format' >&2
    exit 1
fi
cp "$archive" "$sidecar" "$work_dir/"
(cd "$work_dir" && sha256sum --check "$archive_name.sha256")

root_name=${archive_name%.tar.gz}
python3 - "$work_dir/$archive_name" "$root_name" <<'PY'
import sys
import tarfile
archive, root = sys.argv[1:]
expected = {
    root + "/",
    root + "/manifest.json",
    root + "/include/",
    root + "/include/zig.h",
    root + "/src/",
    root + "/src/grpc_lite.c",
    root + "/src/protoc-gen-grpc_lite_cpp.c",
}
with tarfile.open(archive, "r:gz") as value:
    names = set()
    for member in value.getmembers():
        if member.name.startswith("/") or ".." in member.name.split("/"):
            raise SystemExit("unsafe archive member: " + member.name)
        if not (member.isdir() or member.isfile()):
            raise SystemExit("links and special archive members are forbidden: " + member.name)
        names.add(member.name + ("/" if member.isdir() and not member.name.endswith("/") else ""))
    if names != expected:
        raise SystemExit(f"unexpected bundle members: {sorted(names)!r}")
PY

package_source="$work_dir/source"
bundle_parent="$work_dir/bundle"
consumer_source="$work_dir/consumer"
mkdir -p "$package_source" "$bundle_parent" "$consumer_source"
if git -C "$project_root" archive HEAD | tar -tf - | grep -q '^transpiled/'; then
    printf '%s\n' 'The source archive unexpectedly contains transpiled/' >&2
    exit 1
fi
git -C "$project_root" archive HEAD | tar -x -C "$package_source"
tar -xzf "$work_dir/$archive_name" -C "$bundle_parent"
bundle_dir="$bundle_parent/$root_name"
cp -R "$package_source/tests/consumer/cmake_c/." "$consumer_source/"

version=$(python3 - "$bundle_dir/manifest.json" <<'PY'
import json, sys
with open(sys.argv[1], encoding="utf-8") as source:
    print(json.load(source)["project_version"])
PY
)
mismatch_bundle="$work_dir/mismatched-bundle"
cp -R "$bundle_dir" "$mismatch_bundle"
python3 - "$mismatch_bundle/manifest.json" <<'PY'
import json, sys
path = sys.argv[1]
with open(path, encoding="utf-8") as source:
    value = json.load(source)
value["project_version"] = "999.0.0"
with open(path, "w", encoding="utf-8") as output:
    json.dump(value, output)
PY
chmod -R a-w "$bundle_dir"

cache_dir=${GRPC_LITE_CMAKE_DEP_CACHE:-"$project_root/.zig-cache/cmake-dependency-archives"}
mkdir -p "$cache_dir"
download_and_verify() {
    local destination=$1
    local url=$2
    local digest=$3
    if [[ ! -f "$destination" ]] || ! printf '%s  %s\n' "$digest" "$destination" | sha256sum --check --status; then
        local temporary="$destination.tmp.$$"
        rm -f "$temporary"
        python3 - "$url" "$temporary" <<'PY'
import sys
import urllib.request
urllib.request.urlretrieve(sys.argv[1], sys.argv[2])
PY
        printf '%s  %s\n' "$digest" "$temporary" | sha256sum --check --status
        mv "$temporary" "$destination"
    fi
}
nghttp2_archive=${GRPC_LITE_NGHTTP2_ARCHIVE:-"$cache_dir/nghttp2-68cb6900.tar.gz"}
cares_archive=${GRPC_LITE_CARES_ARCHIVE:-"$cache_dir/c-ares-1.34.8.tar.gz"}
download_and_verify "$nghttp2_archive" \
    'https://codeload.github.com/nghttp2/nghttp2/tar.gz/68cb6900fde14c77f0cd7add0e094a862960eb99' \
    '3f042e5284ad349f837b65dd5be953b34c007a05259119092dbe6ebeaa73d306'
download_and_verify "$cares_archive" \
    'https://github.com/c-ares/c-ares/releases/download/v1.34.8/c-ares-1.34.8.tar.gz' \
    'c222b6d681096f9444d2c4863d2c1174019e27cacca0a4a5c114d36dd7d7bf78'

isolated_args=(--isolated "$work_dir" "$package_source" "$consumer_source" "$bundle_dir"
    "$mismatch_bundle" "$nghttp2_archive" "$cares_archive" "$version")
if [[ ${GRPC_LITE_NETWORK_ISOLATED:-} == 1 ]]; then
    "$0" "${isolated_args[@]}"
elif command -v docker >/dev/null 2>&1; then
    image=${GRPC_LITE_CMAKE_CONSUMER_IMAGE:-grpc-lite-cmake-consumer:ubuntu-24.04}
    if ! docker image inspect "$image" >/dev/null 2>&1; then
        docker build --tag "$image" - <<'DOCKERFILE'
FROM ubuntu:24.04
RUN apt-get update && DEBIAN_FRONTEND=noninteractive apt-get install -y --no-install-recommends \
    clang cmake ninja-build protobuf-compiler libprotobuf-dev ca-certificates && \
    rm -rf /var/lib/apt/lists/*
DOCKERFILE
    fi
    docker run --rm --network none --user "$(id -u):$(id -g)" \
        -v "$work_dir:$work_dir" -v "$nghttp2_archive:$nghttp2_archive:ro" \
        -v "$cares_archive:$cares_archive:ro" \
        "$image" "$work_dir/source/tests/consumer/run_cmake_c.sh" "${isolated_args[@]}"
else
    printf '%s\n' 'No usable network namespace or Docker engine is available' >&2
    exit 1
fi
