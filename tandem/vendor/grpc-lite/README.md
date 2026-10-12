# grpc-lite

[![CI](https://github.com/fanyang89/grpc-lite/actions/workflows/ci.yml/badge.svg)](https://github.com/fanyang89/grpc-lite/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/fanyang89/grpc-lite/graph/badge.svg)](https://codecov.io/gh/fanyang89/grpc-lite)
[![Release](https://img.shields.io/github/v/release/fanyang89/grpc-lite)](https://github.com/fanyang89/grpc-lite/releases)
[![Zig](https://img.shields.io/badge/Zig-stable-f7a41d?logo=zig&logoColor=white)](https://ziglang.org/)
[![License](https://img.shields.io/github/license/fanyang89/grpc-lite)](LICENSE)

A lightweight, streaming-first gRPC runtime for Zig with stable C and focused C++ APIs.

grpc-lite delegates HTTP/2 framing, HPACK, stream state, and flow control to nghttp2;
libxev owns sockets and event loops. The transport keeps protobuf optional and exposes raw
wire bytes, explicit allocators, deterministic teardown, and bounded backpressure.

## Highlights

| Transport | Runtime | Integration |
| --- | --- | --- |
| Unary and full-duplex streaming | Persistent multiplexed channels | Raw Zig and stable C APIs |
| Cleartext HTTP/2 and optional native Zig TLS | IPv4 DNS through c-ares | Typed zig-protobuf adapters |
| Deadlines and cancellation | GOAWAY replacement and reconnect | Focused grpcpp-shaped C++ facade |
| ASCII and binary metadata | Graceful server drain | CMake builds from release-generated C without Zig |
| Per-message identity or gzip | Multi-reactor Linux servers | Generated C++ service glue |

## Quick Start

### Zig

Add grpc-lite to `build.zig.zon`, then import its public module:

```zig
const grpc_lite = b.dependency("grpc_lite", .{
    .target = target,
    .optimize = optimize,
    .protobuf = false,
});
exe.root_module.addImport("grpc_lite", grpc_lite.module("grpc_lite"));
```

The raw module does not import zig-protobuf. Package consumers default to protobuf support
being disabled; setting `.protobuf = false` explicitly guarantees that zig-protobuf and
its host code-generation dependencies are not resolved. Set `.protobuf = true` only for
the separate typed adapter and code-generation modules.

```zig
const grpc = @import("grpc_lite");

var channel = try grpc.Channel.init(allocator, "127.0.0.1:50051", .{});
defer channel.deinit();

var result = try channel.callUnary(
    allocator,
    "/demo.EchoService/Echo",
    protobuf_wire_bytes,
    .{},
);
defer result.deinit();
```

### C and C++

Git checkouts and GitHub's automatic “Source code” archives intentionally omit generated
C. To build with CMake without Zig, download the same-version
`grpc-lite-transpiled-c-0.4.0-linux-x86_64.tar.gz` release asset and its checksum, verify
and extract it, then configure the source tree with
`-DGRPC_LITE_TRANSPILED_C_DIR=<extracted-root>`.

The generated-only asset supports insecure Linux x86_64 and contains no third-party
dependencies. TLS is out of scope for this transpiled-C release bundle until separately
selected; the CMake source build rejects `GRPC_LITE_ENABLE_TLS=ON`. This bundle decision
does not affect optional TLS in the native Zig build. A fully offline configure must also
provide hashed local `file://` archives for nghttp2 and c-ares and set
`GRPC_LITE_NO_NETWORK=ON`. `sha256sum --check` detects corruption, but a checksum hosted
beside an asset is not by itself publisher authentication.

Typed generated C++ services keep the official Protobuf runtime as an explicit external
dependency; the raw transport and grpcpp-shaped facade do not require Protobuf or
Abseil. See [the C and C++ API guide](docs/c-cpp-api.md) for extraction, offline CMake,
and code-generation details. The native Zig build supports Linux x86_64 and arm64, with
TLS available through an optional mbedTLS build.

## Compatibility

The `grpc-lite-streaming-insecure-v2` profile covers raw unary, client-streaming,
server-streaming, and bidirectional streaming. It is continuously tested against
official grpc-go peers and public HTTP/2 framing suites on x86_64 and arm64.

| Capability | Status |
| --- | --- |
| Raw unary and all streaming cardinalities | Supported |
| Typed zig-protobuf unary and streaming | Supported |
| Metadata, deadlines, gzip, GOAWAY, drain | Supported |
| Native Zig TLS 1.2+, ALPN `h2`, explicit PEM credentials | Optional |
| Transpiled-C release bundle TLS | Out of scope |
| grpcpp-shaped synchronous common API | Selected subset |
| Official Protobuf C++ runtime | Optional external typed-code dependency |
| General Abseil compatibility | Out of scope |
| IPv6, mTLS, retries, reflection, xDS | Out of scope |

See [the official interoperability profile](tests/official/README.md) for current cases
and results. grpc-lite does not provide grpc-core ABI compatibility or the full grpcpp
CompletionQueue, callback/reactor, generic service, or policy stack.

## Documentation

- [Zig API](docs/zig-api.md): runtime, channels, streaming, servers, TLS, and protobuf
- [C and C++ APIs](docs/c-cpp-api.md): stable ABI, source builds, grpcpp facade, and codegen
- [Development](docs/development.md): toolchain, CI, profiling, benchmarks, and dependencies
- [Official interop](tests/official/README.md): required gRPC and HTTP/2 compatibility matrix

Public types re-exported by `src/root.zig`, the installed C ABI, and installed C++
headers are the supported API surfaces. Lower-level Zig modules may change before 1.0.

## Development

```bash
mise install
mise run bootstrap
mise run check
```

See the [development guide](docs/development.md) for individual tasks and build options.

## License

[MIT](LICENSE)
