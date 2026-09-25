#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir="$project_root/.zig-cache/official"
cleartext_port=${HTTP2_SERVER_PORT:-$((30000 + $$ % 10000))}
tls_port=${HTTP2_TLS_SERVER_PORT:-$((cleartext_port + 1))}
certificate="$project_root/src/testdata/localhost-cert.pem"
private_key="$project_root/src/testdata/localhost-key.pem"
harness_dir="$work_dir/http2_interop"
server_pid=

stop_server() {
  if [[ -n "$server_pid" ]]; then
    kill "$server_pid" 2>/dev/null || true
    wait "$server_pid" 2>/dev/null || true
    server_pid=
  fi
}

cleanup() {
  stop_server
}
trap cleanup EXIT

start_server() {
  local port=$1
  local use_tls=$2
  local log_file=$3
  local args=(--port="$port" --use_tls="$use_tls")
  if [[ "$use_tls" == true ]]; then
    args+=(--tls_cert_file="$certificate" --tls_key_file="$private_key")
  fi

  "$project_root/zig-out/bin/grpc-lite-interop-server" "${args[@]}" >"$log_file" 2>&1 &
  server_pid=$!

  local attempt
  for attempt in {1..100}; do
    if ! kill -0 "$server_pid" 2>/dev/null; then
      cat "$log_file" >&2
      return 1
    fi
    if (exec 3<>"/dev/tcp/127.0.0.1/$port") 2>/dev/null; then
      exec 3>&-
      exec 3<&-
      return 0
    fi
    sleep 0.05
  done
  printf 'timed out waiting for HTTP/2 server on port %s\n' "$port" >&2
  cat "$log_file" >&2
  return 1
}

mkdir -p "$work_dir"
(cd "$project_root" && zig build -Dtls=true)

# The pinned official module predates Go's current TLS error text and TLS 1.3
# CipherSuites behavior. Resolve and validate it before applying narrow patches.
http2_module=github.com/grpc/grpc/tools/http2_interop
module_dir=$(cd "$project_root/tests/official" && \
  go mod download -json "$http2_module" | \
  python3 "$project_root/tests/official/resolve_http2_module.py")
rm -rf "$harness_dir"
mkdir -p "$harness_dir"
cp -R "$module_dir"/. "$harness_dir"
chmod -R u+w "$harness_dir"
patch -d "$harness_dir" -p1 --fuzz=0 --forward --batch \
  <"$project_root/tests/official/http2_interop_go1.26.patch"
patch -d "$harness_dir" -p1 --fuzz=0 --forward --batch \
  <"$project_root/tests/official/http2_interop_goaway.patch"
mkdir -p "$harness_dir/src/core/tsi/test_creds"
cp "$certificate" "$harness_dir/src/core/tsi/test_creds/ca.pem"

output_file="$work_dir/http2-framing.log"
: >"$output_file"

start_server "$cleartext_port" false "$work_dir/http2-server.log"
printf '%s\n' '[ RUN  ] official gRPC HTTP/2 framing suite'
(cd "$harness_dir" && \
  go test -mod=readonly -count=1 -v . --args \
    -server_host=127.0.0.1 \
    -server_port="$cleartext_port" \
    -use_tls=false \
    -test_case=framing) 2>&1 | tee -a "$output_file"
stop_server

start_server "$tls_port" true "$work_dir/http2-tls-server.log"
printf '%s\n' '[ RUN  ] official gRPC HTTP/2 TLS profile suite'
(cd "$harness_dir" && \
  go test -mod=readonly -count=1 -v . --args \
    -server_host=127.0.0.1 \
    -server_port="$tls_port" \
    -server_host_override=localhost \
    -use_tls=true \
    -use_test_ca=true \
    -test_case=tls) 2>&1 | tee -a "$output_file"

python3 "$project_root/tests/official/validate_http2_report.py" "$output_file"
printf '%s\n' '[ COMPLETE ] official gRPC HTTP/2 framing and TLS profile reports'
