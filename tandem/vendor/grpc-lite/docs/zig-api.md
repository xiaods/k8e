# Zig API

The public Zig transport is raw-byte based. Applications may use protobuf wire bytes
directly or opt into the shared zig-protobuf runtime and typed adapters.

## Runtime and Channels

IPv4 literals require no global setup. Hostname targets use c-ares and require a Runtime
initialized before the application creates threads. The Runtime must outlive every
Channel that references it.

```zig
const grpc = @import("grpc_lite");

var runtime = try grpc.Runtime.init();
defer runtime.deinit();

var channel = try grpc.Channel.init(allocator, "api.example.com:50051", .{
    .runtime = &runtime,
    .reconnect = .{ .allow_initial_offline = true },
});
defer channel.deinit();
```

Reconnect backoff follows the gRPC defaults: one-second initial delay, 1.6 multiplier,
120-second cap, and 20 percent jitter. Success resets the backoff. Recovery never retries
an RPC that may have reached the server; applications own RPC retry policy.

`max_response_header_list_size` defaults to 65536 and must be nonzero. The advertised
HTTP/2 setting applies per header list, while grpc-lite enforces the bound cumulatively
across all initial headers and trailers for each RPC. Every decoded physical field costs
its name bytes, value bytes, and 32 bytes. A rejected block is not disclosed; the RPC
finishes with `RESOURCE_EXHAUSTED` and the connection remains reusable. Initial metadata
accepted before oversized trailers remains available.

`Channel.callUnary` supports concurrent callers. `Channel.shutdown` may run while calls
are active; join caller threads before giving `Channel.deinit` exclusive access. The
Channel serializes access to the allocator passed to `Channel.init`.

## Unary Calls

```zig
const std = @import("std");

var result = try channel.callUnary(
    allocator,
    "/demo.EchoService/Echo",
    protobuf_wire_bytes,
    .{ .timeout_ns = 5 * std.time.ns_per_s },
);
defer result.deinit();
```

Each `CallResult` owns its payload, status message, and response metadata through the
result allocator. Callers must synchronize a shared result allocator. A non-thread-safe
result allocator must not alias the Channel backing allocator while the Channel is active.

Raw unary also has an event-driven API:

```zig
try channel.callUnaryAsync(
    "/demo.EchoService/Echo",
    protobuf_wire_bytes,
    .{ .timeout_ns = 5 * std.time.ns_per_s },
    .{ .context = &state, .on_complete = State.onComplete },
);
```

Input is copied before return. A successful return guarantees exactly one completion
callback. Validation, allocation, unavailable-channel, and notification failures return
without invoking it. There is no explicit unary cancellation API.

The callback runs on the transport loop and must not block or call `Channel.deinit`.
Payload, status, and metadata are borrowed until callback return. `Channel.deinit` waits
for every accepted callback before releasing Channel storage.

## Raw Streaming

`Channel.openStream` opens an event-driven client stream for all streaming cardinalities:

```zig
var call = try channel.openStream(
    "/demo.StreamingEchoService/Chat",
    .{},
    callbacks,
);
defer call.deinit();

try call.send(protobuf_wire_bytes, .{});
try call.closeSend();
```

`send` copies and queues one message. It returns `WouldBlock` when the bounded outbound
queue is full. `closeSend` half-closes only the local direction after queued messages
drain. `cancel` terminates both directions. An `on_message` callback may return `.pause`;
call `resumeReceive` to continue delivery.

Callbacks run on transport loop threads and must not block. Callback stream handles are
borrowed; deinitialize only the application-owned handle. Deinitializing an active client
stream cancels it and suppresses future callbacks.

Servers register raw streaming handlers with `Server.registerStream`. `ServerStream` is a
borrowed callback view. Cross-thread work must retain an owning `ServerCall`, release every
clone before Server teardown, and treat `CallClosed` as terminal. `on_terminal` runs once
and is the final callback for a call.

If copying or enqueueing a retained-call response command fails, `ServerCall.abort` makes
an allocation-free, thread-safe emergency request. The reactor submits
`RST_STREAM(INTERNAL_ERROR)` without status trailers; if nghttp2 cannot allocate the reset
command, grpc-lite closes the connection to guarantee local cleanup.

`send`, `finish`, `sendInitialMetadata`, and `resumeReceive` copy command data into bounded
shared storage. Handlers that decide metadata outside `on_start` can use
`receive_initially_paused = true` and `initial_metadata_mode = .explicit`.

## Server

```zig
var server = try grpc.Server.init(allocator, .{
    .host = "127.0.0.1",
    .port = 50051,
    .reactor_count = 4,
});
defer server.deinit();

try server.registerUnary(
    "/demo.EchoService/Echo",
    grpc.UnaryHandler.bind(EchoService, &service, EchoService.echo),
);
try server.start();
server.wait();
```

`reactor_count` defaults to one. Larger values create independent Linux reactor threads,
listeners, HTTP/2 sessions, connection state, deadline heaps, and write pools. Listeners
share the IPv4 port with `SO_REUSEPORT`; each connection and all its streams remain on one
reactor. Multi-core scaling therefore needs multiple client connections or Channels.

Server transport admission is finite by default:

- `max_connections = 1024` caps admitted connections across all reactors. Initializing,
  draining, and asynchronously closing connections continue to occupy a slot until final
  destruction.
- `max_concurrent_streams_per_connection = 100` is advertised exactly as HTTP/2
  `SETTINGS_MAX_CONCURRENT_STREAMS`. Before the peer acknowledges the setting, excess
  streams are reset with `REFUSED_STREAM`; exceeding the acknowledged limit is an HTTP/2
  protocol error and causes GOAWAY.
- `cleartext_preface_timeout_ns = 10s` is an absolute deadline through receipt of the
  complete client preface and initial non-ACK SETTINGS. Partial or trickled input does not
  refresh it.
- `connection_idle_timeout_ns = 5m` bounds a continuous interval with no active inbound
  RPC streams. PING, SETTINGS, reads, and writes do not refresh it. Any active unary or
  streaming RPC disarms idle expiry; after the final stream closes a fresh interval starts.
  Idle retirement sends `GOAWAY(NO_ERROR)` and closes gracefully.

All four values must be nonzero; there is no unlimited or disabled sentinel.

Each reactor uses a local allocator for connection and stream state. Page refills, large
allocations, coordinator state, `std.Io.Threaded`, TLS configuration, and cross-thread
stream commands use a serialized allocator backed by the allocator passed to
`Server.init`, so that backing allocator may be non-thread-safe.

The allocator in `ServerContext` is reactor-local and valid only during its callback.
Callbacks may run concurrently across reactors and must not block. Shared application
state and allocators used across callbacks must be thread-safe.

Handlers can inspect deadlines with `hasDeadline`, `remainingTimeNs`, and
`isDeadlineExceeded`. Handlers are not force-cancelled; a response after the deadline is
replaced with `DEADLINE_EXCEEDED`.

See `examples/echo_server.zig` and `examples/echo_client.zig` for complete programs.

## TLS

TLS is an optional mbedTLS 3.6.6 dependency:

```bash
mise run prepare-tls-deps
zig build -Dtls=true
```

Downstream packages pass `.tls = true` to the grpc-lite dependency. Clients trust only
the supplied PEM CA bundle; system roots are not searched. Hostname verification and SNI
use the Channel target. TLS 1.2 or newer and ALPN `h2` are mandatory. TLS 1.2 is limited
to ECDHE AEAD suites, while TLS 1.3 uses its AES-GCM and ChaCha20-Poly1305 suites, matching
the HTTP/2 cipher profile. The default handshake timeout is ten seconds.

```zig
var channel = try grpc.Channel.init(allocator, "api.example.com:443", .{
    .runtime = &runtime,
    .tls = .{ .ca_certificates_pem = ca_pem },
});
```

Servers accept a PEM certificate chain and unencrypted PEM private key. PEM input is
parsed during `Server.init` and may be released afterward.

## Metadata

Keys use lowercase gRPC header syntax. Outgoing ASCII values must be visible ASCII.
Invalid incoming ASCII fields are discarded. Binary `-bin` values remain raw bytes in
the API; the wire parser accepts padded, unpadded, and comma-joined base64 and rejects
malformed input only on the affected RPC.

## Protobuf

The raw `grpc_lite` module has no zig-protobuf import. Package dependencies disable
protobuf support by default, and an explicit `.protobuf = false` does not resolve or
compile zig-protobuf for either the target or host. Repository builds enable protobuf
support by default so their typed tests and code-generation steps remain available.

Pass `.protobuf = true` to expose the separate `grpc_lite_protobuf_runtime` and
`grpc_lite_protobuf` modules. The build package's `createProtocStep` and
`protobuf_codegen` exports are also opt-in code-generation APIs: using them resolves
zig-protobuf. `createProtocStep` returns an optional step so callers can use
`orelse return` with Zig's lazy dependency fetch. The typed adapter continues to operate
over the raw-byte transport.

Generated service VTables can be registered without manual method paths:

```zig
const demo = @import("demo_proto");
const grpc_pb = @import("grpc_lite_protobuf");

const EchoApi = demo.EchoService(EchoState, EchoError);
var registration = grpc_pb.ServiceRegistration(EchoApi).init(
    allocator,
    &state,
    .{ .Echo = EchoState.echo },
    .{ .map_error = mapError, .context_hook = configureContext },
);
try registration.register(&server);
// Call registration.deinit() after server.deinit().
```

The adapter derives the method path, decodes the request, invokes the generated VTable,
and encodes the response. Registration and userdata must outlive the Server. Returned
fields must be releasable with the registration allocator.

Typed streaming preserves raw transport semantics:

```zig
const StreamingApi = demo.StreamingEchoService(AppState, AppError);
var client = grpc_pb.ServiceClient(StreamingApi).init(&channel);
var call = try client.openStream(callback_allocator, "Chat", .{}, callbacks);
defer call.deinit();

try call.send(send_allocator, demo.EchoRequest{ .message = "hello" }, .{});
try call.closeSend();
```

Decoded callback messages are borrowed. Typed send operations use temporary encoding
buffers and preserve raw `WouldBlock`, pause/resume, cancellation, and compression
behavior without introducing unbounded queues. Registrations, contexts, and allocators
must remain at stable addresses until the Server and active streams stop.
