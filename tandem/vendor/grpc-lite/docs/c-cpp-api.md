# C and C++ APIs

grpc-lite provides a stable C ABI, a low-level C++ RAII API in `grpc_lite::*`, and a
focused synchronous grpcpp-shaped facade in `grpc::*`. All three use the same transport.

## Installed Package

`zig build` installs the versioned shared library, static archive, C and C++ headers,
CMake package files, and `protoc-gen-grpc_lite_cpp` under `zig-out`.

```bash
cc app.c \
  -Izig-out/include \
  -Lzig-out/lib -Wl,-rpath,"$PWD/zig-out/lib" \
  -lgrpc_lite
```

The transport, low-level C++ API, and `grpc_lite::grpcpp` facade do not require
Protobuf or Abseil. Typed generated C++ services use the official Protobuf runtime as an
optional external dependency. grpc-lite does not fetch, vendor, or select a Protobuf or
Abseil version for downstream applications.

The C ABI uses opaque handles, fixed-width error codes, and pointer-length byte views.
Memory allocated by the library must be released by its matching destroy function.

Runtime initialization must happen before application threads are created and must
outlive dependent handles. The API provides:

- version and feature discovery
- owning Runtime and Metadata handles
- connected and reconnecting managed Channels
- blocking raw unary calls
- cancellable event-driven client streams
- event-driven server registration and retained cross-thread calls
- metadata, bounded backpressure, cancellation observation, and graceful drain

`grpc_lite_features()` reports capabilities usable through the C ABI, rather than every
transport capability compiled into the library. `GRPC_LITE_FEATURE_TLS` remains defined
for source compatibility, but the returned mask leaves it clear because C Channel and
Server options currently create only insecure transports. Building with `-Dtls=true`
still enables TLS for the Zig API and does not change the C option struct layouts.

C and C++ callback code runs synchronously on API caller or transport threads, may run
concurrently, and must not block unless the specific synchronous facade contract permits
it.

`grpc_lite_server_call_abort` and `grpc_lite::ServerCall::Abort` make an allocation-free,
thread-safe emergency termination request for a retained server call. The reactor resets
the stream with `INTERNAL_ERROR`, or closes the connection if reset submission fails.

## Managed Channels

`grpc_lite_channel_create_managed` creates a durable Channel. Set
`allow_initial_offline` to return before the first connection succeeds. A managed Channel
reconnects with bounded exponential backoff without replaying submitted RPCs. Unsubmitted
unary calls retain their original deadlines while waiting for a connection.

Use `grpc_lite_channel_shutdown` to stop admission and active work, then
`grpc_lite_channel_wait` before exclusive destruction when calls may be concurrent.

Channel and Server options accept an optional `grpc_lite_logger` for connection recovery,
GOAWAY, startup, and drain events. The logger configuration is borrowed during handle
creation; its user data must outlive the owning handle.

`grpc_lite_server_options` and `grpc_lite::ServerOptions` expose finite server admission
bounds: `max_connections = 1024` across all reactors,
`max_concurrent_streams_per_connection = 100`,
`cleartext_preface_timeout_ns = 10000000000`, and
`connection_idle_timeout_ns = 300000000000`. Every value must be nonzero. The preface
limit is an absolute cleartext deadline through the initial non-ACK SETTINGS; input does
not refresh it. Idle means no active inbound RPC streams, so transport traffic does not
refresh the deadline and any active unary or streaming RPC is exempt.

C callers must initialize `struct_size`. Callers using an older size receive the finite
resource defaults for missing fields; each appended field is read only when its own end
offset is present. Excess streams are reset with `REFUSED_STREAM` before SETTINGS is
acknowledged; exceeding an acknowledged stream limit causes `GOAWAY(PROTOCOL_ERROR)`.
Idle retirement sends `GOAWAY(NO_ERROR)` before graceful closure.

`grpc_lite_channel_options.max_response_header_list_size` and the matching low-level C++
option default to 65536 and accept values from 1 through `UINT32_MAX`. The HTTP/2 setting
is advertised per header list, but local enforcement charges each decoded physical field
as name bytes plus value bytes plus 32 and accumulates across all response header and
trailer blocks in an RPC. Oversize blocks are withheld atomically and finish only that RPC
with `RESOURCE_EXHAUSTED`; an accepted initial block survives oversized trailers and the
connection remains reusable.

## CMake Source Build

Git checkouts and GitHub automatic “Source code” archives intentionally contain no
generated C. A no-Zig CMake build pairs the source at a tag with that release's matching
`grpc-lite-transpiled-c-<version>-linux-x86_64.tar.gz` asset. Verify its sidecar from the
folder containing both files, extract it, and pass the extracted root explicitly:

```bash
sha256sum --check grpc-lite-transpiled-c-0.4.0-linux-x86_64.tar.gz.sha256
tar -xzf grpc-lite-transpiled-c-0.4.0-linux-x86_64.tar.gz
cmake -S grpc-lite -B build \
  -DGRPC_LITE_TRANSPILED_C_DIR="$PWD/grpc-lite-transpiled-c-0.4.0-linux-x86_64"
```

The generated-only asset targets insecure Linux x86_64. TLS is out of scope for this
transpiled-C release bundle until separately selected, and CMake rejects
`GRPC_LITE_ENABLE_TLS=ON`. This does not change the optional mbedTLS support available to
native Zig builds.

The asset contains only its manifest, two generated C translation units, and the matching
`zig.h`; it does not contain project sources or third-party dependencies.
`sha256sum --check` provides integrity/error detection, but a checksum colocated with an
asset is not publisher authentication.

After setting `GRPC_LITE_TRANSPILED_C_DIR`, add the source directory and link its targets:

```cmake
add_subdirectory(third_party/grpc-lite)
target_link_libraries(c_app PRIVATE grpc_lite::c)
target_link_libraries(cpp_app PRIVATE grpc_lite::grpcpp)
```

Typed C++ source builds additionally call `find_package(Protobuf REQUIRED)` and set
`GRPC_LITE_ENABLE_PROTOBUF=ON` before `add_subdirectory`, then link
`grpc_lite::grpcpp_protobuf`. `grpc_lite::grpcpp_protobuf_lite` is defined when the
Protobuf package exports `protobuf::libprotobuf-lite`. The application must select one
compatible Protobuf dependency graph.

`grpc_lite::c` and `grpc_lite::c_static` both refer to the source-built static transport.
An installed package exposes the versioned shared transport through `grpc_lite::c` and
the static archive through `grpc_lite::c_static`.

The generated bundle does not make dependency resolution offline. For a fully offline
configuration, stage nghttp2 and c-ares archives separately, hash them, and enable the
network guard:

```bash
cmake -S grpc-lite -B build \
  -DGRPC_LITE_TRANSPILED_C_DIR="$PWD/grpc-lite-transpiled-c-0.4.0-linux-x86_64" \
  -DGRPC_LITE_NO_NETWORK=ON \
  -DGRPC_LITE_NGHTTP2_URL="file://$PWD/deps/nghttp2.tar.gz" \
  -DGRPC_LITE_NGHTTP2_URL_HASH=SHA256=... \
  -DGRPC_LITE_CARES_URL="file://$PWD/deps/c-ares.tar.gz" \
  -DGRPC_LITE_CARES_URL_HASH=SHA256=...
```

Without `GRPC_LITE_NO_NETWORK`, nghttp2 may use the nested source directory and both
dependencies otherwise use their pinned archives. Maintainers use `mise run transpile-c`
for an ignored local bundle directory and `mise run package-transpiled-c` for an asset;
generated output is never committed.

## Low-Level C++ API

Headers under `<grpc_lite/cpp/...>` provide explicit RAII wrappers over the C ABI,
including `grpc_lite::Runtime`, `Channel`, `ClientStream`, `Server`, `Metadata`, and
`Status`. This is the event-driven API for applications that need ownership and streaming
control without using C directly.

Hostname channels require explicit Runtime ownership. Use
`grpc_lite::Runtime` with `grpc_lite::Channel::CreateManaged`, or the equivalent C API.

## Synchronous grpcpp Facade

`<grpcpp/grpcpp.h>` provides the common synchronous client and generated server shapes:

- `grpc::Status` and status codes
- `grpc::ClientContext` and `grpc::ServerContext`
- Channels and channel arguments
- unary, client-streaming, server-streaming, and bidirectional client calls
- generated nested `Service` classes
- `grpc::ServerReader<T>`, `grpc::ServerWriter<T>`, and
  `grpc::ServerReaderWriter<W, R>` with bounded request and response backpressure

It is source-compatible with this selected API subset, not ABI-compatible with grpcpp.
It does not provide official generated glue, CompletionQueue, callbacks/reactors, generic
services, interceptors, or grpc-core internals.

The grpcpp-shaped `grpc::CreateChannel` facade currently accepts IPv4 targets. Use the
low-level C++ or C API when hostname resolution requires explicit Runtime ownership.

## Generated Services

The protoc plugin generates synchronous glue for unary, client-streaming,
server-streaming, and bidirectional methods beside standard protobuf C++ messages.
Generated services expose both `EventService` and a grpcpp-shaped nested `Service` with
virtual methods. Existing official gRPC
`*.grpc.pb.cc` files must be regenerated with the grpc-lite plugin; relinking official
generated service glue is not supported. Standard `*.pb.h` and `*.pb.cc` message files
continue to come from the selected official `protoc` release.

```bash
protoc -I proto \
  --cpp_out=generated \
  --plugin=protoc-gen-grpc_lite_cpp=zig-out/bin/protoc-gen-grpc_lite_cpp \
  --grpc_lite_cpp_out=generated \
  proto/echo.proto
```

An installed package exposes the optional integration through a CMake component:

```cmake
find_package(grpc_lite 0.4 CONFIG REQUIRED COMPONENTS protobuf)
target_link_libraries(echo_proto PUBLIC grpc_lite::grpcpp_protobuf)
```

The component locates the application's official Protobuf package and propagates
`protobuf::libprotobuf`; it does not make Protobuf a dependency of the raw targets. Use
`grpc_lite::grpcpp_protobuf_lite` only with messages generated for the lite runtime.
Keep `protoc` and the linked runtime on compatible releases so their generated-code
version checks agree.

Create a fresh adapter for every server instance:

```cpp
auto adapter = service.CreateEventService(executor, options);
server.RegisterService(adapter.get());
```

Keep the service, executor, and adapter alive until the Server has stopped. Drain
submitted executor work before destroying the service. Direct `EventService`
implementations receive client-streaming and bidirectional call handles on reactor
threads; move those handles to application workers before calling their blocking `Read`
helpers.

`grpc_lite::ServerExecutor` is application-owned. `Submit` must enqueue without blocking;
handlers may block only on executor threads. Rejection completes the call with
`RESOURCE_EXHAUSTED`. The facade never creates worker threads.

`SynchronousServiceOptions::admission` may provide an application-owned, nonblocking
reactor hook that rejects metadata before a call enters the executor.

Complete consumers live under `examples/cpp`. Build them against an installed prefix:

```bash
cmake -S examples/cpp -B build-cpp -G Ninja \
  -DCMAKE_PREFIX_PATH="$PWD/zig-out"
cmake --build build-cpp
```

`mise run test-consumer-cpp` packages the project, resolves the installed package's
`protobuf` component, regenerates C++ sources, starts the installed server, and runs the
examples.

## Abseil Policy

grpc-lite does not expose Abseil in its selected grpcpp-shaped API and does not install
substitute `absl/*` headers. Abseil used internally by a chosen Protobuf release remains
that package's transitive dependency. Applications that include Abseil directly must
continue to provide it or migrate those call sites separately.
