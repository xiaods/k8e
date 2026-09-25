//! Typed protobuf adapters for grpc-lite's raw unary and streaming transports.

const std = @import("std");
const grpc = @import("grpc_lite");

pub const runtime = @import("protobuf");

pub const MethodKind = enum {
    unary,
    client_streaming,
    server_streaming,
    bidirectional_streaming,
};

threadlocal var active_client_stream_state: ?*anyopaque = null;

/// Borrowed typed command view passed to client callbacks.
pub fn ClientStreamView(comptime Request: type) type {
    return struct {
        raw: grpc.ClientStream,

        pub fn send(
            self: @This(),
            allocator: std.mem.Allocator,
            request: Request,
            options: grpc.StreamSendOptions,
        ) !void {
            try sendMessage(allocator, self.raw, request, options);
        }

        pub fn closeSend(self: @This()) !void {
            try self.raw.closeSend();
        }

        pub fn cancel(self: @This()) void {
            self.raw.cancel();
        }

        pub fn resumeReceive(self: @This()) !void {
            try self.raw.resumeReceive();
        }
    };
}

/// Event-driven typed client callbacks. Decoded responses are borrowed for the callback.
pub fn ClientStreamCallbacks(comptime Request: type, comptime Response: type) type {
    const View = ClientStreamView(Request);
    return struct {
        context: ?*anyopaque = null,
        on_headers: ?*const fn (?*anyopaque, View, *const grpc.Metadata) anyerror!void = null,
        on_message: *const fn (
            ?*anyopaque,
            View,
            *const Response,
            grpc.Compression,
        ) anyerror!grpc.StreamReceiveAction,
        on_remote_end: ?*const fn (?*anyopaque, View) anyerror!void = null,
        on_writable: ?*const fn (?*anyopaque, View) anyerror!void = null,
        on_terminal: *const fn (
            ?*anyopaque,
            View,
            grpc.Status,
            *const grpc.Metadata,
            ?anyerror,
        ) void,
    };
}

/// Owning typed client stream. Callback views are borrowed and must not be deinitialized.
/// The callback allocator must remain valid until deinit and, when deinit is called from
/// a callback, until that callback returns. Shared allocators must be thread-safe.
pub fn ClientStream(comptime Request: type, comptime Response: type) type {
    const View = ClientStreamView(Request);
    const Callbacks = ClientStreamCallbacks(Request, Response);
    const State = struct {
        allocator: std.mem.Allocator,
        callbacks: Callbacks,
        local_error: ?anyerror = null,
        destroy_after_callback: bool = false,

        fn beginCallback(self: *@This()) void {
            std.debug.assert(active_client_stream_state == null);
            active_client_stream_state = @ptrCast(self);
        }

        fn endCallback(self: *@This()) void {
            std.debug.assert(active_client_stream_state == @as(*anyopaque, @ptrCast(self)));
            active_client_stream_state = null;
            if (self.destroy_after_callback) {
                const allocator = self.allocator;
                allocator.destroy(self);
            }
        }

        fn fail(self: *@This(), raw: grpc.ClientStream, err: anyerror) void {
            if (self.local_error == null) self.local_error = err;
            raw.cancel();
        }

        fn onHeaders(
            opaque_context: ?*anyopaque,
            raw: grpc.ClientStream,
            headers: *const grpc.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            self.beginCallback();
            defer self.endCallback();
            const callback = self.callbacks.on_headers orelse return;
            callback(self.callbacks.context, .{ .raw = raw }, headers) catch |err| {
                self.fail(raw, err);
            };
        }

        fn onMessage(
            opaque_context: ?*anyopaque,
            raw: grpc.ClientStream,
            payload: []const u8,
            compression: grpc.Compression,
        ) grpc.StreamReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            self.beginCallback();
            defer self.endCallback();

            var reader: std.Io.Reader = .fixed(payload);
            var arena: std.heap.ArenaAllocator = .init(self.allocator);
            defer arena.deinit();
            const response = Response.decode(&reader, arena.allocator()) catch |err| {
                self.fail(raw, err);
                return .continue_receiving;
            };
            return self.callbacks.on_message(
                self.callbacks.context,
                .{ .raw = raw },
                &response,
                compression,
            ) catch |err| {
                self.fail(raw, err);
                return .continue_receiving;
            };
        }

        fn onRemoteEnd(opaque_context: ?*anyopaque, raw: grpc.ClientStream) void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            self.beginCallback();
            defer self.endCallback();
            const callback = self.callbacks.on_remote_end orelse return;
            callback(self.callbacks.context, .{ .raw = raw }) catch |err| {
                self.fail(raw, err);
            };
        }

        fn onWritable(opaque_context: ?*anyopaque, raw: grpc.ClientStream) void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            self.beginCallback();
            defer self.endCallback();
            const callback = self.callbacks.on_writable orelse return;
            callback(self.callbacks.context, .{ .raw = raw }) catch |err| {
                self.fail(raw, err);
            };
        }

        fn onTerminal(
            opaque_context: ?*anyopaque,
            raw: grpc.ClientStream,
            final_status: grpc.Status,
            trailers: *const grpc.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            self.beginCallback();
            defer self.endCallback();
            self.callbacks.on_terminal(
                self.callbacks.context,
                .{ .raw = raw },
                final_status,
                trailers,
                self.local_error,
            );
        }
    };

    return struct {
        raw: grpc.ClientStream,
        state: *State,

        fn open(
            allocator: std.mem.Allocator,
            channel: *grpc.Channel,
            path: []const u8,
            options: grpc.StreamOptions,
            callbacks: Callbacks,
        ) !@This() {
            const state = try allocator.create(State);
            errdefer allocator.destroy(state);
            state.* = .{ .allocator = allocator, .callbacks = callbacks };
            const raw = try channel.openStream(path, options, .{
                .context = state,
                .on_headers = State.onHeaders,
                .on_message = State.onMessage,
                .on_remote_end = State.onRemoteEnd,
                .on_writable = State.onWritable,
                .on_terminal = State.onTerminal,
            });
            return .{ .raw = raw, .state = state };
        }

        pub fn send(
            self: @This(),
            allocator: std.mem.Allocator,
            request: Request,
            options: grpc.StreamSendOptions,
        ) !void {
            try (View{ .raw = self.raw }).send(allocator, request, options);
        }

        pub fn closeSend(self: @This()) !void {
            try self.raw.closeSend();
        }

        pub fn cancel(self: @This()) void {
            self.raw.cancel();
        }

        pub fn resumeReceive(self: @This()) !void {
            try self.raw.resumeReceive();
        }

        pub fn deinit(self: *@This()) void {
            const state = self.state;
            const called_from_callback = active_client_stream_state == @as(*anyopaque, @ptrCast(state));
            self.raw.deinit();
            self.* = undefined;
            if (called_from_callback) {
                state.destroy_after_callback = true;
            } else {
                state.allocator.destroy(state);
            }
        }
    };
}

/// Borrowed typed server command view, valid only while the raw server stream is active.
pub fn ServerStream(comptime Response: type) type {
    return struct {
        raw: grpc.ServerStream,
        allocator: std.mem.Allocator,

        pub fn send(self: @This(), response: Response, options: grpc.StreamSendOptions) !void {
            try sendMessage(self.allocator, self.raw, response, options);
        }

        pub fn finish(self: @This(), final_status: grpc.Status) !void {
            try self.raw.finish(final_status);
        }

        pub fn resumeReceive(self: @This()) !void {
            try self.raw.resumeReceive();
        }
    };
}

/// Event-driven typed server handler. Requests are borrowed for on_message only.
pub fn ServerStreamHandler(comptime Request: type, comptime Response: type) type {
    const Stream = ServerStream(Response);
    return struct {
        context: ?*anyopaque = null,
        on_start: *const fn (?*anyopaque, Stream, *grpc.ServerContext) anyerror!void,
        on_message: *const fn (
            ?*anyopaque,
            Stream,
            *grpc.ServerContext,
            *const Request,
            grpc.Compression,
        ) anyerror!grpc.StreamReceiveAction,
        on_remote_end: *const fn (?*anyopaque, Stream, *grpc.ServerContext) anyerror!void,
        on_writable: ?*const fn (?*anyopaque, Stream, *grpc.ServerContext) void = null,
        on_cancel: ?*const fn (?*anyopaque, Stream, *grpc.ServerContext) void = null,
    };
}

/// Registers one generated streaming method with an event-driven typed handler.
/// The registration, handler context, and allocator must retain stable addresses and
/// outlive the server and all active streams.
pub fn StreamRegistration(comptime Service: type, comptime method: []const u8) type {
    comptime {
        validateService(Service);
        if (methodKind(Service, method) == .unary) {
            @compileError("expected a streaming protobuf method: " ++ method);
        }
    }
    const Request = RequestType(Service, method);
    const Response = ResponseType(Service, method);
    const Stream = ServerStream(Response);
    const Handler = ServerStreamHandler(Request, Response);

    return struct {
        allocator: std.mem.Allocator,
        handler: Handler,

        pub fn init(allocator: std.mem.Allocator, handler: Handler) @This() {
            return .{ .allocator = allocator, .handler = handler };
        }

        pub fn deinit(self: *@This()) void {
            self.* = undefined;
        }

        pub fn register(self: *@This(), server: *grpc.Server) !void {
            try server.registerStream(methodPath(Service, method), .{
                .context = self,
                .on_start = onStart,
                .on_message = onMessage,
                .on_remote_end = onRemoteEnd,
                .on_writable = onWritable,
                .on_cancel = onCancel,
            });
        }

        fn view(self: *@This(), raw: grpc.ServerStream) Stream {
            return .{ .raw = raw, .allocator = self.allocator };
        }

        fn onStart(
            opaque_context: ?*anyopaque,
            raw: grpc.ServerStream,
            server_context: *grpc.ServerContext,
        ) !void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            try self.handler.on_start(self.handler.context, self.view(raw), server_context);
        }

        fn onMessage(
            opaque_context: ?*anyopaque,
            raw: grpc.ServerStream,
            server_context: *grpc.ServerContext,
            payload: []const u8,
            compression: grpc.Compression,
        ) !grpc.StreamReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            var reader: std.Io.Reader = .fixed(payload);
            var arena: std.heap.ArenaAllocator = .init(self.allocator);
            defer arena.deinit();
            const request = Request.decode(&reader, arena.allocator()) catch |err| {
                if (err == error.OutOfMemory) return err;
                try raw.finish(.init(.invalid_argument, "invalid protobuf request"));
                return .pause;
            };
            return self.handler.on_message(
                self.handler.context,
                self.view(raw),
                server_context,
                &request,
                compression,
            );
        }

        fn onRemoteEnd(
            opaque_context: ?*anyopaque,
            raw: grpc.ServerStream,
            server_context: *grpc.ServerContext,
        ) !void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            try self.handler.on_remote_end(self.handler.context, self.view(raw), server_context);
        }

        fn onWritable(
            opaque_context: ?*anyopaque,
            raw: grpc.ServerStream,
            server_context: *grpc.ServerContext,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            const callback = self.handler.on_writable orelse return;
            callback(self.handler.context, self.view(raw), server_context);
        }

        fn onCancel(
            opaque_context: ?*anyopaque,
            raw: grpc.ServerStream,
            server_context: *grpc.ServerContext,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(opaque_context.?));
            const callback = self.handler.on_cancel orelse return;
            callback(self.handler.context, self.view(raw), server_context);
        }
    };
}

/// A decoded protobuf response paired with the underlying gRPC result.
pub fn TypedResult(comptime Response: type) type {
    return struct {
        allocator: std.mem.Allocator,
        raw: grpc.CallResult,
        response: ?Response,
        response_arena: ?std.heap.ArenaAllocator,

        pub fn deinit(self: *@This()) void {
            if (self.response_arena) |*arena| arena.deinit();
            self.raw.deinit();
            self.* = undefined;
        }
    };
}

/// Creates a typed unary client from a generated zig-protobuf service type.
pub fn ServiceClient(comptime Service: type) type {
    comptime validateService(Service);

    return struct {
        channel: *grpc.Channel,

        pub fn init(channel: *grpc.Channel) @This() {
            return .{ .channel = channel };
        }

        pub fn callUnary(
            self: *@This(),
            allocator: std.mem.Allocator,
            comptime method: []const u8,
            request: RequestType(Service, method),
            options: grpc.CallOptions,
        ) !TypedResult(ResponseType(Service, method)) {
            comptime if (methodKind(Service, method) != .unary) {
                @compileError("expected a unary protobuf method: " ++ method);
            };
            const Response = ResponseType(Service, method);
            var writer: std.Io.Writer.Allocating = .init(allocator);
            defer writer.deinit();
            try request.encode(&writer.writer, allocator);

            var raw = try self.channel.callUnary(
                allocator,
                methodPath(Service, method),
                writer.written(),
                options,
            );
            errdefer raw.deinit();

            var response: ?Response = null;
            var response_arena: ?std.heap.ArenaAllocator = null;
            if (raw.status.isOk()) {
                var reader: std.Io.Reader = .fixed(raw.payload);
                var arena: std.heap.ArenaAllocator = .init(allocator);
                errdefer arena.deinit();
                response = try Response.decode(&reader, arena.allocator());
                response_arena = arena;
            }
            return .{
                .allocator = allocator,
                .raw = raw,
                .response = response,
                .response_arena = response_arena,
            };
        }

        pub fn openStream(
            self: *@This(),
            allocator: std.mem.Allocator,
            comptime method: []const u8,
            options: grpc.StreamOptions,
            callbacks: ClientStreamCallbacks(
                RequestType(Service, method),
                ResponseType(Service, method),
            ),
        ) !ClientStream(RequestType(Service, method), ResponseType(Service, method)) {
            comptime if (methodKind(Service, method) == .unary) {
                @compileError("expected a streaming protobuf method: " ++ method);
            };
            const Stream = ClientStream(RequestType(Service, method), ResponseType(Service, method));
            return Stream.open(
                allocator,
                self.channel,
                methodPath(Service, method),
                options,
                callbacks,
            );
        }
    };
}

/// Adapts and registers every unary method in a generated service VTable.
/// The registration and userdata must outlive the server and all active calls.
pub fn ServiceRegistration(comptime Service: type) type {
    comptime validateService(Service);
    const UserData = UserDataType(Service);
    const ServiceError = ErrorSetType(Service);

    return struct {
        pub const ErrorMapper = *const fn (ServiceError) grpc.Status;
        pub const ContextHook = *const fn (*UserData, *grpc.ServerContext) anyerror!void;
        pub const Options = struct {
            map_error: ?ErrorMapper = null,
            context_hook: ?ContextHook = null,
        };

        allocator: std.mem.Allocator,
        userdata: *UserData,
        vtable: Service,
        options: Options,

        pub fn init(
            allocator: std.mem.Allocator,
            userdata: *UserData,
            vtable: Service,
            options: Options,
        ) @This() {
            return .{
                .allocator = allocator,
                .userdata = userdata,
                .vtable = vtable,
                .options = options,
            };
        }

        pub fn deinit(self: *@This()) void {
            self.* = undefined;
        }

        pub fn register(self: *@This(), server: *grpc.Server) !void {
            inline for (@typeInfo(Service).@"struct".fields) |field| {
                if (methodKind(Service, field.name) == .unary) {
                    try server.registerUnary(
                        methodPath(Service, field.name),
                        handlerFor(field.name, self),
                    );
                }
            }
        }

        fn handlerFor(comptime method: []const u8, self: *@This()) grpc.UnaryHandler {
            const Registration = @This();
            return .{
                .context = self,
                .invoke_fn = struct {
                    fn invoke(
                        opaque_context: ?*anyopaque,
                        _: std.mem.Allocator,
                        server_context: *grpc.ServerContext,
                        request_bytes: []const u8,
                    ) anyerror!grpc.UnaryResponse {
                        const registration: *Registration = @ptrCast(@alignCast(opaque_context.?));
                        return registration.invokeMethod(method, server_context, request_bytes);
                    }
                }.invoke,
            };
        }

        fn invokeMethod(
            self: *@This(),
            comptime method: []const u8,
            server_context: *grpc.ServerContext,
            request_bytes: []const u8,
        ) !grpc.UnaryResponse {
            const Request = RequestType(Service, method);
            var reader: std.Io.Reader = .fixed(request_bytes);
            var arena: std.heap.ArenaAllocator = .init(self.allocator);
            defer arena.deinit();
            const request = Request.decode(&reader, arena.allocator()) catch |err| {
                if (err == error.OutOfMemory) return err;
                return grpc.UnaryResponse.fail(
                    self.allocator,
                    .init(.invalid_argument, "invalid protobuf request"),
                );
            };

            if (self.options.context_hook) |hook| {
                hook(self.userdata, server_context) catch {
                    return grpc.UnaryResponse.fail(
                        self.allocator,
                        .init(.internal, "protobuf context hook failed"),
                    );
                };
            }

            var response = @field(self.vtable, method)(self.userdata, request) catch |err| {
                const mapped = if (self.options.map_error) |mapper|
                    mapper(err)
                else
                    grpc.Status.init(.internal, @errorName(err));
                return grpc.UnaryResponse.fail(self.allocator, mapped);
            };
            defer response.deinit(self.allocator);

            var writer: std.Io.Writer.Allocating = .init(self.allocator);
            defer writer.deinit();
            try response.encode(&writer.writer, self.allocator);
            return grpc.UnaryResponse.ok(self.allocator, writer.written());
        }
    };
}

/// Returns the canonical gRPC method path for a generated service method.
pub fn methodPath(comptime Service: type, comptime method: []const u8) []const u8 {
    comptime validateMethod(Service, method);
    if (Service.package.len == 0) return "/" ++ Service.service_name ++ "/" ++ method;
    return "/" ++ Service.package ++ "." ++ Service.service_name ++ "/" ++ method;
}

/// Returns the generated protobuf request type for a service method.
pub fn RequestType(comptime Service: type, comptime method: []const u8) type {
    const function = MethodFunction(Service, method);
    const parameter = @typeInfo(function).@"fn".params[1].type.?;
    return if (@typeInfo(parameter) == .pointer)
        QueueElementType(parameter)
    else
        parameter;
}

/// Returns the generated protobuf response type for a service method.
pub fn ResponseType(comptime Service: type, comptime method: []const u8) type {
    const function = MethodFunction(Service, method);
    const info = @typeInfo(function).@"fn";
    if (info.params.len == 3) return QueueElementType(info.params[2].type.?);
    const return_type = @typeInfo(function).@"fn".return_type.?;
    return @typeInfo(return_type).error_union.payload;
}

/// Returns the request/response streaming cardinality for a generated method.
pub fn methodKind(comptime Service: type, comptime method: []const u8) MethodKind {
    const info = @typeInfo(MethodFunction(Service, method)).@"fn";
    const streams_requests = @typeInfo(info.params[1].type.?) == .pointer;
    const streams_responses = info.params.len == 3;
    if (streams_requests and streams_responses) return .bidirectional_streaming;
    if (streams_requests) return .client_streaming;
    if (streams_responses) return .server_streaming;
    return .unary;
}

fn sendMessage(
    allocator: std.mem.Allocator,
    raw: anytype,
    message: anytype,
    options: grpc.StreamSendOptions,
) !void {
    var writer: std.Io.Writer.Allocating = .init(allocator);
    defer writer.deinit();
    try message.encode(&writer.writer, allocator);
    try raw.send(writer.written(), options);
}

fn UserDataType(comptime Service: type) type {
    const fields = @typeInfo(Service).@"struct".fields;
    const function = @typeInfo(fields[0].type).pointer.child;
    const userdata_pointer = @typeInfo(function).@"fn".params[0].type.?;
    return @typeInfo(userdata_pointer).pointer.child;
}

fn ErrorSetType(comptime Service: type) type {
    const fields = @typeInfo(Service).@"struct".fields;
    const function = @typeInfo(fields[0].type).pointer.child;
    const return_type = @typeInfo(function).@"fn".return_type.?;
    return @typeInfo(return_type).error_union.error_set;
}

fn MethodFunction(comptime Service: type, comptime method: []const u8) type {
    comptime validateMethod(Service, method);
    return @typeInfo(@FieldType(Service, method)).pointer.child;
}

fn validateService(comptime Service: type) void {
    if (!@hasDecl(Service, "package") or !@hasDecl(Service, "service_name")) {
        @compileError("expected a zig-protobuf generated service type");
    }
    const fields = @typeInfo(Service).@"struct".fields;
    if (fields.len == 0) @compileError("protobuf service has no methods");

    const userdata = UserDataType(Service);
    const errors = ErrorSetType(Service);
    inline for (fields) |field| {
        validateMethod(Service, field.name);
        const function = @typeInfo(field.type).pointer.child;
        const info = @typeInfo(function).@"fn";
        const field_userdata = @typeInfo(info.params[0].type.?).pointer.child;
        const field_errors = @typeInfo(info.return_type.?).error_union.error_set;
        if (field_userdata != userdata or field_errors != errors) {
            @compileError("protobuf service methods must share userdata and error types");
        }
    }
}

fn validateMethod(comptime Service: type, comptime method: []const u8) void {
    if (!@hasField(Service, method)) @compileError("protobuf service has no method named " ++ method);
    const field_type = @FieldType(Service, method);
    if (@typeInfo(field_type) != .pointer or @typeInfo(@typeInfo(field_type).pointer.child) != .@"fn") {
        @compileError("protobuf service field is not a function pointer: " ++ method);
    }
    const info = @typeInfo(@typeInfo(field_type).pointer.child).@"fn";
    if (info.params.len != 2 and info.params.len != 3) {
        @compileError("unsupported protobuf method signature: " ++ method);
    }
    if (@typeInfo(info.return_type.?) != .error_union) {
        @compileError("protobuf method must return an error union: " ++ method);
    }
    const request_parameter = info.params[1].type.?;
    if (@typeInfo(request_parameter) == .pointer) _ = QueueElementType(request_parameter);
    const return_payload = @typeInfo(info.return_type.?).error_union.payload;
    if (info.params.len == 3) {
        _ = QueueElementType(info.params[2].type.?);
        if (return_payload != void) {
            @compileError("streaming-response protobuf method must return void: " ++ method);
        }
    } else if (return_payload == void) {
        @compileError("protobuf method without a response stream must return a message: " ++ method);
    }
}

fn QueueElementType(comptime Pointer: type) type {
    if (@typeInfo(Pointer) != .pointer) @compileError("expected a protobuf queue pointer");
    const Queue = @typeInfo(Pointer).pointer.child;
    if (!@hasDecl(Queue, "putOne")) @compileError("expected a protobuf queue pointer");
    const put_info = @typeInfo(@TypeOf(Queue.putOne)).@"fn";
    if (put_info.params.len != 3) @compileError("unsupported protobuf queue type");
    return put_info.params[2].type.?;
}
