const std = @import("std");
const grpc = @import("grpc_lite");
const testing = @import("grpc_testing");

const Config = struct {
    port: u16 = 10000,
    use_tls: bool = false,
    tls_cert_file: []const u8 = "",
    tls_key_file: []const u8 = "",
};

const StreamMethod = enum {
    output,
    input,
    full_duplex,
    half_duplex,
};

const ResponseSpec = struct {
    payload_type: testing.PayloadType,
    size: i32,
    compression: grpc.Compression,
};

const StreamState = struct {
    method: StreamMethod,
    responses: std.ArrayList(ResponseSpec) = .empty,
    next_response: usize = 0,
    aggregated_payload_size: u64 = 0,
    message_count: usize = 0,
    remote_ended: bool = false,
    receive_paused: bool = false,
    aggregate_sent: bool = false,

    fn deinit(self: *StreamState, allocator: std.mem.Allocator) void {
        self.responses.deinit(allocator);
        self.* = undefined;
    }
};

const Service = struct {
    allocator: std.mem.Allocator,
    streams: std.AutoHashMapUnmanaged(*anyopaque, *StreamState) = .empty,

    fn init(allocator: std.mem.Allocator) Service {
        return .{ .allocator = allocator };
    }

    fn deinit(self: *@This()) void {
        var iterator = self.streams.valueIterator();
        while (iterator.next()) |state| self.destroyState(state.*);
        self.streams.deinit(self.allocator);
        self.* = undefined;
    }

    fn emptyCall(
        _: *@This(),
        allocator: std.mem.Allocator,
        _: *grpc.ServerContext,
        request_bytes: []const u8,
    ) !grpc.UnaryResponse {
        var reader: std.Io.Reader = .fixed(request_bytes);
        var request = testing.Empty.decode(&reader, allocator) catch {
            return grpc.UnaryResponse.fail(
                allocator,
                .init(.invalid_argument, "invalid Empty request"),
            );
        };
        defer request.deinit(allocator);
        return grpc.UnaryResponse.ok(allocator, "");
    }

    fn unaryCall(
        _: *@This(),
        allocator: std.mem.Allocator,
        context: *grpc.ServerContext,
        request_bytes: []const u8,
    ) !grpc.UnaryResponse {
        var reader: std.Io.Reader = .fixed(request_bytes);
        var request = testing.SimpleRequest.decode(&reader, allocator) catch {
            return grpc.UnaryResponse.fail(
                allocator,
                .init(.invalid_argument, "invalid SimpleRequest"),
            );
        };
        defer request.deinit(allocator);

        if (request.expect_compressed) |expected| {
            const compressed = context.request_compression == .gzip;
            if (expected.value != compressed) {
                return grpc.UnaryResponse.fail(
                    allocator,
                    .init(.invalid_argument, "request compression did not match expectation"),
                );
            }
        }
        if (request.response_compressed) |compressed| {
            context.setResponseCompression(if (compressed.value) .gzip else .identity);
        }

        if (request.response_status) |response_status| {
            const code = if (response_status.code >= 0)
                grpc.StatusCode.fromInt(@intCast(response_status.code))
            else
                .unknown;
            return grpc.UnaryResponse.fail(
                allocator,
                .init(code, response_status.message),
            );
        }
        if (request.response_size < 0) {
            return grpc.UnaryResponse.fail(
                allocator,
                .init(.invalid_argument, "negative response size"),
            );
        }

        const body = try allocator.alloc(u8, @intCast(request.response_size));
        defer allocator.free(body);
        @memset(body, 0);
        const response: testing.SimpleResponse = .{
            .payload = .{
                .type = request.response_type,
                .body = body,
            },
        };
        var writer: std.Io.Writer.Allocating = .init(allocator);
        defer writer.deinit();
        try response.encode(&writer.writer, allocator);
        return grpc.UnaryResponse.ok(allocator, writer.written());
    }

    fn streamingOutputStart(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        try self.startStream(stream, context, .output);
    }

    fn streamingInputStart(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        try self.startStream(stream, context, .input);
    }

    fn fullDuplexStart(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        try self.startStream(stream, context, .full_duplex);
    }

    fn halfDuplexStart(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        try self.startStream(stream, context, .half_duplex);
    }

    fn startStream(
        self: *@This(),
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
        method: StreamMethod,
    ) !void {
        if (self.streams.contains(stream.context)) return error.DuplicateStream;

        // Advertising gzip permits per-message compressed responses when requested.
        context.setResponseCompression(.gzip);
        if (method == .full_duplex) try echoStreamingMetadata(context);

        const state = try self.allocator.create(StreamState);
        errdefer self.allocator.destroy(state);
        state.* = .{ .method = method };
        errdefer state.deinit(self.allocator);
        try self.streams.put(self.allocator, stream.context, state);
    }

    fn onMessage(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
        payload: []const u8,
        _: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        return self.handleMessage(stream, payload) catch |err| {
            self.removeState(stream);
            return err;
        };
    }

    fn handleMessage(
        self: *@This(),
        stream: grpc.ServerStream,
        payload: []const u8,
    ) !grpc.StreamReceiveAction {
        const state = self.streams.get(stream.context) orelse return error.UnknownStream;
        state.message_count += 1;

        switch (state.method) {
            .input => {
                var reader: std.Io.Reader = .fixed(payload);
                var request = testing.StreamingInputCallRequest.decode(&reader, self.allocator) catch {
                    try self.finishState(stream, .init(.invalid_argument, "invalid StreamingInputCallRequest"));
                    return .pause;
                };
                defer request.deinit(self.allocator);

                if (request.payload) |request_payload| {
                    state.aggregated_payload_size = std.math.add(
                        u64,
                        state.aggregated_payload_size,
                        @intCast(request_payload.body.len),
                    ) catch {
                        try self.finishState(stream, .init(.out_of_range, "aggregated payload size overflow"));
                        return .pause;
                    };
                    if (state.aggregated_payload_size > std.math.maxInt(i32)) {
                        try self.finishState(stream, .init(.out_of_range, "aggregated payload size exceeds int32"));
                        return .pause;
                    }
                }
                return .continue_receiving;
            },
            .output, .full_duplex, .half_duplex => {
                if (state.method == .output and state.message_count != 1) {
                    try self.finishState(stream, .init(.invalid_argument, "multiple StreamingOutputCallRequest messages"));
                    return .pause;
                }

                var reader: std.Io.Reader = .fixed(payload);
                var request = testing.StreamingOutputCallRequest.decode(&reader, self.allocator) catch {
                    try self.finishState(stream, .init(.invalid_argument, "invalid StreamingOutputCallRequest"));
                    return .pause;
                };
                defer request.deinit(self.allocator);

                if (state.method == .full_duplex) {
                    if (request.response_status) |response_status| {
                        if (response_status.code != 0) {
                            try self.finishState(stream, statusFromEcho(response_status));
                            return .pause;
                        }
                    }
                }

                if (state.method != .half_duplex) {
                    for (request.response_parameters.items) |parameter| {
                        if (parameter.size < 0) {
                            try self.finishState(stream, .init(.invalid_argument, "negative response size"));
                            return .pause;
                        }
                        if (request.response_type != .COMPRESSABLE) {
                            try self.finishState(stream, .init(.invalid_argument, "unsupported response payload type"));
                            return .pause;
                        }
                    }
                }
                for (request.response_parameters.items) |parameter| {
                    // Event-loop callbacks cannot sleep; interval_us is intentionally ignored.
                    try state.responses.append(self.allocator, .{
                        .payload_type = request.response_type,
                        .size = parameter.size,
                        .compression = if (parameter.compressed) |compressed|
                            if (compressed.value) .gzip else .identity
                        else
                            .identity,
                    });
                }

                if (state.method == .half_duplex) return .continue_receiving;
                if (!try self.drainResponses(stream, state)) {
                    state.receive_paused = true;
                    return .pause;
                }
                return .continue_receiving;
            },
        }
    }

    fn onRemoteEnd(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) !void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        self.handleRemoteEnd(stream) catch |err| {
            self.removeState(stream);
            return err;
        };
    }

    fn handleRemoteEnd(self: *@This(), stream: grpc.ServerStream) !void {
        const state = self.streams.get(stream.context) orelse return error.UnknownStream;
        state.remote_ended = true;
        if (state.method == .output and state.message_count == 0) {
            try self.finishState(stream, .init(.invalid_argument, "missing StreamingOutputCallRequest"));
            return;
        }
        if (state.method == .half_duplex) {
            for (state.responses.items) |spec| {
                if (spec.size < 0) {
                    try self.finishState(stream, .init(.invalid_argument, "negative response size"));
                    return;
                }
                if (spec.payload_type != .COMPRESSABLE) {
                    try self.finishState(stream, .init(.invalid_argument, "unsupported response payload type"));
                    return;
                }
            }
        }
        if (!try self.drainResponses(stream, state)) return;
        try self.finishState(stream, .ok);
    }

    fn onWritable(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        self.handleWritable(stream) catch {
            if (self.streams.contains(stream.context)) {
                stream.finish(.init(.internal, "stream continuation failed")) catch {};
                self.removeState(stream);
            }
        };
    }

    fn handleWritable(self: *@This(), stream: grpc.ServerStream) !void {
        const state = self.streams.get(stream.context) orelse return;
        if (!try self.drainResponses(stream, state)) return;
        if (state.remote_ended) {
            try self.finishState(stream, .ok);
            return;
        }
        if (state.receive_paused) {
            state.receive_paused = false;
            try stream.resumeReceive();
        }
    }

    fn onCancel(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
        self.removeState(stream);
    }

    fn drainResponses(
        self: *@This(),
        stream: grpc.ServerStream,
        state: *StreamState,
    ) !bool {
        if (state.method == .input) {
            if (!state.remote_ended or state.aggregate_sent) return true;
            const response: testing.StreamingInputCallResponse = .{
                .aggregated_payload_size = @intCast(state.aggregated_payload_size),
            };
            if (!try sendMessage(self.allocator, stream, response, .identity)) return false;
            state.aggregate_sent = true;
            return true;
        }

        while (state.next_response < state.responses.items.len) {
            const spec = state.responses.items[state.next_response];
            const body = try self.allocator.alloc(u8, @intCast(spec.size));
            defer self.allocator.free(body);
            @memset(body, 0);
            const response: testing.StreamingOutputCallResponse = .{
                .payload = .{
                    .type = spec.payload_type,
                    .body = body,
                },
            };
            if (!try sendMessage(self.allocator, stream, response, spec.compression)) return false;
            state.next_response += 1;
        }
        state.responses.clearRetainingCapacity();
        state.next_response = 0;
        return true;
    }

    fn finishState(self: *@This(), stream: grpc.ServerStream, final_status: grpc.Status) !void {
        try stream.finish(final_status);
        self.removeState(stream);
    }

    fn removeState(self: *@This(), stream: grpc.ServerStream) void {
        if (self.streams.fetchRemove(stream.context)) |entry| self.destroyState(entry.value);
    }

    fn destroyState(self: *@This(), state: *StreamState) void {
        state.deinit(self.allocator);
        self.allocator.destroy(state);
    }
};

fn sendMessage(
    allocator: std.mem.Allocator,
    stream: grpc.ServerStream,
    message: anytype,
    compression: grpc.Compression,
) !bool {
    var writer: std.Io.Writer.Allocating = .init(allocator);
    defer writer.deinit();
    try message.encode(&writer.writer, allocator);
    stream.send(writer.written(), .{ .compression = compression }) catch |err| {
        if (err == error.WouldBlock) return false;
        return err;
    };
    return true;
}

fn statusFromEcho(response_status: testing.EchoStatus) grpc.Status {
    const code = if (response_status.code >= 0)
        grpc.StatusCode.fromInt(@intCast(response_status.code))
    else
        .unknown;
    return .init(code, response_status.message);
}

fn echoStreamingMetadata(context: *grpc.ServerContext) !void {
    if (context.request_metadata.getFirst("x-grpc-test-echo-initial")) |value| {
        try context.addInitialMetadata("x-grpc-test-echo-initial", value);
    }
    if (context.request_metadata.getFirst("x-grpc-test-echo-trailing-bin")) |value| {
        try context.addTrailingMetadata("x-grpc-test-echo-trailing-bin", value);
    }
}

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const config = parseArgs(args) catch |err| {
        std.debug.print("invalid arguments: {s}\n", .{@errorName(err)});
        return err;
    };
    const certificate = if (config.use_tls)
        try std.Io.Dir.cwd().readFileAlloc(init.io, config.tls_cert_file, init.gpa, .limited(4 * 1024 * 1024))
    else
        null;
    defer if (certificate) |bytes| init.gpa.free(bytes);
    const private_key = if (config.use_tls)
        try std.Io.Dir.cwd().readFileAlloc(init.io, config.tls_key_file, init.gpa, .limited(4 * 1024 * 1024))
    else
        null;
    defer if (private_key) |bytes| init.gpa.free(bytes);

    var service = Service.init(init.gpa);
    defer service.deinit();
    var server = try grpc.Server.init(init.gpa, .{
        .host = "127.0.0.1",
        .port = config.port,
        .tls = if (certificate) |cert| .{
            .certificate_chain_pem = cert,
            .private_key_pem = private_key.?,
        } else null,
    });
    defer server.deinit();
    try server.registerUnary(
        "/grpc.testing.TestService/EmptyCall",
        grpc.UnaryHandler.bind(Service, &service, Service.emptyCall),
    );
    try server.registerUnary(
        "/grpc.testing.TestService/UnaryCall",
        grpc.UnaryHandler.bind(Service, &service, Service.unaryCall),
    );
    try server.registerStream(
        "/grpc.testing.TestService/StreamingOutputCall",
        .{
            .context = &service,
            .on_start = Service.streamingOutputStart,
            .on_message = Service.onMessage,
            .on_remote_end = Service.onRemoteEnd,
            .on_writable = Service.onWritable,
            .on_cancel = Service.onCancel,
        },
    );
    try server.registerStream(
        "/grpc.testing.TestService/StreamingInputCall",
        .{
            .context = &service,
            .on_start = Service.streamingInputStart,
            .on_message = Service.onMessage,
            .on_remote_end = Service.onRemoteEnd,
            .on_writable = Service.onWritable,
            .on_cancel = Service.onCancel,
        },
    );
    try server.registerStream(
        "/grpc.testing.TestService/FullDuplexCall",
        .{
            .context = &service,
            .on_start = Service.fullDuplexStart,
            .on_message = Service.onMessage,
            .on_remote_end = Service.onRemoteEnd,
            .on_writable = Service.onWritable,
            .on_cancel = Service.onCancel,
        },
    );
    try server.registerStream(
        "/grpc.testing.TestService/HalfDuplexCall",
        .{
            .context = &service,
            .on_start = Service.halfDuplexStart,
            .on_message = Service.onMessage,
            .on_remote_end = Service.onRemoteEnd,
            .on_writable = Service.onWritable,
            .on_cancel = Service.onCancel,
        },
    );
    try server.start();
    std.debug.print("grpc-lite interop server listening on 127.0.0.1:{d}\n", .{try server.port()});
    server.wait();
}

fn parseArgs(args: []const []const u8) !Config {
    var config: Config = .{};
    var index: usize = 1;
    while (index < args.len) : (index += 1) {
        const arg = args[index];
        if (std.mem.startsWith(u8, arg, "--port=")) {
            config.port = try std.fmt.parseInt(u16, arg["--port=".len..], 10);
        } else if (std.mem.eql(u8, arg, "--port")) {
            index += 1;
            if (index >= args.len) return error.MissingPort;
            config.port = try std.fmt.parseInt(u16, args[index], 10);
        } else if (std.mem.startsWith(u8, arg, "--use_tls=")) {
            config.use_tls = try parseBool(arg["--use_tls=".len..]);
        } else if (std.mem.eql(u8, arg, "--use_tls")) {
            config.use_tls = true;
        } else if (std.mem.startsWith(u8, arg, "--tls_cert_file=")) {
            config.tls_cert_file = arg["--tls_cert_file=".len..];
        } else if (std.mem.eql(u8, arg, "--tls_cert_file")) {
            index += 1;
            if (index >= args.len) return error.MissingTlsCertFile;
            config.tls_cert_file = args[index];
        } else if (std.mem.startsWith(u8, arg, "--tls_key_file=")) {
            config.tls_key_file = arg["--tls_key_file=".len..];
        } else if (std.mem.eql(u8, arg, "--tls_key_file")) {
            index += 1;
            if (index >= args.len) return error.MissingTlsKeyFile;
            config.tls_key_file = args[index];
        } else {
            return error.UnknownArgument;
        }
    }
    if (config.use_tls and config.tls_cert_file.len == 0) return error.MissingTlsCertFile;
    if (config.use_tls and config.tls_key_file.len == 0) return error.MissingTlsKeyFile;
    return config;
}

fn parseBool(value: []const u8) !bool {
    if (std.mem.eql(u8, value, "true")) return true;
    if (std.mem.eql(u8, value, "false")) return false;
    return error.InvalidBoolean;
}
