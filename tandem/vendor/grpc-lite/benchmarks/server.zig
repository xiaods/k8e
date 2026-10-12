const std = @import("std");
const demo = @import("demo_proto");
const grpc = @import("grpc_lite");
const grpc_pb = @import("grpc_lite_protobuf");

const Config = struct {
    host: []const u8 = "127.0.0.1",
    port: u16 = 50061,
    reactors: usize = 1,
};

const ServiceError = error{OutOfMemory};

const PendingMessage = struct {
    bytes: []u8,
    compression: grpc.Compression,
};

const Pending = union(enum) {
    raw: PendingMessage,
    typed_message: PendingMessage,

    fn deinit(self: Pending, allocator: std.mem.Allocator) void {
        switch (self) {
            inline else => |message| allocator.free(message.bytes),
        }
    }
};

const StreamState = struct {
    pending: ?Pending = null,
    remote_ended: bool = false,

    fn deinit(self: *StreamState, allocator: std.mem.Allocator) void {
        if (self.pending) |pending| pending.deinit(allocator);
        self.* = undefined;
    }
};

const Service = struct {
    allocator: std.mem.Allocator,
    streams: std.AutoHashMapUnmanaged(*anyopaque, *StreamState) = .empty,
    streams_mutex: std.Io.Mutex = .init,

    fn init(allocator: std.mem.Allocator) Service {
        return .{ .allocator = allocator };
    }

    fn deinit(self: *Service) void {
        var iterator = self.streams.valueIterator();
        while (iterator.next()) |state| self.destroyState(state.*);
        self.streams.deinit(self.allocator);
        self.* = undefined;
    }

    fn rawUnary(
        _: *Service,
        allocator: std.mem.Allocator,
        context: *grpc.ServerContext,
        payload: []const u8,
    ) !grpc.UnaryResponse {
        if (context.request_compression == .gzip) context.setResponseCompression(.gzip);
        return grpc.UnaryResponse.ok(allocator, payload);
    }

    fn typedUnary(self: *Service, request: demo.EchoRequest) ServiceError!demo.EchoReply {
        return .{ .message = try self.allocator.dupe(u8, request.message) };
    }

    fn configureTypedUnary(_: *Service, context: *grpc.ServerContext) !void {
        if (context.request_compression == .gzip) context.setResponseCompression(.gzip);
    }

    fn rawStart(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
    ) !void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        try self.startStream(stream, context);
    }

    fn typedStart(
        opaque_context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        context: *grpc.ServerContext,
    ) !void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        try self.startStream(stream.raw, context);
    }

    fn startStream(
        self: *Service,
        stream: grpc.ServerStream,
        context: *grpc.ServerContext,
    ) !void {
        // The transport only enables gzip when the peer advertised it.
        context.setResponseCompression(.gzip);
        const state = try self.allocator.create(StreamState);
        errdefer self.allocator.destroy(state);
        state.* = .{};
        self.lockStreams();
        defer self.unlockStreams();
        if (self.streams.contains(stream.context)) return error.DuplicateStream;
        try self.streams.put(self.allocator, stream.context, state);
    }

    fn rawMessage(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
        payload: []const u8,
        compression: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        return self.handleRawMessage(stream, payload, compression) catch |err| {
            self.removeState(stream);
            return err;
        };
    }

    fn handleRawMessage(
        self: *Service,
        stream: grpc.ServerStream,
        payload: []const u8,
        compression: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const state = self.getState(stream.context) orelse return error.UnknownStream;
        sendRaw(stream, payload, compression) catch |err| {
            if (err != error.WouldBlock) return err;
            state.pending = .{ .raw = .{
                .bytes = try self.allocator.dupe(u8, payload),
                .compression = compression,
            } };
            return .pause;
        };
        return .continue_receiving;
    }

    fn typedMessage(
        opaque_context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
        request: *const demo.EchoRequest,
        compression: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        return self.handleTypedMessage(stream, request.message, compression) catch |err| {
            self.removeState(stream.raw);
            return err;
        };
    }

    fn handleTypedMessage(
        self: *Service,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        message: []const u8,
        compression: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const state = self.getState(stream.raw.context) orelse return error.UnknownStream;
        sendTyped(stream, message, compression) catch |err| {
            if (err != error.WouldBlock) return err;
            state.pending = .{ .typed_message = .{
                .bytes = try self.allocator.dupe(u8, message),
                .compression = compression,
            } };
            return .pause;
        };
        return .continue_receiving;
    }

    fn rawRemoteEnd(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) !void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        self.handleRemoteEnd(stream) catch |err| {
            self.removeState(stream);
            return err;
        };
    }

    fn typedRemoteEnd(
        opaque_context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
    ) !void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        self.handleRemoteEnd(stream.raw) catch |err| {
            self.removeState(stream.raw);
            return err;
        };
    }

    fn handleRemoteEnd(self: *Service, stream: grpc.ServerStream) !void {
        const state = self.getState(stream.context) orelse return error.UnknownStream;
        state.remote_ended = true;
        if (state.pending == null) try self.finishState(stream);
    }

    fn rawWritable(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        self.handleRawWritable(stream) catch self.failContinuation(stream);
    }

    fn typedWritable(
        opaque_context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
    ) void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        self.handleTypedWritable(stream) catch self.failContinuation(stream.raw);
    }

    fn handleRawWritable(self: *Service, stream: grpc.ServerStream) !void {
        const state = self.getState(stream.context) orelse return;
        const pending = state.pending orelse return;
        const message = switch (pending) {
            .raw => |message| message,
            .typed_message => return error.PendingTypeMismatch,
        };
        sendRaw(stream, message.bytes, message.compression) catch |err| {
            if (err == error.WouldBlock) return;
            return err;
        };
        try self.pendingDrained(stream, state, pending);
    }

    fn handleTypedWritable(
        self: *Service,
        stream: grpc_pb.ServerStream(demo.EchoReply),
    ) !void {
        const state = self.getState(stream.raw.context) orelse return;
        const pending = state.pending orelse return;
        const message = switch (pending) {
            .typed_message => |message| message,
            .raw => return error.PendingTypeMismatch,
        };
        sendTyped(stream, message.bytes, message.compression) catch |err| {
            if (err == error.WouldBlock) return;
            return err;
        };
        try self.pendingDrained(stream.raw, state, pending);
    }

    fn pendingDrained(
        self: *Service,
        stream: grpc.ServerStream,
        state: *StreamState,
        pending: Pending,
    ) !void {
        state.pending = null;
        pending.deinit(self.allocator);
        if (state.remote_ended) {
            try self.finishState(stream);
        } else {
            try stream.resumeReceive();
        }
    }

    fn rawCancel(
        opaque_context: ?*anyopaque,
        stream: grpc.ServerStream,
        _: *grpc.ServerContext,
    ) void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        self.removeState(stream);
    }

    fn typedCancel(
        opaque_context: ?*anyopaque,
        stream: grpc_pb.ServerStream(demo.EchoReply),
        _: *grpc.ServerContext,
    ) void {
        const self: *Service = @ptrCast(@alignCast(opaque_context.?));
        self.removeState(stream.raw);
    }

    fn failContinuation(self: *Service, stream: grpc.ServerStream) void {
        if (self.getState(stream.context) == null) return;
        stream.finish(.init(.internal, "stream continuation failed")) catch {};
        self.removeState(stream);
    }

    fn finishState(self: *Service, stream: grpc.ServerStream) !void {
        try stream.finish(.ok);
        self.removeState(stream);
    }

    fn removeState(self: *Service, stream: grpc.ServerStream) void {
        self.lockStreams();
        const removed = self.streams.fetchRemove(stream.context);
        self.unlockStreams();
        if (removed) |entry| self.destroyState(entry.value);
    }

    fn destroyState(self: *Service, state: *StreamState) void {
        state.deinit(self.allocator);
        self.allocator.destroy(state);
    }

    fn getState(self: *Service, key: *anyopaque) ?*StreamState {
        // The transport serializes a stream's callbacks on its connection's owner
        // reactor, and this benchmark does not hand ServerStream to another thread.
        // The map lock protects different reactors; transport calls stay outside it.
        self.lockStreams();
        defer self.unlockStreams();
        return self.streams.get(key);
    }

    fn lockStreams(self: *Service) void {
        self.streams_mutex.lockUncancelable(std.Io.Threaded.global_single_threaded.io());
    }

    fn unlockStreams(self: *Service) void {
        self.streams_mutex.unlock(std.Io.Threaded.global_single_threaded.io());
    }
};

const EchoApi = demo.EchoService(Service, ServiceError);
const StreamingEchoApi = demo.StreamingEchoService(Service, ServiceError);

fn sendRaw(stream: grpc.ServerStream, payload: []const u8, compression: grpc.Compression) !void {
    stream.send(payload, .{ .compression = compression }) catch |err| {
        if (compression == .gzip and
            (err == error.CompressionNotAccepted or err == error.ResponseCompressionNotEnabled))
        {
            return stream.send(payload, .{ .compression = .identity });
        }
        return err;
    };
}

fn sendTyped(
    stream: grpc_pb.ServerStream(demo.EchoReply),
    message: []const u8,
    compression: grpc.Compression,
) !void {
    stream.send(.{ .message = message }, .{ .compression = compression }) catch |err| {
        if (compression == .gzip and
            (err == error.CompressionNotAccepted or err == error.ResponseCompressionNotEnabled))
        {
            return stream.send(.{ .message = message }, .{ .compression = .identity });
        }
        return err;
    };
}

fn mapServiceError(err: ServiceError) grpc.Status {
    return switch (err) {
        error.OutOfMemory => .init(.resource_exhausted, "allocation failed"),
    };
}

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const config = parseArgs(args) catch |err| {
        std.debug.print("invalid arguments: {s}\n", .{@errorName(err)});
        return err;
    };

    var service = Service.init(init.gpa);
    defer service.deinit();
    var unary_registration = grpc_pb.ServiceRegistration(EchoApi).init(
        init.gpa,
        &service,
        .{ .Echo = Service.typedUnary },
        .{
            .map_error = mapServiceError,
            .context_hook = Service.configureTypedUnary,
        },
    );
    defer unary_registration.deinit();
    var bidi_registration = grpc_pb.StreamRegistration(StreamingEchoApi, "Chat").init(
        init.gpa,
        .{
            .context = &service,
            .on_start = Service.typedStart,
            .on_message = Service.typedMessage,
            .on_remote_end = Service.typedRemoteEnd,
            .on_writable = Service.typedWritable,
            .on_cancel = Service.typedCancel,
        },
    );
    defer bidi_registration.deinit();
    var server = try grpc.Server.init(init.gpa, .{
        .host = config.host,
        .port = config.port,
        .reactor_count = config.reactors,
    });
    defer server.deinit();

    try server.registerUnary(
        "/grpc.lite.Benchmark/Unary",
        grpc.UnaryHandler.bind(Service, &service, Service.rawUnary),
    );
    try server.registerStream(
        "/grpc.lite.Benchmark/Bidi",
        .{
            .context = &service,
            .on_start = Service.rawStart,
            .on_message = Service.rawMessage,
            .on_remote_end = Service.rawRemoteEnd,
            .on_writable = Service.rawWritable,
            .on_cancel = Service.rawCancel,
        },
    );
    try unary_registration.register(&server);
    try bidi_registration.register(&server);
    try server.start();
    std.debug.print("grpc-lite benchmark server listening on {s}:{d}\n", .{
        config.host,
        try server.port(),
    });
    server.wait();
}

fn parseArgs(args: []const []const u8) !Config {
    var config: Config = .{};
    var host_seen = false;
    var port_seen = false;
    var reactors_seen = false;
    var index: usize = 1;
    while (index < args.len) : (index += 1) {
        const arg = args[index];
        if (std.mem.eql(u8, arg, "--host")) {
            if (host_seen) return error.DuplicateHost;
            host_seen = true;
            index += 1;
            if (index >= args.len or std.mem.startsWith(u8, args[index], "--")) return error.MissingHost;
            config.host = try parseHost(args[index]);
        } else if (std.mem.startsWith(u8, arg, "--host=")) {
            if (host_seen) return error.DuplicateHost;
            host_seen = true;
            config.host = try parseHost(arg["--host=".len..]);
        } else if (std.mem.eql(u8, arg, "--port")) {
            if (port_seen) return error.DuplicatePort;
            port_seen = true;
            index += 1;
            if (index >= args.len or std.mem.startsWith(u8, args[index], "--")) return error.MissingPort;
            config.port = try parsePort(args[index]);
        } else if (std.mem.startsWith(u8, arg, "--port=")) {
            if (port_seen) return error.DuplicatePort;
            port_seen = true;
            config.port = try parsePort(arg["--port=".len..]);
        } else if (std.mem.eql(u8, arg, "--reactors")) {
            if (reactors_seen) return error.DuplicateReactors;
            reactors_seen = true;
            index += 1;
            if (index >= args.len or std.mem.startsWith(u8, args[index], "--")) return error.MissingReactors;
            config.reactors = try parseReactors(args[index]);
        } else if (std.mem.startsWith(u8, arg, "--reactors=")) {
            if (reactors_seen) return error.DuplicateReactors;
            reactors_seen = true;
            config.reactors = try parseReactors(arg["--reactors=".len..]);
        } else {
            return error.UnknownArgument;
        }
    }
    return config;
}

fn parseHost(value: []const u8) ![]const u8 {
    if (value.len == 0 or std.mem.indexOfScalar(u8, value, 0) != null) return error.InvalidHost;
    _ = std.Io.net.IpAddress.parseIp4(value, 0) catch return error.InvalidHost;
    return value;
}

fn parsePort(value: []const u8) !u16 {
    if (value.len == 0) return error.InvalidPort;
    return std.fmt.parseInt(u16, value, 10) catch error.InvalidPort;
}

fn parseReactors(value: []const u8) !usize {
    if (value.len == 0) return error.InvalidReactorCount;
    const count = std.fmt.parseInt(usize, value, 10) catch return error.InvalidReactorCount;
    if (count == 0) return error.InvalidReactorCount;
    return count;
}

test "parse benchmark server defaults" {
    const config = try parseArgs(&.{"server"});
    try std.testing.expectEqualStrings("127.0.0.1", config.host);
    try std.testing.expectEqual(@as(u16, 50061), config.port);
    try std.testing.expectEqual(@as(usize, 1), config.reactors);
}

test "parse benchmark server options" {
    var config = try parseArgs(&.{ "server", "--host", "0.0.0.0", "--port=60000", "--reactors=4" });
    try std.testing.expectEqualStrings("0.0.0.0", config.host);
    try std.testing.expectEqual(@as(u16, 60000), config.port);
    try std.testing.expectEqual(@as(usize, 4), config.reactors);

    config = try parseArgs(&.{ "server", "--host=127.0.0.2", "--port", "0" });
    try std.testing.expectEqualStrings("127.0.0.2", config.host);
    try std.testing.expectEqual(@as(u16, 0), config.port);
}

test "reject malformed benchmark server options" {
    try std.testing.expectError(error.UnknownArgument, parseArgs(&.{ "server", "--other" }));
    try std.testing.expectError(error.MissingHost, parseArgs(&.{ "server", "--host", "--port", "1" }));
    try std.testing.expectError(error.InvalidHost, parseArgs(&.{ "server", "--host=localhost" }));
    try std.testing.expectError(error.MissingPort, parseArgs(&.{ "server", "--port" }));
    try std.testing.expectError(error.InvalidPort, parseArgs(&.{ "server", "--port=65536" }));
    try std.testing.expectError(error.DuplicateHost, parseArgs(&.{ "server", "--host=127.0.0.1", "--host=127.0.0.2" }));
    try std.testing.expectError(error.DuplicatePort, parseArgs(&.{ "server", "--port=1", "--port=2" }));
    try std.testing.expectError(error.InvalidReactorCount, parseArgs(&.{ "server", "--reactors=0" }));
    try std.testing.expectError(error.MissingReactors, parseArgs(&.{ "server", "--reactors" }));
    try std.testing.expectError(error.DuplicateReactors, parseArgs(&.{ "server", "--reactors=2", "--reactors=4" }));
}
