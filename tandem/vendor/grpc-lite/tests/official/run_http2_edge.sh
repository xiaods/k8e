#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir="$project_root/.zig-cache/official"
grpc_commit=8542e01ff47eb07247ff6cfbd545f3b6f4e9b5d3
image=grpc-lite-http2-edge:${grpc_commit:0:8}-py311-v1
dockerfile="$project_root/third_party/grpc-http2-test/Dockerfile"
base_port=${HTTP2_EDGE_BASE_PORT:-$((40000 + $$ % 10000))}
container_name="grpc-lite-http2-edge-$$"
server_log="$work_dir/http2-edge-server.log"
server_pid=

cases=(
  data_frame_padding
  goaway
  max_streams
  no_df_padding_sanity_test
  ping
  rst_after_data
  rst_after_header
  rst_during_data
)

stop_server() {
  if [[ -n "$server_pid" ]]; then
    docker stop --time 2 "$container_name" >/dev/null 2>&1 || true
    wait "$server_pid" 2>/dev/null || true
    server_pid=
  fi
}

cleanup() {
  stop_server
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

mkdir -p "$work_dir"

printf '%s\n' 'Building grpc-lite HTTP/2 edge-case client...'
(cd "$project_root" && zig build)

printf '%s\n' 'Building vendored official gRPC HTTP/2 edge-case server...'
image_id=$(docker build --quiet --file "$dockerfile" --tag "$image" "$project_root")

printf '%s\n' 'Starting vendored official gRPC HTTP/2 edge-case server...'
docker run --rm \
  --name "$container_name" \
  --network host \
  "$image_id" \
  --host=127.0.0.1 \
  --base_port="$base_port" >"$server_log" 2>&1 &
server_pid=$!

for _ in {1..200}; do
  if (exec 3<>"/dev/tcp/127.0.0.1/$base_port") 2>/dev/null; then
    exec 3>&-
    exec 3<&-
    break
  fi
  if ! kill -0 "$server_pid" 2>/dev/null; then
    cat "$server_log" >&2
    exit 1
  fi
  sleep 0.05
done

for index in "${!cases[@]}"; do
  test_case=${cases[$index]}
  port=$((base_port + index))
  printf '[ RUN  ] %-26s official HTTP/2 edge-case server\n' "$test_case"
  if "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$port" \
    --test_case="$test_case" \
    --use_tls=false; then
    printf '[ PASS ] %-26s official HTTP/2 edge-case server\n' "$test_case"
  else
    printf '[ FAIL ] %-26s official HTTP/2 edge-case server\n' "$test_case" >&2
    cat "$server_log" >&2
    exit 1
  fi
done

stop_server
if grep -Eq 'AssertionError|Unhandled Error' "$server_log"; then
  printf '%s\n' 'official HTTP/2 edge-case server reported an assertion failure' >&2
  cat "$server_log" >&2
  exit 1
fi
printf '%s\n' 'All 8 official HTTP/2 edge-case server runs passed.'
