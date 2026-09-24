const std = @import("std");
const call = @import("call.zig");
const channel = @import("channel.zig");
const Compression = @import("compression.zig").Compression;
const event_logger = @import("logger.zig");
const metadata = @import("metadata.zig");
const Runtime = @import("runtime.zig").Runtime;
const raw_stream = @import("stream.zig");
const Server = @import("server.zig").Server;
const ServerContext = @import("service.zig").ServerContext;
const StatusCode = @import("status.zig").Code;
const Status = @import("status.zig").Status;
const version = @import("version.zig");

pub const abi_major: u16 = 1;
pub const abi_minor: u16 = 6;

pub const Error = enum(i32) {
    ok = 0,
    invalid_argument = 1,
    invalid_state = 2,
    out_of_memory = 3,
    unsupported = 4,
    unavailable = 5,
    out_of_range = 6,
    closed = 7,
    would_block = 8,
    internal = 255,
};

pub const Feature = struct {
    pub const raw_unary: u64 = 1 << 0;
    pub const streaming: u64 = 1 << 1;
    pub const gzip: u64 = 1 << 2;
    pub const dns: u64 = 1 << 3;
    pub const tls: u64 = 1 << 4;
    pub const graceful_server_drain: u64 = 1 << 5;
    pub const c_streaming: u64 = 1 << 6;
    pub const c_server: u64 = 1 << 7;
    pub const managed_channel: u64 = 1 << 8;
    pub const logging_callback: u64 = 1 << 9;
};

pub const BytesView = event_logger.BytesView;

pub const Logger = extern struct {
    struct_size: usize = @sizeOf(Logger),
    user_data: ?*anyopaque = null,
    log: ?event_logger.Callback = null,
};

pub const MetadataEntryView = extern struct {
    key: BytesView = .{},
    value: BytesView = .{},
};

pub const RuntimeHandle = opaque {};
pub const MetadataHandle = opaque {};
pub const ChannelHandle = opaque {};
pub const UnaryResultHandle = opaque {};
pub const MetadataViewHandle = opaque {};
pub const ClientStreamHandle = opaque {};
pub const ServerHandle = opaque {};
pub const ServerStreamHandle = opaque {};
pub const ServerCallHandle = opaque {};
pub const ServerContextHandle = opaque {};

pub const UnaryOptions = extern struct {
    struct_size: usize = @sizeOf(UnaryOptions),
    metadata: ?*const MetadataHandle = null,
    has_timeout: u32 = 0,
    request_compression: u32 = 0,
    timeout_ns: u64 = 0,
    max_response_size: u64 = call.default_max_message_size,
};

pub const ChannelOptions = extern struct {
    struct_size: usize = @sizeOf(ChannelOptions),
    allow_initial_offline: u32 = 0,
    initial_backoff_ns: u64 = std.time.ns_per_s,
    max_backoff_ns: u64 = 120 * std.time.ns_per_s,
    multiplier_millis: u32 = 1600,
    jitter_percent: u32 = 20,
    logger: ?*const Logger = null,
    max_response_header_list_size: u64 = 64 * 1024,
};

pub const ClientStreamOptions = extern struct {
    struct_size: usize = @sizeOf(ClientStreamOptions),
    metadata: ?*const MetadataHandle = null,
    has_timeout: u32 = 0,
    send_compression: u32 = 0,
    timeout_ns: u64 = 0,
    max_message_size: u64 = call.default_max_message_size,
    max_inbound_buffer_size: u64 = 8 * 1024 * 1024,
    max_outbound_buffer_size: u64 = 8 * 1024 * 1024,
};

pub const ClientStreamCallbacks = extern struct {
    struct_size: usize = @sizeOf(ClientStreamCallbacks),
    user_data: ?*anyopaque = null,
    on_headers: ?*const fn (?*anyopaque, *ClientStreamHandle, *const MetadataViewHandle) callconv(.c) void = null,
    on_message: ?*const fn (?*anyopaque, *ClientStreamHandle, BytesView, u32) callconv(.c) u32 = null,
    on_remote_end: ?*const fn (?*anyopaque, *ClientStreamHandle) callconv(.c) void = null,
    on_writable: ?*const fn (?*anyopaque, *ClientStreamHandle) callconv(.c) void = null,
    on_terminal: ?*const fn (?*anyopaque, *ClientStreamHandle, i32, BytesView, *const MetadataViewHandle) callconv(.c) void = null,
};

pub const ServerOptions = extern struct {
    struct_size: usize = @sizeOf(ServerOptions),
    host: BytesView = .{ .data = "127.0.0.1".ptr, .size = "127.0.0.1".len },
    port: u32 = 0,
    reactor_count: u32 = 1,
    max_message_size: u64 = call.default_max_message_size,
    max_inbound_buffer_size: u64 = 8 * 1024 * 1024,
    max_outbound_buffer_size: u64 = 8 * 1024 * 1024,
    logger: ?*const Logger = null,
    max_connections: u64 = 1024,
    cleartext_preface_timeout_ns: u64 = 10 * std.time.ns_per_s,
    connection_idle_timeout_ns: u64 = 5 * 60 * std.time.ns_per_s,
    max_concurrent_streams_per_connection: u32 = 100,
};

pub const ServerMethodOptions = extern struct {
    struct_size: usize = @sizeOf(ServerMethodOptions),
    receive_initially_paused: u32 = 0,
    explicit_initial_metadata: u32 = 1,
};

pub const ServerMethodCallbacks = extern struct {
    struct_size: usize = @sizeOf(ServerMethodCallbacks),
    user_data: ?*anyopaque = null,
    on_start: ?*const fn (?*anyopaque, *ServerStreamHandle, *const ServerContextHandle) callconv(.c) void = null,
    on_message: ?*const fn (?*anyopaque, *ServerStreamHandle, *const ServerContextHandle, BytesView, u32) callconv(.c) u32 = null,
    on_remote_end: ?*const fn (?*anyopaque, *ServerStreamHandle, *const ServerContextHandle) callconv(.c) void = null,
    on_writable: ?*const fn (?*anyopaque, *ServerStreamHandle, *const ServerContextHandle) callconv(.c) void = null,
    on_cancel: ?*const fn (?*anyopaque, *ServerStreamHandle, *const ServerContextHandle) callconv(.c) void = null,
    on_terminal: ?*const fn (?*anyopaque, usize, u32) callconv(.c) void = null,
};

const RuntimeStorage = struct {
    value: Runtime,
};

const MetadataStorage = struct {
    value: metadata.Metadata,
};

const ChannelStorage = struct { value: channel.Channel };
const UnaryResultStorage = struct { value: call.Result };
const ClientStreamStorage = struct {
    callbacks: ClientStreamCallbacks,
    value: raw_stream.ClientStream,
    ready: std.atomic.Value(bool),
};
const ServerStorage = struct {
    value: Server,
    methods: std.ArrayListUnmanaged(*ServerMethodStorage) = .empty,
};
const ServerMethodStorage = struct {
    callbacks: ServerMethodCallbacks,
};
const BorrowedServerStreamStorage = struct {
    value: raw_stream.ServerStream,
};
const ServerCallStorage = struct {
    value: raw_stream.ServerCall,
};

const client_stream_options_v1_size = @offsetOf(ClientStreamOptions, "max_outbound_buffer_size") + @sizeOf(u64);
const client_stream_callbacks_v1_size = @offsetOf(ClientStreamCallbacks, "on_terminal") + @sizeOf(?*const fn (?*anyopaque, *ClientStreamHandle, i32, BytesView, *const MetadataViewHandle) callconv(.c) void);
const server_options_v1_size = @offsetOf(ServerOptions, "max_outbound_buffer_size") + @sizeOf(u64);
const server_options_logger_size = @offsetOf(ServerOptions, "logger") + @sizeOf(?*const Logger);
const server_options_max_connections_size = @offsetOf(ServerOptions, "max_connections") + @sizeOf(u64);
const server_options_preface_timeout_size = @offsetOf(ServerOptions, "cleartext_preface_timeout_ns") + @sizeOf(u64);
const server_options_idle_timeout_size = @offsetOf(ServerOptions, "connection_idle_timeout_ns") + @sizeOf(u64);
const server_options_max_streams_size = @offsetOf(ServerOptions, "max_concurrent_streams_per_connection") + @sizeOf(u32);
const server_method_options_v1_size = @offsetOf(ServerMethodOptions, "explicit_initial_metadata") + @sizeOf(u32);
const server_method_callbacks_v1_size = @offsetOf(ServerMethodCallbacks, "on_terminal") + @sizeOf(?*const fn (?*anyopaque, usize, u32) callconv(.c) void);
const channel_options_v1_size = @offsetOf(ChannelOptions, "jitter_percent") + @sizeOf(u32);
const channel_options_v2_size = @offsetOf(ChannelOptions, "logger") + @sizeOf(?*const Logger);
const channel_options_v3_size = @offsetOf(ChannelOptions, "max_response_header_list_size") + @sizeOf(u64);

const allocator = std.heap.c_allocator;

const ChannelCreateOptions = struct {
    reconnect: ?channel.ReconnectOptions = null,
    logger: event_logger.Logger = .{},
    max_response_header_list_size: u32 = 64 * 1024,
};

pub fn grpc_lite_abi_version() callconv(.c) u32 {
    return (@as(u32, abi_major) << 16) | abi_minor;
}

pub fn grpc_lite_library_version() callconv(.c) [*:0]const u8 {
    return version.string ++ "\x00";
}

pub fn grpc_lite_features() callconv(.c) u64 {
    return Feature.raw_unary |
        Feature.streaming |
        Feature.gzip |
        Feature.dns |
        Feature.graceful_server_drain |
        Feature.c_streaming |
        Feature.c_server |
        Feature.managed_channel |
        Feature.logging_callback;
}

pub fn grpc_lite_error_string(error_code: i32) callconv(.c) [*:0]const u8 {
    const value = std.enums.fromInt(Error, error_code) orelse return "unknown error";
    return switch (value) {
        .ok => "ok",
        .invalid_argument => "invalid argument",
        .invalid_state => "invalid state",
        .out_of_memory => "out of memory",
        .unsupported => "unsupported",
        .unavailable => "unavailable",
        .out_of_range => "out of range",
        .closed => "closed",
        .would_block => "would block",
        .internal => "internal error",
    };
}

pub fn grpc_lite_runtime_create(out_runtime: ?*?*RuntimeHandle) callconv(.c) Error {
    const output = out_runtime orelse return .invalid_argument;
    output.* = null;
    const storage = allocator.create(RuntimeStorage) catch return .out_of_memory;
    storage.value = Runtime.init() catch |err| {
        allocator.destroy(storage);
        return switch (err) {
            error.RuntimeAlreadyInitialized => .invalid_state,
            error.ResolverInitializationFailed => .unavailable,
            else => .internal,
        };
    };
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_runtime_destroy(runtime: ?*RuntimeHandle) callconv(.c) void {
    const handle = runtime orelse return;
    const storage: *RuntimeStorage = @ptrCast(@alignCast(handle));
    storage.value.deinit();
    allocator.destroy(storage);
}

pub fn grpc_lite_metadata_create(out_metadata: ?*?*MetadataHandle) callconv(.c) Error {
    const output = out_metadata orelse return .invalid_argument;
    output.* = null;
    const storage = allocator.create(MetadataStorage) catch return .out_of_memory;
    storage.value = metadata.Metadata.init(allocator);
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_metadata_destroy(metadata_handle: ?*MetadataHandle) callconv(.c) void {
    const handle = metadata_handle orelse return;
    const storage = metadataStorage(handle);
    storage.value.deinit();
    allocator.destroy(storage);
}

pub fn grpc_lite_metadata_add(
    metadata_handle: ?*MetadataHandle,
    key: BytesView,
    value: BytesView,
) callconv(.c) Error {
    const handle = metadata_handle orelse return .invalid_argument;
    const key_bytes = bytes(key) orelse return .invalid_argument;
    const value_bytes = bytes(value) orelse return .invalid_argument;
    metadataStorage(handle).value.append(key_bytes, value_bytes) catch |err| return switch (err) {
        error.OutOfMemory => .out_of_memory,
        error.InvalidMetadataKey, error.InvalidMetadataValue => .invalid_argument,
    };
    return .ok;
}

pub fn grpc_lite_metadata_count(metadata_handle: ?*const MetadataHandle) callconv(.c) usize {
    const handle = metadata_handle orelse return 0;
    return metadataStorageConst(handle).value.items().len;
}

pub fn grpc_lite_metadata_at(
    metadata_handle: ?*const MetadataHandle,
    index: usize,
    out_entry: ?*MetadataEntryView,
) callconv(.c) Error {
    const output = out_entry orelse return .invalid_argument;
    output.* = .{};
    const handle = metadata_handle orelse return .invalid_argument;
    const entries = metadataStorageConst(handle).value.items();
    if (index >= entries.len) return .out_of_range;
    output.* = .{
        .key = view(entries[index].key),
        .value = view(entries[index].value),
    };
    return .ok;
}

pub fn grpc_lite_metadata_view_count(view_handle: ?*const MetadataViewHandle) callconv(.c) usize {
    const handle = view_handle orelse return 0;
    return metadataView(handle).items().len;
}

pub fn grpc_lite_metadata_view_at(
    view_handle: ?*const MetadataViewHandle,
    index: usize,
    out_entry: ?*MetadataEntryView,
) callconv(.c) Error {
    const output = out_entry orelse return .invalid_argument;
    output.* = .{};
    const handle = view_handle orelse return .invalid_argument;
    const entries = metadataView(handle).items();
    if (index >= entries.len) return .out_of_range;
    output.* = .{ .key = view(entries[index].key), .value = view(entries[index].value) };
    return .ok;
}

pub fn grpc_lite_channel_create(
    runtime_handle: ?*RuntimeHandle,
    target: BytesView,
    out_channel: ?*?*ChannelHandle,
) callconv(.c) Error {
    return createChannel(runtime_handle, target, .{}, out_channel);
}

pub fn grpc_lite_channel_create_managed(
    runtime_handle: ?*RuntimeHandle,
    target: BytesView,
    options_pointer: ?*const ChannelOptions,
    out_channel: ?*?*ChannelHandle,
) callconv(.c) Error {
    const options = parseChannelOptions(options_pointer) orelse {
        if (out_channel) |output| output.* = null;
        return .invalid_argument;
    };
    return createChannel(runtime_handle, target, options, out_channel);
}

fn createChannel(
    runtime_handle: ?*RuntimeHandle,
    target: BytesView,
    options: ChannelCreateOptions,
    out_channel: ?*?*ChannelHandle,
) Error {
    const output = out_channel orelse return .invalid_argument;
    output.* = null;
    const target_bytes = bytes(target) orelse return .invalid_argument;
    const storage = allocator.create(ChannelStorage) catch return .out_of_memory;
    errdefer allocator.destroy(storage);
    storage.value = channel.Channel.init(allocator, target_bytes, .{
        .runtime = if (runtime_handle) |handle| &runtimeStorage(handle).value else null,
        .reconnect = options.reconnect,
        .logger = options.logger,
        .max_response_header_list_size = options.max_response_header_list_size,
    }) catch |err| return switch (err) {
        error.OutOfMemory => .out_of_memory,
        error.InvalidTarget, error.InvalidReconnectOptions, error.InvalidMaxResponseHeaderListSize => .invalid_argument,
        error.RuntimeRequired, error.RuntimeNotInitialized => .invalid_state,
        else => .unavailable,
    };
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_channel_shutdown(channel_handle: ?*ChannelHandle) callconv(.c) void {
    const handle = channel_handle orelse return;
    channelStorage(handle).value.shutdown();
}

pub fn grpc_lite_channel_wait(channel_handle: ?*ChannelHandle) callconv(.c) void {
    const handle = channel_handle orelse return;
    channelStorage(handle).value.wait();
}

pub fn grpc_lite_channel_destroy(channel_handle: ?*ChannelHandle) callconv(.c) void {
    const handle = channel_handle orelse return;
    const storage = channelStorage(handle);
    storage.value.deinit();
    allocator.destroy(storage);
}

pub fn grpc_lite_channel_call_unary(
    channel_handle: ?*ChannelHandle,
    full_method_path: BytesView,
    request: BytesView,
    options_pointer: ?*const UnaryOptions,
    out_result: ?*?*UnaryResultHandle,
) callconv(.c) Error {
    const output = out_result orelse return .invalid_argument;
    output.* = null;
    const handle = channel_handle orelse return .invalid_argument;
    const method = bytes(full_method_path) orelse return .invalid_argument;
    const request_bytes = bytes(request) orelse return .invalid_argument;
    const options = parseUnaryOptions(options_pointer) orelse return .invalid_argument;

    const storage = allocator.create(UnaryResultStorage) catch return .out_of_memory;
    errdefer allocator.destroy(storage);
    storage.value = channelStorage(handle).value.callUnary(
        allocator,
        method,
        request_bytes,
        options,
    ) catch |err| return switch (err) {
        error.OutOfMemory => .out_of_memory,
        error.InvalidMethodPath, error.InvalidMaxResponseSize, error.InvalidMetadataKey, error.InvalidMetadataValue => .invalid_argument,
        else => .internal,
    };
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_channel_open_stream(
    channel_handle: ?*ChannelHandle,
    full_method_path: BytesView,
    options_pointer: ?*const ClientStreamOptions,
    callbacks_pointer: ?*const ClientStreamCallbacks,
    out_stream: ?*?*ClientStreamHandle,
) callconv(.c) Error {
    const output = out_stream orelse return .invalid_argument;
    output.* = null;
    const handle = channel_handle orelse return .invalid_argument;
    const method = bytes(full_method_path) orelse return .invalid_argument;
    const options = parseClientStreamOptions(options_pointer) orelse return .invalid_argument;
    const callbacks = parseClientStreamCallbacks(callbacks_pointer) orelse return .invalid_argument;

    const storage = allocator.create(ClientStreamStorage) catch return .out_of_memory;
    errdefer allocator.destroy(storage);
    storage.callbacks = callbacks;
    storage.ready = .init(false);
    storage.value = channelStorage(handle).value.openStream(method, options, .{
        .context = storage,
        .on_headers = clientStreamOnHeaders,
        .on_message = clientStreamOnMessage,
        .on_remote_end = clientStreamOnRemoteEnd,
        .on_writable = clientStreamOnWritable,
        .on_terminal = clientStreamOnTerminal,
    }) catch |err| return mapStreamError(err);
    storage.ready.store(true, .release);
    output.* = clientStreamHandle(storage);
    return .ok;
}

pub fn grpc_lite_client_stream_send(
    stream_handle: ?*ClientStreamHandle,
    payload: BytesView,
    compression: u32,
) callconv(.c) Error {
    const handle = stream_handle orelse return .invalid_argument;
    const payload_bytes = bytes(payload) orelse return .invalid_argument;
    const algorithm = parseCompression(compression) orelse return .invalid_argument;
    clientStreamStorage(handle).value.send(payload_bytes, .{ .compression = algorithm }) catch |err| {
        return mapStreamError(err);
    };
    return .ok;
}

pub fn grpc_lite_client_stream_close_send(stream_handle: ?*ClientStreamHandle) callconv(.c) Error {
    const handle = stream_handle orelse return .invalid_argument;
    clientStreamStorage(handle).value.closeSend() catch |err| return mapStreamError(err);
    return .ok;
}

pub fn grpc_lite_client_stream_cancel(stream_handle: ?*ClientStreamHandle) callconv(.c) void {
    const handle = stream_handle orelse return;
    clientStreamStorage(handle).value.cancel();
}

pub fn grpc_lite_client_stream_resume_receive(stream_handle: ?*ClientStreamHandle) callconv(.c) Error {
    const handle = stream_handle orelse return .invalid_argument;
    clientStreamStorage(handle).value.resumeReceive() catch |err| return mapStreamError(err);
    return .ok;
}

pub fn grpc_lite_client_stream_destroy(stream_handle: ?*ClientStreamHandle) callconv(.c) void {
    const handle = stream_handle orelse return;
    const storage = clientStreamStorage(handle);
    storage.value.deinit();
    allocator.destroy(storage);
}

pub fn grpc_lite_server_create(
    options_pointer: ?*const ServerOptions,
    out_server: ?*?*ServerHandle,
) callconv(.c) Error {
    const output = out_server orelse return .invalid_argument;
    output.* = null;
    const options = parseServerOptions(options_pointer) orelse return .invalid_argument;
    const storage = allocator.create(ServerStorage) catch return .out_of_memory;
    const value = Server.init(allocator, options) catch |err| {
        allocator.destroy(storage);
        return mapServerError(err);
    };
    storage.* = .{ .value = value };
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_server_register_stream(
    server_handle: ?*ServerHandle,
    full_method_path: BytesView,
    options_pointer: ?*const ServerMethodOptions,
    callbacks_pointer: ?*const ServerMethodCallbacks,
) callconv(.c) Error {
    const handle = server_handle orelse return .invalid_argument;
    const path = bytes(full_method_path) orelse return .invalid_argument;
    const options = parseServerMethodOptions(options_pointer) orelse return .invalid_argument;
    const callbacks = parseServerMethodCallbacks(callbacks_pointer) orelse return .invalid_argument;
    const server_storage = serverStorage(handle);
    const method = allocator.create(ServerMethodStorage) catch return .out_of_memory;
    errdefer allocator.destroy(method);
    method.* = .{ .callbacks = callbacks };
    server_storage.methods.ensureUnusedCapacity(allocator, 1) catch {
        allocator.destroy(method);
        return .out_of_memory;
    };
    server_storage.value.registerStream(path, .{
        .context = method,
        .receive_initially_paused = options.receive_initially_paused == 1,
        .initial_metadata_mode = if (options.explicit_initial_metadata == 1) .explicit else .automatic_after_start,
        .on_start = serverMethodOnStart,
        .on_message = serverMethodOnMessage,
        .on_remote_end = serverMethodOnRemoteEnd,
        .on_writable = serverMethodOnWritable,
        .on_cancel = serverMethodOnCancel,
        .on_terminal = serverMethodOnTerminal,
    }) catch |err| {
        allocator.destroy(method);
        return mapServerError(err);
    };
    server_storage.methods.appendAssumeCapacity(method);
    return .ok;
}

pub fn grpc_lite_server_start(server_handle: ?*ServerHandle) callconv(.c) Error {
    const handle = server_handle orelse return .invalid_argument;
    serverStorage(handle).value.start() catch |err| return mapServerError(err);
    return .ok;
}

pub fn grpc_lite_server_port(server_handle: ?*const ServerHandle, out_port: ?*u32) callconv(.c) Error {
    const output = out_port orelse return .invalid_argument;
    output.* = 0;
    const handle = server_handle orelse return .invalid_argument;
    output.* = serverStorageConst(handle).value.port() catch |err| return mapServerError(err);
    return .ok;
}

pub fn grpc_lite_server_shutdown(server_handle: ?*ServerHandle) callconv(.c) void {
    const handle = server_handle orelse return;
    serverStorage(handle).value.shutdown();
}

pub fn grpc_lite_server_shutdown_gracefully(server_handle: ?*ServerHandle, timeout_ns: u64) callconv(.c) void {
    const handle = server_handle orelse return;
    serverStorage(handle).value.shutdownGracefully(timeout_ns);
}

pub fn grpc_lite_server_wait(server_handle: ?*ServerHandle) callconv(.c) void {
    const handle = server_handle orelse return;
    serverStorage(handle).value.wait();
}

pub fn grpc_lite_server_destroy(server_handle: ?*ServerHandle) callconv(.c) void {
    const handle = server_handle orelse return;
    const storage = serverStorage(handle);
    storage.value.deinit();
    for (storage.methods.items) |method| allocator.destroy(method);
    storage.methods.deinit(allocator);
    allocator.destroy(storage);
}

pub fn grpc_lite_server_stream_id(stream_handle: ?*const ServerStreamHandle) callconv(.c) usize {
    const handle = stream_handle orelse return 0;
    return @intFromEnum(serverStreamStorage(handle).value.id());
}

pub fn grpc_lite_server_stream_retain(
    stream_handle: ?*const ServerStreamHandle,
    out_call: ?*?*ServerCallHandle,
) callconv(.c) Error {
    const output = out_call orelse return .invalid_argument;
    output.* = null;
    const handle = stream_handle orelse return .invalid_argument;
    const storage = allocator.create(ServerCallStorage) catch return .out_of_memory;
    storage.value = serverStreamStorage(handle).value.retain() catch |err| {
        allocator.destroy(storage);
        return mapServerError(err);
    };
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_server_call_clone(
    call_handle: ?*const ServerCallHandle,
    out_call: ?*?*ServerCallHandle,
) callconv(.c) Error {
    const output = out_call orelse return .invalid_argument;
    output.* = null;
    const handle = call_handle orelse return .invalid_argument;
    const storage = allocator.create(ServerCallStorage) catch return .out_of_memory;
    storage.value = serverCallStorageConst(handle).value.clone();
    output.* = @ptrCast(storage);
    return .ok;
}

pub fn grpc_lite_server_call_destroy(call_handle: ?*ServerCallHandle) callconv(.c) void {
    const handle = call_handle orelse return;
    const storage = serverCallStorage(handle);
    storage.value.deinit();
    allocator.destroy(storage);
}

pub fn grpc_lite_server_call_id(call_handle: ?*const ServerCallHandle) callconv(.c) usize {
    const handle = call_handle orelse return 0;
    return @intFromEnum(serverCallStorageConst(handle).value.id());
}

pub fn grpc_lite_server_call_is_cancelled(call_handle: ?*const ServerCallHandle) callconv(.c) u32 {
    const handle = call_handle orelse return 1;
    return @intFromBool(serverCallStorageConst(handle).value.isCancelled());
}

pub fn grpc_lite_server_call_is_terminal(call_handle: ?*const ServerCallHandle) callconv(.c) u32 {
    const handle = call_handle orelse return 1;
    return @intFromBool(serverCallStorageConst(handle).value.isTerminal());
}

pub fn grpc_lite_server_call_abort(call_handle: ?*ServerCallHandle) callconv(.c) void {
    const handle = call_handle orelse return;
    serverCallStorage(handle).value.abort();
}

pub fn grpc_lite_server_call_send_initial_metadata(
    call_handle: ?*ServerCallHandle,
    metadata_handle: ?*const MetadataHandle,
    compression: u32,
) callconv(.c) Error {
    const handle = call_handle orelse return .invalid_argument;
    const algorithm = parseCompression(compression) orelse return .invalid_argument;
    const entries = if (metadata_handle) |value| metadataStorageConst(value).value.items() else &.{};
    serverCallStorage(handle).value.sendInitialMetadata(entries, algorithm) catch |err| return mapServerError(err);
    return .ok;
}

pub fn grpc_lite_server_call_send(
    call_handle: ?*ServerCallHandle,
    payload: BytesView,
    compression: u32,
) callconv(.c) Error {
    const handle = call_handle orelse return .invalid_argument;
    const payload_bytes = bytes(payload) orelse return .invalid_argument;
    const algorithm = parseCompression(compression) orelse return .invalid_argument;
    serverCallStorage(handle).value.send(payload_bytes, .{ .compression = algorithm }) catch |err| return mapServerError(err);
    return .ok;
}

pub fn grpc_lite_server_call_finish(
    call_handle: ?*ServerCallHandle,
    status_code: u32,
    status_message: BytesView,
    trailing_metadata_handle: ?*const MetadataHandle,
) callconv(.c) Error {
    const handle = call_handle orelse return .invalid_argument;
    if (status_code > 16) return .invalid_argument;
    const message = bytes(status_message) orelse return .invalid_argument;
    const entries = if (trailing_metadata_handle) |value| metadataStorageConst(value).value.items() else &.{};
    serverCallStorage(handle).value.finish(
        .init(StatusCode.fromInt(status_code), message),
        entries,
    ) catch |err| return mapServerError(err);
    return .ok;
}

pub fn grpc_lite_server_call_resume_receive(call_handle: ?*ServerCallHandle) callconv(.c) Error {
    const handle = call_handle orelse return .invalid_argument;
    serverCallStorage(handle).value.resumeReceive() catch |err| return mapServerError(err);
    return .ok;
}

pub fn grpc_lite_server_context_request_metadata(
    context_handle: ?*const ServerContextHandle,
) callconv(.c) ?*const MetadataViewHandle {
    const handle = context_handle orelse return null;
    return metadataViewHandle(&serverContext(handle).request_metadata);
}

pub fn grpc_lite_server_context_request_compression(context_handle: ?*const ServerContextHandle) callconv(.c) u32 {
    const handle = context_handle orelse return 0;
    return @intFromEnum(serverContext(handle).request_compression);
}

pub fn grpc_lite_server_context_has_deadline(context_handle: ?*const ServerContextHandle) callconv(.c) u32 {
    const handle = context_handle orelse return 0;
    return @intFromBool(serverContext(handle).hasDeadline());
}

pub fn grpc_lite_server_context_remaining_time_ns(context_handle: ?*const ServerContextHandle) callconv(.c) u64 {
    const handle = context_handle orelse return 0;
    return serverContext(handle).remainingTimeNs() orelse 0;
}

pub fn grpc_lite_unary_result_destroy(result_handle: ?*UnaryResultHandle) callconv(.c) void {
    const handle = result_handle orelse return;
    const storage = unaryResultStorage(handle);
    storage.value.deinit();
    allocator.destroy(storage);
}

pub fn grpc_lite_unary_result_status_code(result_handle: ?*const UnaryResultHandle) callconv(.c) i32 {
    const handle = result_handle orelse return 2;
    return @intFromEnum(unaryResultStorageConst(handle).value.status.code);
}

pub fn grpc_lite_unary_result_status_message(result_handle: ?*const UnaryResultHandle) callconv(.c) BytesView {
    const handle = result_handle orelse return .{};
    return view(unaryResultStorageConst(handle).value.status.message);
}

pub fn grpc_lite_unary_result_payload(result_handle: ?*const UnaryResultHandle) callconv(.c) BytesView {
    const handle = result_handle orelse return .{};
    return view(unaryResultStorageConst(handle).value.payload);
}

pub fn grpc_lite_unary_result_response_compression(result_handle: ?*const UnaryResultHandle) callconv(.c) u32 {
    const handle = result_handle orelse return 0;
    return @intFromEnum(unaryResultStorageConst(handle).value.response_compression);
}

pub fn grpc_lite_unary_result_metadata_count(result_handle: ?*const UnaryResultHandle, trailing: u32) callconv(.c) usize {
    const handle = result_handle orelse return 0;
    return resultMetadata(unaryResultStorageConst(handle), trailing).items().len;
}

pub fn grpc_lite_unary_result_metadata_at(result_handle: ?*const UnaryResultHandle, trailing: u32, index: usize, out_entry: ?*MetadataEntryView) callconv(.c) Error {
    const output = out_entry orelse return .invalid_argument;
    output.* = .{};
    const handle = result_handle orelse return .invalid_argument;
    const entries = resultMetadata(unaryResultStorageConst(handle), trailing).items();
    if (index >= entries.len) return .out_of_range;
    output.* = .{ .key = view(entries[index].key), .value = view(entries[index].value) };
    return .ok;
}

fn serverMethodOnStart(
    context: ?*anyopaque,
    stream: raw_stream.ServerStream,
    server_context: *ServerContext,
) !void {
    const method = serverMethodContext(context);
    const callback = method.callbacks.on_start orelse return;
    var borrowed: BorrowedServerStreamStorage = .{ .value = stream };
    callback(
        method.callbacks.user_data,
        @ptrCast(&borrowed),
        serverContextHandle(server_context),
    );
}

fn serverMethodOnMessage(
    context: ?*anyopaque,
    stream: raw_stream.ServerStream,
    server_context: *ServerContext,
    payload: []const u8,
    compression: Compression,
) !raw_stream.ReceiveAction {
    const method = serverMethodContext(context);
    var borrowed: BorrowedServerStreamStorage = .{ .value = stream };
    return if (method.callbacks.on_message.?(
        method.callbacks.user_data,
        @ptrCast(&borrowed),
        serverContextHandle(server_context),
        view(payload),
        @intFromEnum(compression),
    ) == 0) .continue_receiving else .pause;
}

fn serverMethodOnRemoteEnd(
    context: ?*anyopaque,
    stream: raw_stream.ServerStream,
    server_context: *ServerContext,
) !void {
    const method = serverMethodContext(context);
    const callback = method.callbacks.on_remote_end orelse return;
    var borrowed: BorrowedServerStreamStorage = .{ .value = stream };
    callback(
        method.callbacks.user_data,
        @ptrCast(&borrowed),
        serverContextHandle(server_context),
    );
}

fn serverMethodOnWritable(
    context: ?*anyopaque,
    stream: raw_stream.ServerStream,
    server_context: *ServerContext,
) void {
    const method = serverMethodContext(context);
    const callback = method.callbacks.on_writable orelse return;
    var borrowed: BorrowedServerStreamStorage = .{ .value = stream };
    callback(
        method.callbacks.user_data,
        @ptrCast(&borrowed),
        serverContextHandle(server_context),
    );
}

fn serverMethodOnCancel(
    context: ?*anyopaque,
    stream: raw_stream.ServerStream,
    server_context: *ServerContext,
) void {
    const method = serverMethodContext(context);
    const callback = method.callbacks.on_cancel orelse return;
    var borrowed: BorrowedServerStreamStorage = .{ .value = stream };
    callback(
        method.callbacks.user_data,
        @ptrCast(&borrowed),
        serverContextHandle(server_context),
    );
}

fn serverMethodOnTerminal(
    context: ?*anyopaque,
    call_id: raw_stream.ServerCallId,
    reason: raw_stream.ServerTerminalReason,
) void {
    const method = serverMethodContext(context);
    const callback = method.callbacks.on_terminal orelse return;
    callback(method.callbacks.user_data, @intFromEnum(call_id), @intFromEnum(reason));
}

fn serverMethodContext(context: ?*anyopaque) *ServerMethodStorage {
    return @ptrCast(@alignCast(context.?));
}

fn serverContextHandle(context: *ServerContext) *const ServerContextHandle {
    return @ptrCast(context);
}

fn serverContext(handle: *const ServerContextHandle) *const ServerContext {
    return @ptrCast(@alignCast(handle));
}

fn serverStorage(handle: *ServerHandle) *ServerStorage {
    return @ptrCast(@alignCast(handle));
}

fn serverStorageConst(handle: *const ServerHandle) *const ServerStorage {
    return @ptrCast(@alignCast(handle));
}

fn serverStreamStorage(handle: *const ServerStreamHandle) *const BorrowedServerStreamStorage {
    return @ptrCast(@alignCast(handle));
}

fn serverCallStorage(handle: *ServerCallHandle) *ServerCallStorage {
    return @ptrCast(@alignCast(handle));
}

fn serverCallStorageConst(handle: *const ServerCallHandle) *const ServerCallStorage {
    return @ptrCast(@alignCast(handle));
}

fn mapServerError(err: anyerror) Error {
    return switch (err) {
        error.OutOfMemory => .out_of_memory,
        error.WouldBlock => .would_block,
        error.CallClosed, error.ServerCallUnavailable => .closed,
        error.ServerAlreadyStarted,
        error.MethodAlreadyRegistered,
        error.InitialMetadataAlreadySent,
        error.FinishAlreadyQueued,
        error.InitialMetadataRequired,
        error.InitialMetadataNotExplicit,
        error.ReceiveNotPaused,
        => .invalid_state,
        error.InvalidMethodPath,
        error.InvalidReactorCount,
        error.InvalidMaxConnections,
        error.InvalidMaxConcurrentStreams,
        error.InvalidCleartextPrefaceTimeout,
        error.InvalidConnectionIdleTimeout,
        error.InvalidMaxMessageSize,
        error.InvalidInboundBufferSize,
        error.InvalidOutboundBufferSize,
        error.InvalidInitialStreamWindowSize,
        error.InvalidWriteWatermarks,
        error.MessageTooLarge,
        error.CompressionNotConfigured,
        error.CompressionNotAccepted,
        error.ResponseCompressionNotEnabled,
        => .invalid_argument,
        error.OutboundBufferLimitExceeded => .out_of_range,
        error.ServerNotRunning,
        error.BindFailed,
        error.ListenFailed,
        error.AddressQueryFailed,
        error.ListenerInitializationFailed,
        error.LoopInitializationFailed,
        error.AsyncInitializationFailed,
        error.TimerInitializationFailed,
        => .unavailable,
        else => .internal,
    };
}

fn clientStreamOnHeaders(
    context: ?*anyopaque,
    _: raw_stream.ClientStream,
    headers: *const metadata.Metadata,
) void {
    const storage = clientStreamContext(context);
    const callback = storage.callbacks.on_headers orelse return;
    callback(storage.callbacks.user_data, clientStreamHandle(storage), metadataViewHandle(headers));
}

fn clientStreamOnMessage(
    context: ?*anyopaque,
    _: raw_stream.ClientStream,
    payload: []const u8,
    compression: Compression,
) raw_stream.ReceiveAction {
    const storage = clientStreamContext(context);
    const callback = storage.callbacks.on_message.?;
    return if (callback(
        storage.callbacks.user_data,
        clientStreamHandle(storage),
        view(payload),
        @intFromEnum(compression),
    ) == 0) .continue_receiving else .pause;
}

fn clientStreamOnRemoteEnd(context: ?*anyopaque, _: raw_stream.ClientStream) void {
    const storage = clientStreamContext(context);
    const callback = storage.callbacks.on_remote_end orelse return;
    callback(storage.callbacks.user_data, clientStreamHandle(storage));
}

fn clientStreamOnWritable(context: ?*anyopaque, _: raw_stream.ClientStream) void {
    const storage = clientStreamContext(context);
    const callback = storage.callbacks.on_writable orelse return;
    callback(storage.callbacks.user_data, clientStreamHandle(storage));
}

fn clientStreamOnTerminal(
    context: ?*anyopaque,
    _: raw_stream.ClientStream,
    final_status: Status,
    trailing_metadata: *const metadata.Metadata,
) void {
    const storage = clientStreamContext(context);
    const callback = storage.callbacks.on_terminal.?;
    callback(
        storage.callbacks.user_data,
        clientStreamHandle(storage),
        @intFromEnum(final_status.code),
        view(final_status.message),
        metadataViewHandle(trailing_metadata),
    );
}

fn clientStreamContext(context: ?*anyopaque) *ClientStreamStorage {
    const storage: *ClientStreamStorage = @ptrCast(@alignCast(context.?));
    while (!storage.ready.load(.acquire)) std.atomic.spinLoopHint();
    return storage;
}

fn clientStreamHandle(storage: *ClientStreamStorage) *ClientStreamHandle {
    return @ptrCast(storage);
}

fn clientStreamStorage(handle: *ClientStreamHandle) *ClientStreamStorage {
    return @ptrCast(@alignCast(handle));
}

fn metadataViewHandle(value: *const metadata.Metadata) *const MetadataViewHandle {
    return @ptrCast(value);
}

fn metadataView(handle: *const MetadataViewHandle) *const metadata.Metadata {
    return @ptrCast(@alignCast(handle));
}

fn mapStreamError(err: anyerror) Error {
    return switch (err) {
        error.OutOfMemory => .out_of_memory,
        error.WouldBlock => .would_block,
        error.StreamClosed, error.SendClosed => .closed,
        error.ChannelUnavailable => .unavailable,
        error.MessageTooLarge, error.OutboundBufferLimitExceeded => .out_of_range,
        error.InvalidMethodPath,
        error.InvalidMetadataKey,
        error.InvalidMetadataValue,
        error.InvalidMaxMessageSize,
        error.InvalidInboundBufferSize,
        error.InvalidOutboundBufferSize,
        error.CompressionNotConfigured,
        => .invalid_argument,
        else => .internal,
    };
}

fn metadataStorage(handle: *MetadataHandle) *MetadataStorage {
    return @ptrCast(@alignCast(handle));
}

fn runtimeStorage(handle: *RuntimeHandle) *RuntimeStorage {
    return @ptrCast(@alignCast(handle));
}

fn channelStorage(handle: *ChannelHandle) *ChannelStorage {
    return @ptrCast(@alignCast(handle));
}

fn unaryResultStorage(handle: *UnaryResultHandle) *UnaryResultStorage {
    return @ptrCast(@alignCast(handle));
}

fn unaryResultStorageConst(handle: *const UnaryResultHandle) *const UnaryResultStorage {
    return @ptrCast(@alignCast(handle));
}

fn resultMetadata(result: *const UnaryResultStorage, trailing: u32) *const metadata.Metadata {
    return if (trailing == 0) &result.value.initial_metadata else &result.value.trailing_metadata;
}

fn parseUnaryOptions(pointer: ?*const UnaryOptions) ?call.Options {
    const value = pointer orelse return .{};
    if (value.struct_size < @sizeOf(UnaryOptions) or value.has_timeout > 1) return null;
    const compression = parseCompression(value.request_compression) orelse return null;
    if (value.max_response_size > std.math.maxInt(usize)) return null;
    return .{
        .metadata = if (value.metadata) |handle| metadataStorageConst(handle).value.items() else &.{},
        .timeout_ns = if (value.has_timeout == 1) value.timeout_ns else null,
        .max_response_size = @intCast(value.max_response_size),
        .request_compression = compression,
    };
}

fn parseChannelOptions(pointer: ?*const ChannelOptions) ?ChannelCreateOptions {
    const value = pointer orelse return .{ .reconnect = .{} };
    if (value.struct_size < channel_options_v1_size or
        value.allow_initial_offline > 1 or
        value.initial_backoff_ns == 0 or
        value.max_backoff_ns < value.initial_backoff_ns or
        value.multiplier_millis < 1000 or
        value.jitter_percent > 100) return null;
    const max_response_header_list_size = if (value.struct_size >= channel_options_v3_size) blk: {
        if (value.max_response_header_list_size == 0 or
            value.max_response_header_list_size > std.math.maxInt(u32)) return null;
        break :blk @as(u32, @intCast(value.max_response_header_list_size));
    } else 64 * 1024;
    return .{
        .reconnect = .{
            .allow_initial_offline = value.allow_initial_offline == 1,
            .initial_backoff_ns = value.initial_backoff_ns,
            .max_backoff_ns = value.max_backoff_ns,
            .multiplier_millis = value.multiplier_millis,
            .jitter_percent = @intCast(value.jitter_percent),
        },
        .logger = if (value.struct_size >= channel_options_v2_size)
            parseLogger(value.logger) orelse return null
        else
            .{},
        .max_response_header_list_size = max_response_header_list_size,
    };
}

fn parseClientStreamOptions(pointer: ?*const ClientStreamOptions) ?raw_stream.Options {
    const value = pointer orelse return .{};
    if (value.struct_size < client_stream_options_v1_size or value.has_timeout > 1) return null;
    const compression = parseCompression(value.send_compression) orelse return null;
    if (value.max_message_size > std.math.maxInt(usize) or
        value.max_inbound_buffer_size > std.math.maxInt(usize) or
        value.max_outbound_buffer_size > std.math.maxInt(usize)) return null;
    return .{
        .metadata = if (value.metadata) |handle| metadataStorageConst(handle).value.items() else &.{},
        .timeout_ns = if (value.has_timeout == 1) value.timeout_ns else null,
        .limits = .{
            .max_message_size = @intCast(value.max_message_size),
            .max_inbound_buffer_size = @intCast(value.max_inbound_buffer_size),
            .max_outbound_buffer_size = @intCast(value.max_outbound_buffer_size),
        },
        .send_compression = compression,
    };
}

fn parseClientStreamCallbacks(pointer: ?*const ClientStreamCallbacks) ?ClientStreamCallbacks {
    const value = pointer orelse return null;
    if (value.struct_size < client_stream_callbacks_v1_size or
        value.on_message == null or value.on_terminal == null) return null;
    return value.*;
}

fn parseCompression(value: u32) ?Compression {
    return switch (value) {
        0 => .identity,
        1 => .gzip,
        else => null,
    };
}

fn parseServerOptions(pointer: ?*const ServerOptions) ?@import("server.zig").Options {
    const value = pointer orelse return .{};
    if (value.struct_size < server_options_v1_size or
        value.port > std.math.maxInt(u16) or
        value.reactor_count == 0 or
        value.max_message_size > std.math.maxInt(usize) or
        value.max_inbound_buffer_size > std.math.maxInt(usize) or
        value.max_outbound_buffer_size > std.math.maxInt(usize)) return null;
    const host = bytes(value.host) orelse return null;
    var parsed: @import("server.zig").Options = .{
        .host = host,
        .port = @intCast(value.port),
        .reactor_count = value.reactor_count,
        .max_request_size = @intCast(value.max_message_size),
        .stream_limits = .{
            .max_message_size = @intCast(value.max_message_size),
            .max_inbound_buffer_size = @intCast(value.max_inbound_buffer_size),
            .max_outbound_buffer_size = @intCast(value.max_outbound_buffer_size),
        },
        .logger = if (value.struct_size >= server_options_logger_size)
            parseLogger(value.logger) orelse return null
        else
            .{},
    };
    if (value.struct_size >= server_options_max_connections_size) {
        if (value.max_connections == 0 or value.max_connections > std.math.maxInt(usize)) return null;
        parsed.max_connections = @intCast(value.max_connections);
    }
    if (value.struct_size >= server_options_preface_timeout_size) {
        if (value.cleartext_preface_timeout_ns == 0) return null;
        parsed.cleartext_preface_timeout_ns = value.cleartext_preface_timeout_ns;
    }
    if (value.struct_size >= server_options_idle_timeout_size) {
        if (value.connection_idle_timeout_ns == 0) return null;
        parsed.connection_idle_timeout_ns = value.connection_idle_timeout_ns;
    }
    if (value.struct_size >= server_options_max_streams_size) {
        if (value.max_concurrent_streams_per_connection == 0) return null;
        parsed.max_concurrent_streams_per_connection = value.max_concurrent_streams_per_connection;
    }
    return parsed;
}

fn parseLogger(pointer: ?*const Logger) ?event_logger.Logger {
    const value = pointer orelse return .{};
    if (value.struct_size < @sizeOf(Logger) or value.log == null) return null;
    return .{ .context = value.user_data, .callback = value.log };
}

fn parseServerMethodOptions(pointer: ?*const ServerMethodOptions) ?ServerMethodOptions {
    const value = pointer orelse return .{};
    if (value.struct_size < server_method_options_v1_size or
        value.receive_initially_paused > 1 or
        value.explicit_initial_metadata > 1) return null;
    return value.*;
}

fn parseServerMethodCallbacks(pointer: ?*const ServerMethodCallbacks) ?ServerMethodCallbacks {
    const value = pointer orelse return null;
    if (value.struct_size < server_method_callbacks_v1_size or value.on_message == null) return null;
    return value.*;
}

fn metadataStorageConst(handle: *const MetadataHandle) *const MetadataStorage {
    return @ptrCast(@alignCast(handle));
}

fn bytes(value: BytesView) ?[]const u8 {
    if (value.size == 0) return &.{};
    const pointer = value.data orelse return null;
    return pointer[0..value.size];
}

fn view(value: []const u8) BytesView {
    return .{
        .data = if (value.len == 0) null else value.ptr,
        .size = value.len,
    };
}

test "C ABI reports version and usable features" {
    try std.testing.expectEqual((@as(u32, abi_major) << 16) | abi_minor, grpc_lite_abi_version());
    try std.testing.expectEqualStrings(version.string, std.mem.span(grpc_lite_library_version()));
    try std.testing.expect(grpc_lite_features() & Feature.streaming != 0);
    try std.testing.expect(grpc_lite_features() & Feature.managed_channel != 0);
    try std.testing.expect(grpc_lite_features() & Feature.logging_callback != 0);
    try std.testing.expectEqual(@as(u64, 0), grpc_lite_features() & Feature.tls);
    try std.testing.expectEqualStrings(
        "invalid argument",
        std.mem.span(grpc_lite_error_string(@intFromEnum(Error.invalid_argument))),
    );
    try std.testing.expectEqualStrings("unknown error", std.mem.span(grpc_lite_error_string(-1)));
    try std.testing.expectEqual(2 * @sizeOf(usize), @sizeOf(BytesView));
    try std.testing.expectEqual(@alignOf(usize), @alignOf(BytesView));
    try std.testing.expectEqual(2 * @sizeOf(BytesView), @sizeOf(MetadataEntryView));
}

test "C managed channel options have stable layout and defaults" {
    const options: ChannelOptions = .{};
    try std.testing.expectEqual(
        @as(u32, 64 * 1024),
        parseChannelOptions(null).?.max_response_header_list_size,
    );
    try std.testing.expectEqual(@sizeOf(ChannelOptions), options.struct_size);
    try std.testing.expectEqual(@as(usize, 0), @offsetOf(ChannelOptions, "struct_size"));
    try std.testing.expectEqual(@sizeOf(usize), @offsetOf(ChannelOptions, "allow_initial_offline"));
    try std.testing.expectEqual(2 * @sizeOf(usize), @offsetOf(ChannelOptions, "initial_backoff_ns"));
    try std.testing.expectEqual(@offsetOf(ChannelOptions, "initial_backoff_ns") + @sizeOf(u64), @offsetOf(ChannelOptions, "max_backoff_ns"));
    try std.testing.expectEqual(@offsetOf(ChannelOptions, "max_backoff_ns") + @sizeOf(u64), @offsetOf(ChannelOptions, "multiplier_millis"));
    try std.testing.expectEqual(@offsetOf(ChannelOptions, "multiplier_millis") + @sizeOf(u32), @offsetOf(ChannelOptions, "jitter_percent"));
    try std.testing.expectEqual(@as(u32, 0), options.allow_initial_offline);
    try std.testing.expectEqual(@as(u64, std.time.ns_per_s), options.initial_backoff_ns);
    try std.testing.expectEqual(@as(u64, 120 * std.time.ns_per_s), options.max_backoff_ns);
    try std.testing.expectEqual(@as(u32, 1600), options.multiplier_millis);
    try std.testing.expectEqual(@as(u32, 20), options.jitter_percent);
    try std.testing.expectEqual(null, options.logger);
    try std.testing.expectEqual(
        @offsetOf(ChannelOptions, "logger") + @sizeOf(?*const Logger),
        @offsetOf(ChannelOptions, "max_response_header_list_size"),
    );
    try std.testing.expectEqual(@as(u64, 64 * 1024), options.max_response_header_list_size);
}

test "C logger validates layout and channel option compatibility" {
    const Callback = struct {
        fn log(_: ?*anyopaque, _: u32, _: BytesView) callconv(.c) void {}
    };
    var logger: Logger = .{ .log = Callback.log };
    try std.testing.expectEqual(@sizeOf(Logger), logger.struct_size);
    try std.testing.expectEqual(@as(usize, 0), @offsetOf(Logger, "struct_size"));
    try std.testing.expectEqual(@sizeOf(usize), @offsetOf(Logger, "user_data"));

    var options: ChannelOptions = .{ .logger = &logger, .max_response_header_list_size = 1234 };
    const parsed = parseChannelOptions(&options).?;
    try std.testing.expect(parsed.logger.callback != null);
    try std.testing.expectEqual(@as(u32, 1234), parsed.max_response_header_list_size);

    options.struct_size = channel_options_v1_size;
    const parsed_v1 = parseChannelOptions(&options).?;
    try std.testing.expect(parsed_v1.logger.callback == null);
    try std.testing.expectEqual(@as(u32, 64 * 1024), parsed_v1.max_response_header_list_size);

    options.struct_size = channel_options_v2_size;
    const parsed_v2 = parseChannelOptions(&options).?;
    try std.testing.expect(parsed_v2.logger.callback != null);
    try std.testing.expectEqual(@as(u32, 64 * 1024), parsed_v2.max_response_header_list_size);

    options.struct_size = channel_options_v3_size;
    logger.struct_size = @sizeOf(Logger) - 1;
    try std.testing.expectEqual(null, parseChannelOptions(&options));

    logger.struct_size = @sizeOf(Logger);
    options.max_response_header_list_size = 0;
    try std.testing.expectEqual(null, parseChannelOptions(&options));
    options.max_response_header_list_size = @as(u64, std.math.maxInt(u32)) + 1;
    try std.testing.expectEqual(null, parseChannelOptions(&options));
    options.max_response_header_list_size = std.math.maxInt(u32);
    try std.testing.expectEqual(
        std.math.maxInt(u32),
        parseChannelOptions(&options).?.max_response_header_list_size,
    );

    var server_options: ServerOptions = .{ .logger = &logger };
    try std.testing.expect(parseServerOptions(&server_options).?.logger.callback != null);
    server_options.struct_size = server_options_v1_size;
    try std.testing.expect(parseServerOptions(&server_options).?.logger.callback == null);
    server_options.struct_size = @sizeOf(ServerOptions);
    logger.struct_size = @sizeOf(Logger) - 1;
    try std.testing.expectEqual(null, parseServerOptions(&server_options));
}

test "C server options preserve legacy layouts and bound appended reads" {
    const Callback = struct {
        fn log(_: ?*anyopaque, _: u32, _: BytesView) callconv(.c) void {}
    };
    const logger: Logger = .{ .log = Callback.log };
    var options: ServerOptions = .{
        .logger = &logger,
        .max_connections = 17,
        .cleartext_preface_timeout_ns = 23,
        .connection_idle_timeout_ns = 29,
        .max_concurrent_streams_per_connection = 31,
    };

    options.struct_size = server_options_v1_size;
    var parsed = parseServerOptions(&options).?;
    try std.testing.expect(parsed.logger.callback == null);
    try std.testing.expectEqual(@as(usize, 1024), parsed.max_connections);
    try std.testing.expectEqual(@as(u32, 100), parsed.max_concurrent_streams_per_connection);

    options.struct_size = server_options_logger_size;
    parsed = parseServerOptions(&options).?;
    try std.testing.expect(parsed.logger.callback != null);
    try std.testing.expectEqual(@as(usize, 1024), parsed.max_connections);

    options.struct_size = server_options_max_connections_size;
    parsed = parseServerOptions(&options).?;
    try std.testing.expectEqual(@as(usize, 17), parsed.max_connections);
    try std.testing.expectEqual(@as(u64, 10 * std.time.ns_per_s), parsed.cleartext_preface_timeout_ns);

    options.struct_size = server_options_preface_timeout_size;
    parsed = parseServerOptions(&options).?;
    try std.testing.expectEqual(@as(u64, 23), parsed.cleartext_preface_timeout_ns);
    try std.testing.expectEqual(@as(u64, 5 * 60 * std.time.ns_per_s), parsed.connection_idle_timeout_ns);

    options.struct_size = server_options_idle_timeout_size;
    parsed = parseServerOptions(&options).?;
    try std.testing.expectEqual(@as(u64, 29), parsed.connection_idle_timeout_ns);
    try std.testing.expectEqual(@as(u32, 100), parsed.max_concurrent_streams_per_connection);

    options.struct_size = server_options_max_streams_size;
    parsed = parseServerOptions(&options).?;
    try std.testing.expectEqual(@as(u32, 31), parsed.max_concurrent_streams_per_connection);

    const invalid = [_]ServerOptions{
        .{ .max_connections = 0 },
        .{ .cleartext_preface_timeout_ns = 0 },
        .{ .connection_idle_timeout_ns = 0 },
        .{ .max_concurrent_streams_per_connection = 0 },
    };
    for (&invalid) |*value| try std.testing.expectEqual(null, parseServerOptions(value));

    if (comptime @sizeOf(usize) < @sizeOf(u64)) {
        var overflow: ServerOptions = .{ .max_connections = std.math.maxInt(u64) };
        try std.testing.expectEqual(null, parseServerOptions(&overflow));
    }
}

test "C server reports ordered lifecycle logs" {
    const Capture = struct {
        state: std.atomic.Value(u8) = .init(0),

        fn log(context: ?*anyopaque, _: u32, message: BytesView) callconv(.c) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            const text = bytes(message) orelse return;
            const expected: ?struct { before: u8, after: u8 } = if (std.mem.indexOf(u8, text, "server started") != null)
                .{ .before = 0, .after = 1 }
            else if (std.mem.indexOf(u8, text, "server drain requested") != null)
                .{ .before = 1, .after = 2 }
            else if (std.mem.indexOf(u8, text, "server stopped") != null)
                .{ .before = 2, .after = 3 }
            else
                null;
            if (expected) |transition| {
                if (self.state.cmpxchgStrong(transition.before, transition.after, .acq_rel, .acquire) != null) {
                    self.state.store(255, .release);
                }
            }
        }
    };

    var capture: Capture = .{};
    const logger: Logger = .{ .user_data = &capture, .log = Capture.log };
    const options: ServerOptions = .{ .logger = &logger };
    var handle: ?*ServerHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_server_create(&options, &handle));
    defer grpc_lite_server_destroy(handle);
    try std.testing.expectEqual(Error.ok, grpc_lite_server_start(handle));
    grpc_lite_server_shutdown_gracefully(handle, std.time.ns_per_s);
    grpc_lite_server_wait(handle);
    try std.testing.expectEqual(@as(u8, 3), capture.state.load(.acquire));
}

test "C managed channel validates options and clears outputs" {
    var output: ?*ChannelHandle = @ptrFromInt(1);
    var options: ChannelOptions = .{};
    options.struct_size = channel_options_v1_size - 1;
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_channel_create_managed(
        null,
        view("127.0.0.1:1"),
        &options,
        &output,
    ));
    try std.testing.expectEqual(null, output);

    const invalid_options = [_]ChannelOptions{
        .{ .allow_initial_offline = 2 },
        .{ .initial_backoff_ns = 0 },
        .{ .initial_backoff_ns = 2, .max_backoff_ns = 1 },
        .{ .multiplier_millis = 999 },
        .{ .jitter_percent = 101 },
        .{ .max_response_header_list_size = 0 },
        .{ .max_response_header_list_size = @as(u64, std.math.maxInt(u32)) + 1 },
    };
    for (invalid_options) |invalid| {
        output = @ptrFromInt(1);
        try std.testing.expectEqual(Error.invalid_argument, grpc_lite_channel_create_managed(
            null,
            view("127.0.0.1:1"),
            &invalid,
            &output,
        ));
        try std.testing.expectEqual(null, output);
    }

    output = @ptrFromInt(1);
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_channel_create_managed(
        null,
        view("invalid"),
        null,
        &output,
    ));
    try std.testing.expectEqual(null, output);
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_channel_create_managed(
        null,
        view("127.0.0.1:1"),
        null,
        null,
    ));
    grpc_lite_channel_shutdown(null);
    grpc_lite_channel_wait(null);
}

test "C managed channel may start before its endpoint" {
    const Capture = struct {
        saw_reconnect: std.atomic.Value(bool) = .init(false),

        fn log(context: ?*anyopaque, level: u32, message: BytesView) callconv(.c) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            const text = bytes(message) orelse return;
            if (level == @intFromEnum(event_logger.Level.debug) and
                std.mem.indexOf(u8, text, "channel reconnect scheduled") != null)
            {
                self.saw_reconnect.store(true, .release);
            }
        }
    };

    var address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var reservation = try address.listen(std.testing.io, .{});
    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(
        reservation.socket.handle,
        @ptrCast(&local_address),
        &address_length,
    )) != .SUCCESS) return error.AddressQueryFailed;
    const port = std.mem.bigToNative(u16, local_address.port);
    reservation.deinit(std.testing.io);

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{port});
    var capture: Capture = .{};
    const logger: Logger = .{ .user_data = &capture, .log = Capture.log };
    const options: ChannelOptions = .{
        .allow_initial_offline = 1,
        .initial_backoff_ns = std.time.ns_per_hour,
        .max_backoff_ns = std.time.ns_per_hour,
        .jitter_percent = 0,
        .logger = &logger,
    };
    var handle: ?*ChannelHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_channel_create_managed(
        null,
        view(target),
        &options,
        &handle,
    ));
    var attempts: usize = 0;
    while (!capture.saw_reconnect.load(.acquire) and attempts < 1000) : (attempts += 1) {
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.testing.expect(capture.saw_reconnect.load(.acquire));
    grpc_lite_channel_shutdown(handle);
    grpc_lite_channel_wait(handle);
    grpc_lite_channel_destroy(handle);
}

test "C metadata owns duplicate binary entries" {
    var handle: ?*MetadataHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_metadata_create(&handle));
    defer grpc_lite_metadata_destroy(handle);

    var value = [_]u8{ 0, 1, 2 };
    try std.testing.expectEqual(Error.ok, grpc_lite_metadata_add(
        handle,
        view("trace-bin"),
        view(&value),
    ));
    value[0] = 9;
    try std.testing.expectEqual(Error.ok, grpc_lite_metadata_add(
        handle,
        view("trace-bin"),
        view("second"),
    ));
    try std.testing.expectEqual(@as(usize, 2), grpc_lite_metadata_count(handle));

    var entry: MetadataEntryView = .{};
    try std.testing.expectEqual(Error.ok, grpc_lite_metadata_at(handle, 0, &entry));
    try std.testing.expectEqualStrings("trace-bin", bytes(entry.key).?);
    try std.testing.expectEqualSlices(u8, &.{ 0, 1, 2 }, bytes(entry.value).?);
    try std.testing.expectEqual(Error.out_of_range, grpc_lite_metadata_at(handle, 2, &entry));
}

test "C runtime has deterministic ownership" {
    var handle: ?*RuntimeHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_runtime_create(&handle));
    try std.testing.expect(handle != null);
    var duplicate: ?*RuntimeHandle = @ptrFromInt(1);
    try std.testing.expectEqual(Error.invalid_state, grpc_lite_runtime_create(&duplicate));
    try std.testing.expectEqual(null, duplicate);
    grpc_lite_runtime_destroy(handle);
    grpc_lite_runtime_destroy(null);
}

test "C ABI rejects invalid pointers and clears outputs" {
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_metadata_create(null));
    var handle: ?*MetadataHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_metadata_create(&handle));
    defer grpc_lite_metadata_destroy(handle);

    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_metadata_add(
        handle,
        .{ .data = null, .size = 1 },
        view("value"),
    ));
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_metadata_add(
        null,
        view("key"),
        view("value"),
    ));
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_metadata_at(handle, 0, null));

    var entry: MetadataEntryView = .{
        .key = view("stale"),
        .value = view("stale"),
    };
    try std.testing.expectEqual(Error.out_of_range, grpc_lite_metadata_at(handle, 0, &entry));
    try std.testing.expectEqual(@as(usize, 0), entry.key.size);
    try std.testing.expectEqual(@as(usize, 0), entry.value.size);
    grpc_lite_metadata_destroy(null);
}

test "C unary ABI validates handles and extensible options" {
    var channel_handle: ?*ChannelHandle = @ptrFromInt(1);
    try std.testing.expectEqual(
        Error.invalid_argument,
        grpc_lite_channel_create(null, view("invalid"), &channel_handle),
    );
    try std.testing.expectEqual(null, channel_handle);

    var result_handle: ?*UnaryResultHandle = @ptrFromInt(1);
    try std.testing.expectEqual(
        Error.invalid_argument,
        grpc_lite_channel_call_unary(null, view("/test.Echo/Unary"), view("request"), null, &result_handle),
    );
    try std.testing.expectEqual(null, result_handle);
    try std.testing.expectEqual(@as(i32, 2), grpc_lite_unary_result_status_code(null));
    try std.testing.expectEqual(@as(usize, 0), grpc_lite_unary_result_payload(null).size);
    grpc_lite_channel_destroy(null);
    grpc_lite_unary_result_destroy(null);

    var options: UnaryOptions = .{};
    try std.testing.expect(parseUnaryOptions(&options) != null);
    options.struct_size = 0;
    try std.testing.expectEqual(null, parseUnaryOptions(&options));
    options = .{};
    options.request_compression = 2;
    try std.testing.expectEqual(null, parseUnaryOptions(&options));
}

test "C unary result exposes owned response data" {
    const storage = try allocator.create(UnaryResultStorage);
    storage.value = try call.Result.initWithCompression(
        allocator,
        .init(.permission_denied, "denied"),
        "response",
        .gzip,
    );
    const handle: *UnaryResultHandle = @ptrCast(storage);
    defer grpc_lite_unary_result_destroy(handle);
    try storage.value.initial_metadata.append("x-initial", "one");
    try storage.value.trailing_metadata.append("trace-bin", &.{ 0, 1 });

    try std.testing.expectEqual(@as(i32, 7), grpc_lite_unary_result_status_code(handle));
    try std.testing.expectEqualStrings("denied", bytes(grpc_lite_unary_result_status_message(handle)).?);
    try std.testing.expectEqualStrings("response", bytes(grpc_lite_unary_result_payload(handle)).?);
    try std.testing.expectEqual(@as(u32, 1), grpc_lite_unary_result_response_compression(handle));
    try std.testing.expectEqual(@as(usize, 1), grpc_lite_unary_result_metadata_count(handle, 0));
    try std.testing.expectEqual(@as(usize, 1), grpc_lite_unary_result_metadata_count(handle, 1));

    var entry: MetadataEntryView = .{};
    try std.testing.expectEqual(Error.ok, grpc_lite_unary_result_metadata_at(handle, 1, 0, &entry));
    try std.testing.expectEqualStrings("trace-bin", bytes(entry.key).?);
    try std.testing.expectEqualSlices(u8, &.{ 0, 1 }, bytes(entry.value).?);
}

test "C stream ABI validates extensible options and callbacks" {
    const Callbacks = struct {
        fn onMessage(_: ?*anyopaque, _: *ClientStreamHandle, _: BytesView, _: u32) callconv(.c) u32 {
            return 0;
        }

        fn onTerminal(_: ?*anyopaque, _: *ClientStreamHandle, _: i32, _: BytesView, _: *const MetadataViewHandle) callconv(.c) void {}
    };

    var options: ClientStreamOptions = .{};
    try std.testing.expect(parseClientStreamOptions(&options) != null);
    options.send_compression = 2;
    try std.testing.expectEqual(null, parseClientStreamOptions(&options));
    options = .{};
    options.max_message_size = 0;
    try std.testing.expect(parseClientStreamOptions(&options) != null);

    var callbacks: ClientStreamCallbacks = .{};
    try std.testing.expectEqual(null, parseClientStreamCallbacks(&callbacks));
    callbacks.on_message = Callbacks.onMessage;
    callbacks.on_terminal = Callbacks.onTerminal;
    try std.testing.expect(parseClientStreamCallbacks(&callbacks) != null);
    callbacks.struct_size = 0;
    try std.testing.expectEqual(null, parseClientStreamCallbacks(&callbacks));

    var stream_handle: ?*ClientStreamHandle = @ptrFromInt(1);
    try std.testing.expectEqual(
        Error.invalid_argument,
        grpc_lite_channel_open_stream(null, view("/test.Echo/Stream"), null, &callbacks, &stream_handle),
    );
    try std.testing.expectEqual(null, stream_handle);
    try std.testing.expectEqual(Error.invalid_argument, grpc_lite_client_stream_send(null, view("x"), 0));
    grpc_lite_client_stream_cancel(null);
    grpc_lite_client_stream_destroy(null);
    grpc_lite_server_call_abort(null);
}

test "C borrowed metadata view exposes entries" {
    var value = metadata.Metadata.init(std.testing.allocator);
    defer value.deinit();
    try value.append("x-test", "value");
    const handle = metadataViewHandle(&value);
    try std.testing.expectEqual(@as(usize, 1), grpc_lite_metadata_view_count(handle));
    var entry: MetadataEntryView = .{};
    try std.testing.expectEqual(Error.ok, grpc_lite_metadata_view_at(handle, 0, &entry));
    try std.testing.expectEqualStrings("x-test", bytes(entry.key).?);
    try std.testing.expectEqualStrings("value", bytes(entry.value).?);
}

test "C client stream completes an event-driven round trip" {
    const ServerState = struct {
        worker: ?std.Thread = null,
        worker_ready: std.atomic.Value(bool) = .init(false),
    };
    const ServerCallbacks = struct {
        fn onMessage(
            _: ?*anyopaque,
            stream: *ServerStreamHandle,
            context: *const ServerContextHandle,
            payload: BytesView,
            _: u32,
        ) callconv(.c) u32 {
            if (grpc_lite_server_context_request_compression(context) != 0) return 1;
            var call_handle: ?*ServerCallHandle = null;
            if (grpc_lite_server_stream_retain(stream, &call_handle) != .ok) return 1;
            defer grpc_lite_server_call_destroy(call_handle);
            if (grpc_lite_server_call_send_initial_metadata(call_handle, null, 0) != .ok) return 1;
            if (!std.mem.eql(u8, bytes(payload).?, "hello")) return 1;
            return 0;
        }

        fn onRemoteEnd(
            user_data: ?*anyopaque,
            stream: *ServerStreamHandle,
            _: *const ServerContextHandle,
        ) callconv(.c) void {
            var call_handle: ?*ServerCallHandle = null;
            if (grpc_lite_server_stream_retain(stream, &call_handle) != .ok) return;
            const state: *ServerState = @ptrCast(@alignCast(user_data.?));
            state.worker = std.Thread.spawn(.{}, worker, .{call_handle.?}) catch {
                grpc_lite_server_call_destroy(call_handle);
                return;
            };
            state.worker_ready.store(true, .release);
        }

        fn worker(call_handle: *ServerCallHandle) void {
            defer grpc_lite_server_call_destroy(call_handle);
            if (grpc_lite_server_call_send(call_handle, view("hello"), 0) != .ok) return;
            _ = grpc_lite_server_call_finish(call_handle, 0, view(""), null);
        }
    };
    const State = struct {
        message_seen: std.atomic.Value(bool) = .init(false),
        done: std.atomic.Value(bool) = .init(false),
        succeeded: std.atomic.Value(bool) = .init(false),

        fn onMessage(
            user_data: ?*anyopaque,
            _: *ClientStreamHandle,
            payload: BytesView,
            compression: u32,
        ) callconv(.c) u32 {
            const self: *@This() = @ptrCast(@alignCast(user_data.?));
            self.message_seen.store(
                compression == @intFromEnum(Compression.identity) and
                    std.mem.eql(u8, bytes(payload).?, "hello"),
                .release,
            );
            return 0;
        }

        fn onTerminal(
            user_data: ?*anyopaque,
            _: *ClientStreamHandle,
            status_code: i32,
            _: BytesView,
            trailing_metadata: *const MetadataViewHandle,
        ) callconv(.c) void {
            const self: *@This() = @ptrCast(@alignCast(user_data.?));
            self.succeeded.store(
                status_code == 0 and grpc_lite_metadata_view_count(trailing_metadata) == 0,
                .release,
            );
            self.done.store(true, .release);
        }
    };

    var server_state: ServerState = .{};
    var method_callbacks: ServerMethodCallbacks = .{
        .user_data = &server_state,
        .on_message = ServerCallbacks.onMessage,
        .on_remote_end = ServerCallbacks.onRemoteEnd,
    };
    var server_handle: ?*ServerHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_server_create(null, &server_handle));
    defer grpc_lite_server_destroy(server_handle);
    try std.testing.expectEqual(Error.ok, grpc_lite_server_register_stream(
        server_handle,
        view("/test.CAbi/Echo"),
        null,
        &method_callbacks,
    ));
    try std.testing.expectEqual(Error.ok, grpc_lite_server_start(server_handle));

    var target_buffer: [32]u8 = undefined;
    var port: u32 = 0;
    try std.testing.expectEqual(Error.ok, grpc_lite_server_port(server_handle, &port));
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{port});
    var channel_handle: ?*ChannelHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_channel_create(null, view(target), &channel_handle));
    defer grpc_lite_channel_destroy(channel_handle);

    var state: State = .{};
    var callbacks: ClientStreamCallbacks = .{
        .user_data = &state,
        .on_message = State.onMessage,
        .on_terminal = State.onTerminal,
    };
    var stream_handle: ?*ClientStreamHandle = null;
    try std.testing.expectEqual(Error.ok, grpc_lite_channel_open_stream(
        channel_handle,
        view("/test.CAbi/Echo"),
        null,
        &callbacks,
        &stream_handle,
    ));
    defer grpc_lite_client_stream_destroy(stream_handle);
    try std.testing.expectEqual(Error.ok, grpc_lite_client_stream_send(stream_handle, view("hello"), 0));
    try std.testing.expectEqual(Error.ok, grpc_lite_client_stream_close_send(stream_handle));

    var attempts: usize = 0;
    while (!state.done.load(.acquire)) {
        if (attempts == 5000) return error.StreamTimeout;
        attempts += 1;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    while (!server_state.worker_ready.load(.acquire)) std.atomic.spinLoopHint();
    server_state.worker.?.join();
    try std.testing.expect(state.message_seen.load(.acquire));
    try std.testing.expect(state.succeeded.load(.acquire));
}
