const std = @import("std");
const demo = @import("demo_proto");
const grpc = @import("grpc_lite");
const grpc_pb = @import("grpc_lite_protobuf");

const AppError = error{ Rejected, OutOfMemory };

const AppState = struct {
    response_allocator: std.mem.Allocator,
    scratch_allocator: std.mem.Allocator,
    context_hook_called: std.atomic.Value(bool) = .init(false),

    fn echo(self: *AppState, request: demo.EchoRequest) AppError!demo.EchoReply {
        if (std.mem.eql(u8, request.message, "reject")) return error.Rejected;
        const scratch = try self.scratch_allocator.dupe(u8, request.message);
        defer self.scratch_allocator.free(scratch);
        return .{ .message = try self.response_allocator.dupe(u8, scratch) };
    }
};

const EchoApi = demo.EchoService(AppState, AppError);
const StreamingEchoApi = demo.StreamingEchoService(AppState, AppError);

fn mapError(err: AppError) grpc.Status {
    return switch (err) {
        error.Rejected => .init(.invalid_argument, "request rejected"),
        error.OutOfMemory => .init(.resource_exhausted, "allocation failed"),
    };
}

fn configureContext(state: *AppState, context: *grpc.ServerContext) !void {
    state.context_hook_called.store(true, .release);
    try context.addInitialMetadata("x-protobuf", "enabled");
    try context.addTrailingMetadata("x-service", "demo.EchoService");
}

test "generated service isolates transport registration handler and client allocators" {
    var transport_gpa: std.heap.DebugAllocator(.{ .canary = 0x11_11_11_11 }) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, transport_gpa.deinit()) catch @panic("transport allocator leak");
    var registration_gpa: std.heap.DebugAllocator(.{ .canary = 0x22_22_22_22 }) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, registration_gpa.deinit()) catch @panic("registration allocator leak");
    var scratch_gpa: std.heap.DebugAllocator(.{ .canary = 0x33_33_33_33 }) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, scratch_gpa.deinit()) catch @panic("handler scratch allocator leak");
    var client_gpa: std.heap.DebugAllocator(.{ .canary = 0x44_44_44_44 }) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, client_gpa.deinit()) catch @panic("client allocator leak");

    const transport_allocator = transport_gpa.allocator();
    const registration_allocator = registration_gpa.allocator();
    const client_allocator = client_gpa.allocator();
    var state: AppState = .{
        .response_allocator = registration_allocator,
        .scratch_allocator = scratch_gpa.allocator(),
    };
    const vtable: EchoApi = .{ .Echo = AppState.echo };
    var registration = grpc_pb.ServiceRegistration(EchoApi).init(
        registration_allocator,
        &state,
        vtable,
        .{
            .map_error = mapError,
            .context_hook = configureContext,
        },
    );
    defer registration.deinit();

    var server = try grpc.Server.init(transport_allocator, .{});
    defer server.deinit();
    try registration.register(&server);
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try grpc.Channel.init(transport_allocator, target, .{});
    defer channel.deinit();
    var client = grpc_pb.ServiceClient(EchoApi).init(&channel);

    var success = try client.callUnary(
        client_allocator,
        "Echo",
        demo.EchoRequest{ .message = "hello protobuf" },
        .{},
    );
    defer success.deinit();
    try std.testing.expect(success.raw.status.isOk());
    try std.testing.expectEqualStrings("hello protobuf", success.response.?.message);
    try std.testing.expectEqualStrings("enabled", success.raw.initial_metadata.getFirst("x-protobuf").?);
    try std.testing.expectEqualStrings("demo.EchoService", success.raw.trailing_metadata.getFirst("x-service").?);
    try std.testing.expect(state.context_hook_called.load(.acquire));

    var rejected = try client.callUnary(
        client_allocator,
        "Echo",
        demo.EchoRequest{ .message = "reject" },
        .{},
    );
    defer rejected.deinit();
    try std.testing.expectEqual(grpc.StatusCode.invalid_argument, rejected.raw.status.code);
    try std.testing.expectEqualStrings("request rejected", rejected.raw.status.message);
    try std.testing.expectEqual(@as(?demo.EchoReply, null), rejected.response);
}

test "automatic method path includes package and service" {
    try std.testing.expectEqualStrings(
        "/demo.EchoService/Echo",
        grpc_pb.methodPath(EchoApi, "Echo"),
    );
}

test "malformed protobuf request is rejected before dispatch" {
    var state: AppState = .{
        .response_allocator = std.testing.allocator,
        .scratch_allocator = std.testing.allocator,
    };
    var registration = grpc_pb.ServiceRegistration(EchoApi).init(
        std.testing.allocator,
        &state,
        .{ .Echo = AppState.echo },
        .{},
    );
    defer registration.deinit();

    var server = try grpc.Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try registration.register(&server);
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try grpc.Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    var result = try channel.callUnary(
        std.testing.allocator,
        "/demo.EchoService/Echo",
        &.{ 0x0a, 0x01, 'v', 0x0a, 0x05, 'x' },
        .{},
    );
    defer result.deinit();
    try std.testing.expectEqual(grpc.StatusCode.invalid_argument, result.status.code);
}

test "streaming method reflection infers cardinality and message types" {
    try std.testing.expectEqual(
        grpc_pb.MethodKind.server_streaming,
        grpc_pb.methodKind(StreamingEchoApi, "ServerEcho"),
    );
    try std.testing.expectEqual(
        grpc_pb.MethodKind.client_streaming,
        grpc_pb.methodKind(StreamingEchoApi, "ClientEcho"),
    );
    try std.testing.expectEqual(
        grpc_pb.MethodKind.bidirectional_streaming,
        grpc_pb.methodKind(StreamingEchoApi, "Chat"),
    );
    try std.testing.expect(grpc_pb.RequestType(StreamingEchoApi, "Chat") == demo.EchoRequest);
    try std.testing.expect(grpc_pb.ResponseType(StreamingEchoApi, "Chat") == demo.EchoReply);
    try std.testing.expectEqualStrings(
        "/demo.StreamingEchoService/Chat",
        grpc_pb.methodPath(StreamingEchoApi, "Chat"),
    );
}

test "typed send preserves raw backpressure errors" {
    const FakeState = struct {
        encoded: [64]u8 = undefined,
        encoded_len: usize = 0,

        fn send(context: *anyopaque, payload: []const u8, _: grpc.StreamSendOptions) !void {
            const self: *@This() = @ptrCast(@alignCast(context));
            @memcpy(self.encoded[0..payload.len], payload);
            self.encoded_len = payload.len;
            return error.WouldBlock;
        }

        fn closeSend(_: *anyopaque) !void {}
        fn cancel(_: *anyopaque) void {}
        fn resumeReceive(_: *anyopaque) !void {}
        fn finish(_: *anyopaque, _: grpc.Status) !void {}
        fn release(_: *anyopaque) void {}
    };

    var fake = FakeState{};
    const raw = grpc.ClientStream.init(
        &fake,
        FakeState.send,
        FakeState.closeSend,
        FakeState.cancel,
        FakeState.resumeReceive,
        FakeState.release,
    );
    const stream: grpc_pb.ClientStreamView(demo.EchoRequest) = .{ .raw = raw };
    try std.testing.expectError(
        error.WouldBlock,
        stream.send(std.testing.allocator, .{ .message = "queued" }, .{}),
    );

    var reader: std.Io.Reader = .fixed(fake.encoded[0..fake.encoded_len]);
    var decoded = try demo.EchoRequest.decode(&reader, std.testing.allocator);
    defer decoded.deinit(std.testing.allocator);
    try std.testing.expectEqualStrings("queued", decoded.message);

    const raw_server = grpc.ServerStream.init(
        &fake,
        FakeState.send,
        FakeState.finish,
        FakeState.resumeReceive,
    );
    const server_stream: grpc_pb.ServerStream(demo.EchoReply) = .{
        .raw = raw_server,
        .allocator = std.testing.allocator,
    };
    try std.testing.expectError(
        error.WouldBlock,
        server_stream.send(.{ .message = "response" }, .{}),
    );
    reader = .fixed(fake.encoded[0..fake.encoded_len]);
    var decoded_response = try demo.EchoReply.decode(&reader, std.testing.allocator);
    defer decoded_response.deinit(std.testing.allocator);
    try std.testing.expectEqualStrings("response", decoded_response.message);
}

const StreamingMode = enum { server, client, bidi };

const StreamingServerState = struct {
    mode: StreamingMode,
    messages: usize = 0,
    compressed_messages: usize = 0,
    canceled: bool = false,

    fn onStart(
        context: ?*anyopaque,
        _: grpc_pb.ServerStream(demo.EchoReply),
        server_context: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        if (self.mode == .server) server_context.setResponseCompression(.gzip);
    }

    fn onMessage(
        context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
        request: *const demo.EchoRequest,
        compression: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.messages += 1;
        if (compression == .gzip) self.compressed_messages += 1;
        switch (self.mode) {
            .server => {
                try stream.send(.{ .message = request.message }, .{ .compression = .gzip });
                try stream.send(.{ .message = request.message }, .{ .compression = .gzip });
            },
            .client => {},
            .bidi => try stream.send(.{ .message = request.message }, .{}),
        }
        return .continue_receiving;
    }

    fn onRemoteEnd(
        context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        if (self.mode == .client) {
            const message = if (self.messages == 2) "2" else "unexpected";
            try stream.send(.{ .message = message }, .{});
        }
        try stream.finish(.ok);
    }

    fn onCancel(
        context: ?*anyopaque,
        _: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.canceled = true;
    }

    fn handler(self: *@This()) grpc_pb.ServerStreamHandler(demo.EchoRequest, demo.EchoReply) {
        return .{
            .context = self,
            .on_start = onStart,
            .on_message = onMessage,
            .on_remote_end = onRemoteEnd,
            .on_cancel = onCancel,
        };
    }
};

const StreamingClientState = struct {
    expected: []const []const u8,
    pause_first_response: bool = false,
    messages: usize = 0,
    compressed_messages: usize = 0,
    remote_ended: bool = false,
    terminal_status: grpc.StatusCode = .unknown,
    adapter_error: ?anyerror = null,
    paused: std.atomic.Value(bool) = .init(false),
    done: std.atomic.Value(bool) = .init(false),

    fn onMessage(
        context: ?*anyopaque,
        _: grpc_pb.ClientStreamView(demo.EchoRequest),
        response: *const demo.EchoReply,
        compression: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        if (self.messages >= self.expected.len) return error.UnexpectedResponseCount;
        if (!std.mem.eql(u8, self.expected[self.messages], response.message)) {
            return error.UnexpectedResponseMessage;
        }
        self.messages += 1;
        if (compression == .gzip) self.compressed_messages += 1;
        if (self.pause_first_response and self.messages == 1) {
            self.paused.store(true, .release);
            return .pause;
        }
        return .continue_receiving;
    }

    fn onRemoteEnd(
        context: ?*anyopaque,
        _: grpc_pb.ClientStreamView(demo.EchoRequest),
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.remote_ended = true;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc_pb.ClientStreamView(demo.EchoRequest),
        final_status: grpc.Status,
        _: *const grpc.Metadata,
        adapter_error: ?anyerror,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.terminal_status = final_status.code;
        self.adapter_error = adapter_error;
        self.done.store(true, .release);
    }

    fn callbacks(self: *@This()) grpc_pb.ClientStreamCallbacks(demo.EchoRequest, demo.EchoReply) {
        return .{
            .context = self,
            .on_message = onMessage,
            .on_remote_end = onRemoteEnd,
            .on_terminal = onTerminal,
        };
    }
};

fn runTypedStreamingCase(
    client: *grpc_pb.ServiceClient(StreamingEchoApi),
    callback_allocator: std.mem.Allocator,
    io: std.Io,
    comptime method: []const u8,
    requests: []const []const u8,
    expected: []const []const u8,
    compress_first_request: bool,
    expected_compressed_responses: usize,
    pause_first_response: bool,
) !void {
    var state: StreamingClientState = .{
        .expected = expected,
        .pause_first_response = pause_first_response,
    };
    var call = try client.openStream(
        callback_allocator,
        method,
        .{ .send_compression = if (compress_first_request) .gzip else .identity },
        state.callbacks(),
    );
    defer call.deinit();

    for (requests, 0..) |message, index| {
        try call.send(
            std.heap.page_allocator,
            .{ .message = message },
            .{ .compression = if (compress_first_request and index == 0) .gzip else .identity },
        );
    }
    try call.closeSend();

    const deadline = std.Io.Clock.awake.now(io).nanoseconds +| 10 * std.time.ns_per_s;
    if (pause_first_response) {
        while (!state.paused.load(.acquire)) {
            if (std.Io.Clock.awake.now(io).nanoseconds >= deadline) return error.StreamTimeout;
            try std.Io.sleep(io, .fromMilliseconds(1), .awake);
        }
        try call.resumeReceive();
    }
    while (!state.done.load(.acquire)) {
        if (std.Io.Clock.awake.now(io).nanoseconds >= deadline) return error.StreamTimeout;
        try std.Io.sleep(io, .fromMilliseconds(1), .awake);
    }
    if (state.adapter_error) |err| return err;
    try std.testing.expectEqual(grpc.StatusCode.ok, state.terminal_status);
    try std.testing.expect(state.remote_ended);
    try std.testing.expectEqual(expected.len, state.messages);
    try std.testing.expectEqual(expected_compressed_responses, state.compressed_messages);
}

test "typed streaming client and server cover all cardinalities" {
    var server_gpa: std.heap.DebugAllocator(.{}) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, server_gpa.deinit()) catch @panic("server adapter leak");
    var client_gpa: std.heap.DebugAllocator(.{}) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, client_gpa.deinit()) catch @panic("client adapter leak");

    var server_state: StreamingServerState = .{ .mode = .server };
    var client_state: StreamingServerState = .{ .mode = .client };
    var bidi_state: StreamingServerState = .{ .mode = .bidi };
    var server_registration = grpc_pb.StreamRegistration(StreamingEchoApi, "ServerEcho").init(
        server_gpa.allocator(),
        server_state.handler(),
    );
    defer server_registration.deinit();
    var client_registration = grpc_pb.StreamRegistration(StreamingEchoApi, "ClientEcho").init(
        server_gpa.allocator(),
        client_state.handler(),
    );
    defer client_registration.deinit();
    var bidi_registration = grpc_pb.StreamRegistration(StreamingEchoApi, "Chat").init(
        server_gpa.allocator(),
        bidi_state.handler(),
    );
    defer bidi_registration.deinit();

    var server = try grpc.Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server_registration.register(&server);
    try client_registration.register(&server);
    try bidi_registration.register(&server);
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try grpc.Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    var client = grpc_pb.ServiceClient(StreamingEchoApi).init(&channel);

    const server_expected = [_][]const u8{ "server", "server" };
    try runTypedStreamingCase(
        &client,
        client_gpa.allocator(),
        std.testing.io,
        "ServerEcho",
        &.{"server"},
        &server_expected,
        false,
        2,
        true,
    );
    const client_expected = [_][]const u8{"2"};
    try runTypedStreamingCase(
        &client,
        client_gpa.allocator(),
        std.testing.io,
        "ClientEcho",
        &.{ "first", "second" },
        &client_expected,
        false,
        0,
        false,
    );
    const bidi_expected = [_][]const u8{ "one", "two" };
    try runTypedStreamingCase(
        &client,
        client_gpa.allocator(),
        std.testing.io,
        "Chat",
        &bidi_expected,
        &bidi_expected,
        true,
        0,
        false,
    );

    try std.testing.expectEqual(@as(usize, 1), server_state.messages);
    try std.testing.expectEqual(@as(usize, 2), client_state.messages);
    try std.testing.expectEqual(@as(usize, 2), bidi_state.messages);
    try std.testing.expectEqual(@as(usize, 1), bidi_state.compressed_messages);
    try std.testing.expect(!server_state.canceled);
    try std.testing.expect(!client_state.canceled);
    try std.testing.expect(!bidi_state.canceled);
}

const RawCompletion = struct {
    messages: usize = 0,
    status: grpc.StatusCode = .unknown,
    done: std.atomic.Value(bool) = .init(false),

    fn onMessage(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        _: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.messages += 1;
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        final_status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.status = final_status.code;
        self.done.store(true, .release);
    }
};

test "typed streaming server rejects malformed protobuf requests" {
    var server_state: StreamingServerState = .{ .mode = .bidi };
    var registration = grpc_pb.StreamRegistration(StreamingEchoApi, "Chat").init(
        std.testing.allocator,
        server_state.handler(),
    );
    defer registration.deinit();
    var server = try grpc.Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try registration.register(&server);
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try grpc.Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    var completion: RawCompletion = .{};
    var call = try channel.openStream(
        "/demo.StreamingEchoService/Chat",
        .{},
        .{
            .context = &completion,
            .on_message = RawCompletion.onMessage,
            .on_terminal = RawCompletion.onTerminal,
        },
    );
    defer call.deinit();
    try call.send(&.{ 0x0a, 0x01, 'v', 0x0a, 0x05, 'x' }, .{});
    try call.closeSend();

    const deadline = std.Io.Clock.awake.now(std.testing.io).nanoseconds +| 10 * std.time.ns_per_s;
    while (!completion.done.load(.acquire)) {
        if (std.Io.Clock.awake.now(std.testing.io).nanoseconds >= deadline) return error.StreamTimeout;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.testing.expectEqual(grpc.StatusCode.invalid_argument, completion.status);
    try std.testing.expectEqual(@as(usize, 0), completion.messages);
    try std.testing.expectEqual(@as(usize, 0), server_state.messages);
}

const MalformedResponseHandler = struct {
    fn onStart(
        _: ?*anyopaque,
        _: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) !void {}

    fn onMessage(
        _: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
        _: []const u8,
        _: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        try stream.send(&.{ 0x0a, 0x01, 'v', 0x0a, 0x05, 'x' }, .{});
        try stream.finish(.ok);
        return .pause;
    }

    fn onRemoteEnd(
        _: ?*anyopaque,
        _: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) !void {}
};

test "typed streaming client reports malformed protobuf responses" {
    var server = try grpc.Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerStream(
        "/demo.StreamingEchoService/Chat",
        .{
            .on_start = MalformedResponseHandler.onStart,
            .on_message = MalformedResponseHandler.onMessage,
            .on_remote_end = MalformedResponseHandler.onRemoteEnd,
        },
    );
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try grpc.Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    var client = grpc_pb.ServiceClient(StreamingEchoApi).init(&channel);
    var state: StreamingClientState = .{ .expected = &.{} };
    var call = try client.openStream(
        std.testing.allocator,
        "Chat",
        .{},
        state.callbacks(),
    );
    defer call.deinit();
    try call.send(std.testing.allocator, .{ .message = "request" }, .{});
    try call.closeSend();

    const deadline = std.Io.Clock.awake.now(std.testing.io).nanoseconds +| 10 * std.time.ns_per_s;
    while (!state.done.load(.acquire)) {
        if (std.Io.Clock.awake.now(std.testing.io).nanoseconds >= deadline) return error.StreamTimeout;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.testing.expect(state.adapter_error != null);
    try std.testing.expectEqual(@as(usize, 0), state.messages);
}

const CallbackDeinitState = struct {
    call: ?*grpc_pb.ClientStream(demo.EchoRequest, demo.EchoReply) = null,
    returned_from_deinit: std.atomic.Value(bool) = .init(false),
    terminals: std.atomic.Value(usize) = .init(0),

    fn onMessage(
        context: ?*anyopaque,
        _: grpc_pb.ClientStreamView(demo.EchoRequest),
        response: *const demo.EchoReply,
        _: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        if (!std.mem.eql(u8, response.message, "deinit")) return error.UnexpectedResponseMessage;
        self.call.?.deinit();
        self.call = null;
        self.returned_from_deinit.store(true, .release);
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc_pb.ClientStreamView(demo.EchoRequest),
        _: grpc.Status,
        _: *const grpc.Metadata,
        _: ?anyerror,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        _ = self.terminals.fetchAdd(1, .monotonic);
    }
};

test "typed client stream can be deinitialized from a callback" {
    var callback_gpa: std.heap.DebugAllocator(.{}) = .init;
    defer std.testing.expectEqual(std.heap.Check.ok, callback_gpa.deinit()) catch @panic("callback adapter leak");
    var server_state: StreamingServerState = .{ .mode = .bidi };
    var registration = grpc_pb.StreamRegistration(StreamingEchoApi, "Chat").init(
        std.testing.allocator,
        server_state.handler(),
    );
    defer registration.deinit();
    var server = try grpc.Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try registration.register(&server);
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try grpc.Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    var client = grpc_pb.ServiceClient(StreamingEchoApi).init(&channel);
    var state: CallbackDeinitState = .{};
    var call = try client.openStream(
        callback_gpa.allocator(),
        "Chat",
        .{},
        .{
            .context = &state,
            .on_message = CallbackDeinitState.onMessage,
            .on_terminal = CallbackDeinitState.onTerminal,
        },
    );
    state.call = &call;
    try call.send(std.testing.allocator, .{ .message = "deinit" }, .{});

    const deadline = std.Io.Clock.awake.now(std.testing.io).nanoseconds +| 10 * std.time.ns_per_s;
    while (!state.returned_from_deinit.load(.acquire)) {
        if (std.Io.Clock.awake.now(std.testing.io).nanoseconds >= deadline) return error.StreamTimeout;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.Io.sleep(std.testing.io, .fromMilliseconds(10), .awake);
    try std.testing.expectEqual(@as(usize, 0), state.terminals.load(.acquire));
}
