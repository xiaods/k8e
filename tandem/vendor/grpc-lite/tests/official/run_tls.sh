#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir="$project_root/.zig-cache/official"
zig_server_port=${ZIG_TLS_SERVER_PORT:-$((30000 + $$ % 10000))}
go_server_port=${GO_TLS_SERVER_PORT:-$((zig_server_port + 1))}
certificate="$project_root/src/testdata/localhost-cert.pem"
private_key="$project_root/src/testdata/localhost-key.pem"
cases=(empty_unary large_unary client_streaming server_streaming ping_pong empty_stream)
peer_pid=
peer_log=

stop_peer() {
  if [[ -n "$peer_pid" ]]; then
    kill "$peer_pid" 2>/dev/null || true
    wait "$peer_pid" 2>/dev/null || true
    peer_pid=
  fi
}

trap stop_peer EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

wait_for_peer() {
  local port=$1
  local attempt
  for attempt in {1..100}; do
    if ! kill -0 "$peer_pid" 2>/dev/null; then
      printf 'peer exited before accepting TLS connections\n' >&2
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
  printf 'timed out waiting for TLS peer on port %s\n' "$port" >&2
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
    cat "$peer_log" >&2
    return 1
  fi
}

mkdir -p "$work_dir"
zig build -Dtls=true
(cd "$project_root/tests/official" && \
  go build -mod=readonly -o "$work_dir/grpc-go-interop-client" google.golang.org/grpc/interop/client && \
  go build -mod=readonly -o "$work_dir/grpc-go-interop-server" google.golang.org/grpc/interop/server)

peer_log="$work_dir/grpc-lite-tls-server.log"
"$project_root/zig-out/bin/grpc-lite-interop-server" \
  --port="$zig_server_port" \
  --use_tls=true \
  --tls_cert_file="$certificate" \
  --tls_key_file="$private_key" >"$peer_log" 2>&1 &
peer_pid=$!
wait_for_peer "$zig_server_port"
for test_case in "${cases[@]}"; do
  run_case 'grpc-go client -> grpc-lite TLS server' "$test_case" \
    "$work_dir/grpc-go-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$zig_server_port" \
    --server_host_override=localhost \
    --test_case="$test_case" \
    --use_tls=true \
    --use_test_ca=true \
    --ca_file="$certificate"
done
stop_peer

peer_log="$work_dir/grpc-go-tls-server.log"
"$work_dir/grpc-go-interop-server" \
  --port="$go_server_port" \
  --use_tls=true \
  --tls_cert_file="$certificate" \
  --tls_key_file="$private_key" >"$peer_log" 2>&1 &
peer_pid=$!
wait_for_peer "$go_server_port"
for test_case in "${cases[@]}"; do
  run_case 'grpc-lite TLS client -> grpc-go server' "$test_case" \
    "$project_root/zig-out/bin/grpc-lite-interop-client" \
    --server_host=127.0.0.1 \
    --server_port="$go_server_port" \
    --test_case="$test_case" \
    --use_tls=true \
    --ca_file="$certificate"
done
