#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/.." && pwd)
cd "$project_root"

zig build
port=50091
./zig-out/bin/grpc-lite-echo-server "$port" >.zig-cache/interop-server.log 2>&1 &
server_pid=$!
trap 'kill "$server_pid" 2>/dev/null || true; wait "$server_pid" 2>/dev/null || true' EXIT

attempt=0
until output=$(grpcurl -plaintext -import-path proto -proto echo.proto \
  -d '{"message":"hello grpc-lite"}' \
  "127.0.0.1:$port" demo.EchoService/Echo 2>/dev/null); do
  attempt=$((attempt + 1))
  if [[ "$attempt" -ge 30 ]]; then
    printf '%s\n' "grpcurl interoperability test failed" >&2
    exit 1
  fi
  sleep 0.1
done

case "$output" in
  *'"message": "hello grpc-lite"'*) ;;
  *)
    printf '%s\n' "$output" >&2
    exit 1
    ;;
esac
