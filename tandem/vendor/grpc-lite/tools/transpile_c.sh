#!/usr/bin/env bash
set -euo pipefail

export LC_ALL=C
export TZ=UTC
umask 022

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
output_dir=${1:-"$project_root/transpiled"}

if [[ $# -gt 1 ]]; then
    printf 'usage: %s [output-directory]\n' "$0" >&2
    exit 2
fi

version=$(sed -n 's/^[[:space:]]*\.version = "\([^"]*\)",$/\1/p' "$project_root/build.zig.zon")
required_zig=$(sed -n 's/^[[:space:]]*\.minimum_zig_version = "\([^"]*\)",$/\1/p' "$project_root/build.zig.zon")
if [[ -z "$version" || -z "$required_zig" ]]; then
    printf '%s\n' 'Could not read project or Zig version from build.zig.zon' >&2
    exit 1
fi
actual_zig=$(zig version)
if [[ "$actual_zig" != "$required_zig" ]]; then
    printf 'Zig version mismatch: expected %s, got %s\n' "$required_zig" "$actual_zig" >&2
    exit 1
fi

for command_name in python3 taskset; do
    if ! command -v "$command_name" >/dev/null 2>&1; then
        printf 'Required command not found: %s\n' "$command_name" >&2
        exit 1
    fi
done
selected_cpu=$(python3 - <<'PY'
import os
import sys

try:
    allowed_cpus = os.sched_getaffinity(0)
except (AttributeError, OSError) as error:
    print(f"Could not query CPU affinity: {error}", file=sys.stderr)
    raise SystemExit(1)
if not allowed_cpus:
    print("Could not select a CPU: affinity set is empty", file=sys.stderr)
    raise SystemExit(1)
print(min(allowed_cpus))
PY
)

cache_dir=${GRPC_LITE_ZIG_CACHE_DIR:-"$project_root/.zig-cache/transpile-c"}
global_cache_dir=${GRPC_LITE_ZIG_GLOBAL_CACHE_DIR:-"$project_root/.zig-cache/transpile-c-global"}
rm -rf "$output_dir" "$cache_dir"
mkdir -p "$output_dir" "$cache_dir" "$global_cache_dir"
(
    cd "$project_root"
    taskset -c "$selected_cpu" zig build transpile-c -j1 \
        -Dtarget=x86_64-linux-gnu \
        -Dtranspile-c=true \
        -Dprotobuf=true \
        -Doptimize=ReleaseSafe \
        --seed 0 \
        --cache-dir "$cache_dir" \
        --global-cache-dir "$global_cache_dir" \
        --prefix "$output_dir" \
        --summary all
)

# Zig's x86 clone assembly uses LLVM-style comments, which GNU as rejects.
perl -pi -e 's{// SYS_}{# SYS_}g' "$output_dir/src/protoc-gen-grpc_lite_cpp.c"

# Zig 0.16 emits unstable numeric suffixes for these otherwise identical tuple
# types. Canonicalize the C identifiers so clean transpilation is reproducible.
perl -pi -e 's{\b(tuple_2_[A-Za-z0-9_]*_u1)_\d+\b}{${1}_0}g' "$output_dir/src/"*.c

commit=$(git -C "$project_root" rev-parse HEAD)
if [[ -n "$(git -C "$project_root" status --porcelain)" ]]; then
    dirty=true
else
    dirty=false
fi
cat >"$output_dir/manifest.json" <<EOF
{
  "schema_version": 1,
  "project_version": "$version",
  "source_commit": "$commit",
  "dirty": $dirty,
  "zig_version": "$actual_zig",
  "target": "x86_64-linux-gnu",
  "optimization": "ReleaseSafe",
  "tls": false
}
EOF

expected_files=$'include/zig.h\nmanifest.json\nsrc/grpc_lite.c\nsrc/protoc-gen-grpc_lite_cpp.c'
actual_files=$(cd "$output_dir" && find . -type f -print | sed 's|^\./||' | LC_ALL=C sort)
if [[ "$actual_files" != "$expected_files" ]]; then
    printf 'Unexpected transpiled C output in %s:\n%s\n' "$output_dir" "$actual_files" >&2
    exit 1
fi
