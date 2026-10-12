#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir="$project_root/.zig-cache/official"
zig_server_port=${ZIG_SERVER_PORT:-$((20000 + $$ % 10000))}
go_server_port=${GO_SERVER_PORT:-$((zig_server_port + 1))}
cpp_server_port=${CPP_SERVER_PORT:-$((go_server_port + 1))}
soak_iterations=${SOAK_ITERATIONS:-10}
soak_max_failures=${SOAK_MAX_FAILURES:-0}
soak_overall_timeout_seconds=${SOAK_OVERALL_TIMEOUT_SECONDS:-10}
official_cases=(
  empty_unary
  large_unary
  special_status_message
  unimplemented_method
  unimplemented_service
)
streaming_cases=(
  client_streaming
  server_streaming
  ping_pong
  empty_stream
)
stream_lifecycle_cases=(
  cancel_after_begin
  cancel_after_first_response
  timeout_on_sleeping_server
)
soak_cases=(
  rpc_soak
  channel_soak
)
compression_cases=(
  client_compressed_unary
  server_compressed_unary
)
grpc_go_gzip_cases=(
  identity_unary
  gzip_request
  gzip_response
  identity_expectation_error
  gzip_expectation_error
  unsupported_request_encoding
)

peer_pid=
peer_log=

stop_peer() {
  if [[ -n "$peer_pid" ]]; then
    kill "$peer_pid" 2>/dev/null || true
    wait "$peer_pid" 2>/dev/null || true
    peer_pid=
  fi
}

cleanup() {
  stop_peer
}
trap cleanup EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_peer() {
  local port=$1
  local attempt
  for attempt in {1..100}; do
    if ! kill -0 "$peer_pid" 2>/dev/null; then
      printf 'peer exited before accepting connections\n' >&2
      printf '%s\n' '--- peer log ---' >&2
      cat "$peer_log" >&2
      return 1
    fi
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      exec 3>&-
      exec 3<&-
      return 0
    fi
    sleep 0.05
  done
  printf 'timed out waiting for peer on port %s\n' "$port" >&2
  printf '%s\n' '--- peer log ---' >&2
  cat "$peer_log" >&2
  return 1
}

run_case() {
  local direction=$1
  local test_case=$2
  shift 2
  printf '[ RUN  ] %-24s %s\n' "$test_case" "$direction"
  if "$@"; then
    printf '[ PASS ] %-24s %s\n' "$test_case" "$direction"
  else
    printf '[ FAIL ] %-24s %s\n' "$test_case" "$direction" >&2
    printf '%s\n' '--- peer log ---' >&2
    cat "$peer_log" >&2
    return 1
  fi
}

mkdir -p "$work_dir"

printf '%s\n' 'Building grpc-lite interop binaries...'
(cd "$project_root" && zig build)

printf '%s\n' 'Building grpc-go v1.82.1 interop binaries...'
(cd "$project_root/tests/official" && \
  go build -mod=readonly -o "$work_dir/grpc-go-interop-client" google.golang.org/grpc/interop/client && \
  go build -mod=readonly -o "$work_dir/grpc-go-interop-server" google.golang.org/grpc/interop/server && \
  go build -mod=readonly -o "$work_dir/grpc-go-half-duplex-client" half_duplex_client.go && \
  go build -mod=readonly -o "$work_dir/grpc-go-gzip-client" ./gzip_client && \
  go build -mod=readonly -o "$work_dir/grpc-go-gzip-server" ./gzip_server)

printf '%s\n' 'Building generated C++ interop binaries...'
rm -rf "$work_dir/cpp-proto-stage" "$work_dir/cpp-build"
(cd "$project_root" && \
  zig build install-cpp-interop-protos --prefix "$work_dir/cpp-proto-stage")
cmake \
  -S "$project_root/tests/official/cpp" \
  -B "$work_dir/cpp-build" \
  -G Ninja \
  -DCMAKE_BUILD_TYPE=Release \
  -DCMAKE_PREFIX_PATH="$project_root/zig-out" \
  -DGRPC_LITE_INTEROP_PROTO_ROOT="$work_dir/cpp-proto-stage/share/grpc-lite/interop-proto"
cmake --build "$work_dir/cpp-build"

peer_log="$work_dir/grpc-lite-server.log"
"$project_root/zig-out/bin/grpc-lite-interop-server" \
  --port="$zig_server_port" --use_tls=false >"$peer_log" 2>&1 &
peer_pid=$!
wait_for_peer "$zig_server_port"
for test_case in "${official_cases[@]}"; do
  run_case 'grpc-go client -> grpc-lite server' "$test_case" \
    "$work_dir/grpc-go-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$zig_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
for test_case in "${streaming_cases[@]}"; do
  run_case 'grpc-go client -> grpc-lite server' "$test_case" \
    "$work_dir/grpc-go-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$zig_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
run_case 'grpc-go client -> grpc-lite server' 'half_duplex' \
  "$work_dir/grpc-go-half-duplex-client" \
  --server_host=127.0.0.1 \
  --server_port="$zig_server_port"
for test_case in "${stream_lifecycle_cases[@]}"; do
  run_case 'grpc-go client -> grpc-lite server' "$test_case" \
    "$work_dir/grpc-go-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$zig_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
for test_case in "${grpc_go_gzip_cases[@]}"; do
  run_case 'grpc-go gzip client -> grpc-lite server' "$test_case" \
    "$work_dir/grpc-go-gzip-client" \
    --server_host=127.0.0.1 \
    --server_port="$zig_server_port" \
    --test_case="$test_case"
done
for test_case in "${soak_cases[@]}"; do
  run_case 'grpc-go client -> grpc-lite server' "$test_case" \
    "$work_dir/grpc-go-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$zig_server_port" \
    --test_case="$test_case" \
    --soak_iterations="$soak_iterations" \
    --soak_max_failures="$soak_max_failures" \
    --soak_overall_timeout_seconds="$soak_overall_timeout_seconds" \
    --use_tls=false
done
stop_peer

peer_log="$work_dir/grpc-lite-cpp-server.log"
"$work_dir/cpp-build/grpc-lite-cpp-interop-server" \
  --port="$cpp_server_port" --use_tls=false >"$peer_log" 2>&1 &
peer_pid=$!
wait_for_peer "$cpp_server_port"
for test_case in "${streaming_cases[@]}"; do
  run_case 'grpc-go client -> generated C++ server' "$test_case" \
    "$work_dir/grpc-go-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$cpp_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
run_case 'grpc-go client -> generated C++ server' 'half_duplex' \
  "$work_dir/grpc-go-half-duplex-client" \
  --server_host=127.0.0.1 \
  --server_port="$cpp_server_port"
stop_peer

peer_log="$work_dir/grpc-go-server.log"
"$work_dir/grpc-go-interop-server" \
  --port="$go_server_port" --use_tls=false >"$peer_log" 2>&1 &
peer_pid=$!
wait_for_peer "$go_server_port"
for test_case in "${official_cases[@]}"; do
  run_case 'grpc-lite client -> grpc-go server' "$test_case" \
    "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
for test_case in "${streaming_cases[@]}"; do
  run_case 'grpc-lite client -> grpc-go server' "$test_case" \
    "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
for test_case in "${stream_lifecycle_cases[@]}"; do
  run_case 'grpc-lite client -> grpc-go server' "$test_case" \
    "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
for test_case in "${soak_cases[@]}"; do
  run_case 'grpc-lite client -> grpc-go server' "$test_case" \
    "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --soak_iterations="$soak_iterations" \
    --soak_max_failures="$soak_max_failures" \
    --soak_overall_timeout_seconds="$soak_overall_timeout_seconds" \
    --use_tls=false
done
for test_case in "${streaming_cases[@]}"; do
  run_case 'generated C++ client -> grpc-go server' "$test_case" \
    "$work_dir/cpp-build/grpc-lite-cpp-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
stop_peer

peer_log="$work_dir/grpc-go-gzip-server.log"
"$work_dir/grpc-go-gzip-server" --port="$go_server_port" >"$peer_log" 2>&1 &
peer_pid=$!
wait_for_peer "$go_server_port"
for test_case in "${compression_cases[@]}"; do
  run_case 'grpc-lite client -> grpc-go gzip server' "$test_case" \
    "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --use_tls=false
done
stop_peer

printf '%s\n' "All 10 bidirectional unary runs passed."
printf '%s\n' "All 8 raw bidirectional streaming runs passed."
printf '%s\n' "All 8 generated C++ streaming interop runs passed."
printf '%s\n' "Both raw and generated C++ half-duplex server runs passed."
printf '%s\n' "All 6 bidirectional streaming cancellation and deadline runs passed."
printf '%s\n' "All 4 bidirectional soak runs passed: rpc_soak and channel_soak in each direction ($soak_iterations iteration(s) each)."
printf '%s\n' 'All 6 grpc-go client to grpc-lite gzip negotiation and error runs passed.'
printf '%s\n' 'Both grpc-lite client to grpc-go gzip interop runs passed.'
