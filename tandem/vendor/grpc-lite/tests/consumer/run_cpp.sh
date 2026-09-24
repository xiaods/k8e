#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/grpc-lite-cpp-consumer.XXXXXX")
package_source="$work_dir/package"
consumer_source="$work_dir/consumer"
stage="$work_dir/stage"
server_pid=
cleanup() {
    local status=$?
    if [[ -n "$server_pid" ]]; then
        kill "$server_pid" 2>/dev/null || true
        for _ in {1..50}; do
            kill -0 "$server_pid" 2>/dev/null || break
            sleep 0.02
        done
        if kill -0 "$server_pid" 2>/dev/null; then
            kill -KILL "$server_pid" 2>/dev/null || true
        fi
        wait "$server_pid" 2>/dev/null || true
    fi
    if [[ $status -ne 0 && -f "$work_dir/server.log" ]]; then
        printf '%s\n' 'C++ E2E server log:' >&2
        while IFS= read -r line; do
            printf '%s\n' "$line" >&2
        done <"$work_dir/server.log"
    fi
    if [[ -n "${GRPC_LITE_KEEP_E2E:-}" ]]; then
        printf 'Kept C++ E2E work directory: %s\n' "$work_dir" >&2
    else
        rm -rf "$work_dir"
    fi
    return "$status"
}
trap cleanup EXIT

mkdir -p "$package_source" "$consumer_source"
git -C "$project_root" archive HEAD | tar -x -C "$package_source"
git -C "$project_root" diff --binary HEAD >"$work_dir/worktree.patch"
if [[ -s "$work_dir/worktree.patch" ]]; then
    git -C "$package_source" apply --whitespace=nowarn "$work_dir/worktree.patch"
fi
while IFS= read -r -d '' path; do
    mkdir -p "$package_source/$(dirname "$path")"
    cp -a "$project_root/$path" "$package_source/$path"
done < <(git -C "$project_root" ls-files --others --exclude-standard -z)
cp -R "$package_source/examples/cpp/." "$consumer_source/"

(
    cd "$package_source"
    zig build -Dprotobuf=true -Doptimize=ReleaseSafe --prefix "$stage"
    zig build -Dprotobuf=false -Doptimize=ReleaseSafe \
        install-cpp-e2e-server --prefix "$stage"
)
cmake \
    -S "$consumer_source" \
    -B "$work_dir/build" \
    -G Ninja \
    -DCMAKE_BUILD_TYPE=Release \
    -DCMAKE_PREFIX_PATH="$stage"
cmake --build "$work_dir/build"

: >"$work_dir/server.log"
"$stage/bin/grpc-lite-cpp-e2e-server" 0 >"$work_dir/server.log" 2>&1 &
server_pid=$!
port=
for _ in {1..100}; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
        wait "$server_pid" 2>/dev/null || true
        printf '%s\n' 'C++ E2E server exited before becoming ready' >&2
        exit 1
    fi
    log=$(<"$work_dir/server.log")
    if [[ "$log" =~ listening\ on\ 127\.0\.0\.1:([0-9]+) ]]; then
        port=${BASH_REMATCH[1]}
        break
    fi
    sleep 0.05
done
if [[ -z "$port" ]]; then
    printf '%s\n' 'timed out waiting for the C++ E2E server' >&2
    exit 1
fi

target="127.0.0.1:$port"
printf 'Running basic echo against %s\n' "$target"
timeout 15 "$work_dir/build/grpc_lite_cpp_basic_echo" "$target"
printf 'Running metadata and large-message echo against %s\n' "$target"
timeout 15 "$work_dir/build/grpc_lite_cpp_metadata_and_large" "$target"
printf 'Running status and deadline checks against %s\n' "$target"
timeout 15 "$work_dir/build/grpc_lite_cpp_errors_and_deadline" "$target"
