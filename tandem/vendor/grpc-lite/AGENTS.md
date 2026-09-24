# grpc-lite Developer Guide

## Toolchain

- Latest stable Zig release through mise
- CMake and Ninja for upstream C dependencies
- mise for tool versions and project tasks

Run `mise install` followed by `mise run bootstrap` before the first build.

Before every push, run `mise run check` and require it to pass.

## Commands

```bash
mise run build
mise run test
mise run test-release-safe
mise run test-tsan
mise run test-ubsan
mise run test-consumer
mise run test-consumer-cpp
mise run prepare-gperftools
mise run build-gperftools
mise run test-gperftools
mise run fmt
mise run ci-lint
mise run check
mise run interop
mise run interop-official
mise run interop-tls
mise run interop-http2
mise run interop-http2-edge
mise run bench
mise run bench-all
mise run bench-client
mise run bench-server
mise run gen-proto
mise run gen-grpcpp
```

## Architecture

- `src/root.zig` is the public module entry point.
- `src/protobuf_adapter.zig` is the optional typed zig-protobuf integration.
- Public APIs use explicit allocators and deterministic `deinit` methods.
- `nghttp2` owns HTTP/2 framing, HPACK, stream state, and flow control.
- `libxev` owns socket and event-loop integration.
- gRPC payloads remain raw protobuf wire bytes.
- Generated protobuf sources live under `.zig-cache` and are not committed.
- The native Zig transport supports raw unary and full-duplex streaming over cleartext or TLS IPv4.
- The insecure Linux x86_64 transpiled-C release bundle supports cleartext transport only.
- c-ares resolves client hostnames to IPv4 candidates on the libxev loop.

Keep C types private to transport modules. Keep protobuf out of `src/root.zig` so the
raw transport remains independently usable. The optional C++ compatibility layer must
remain outside the raw transport and depend only on its public API or a stable C ABI.

## Scope Decisions

The compatibility target is `grpc-lite-streaming-insecure-v2`. This table is
authoritative: change it before implementing a feature with a different decision.

| Capability | Decision | Notes |
| --- | --- | --- |
| Raw unary RPC | Required | Client and server |
| Typed unary RPC | Required | Follows the raw unary transport |
| Raw client streaming | Required | Explicit client half-close |
| Raw server streaming | Required | Bounded response backpressure |
| Raw bidirectional streaming | Required | Independent full-duplex message flow |
| Typed streaming | Selected | Event-driven zig-protobuf adapters cover all streaming cardinalities |
| Official insecure unary and streaming interop profile | Required | Test against official grpc-go peers |
| Large messages and HTTP/2 flow control | Required | Include padding and max-stream tests |
| ASCII and binary metadata | Required | Emit padded base64; accept padded, unpadded, and comma-joined `-bin` values |
| Status codes and Unicode status messages | Required | Preserve percent-encoded UTF-8 bytes |
| Deadlines | Required | Explicit public cancellation is streaming-only |
| Per-message compression | Selected | Only `identity` and `gzip` |
| GOAWAY connection replacement | Selected | New calls use a new connection; no RPC retry |
| Graceful server drain | Selected | Stop admission, send GOAWAY, wait with timeout |
| Server resource limits | Selected | Aggregate connection cap, per-connection stream cap, absolute cleartext preface timeout, and idle timeout only while no inbound RPC streams are active |
| Native Zig TLS and ALPN | Selected | Optional mbedTLS; TLS 1.2+, mandatory `h2`, explicit PEM credentials |
| Transpiled-C release bundle TLS | Out of scope | The insecure Linux x86_64 bundle remains cleartext-only until TLS is separately selected |
| mTLS | Out of scope | Client certificates and server-side client verification are not required |
| DNS | Selected | c-ares on Linux io_uring; client IPv4 hostnames only |
| IPv6 | Out of scope | No AAAA, Happy Eyeballs, or IPv6 listeners |
| Reflection and health services | Out of scope | Not part of the lite profile |
| Interceptors and middleware | Out of scope | Keep the core API small |
| Connection recovery and backoff | Selected | Channels reconnect without replaying RPCs |
| Retry policies | Out of scope | Applications retry explicitly |
| Service config and load balancing | Out of scope | No policy layer |
| xDS, ORCA, ALTS, and cloud credentials | Out of scope | Not required for lite deployments |
| Cacheable unary GET | Out of scope | Depends on proxy cache semantics |
| grpcpp-shaped synchronous C++ API | Selected | Regenerated unary and all streaming cardinalities; source-compatible common sync API with no ABI compatibility |
| Official Protobuf C++ runtime integration | Selected | Optional external dependency for typed C++ code; keep it out of the raw transport |
| General Abseil API compatibility | Out of scope | Do not install substitute `absl/*` headers; direct application use remains an application dependency |
| Full grpcpp compatibility | Out of scope | No official generated glue, CompletionQueue, callback/reactor, generic services, or grpc-core ABI |

## Raw Streaming Direction

Raw streaming APIs are planned as all-event-driven while implementation is ongoing:

- Application callbacks run on transport loop threads and must not block.
- Half-close ends only the local send direction. Explicit cancellation terminates both
  directions and is exposed only for streaming calls.
- `ServerStream` is callback-borrowed; retained cross-thread server work uses an owning
  `ServerCall` and releases it before Server teardown.
- Public objects use explicit allocators, clear ownership, and deterministic `deinit`.
- Bounded queues and HTTP/2 flow control provide backpressure for each direction.
- Compression is selected per message, with only `identity` and `gzip` supported.

## Official Interop Profile

The v2 target requires `empty_unary`, `large_unary`,
`special_status_message`, `unimplemented_method`, `unimplemented_service`,
`client_compressed_unary`, `server_compressed_unary`, `client_streaming`,
`server_streaming`, `ping_pong`, and `empty_stream`. Also run `rpc_soak`, `channel_soak`,
the public HTTP/2 framing suite, and the negative HTTP/2 client cases.

The official framing harness requires the TLS application-protocol, minimum-version, and
bad-cipher-suite cases in addition to the insecure framing profile. Cases requiring auth,
service config, ORCA, or xDS are skipped
with the scope-table reason. grpc-core `bad_client` tests use private C APIs and are not
part of this profile; use the public HTTP/2 interop tools instead.

## Style

Use `zig fmt`. Prefer small modules, explicit ownership, and no detached threads.
