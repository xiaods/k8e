#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C
export TZ=UTC
umask 022

if [[ $# -ne 1 ]]; then
    printf 'usage: %s <dist-directory>\n' "$0" >&2
    exit 2
fi

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
dist_dir=$(mkdir -p "$1" && cd "$1" && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/grpc-lite-package-transpiled-c.XXXXXX")
cleanup() {
    local status=$?
    rm -rf "$work_dir"
    return "$status"
}
trap cleanup EXIT

read_single_version() {
    local description=$1
    local pattern=$2
    local file=$3
    local values
    values=$(sed -n "$pattern" "$file")
    if [[ $(printf '%s\n' "$values" | sed '/^$/d' | wc -l) -ne 1 ]]; then
        printf 'Expected exactly one %s in %s\n' "$description" "$file" >&2
        exit 1
    fi
    printf '%s\n' "$values"
}

zon_version=$(read_single_version 'project version' \
    's/^[[:space:]]*\.version = "\([^"]*\)",$/\1/p' "$project_root/build.zig.zon")
cmake_version=$(read_single_version 'CMake project version' \
    's/^project(grpc_lite VERSION \([^ ]*\) LANGUAGES C)$/\1/p' "$project_root/CMakeLists.txt")
config_version=$(read_single_version 'CMake package version' \
    's/^set(PACKAGE_VERSION "\([^"]*\)")$/\1/p' "$project_root/cmake/grpc_liteConfigVersion.cmake")
required_zig=$(read_single_version 'minimum Zig version' \
    's/^[[:space:]]*\.minimum_zig_version = "\([^"]*\)",$/\1/p' "$project_root/build.zig.zon")
actual_zig=$(zig version)

if [[ "$zon_version" != "$cmake_version" || "$zon_version" != "$config_version" ]]; then
    printf 'Version mismatch: build.zig.zon=%s CMakeLists.txt=%s ConfigVersion=%s\n' \
        "$zon_version" "$cmake_version" "$config_version" >&2
    exit 1
fi
if [[ "$actual_zig" != "$required_zig" ]]; then
    printf 'Zig version mismatch: expected %s, got %s\n' "$required_zig" "$actual_zig" >&2
    exit 1
fi

GRPC_LITE_ZIG_CACHE_DIR=${GRPC_LITE_ZIG_CACHE_DIR:-"$work_dir/zig-cache"} \
    GRPC_LITE_ZIG_GLOBAL_CACHE_DIR=${GRPC_LITE_ZIG_GLOBAL_CACHE_DIR:-"$work_dir/zig-global-cache"} \
    "$project_root/tools/transpile_c.sh" "$work_dir/generated"

expected_files=$'include/zig.h\nmanifest.json\nsrc/grpc_lite.c\nsrc/protoc-gen-grpc_lite_cpp.c'
actual_files=$(cd "$work_dir/generated" && find . -type f -print | sed 's|^\./||' | LC_ALL=C sort)
if [[ "$actual_files" != "$expected_files" ]]; then
    printf 'Unexpected generated bundle files:\n%s\n' "$actual_files" >&2
    exit 1
fi

python3 - "$work_dir/generated/manifest.json" "$zon_version" "$actual_zig" "$project_root" <<'PY'
import json
import subprocess
import sys
with open(sys.argv[1], encoding="utf-8") as source:
    value = json.load(source)
expected = {
    "schema_version": 1,
    "project_version": sys.argv[2],
    "source_commit": subprocess.check_output(
        ["git", "rev-parse", "HEAD"], cwd=sys.argv[4], text=True).strip(),
    "dirty": bool(subprocess.check_output(
        ["git", "status", "--porcelain"], cwd=sys.argv[4], text=True).strip()),
    "zig_version": sys.argv[3],
    "target": "x86_64-linux-gnu",
    "optimization": "ReleaseSafe",
    "tls": False,
}
if value != expected:
    raise SystemExit(f"manifest mismatch: expected {expected!r}, got {value!r}")
PY

root="grpc-lite-transpiled-c-$zon_version-linux-x86_64"
mkdir -p "$work_dir/stage/$root/include" "$work_dir/stage/$root/src"
cp "$work_dir/generated/manifest.json" "$work_dir/stage/$root/manifest.json"
cp "$work_dir/generated/include/zig.h" "$work_dir/stage/$root/include/zig.h"
cp "$work_dir/generated/src/"*.c "$work_dir/stage/$root/src/"
find "$work_dir/stage" -type d -exec chmod 0755 {} +
find "$work_dir/stage" -type f -exec chmod 0644 {} +

source_epoch=$(git -C "$project_root" show -s --format=%ct HEAD)
archive="$dist_dir/$root.tar.gz"
(
    cd "$work_dir/stage"
    tar --sort=name --format=ustar --mtime="@$source_epoch" \
        --owner=0 --group=0 --numeric-owner \
        --mode='u+rwX,go+rX,go-w' -cf - "$root"
) | gzip -n -9 >"$archive"
(
    cd "$dist_dir"
    sha256sum "$(basename "$archive")" >"$(basename "$archive").sha256"
    sha256sum --check "$(basename "$archive").sha256"
)
