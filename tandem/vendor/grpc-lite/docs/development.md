# Development

## Toolchain

```bash
mise install
mise run bootstrap
```

The repository pins Zig and supporting tools through mise. CMake and Ninja build pinned
upstream C dependencies.

## Validation

Run the complete local release gate before pushing:

```bash
mise run check
```

It covers formatting, workflow lint, Debug and ReleaseSafe builds, package consumers,
ThreadSanitizer, C undefined behavior detection, TLS, gperftools, official gRPC interop,
and public HTTP/2 framing suites.

Useful focused tasks:

```bash
mise run build
mise run test
mise run test-release-safe
mise run coverage
mise run test-tsan
mise run test-ubsan
mise run test-tls
mise run test-tls-tsan
mise run test-consumer
mise run test-consumer-cmake-c
mise run test-consumer-cpp
mise run transpile-c
mise run package-transpiled-c
mise run check-transpiled-c
mise run prepare-network-deps
mise run prepare-tls-deps
mise run prepare-gperftools
mise run build-gperftools
mise run test-gperftools
mise run fmt
mise run ci-lint
mise run interop
mise run interop-official
mise run interop-tls
mise run interop-http2
mise run interop-http2-edge
mise run gen-proto
mise run gen-grpcpp
```

CI runs core tests on Linux x86_64 and arm64 in Debug and ReleaseSafe modes. Required
x86_64 jobs instrument Zig, libxev, and nghttp2 with ThreadSanitizer and C undefined
behavior detection. Runtime interop and vendored HTTP/2 edge cases run on both
architectures. A scheduled x86_64 workflow runs extended unary soak tests.

The authoritative compatibility matrix lives in
[the official interop guide](../tests/official/README.md).

## Transpiled C Sources

`mise run transpile-c` emits the runtime and C++ protoc plugin through Zig's C backend,
vendors the matching `zig.h`, writes a manifest, and applies deterministic compatibility
transforms. The ignored `transpiled/` directory is for local use and is never committed.

`mise run check-transpiled-c` generates and packages twice with independent caches,
compares the resulting archives byte for byte, verifies the SHA-256 sidecar, and tests
that neither tracked files nor `git archive` contain `transpiled/`. The generated-only
release asset is named `grpc-lite-transpiled-c-<version>-linux-x86_64.tar.gz` and contains
only the manifest, two C translation units, and `zig.h`.

CI consumes the exact archive in a separate environment where Zig is absent and the
network is disabled. Git checkouts and GitHub automatic “Source code” archives do not
support no-Zig CMake by themselves: consumers must extract the same-version asset and
pass `GRPC_LITE_TRANSPILED_C_DIR`. The asset is insecure Linux x86_64 only and includes
no nghttp2 or c-ares sources. TLS is out of scope for this transpiled-C release bundle
until separately selected, so CMake rejects `GRPC_LITE_ENABLE_TLS=ON`. Native Zig TLS
remains selected and is validated by the separate TLS tasks above. Fully offline builds
must stage those dependencies as hashed `file://` archives and set
`GRPC_LITE_NO_NETWORK=ON`. A colocated SHA-256 sidecar detects corruption but is not, by
itself, publisher authentication.

## Coverage

`mise run coverage` builds the core Debug test binary with the LLVM backend and runs it
through kcov. The task uses `kcov` on `PATH` by default, or the Docker image named by
`KCOV_IMAGE`, and writes a non-empty Cobertura report below `coverage/`. The dedicated
GitHub Actions workflow uploads that report to Codecov using OIDC authentication.

## Gperftools

Linux builds can optionally link the pinned gperftools fork for tcmalloc, CPU and heap
profiling, and guarded allocation sampling:

```bash
mise run prepare-gperftools
mise run build-gperftools -- -Doptimize=ReleaseFast
mise run test-gperftools
```

Downstream Zig packages pass `.gperftools = true` and import
`grpc_lite_gperftools`. For compatibility, this also replaces the final process C
allocator with tcmalloc; `perf.allocator` exposes the same allocator to Zig APIs.
Pass `.tcmalloc = false` to retain CPU profiling without replacing the allocator, or
pass `.tcmalloc = true, .gperftools = false` to enable only minimal tcmalloc. Heap
profiling and guarded allocation sampling require both options; the base MallocExtension
APIs require tcmalloc.

```zig
const grpc = @import("grpc_lite");
const perf = @import("grpc_lite_gperftools");

try perf.startCpuProfiler("/tmp/server.prof");
defer perf.stopCpuProfiler();

var server = try grpc.Server.init(perf.allocator, .{
    .host = "127.0.0.1",
    .port = 50051,
});
```

CPU and heap profilers are process-global and may also use `CPUPROFILE` and `HEAPPROFILE`.
Tcmalloc cannot be combined with ThreadSanitizer. Both options are Linux-only.

## Benchmarks

The cross-process E2E harness builds dedicated ReleaseFast client and server binaries. It
measures completed exchanges after warmup rather than local send-queue admission:

```bash
mise run bench -- --scenario bidi-ping-pong --transport typed
mise run bench -- --reactors=4 --channels=16 --streams=512
mise run bench-all
```

Scenarios are `unary`, `bidi-ping-pong`, and `bidi-throughput`. Common options include
`--warmup`, `--duration`, `--streams`, `--channels`, `--reactors`, `--pipeline`,
`--payload-bytes`, `--payload-pattern`, and `--compression`. For multi-reactor servers use
at least as many Channels as reactors.

`mise run bench-server` starts a separately managed server. `mise run bench-client` can
target it locally or remotely. Output is pretty JSON by default; `--compact` emits one
line.

Reported bytes are application request and response payloads, not HTTP/2 wire bytes. Raw
unary is event-driven; typed unary uses blocking worker slots and also includes protobuf
costs, so these modes are not a pure transport comparison.

The harness is closed-loop with fixed concurrency. Exchanges beginning inside the
measurement window remain eligible during bounded drain. `min_us` and `max_us` cover all
eligible latency; percentiles and mean may use the deterministic reservoir. Clock fields
report whether libcpucycles passed stability checks or fell back to `CLOCK_MONOTONIC`.

Compare only within one scenario: unary operations are complete RPCs while bidirectional
operations are messages on persistent streams. `repeated` is intentionally compressible;
`deterministic-random` is repeatable higher-entropy input.

## Dependencies

- Latest stable Zig release through mise (minimum 0.16.0)
- nghttp2 1.69.0
- c-ares 1.34.8
- libcpucycles 20260625
- libxev b0650f0
- zig-protobuf 5.0.0
- nanozlog 0.1.0
- mbedTLS 3.6.6 (optional)
- gperftools 2.18.1-based fork (optional)
- CMake and Ninja for upstream C builds
- mise for tool versions and project tasks
