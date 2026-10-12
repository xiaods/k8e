const std = @import("std");
const builtin = @import("builtin");
const build_options = @import("grpc_lite_options");
const c = @import("c.zig").api;
const cares_adapter = @import("cares_adapter.zig");
const xev = @import("xev");
const call = @import("call.zig");
const Compression = @import("compression.zig").Compression;
const deadline_wire = @import("deadline.zig");
const fast_clock = @import("fast_clock.zig");
const frame = @import("frame.zig");
const message = @import("message.zig");
const metadata = @import("metadata.zig");
const event_logger = @import("logger.zig");
const socket_options = @import("socket_options.zig");
const status = @import("status.zig");
const stream = @import("stream.zig");
const version = @import("version.zig");
const Runtime = @import("runtime.zig").Runtime;
const tls_record = if (build_options.tls) @import("tls_record.zig") else @import("tls_disabled.zig");

// Bounds socket-write aggregation only; HTTP/2 frame boundaries remain unchanged.
const socket_write_batch_target = 64 * 1024;
const request_header_stack_capacity = 10;
const encoded_value_stack_capacity = 4;

fn StackFirstBuilder(comptime T: type, comptime stack_capacity: usize) type {
    return struct {
        stack: [stack_capacity]T = undefined,
        stack_len: usize = 0,
        overflow: std.ArrayList(T) = .empty,
        overflowed: bool = false,

        fn append(self: *@This(), allocator: std.mem.Allocator, value: T) !void {
            if (self.overflowed) return self.overflow.append(allocator, value);
            if (self.stack_len < stack_capacity) {
                self.stack[self.stack_len] = value;
                self.stack_len += 1;
                return;
            }

            var overflow: std.ArrayList(T) = .empty;
            errdefer overflow.deinit(allocator);
            try overflow.ensureTotalCapacity(allocator, stack_capacity * 2);
            overflow.appendSliceAssumeCapacity(self.stack[0..self.stack_len]);
            overflow.appendAssumeCapacity(value);
            self.overflow = overflow;
            self.overflowed = true;
        }

        fn items(self: *@This()) []T {
            return if (self.overflowed) self.overflow.items else self.stack[0..self.stack_len];
        }

        fn deinit(self: *@This(), allocator: std.mem.Allocator) void {
            self.overflow.deinit(allocator);
            self.* = undefined;
        }
    };
}

const HeaderBuilder = StackFirstBuilder(c.nghttp2_nv, request_header_stack_capacity);
const EncodedValueBuilder = StackFirstBuilder(metadata.OutboundValue, encoded_value_stack_capacity);

threadlocal var receiving_http2_impl: ?*Impl = null;

pub const TlsOptions = struct {
    ca_certificates_pem: []const u8,
    handshake_timeout_ns: u64 = 10 * std.time.ns_per_s,
};

pub const ReconnectOptions = struct {
    allow_initial_offline: bool = false,
    initial_backoff_ns: u64 = std.time.ns_per_s,
    max_backoff_ns: u64 = 120 * std.time.ns_per_s,
    multiplier_millis: u32 = 1600,
    jitter_percent: u8 = 20,
};

const ChannelOptions = struct {
    user_agent: []const u8 = version.user_agent,
    initial_stream_window_size: u32 = 64 * 1024,
    max_response_header_list_size: u32 = 64 * 1024,
    write_high_watermark_bytes: usize = 1024 * 1024,
    write_low_watermark_bytes: usize = 512 * 1024,
    runtime: ?*Runtime = null,
    tls: ?TlsOptions = null,
    reconnect: ?ReconnectOptions = null,
    logger: event_logger.Logger = .{},
};

pub const Options = ChannelOptions;

const AllocatorOperation = enum { alloc, resize, remap, free };

const SerializedAllocator = struct {
    backing: std.mem.Allocator,
    mutex: std.Io.Mutex = .init,
    references: std.atomic.Value(usize) = .init(1),
    test_hook_context: if (builtin.is_test) std.atomic.Value(?*anyopaque) else void = if (builtin.is_test) .init(null) else {},
    test_before_lock: if (builtin.is_test) std.atomic.Value(?*const fn (?*anyopaque, AllocatorOperation) void) else void = if (builtin.is_test) .init(null) else {},

    fn init(backing: std.mem.Allocator) SerializedAllocator {
        return .{ .backing = backing };
    }

    fn allocator(self: *SerializedAllocator) std.mem.Allocator {
        return .{
            .ptr = self,
            .vtable = &.{
                .alloc = alloc,
                .resize = resize,
                .remap = remap,
                .free = free,
            },
        };
    }

    fn retain(self: *SerializedAllocator) void {
        _ = self.references.fetchAdd(1, .monotonic);
    }

    fn release(self: *SerializedAllocator) void {
        if (self.references.fetchSub(1, .acq_rel) == 1) {
            const backing = self.backing;
            backing.destroy(self);
        }
    }

    fn lock(self: *SerializedAllocator, operation: AllocatorOperation) void {
        if (comptime builtin.is_test) {
            if (self.test_before_lock.load(.acquire)) |hook| {
                hook(self.test_hook_context.load(.acquire), operation);
            }
        }
        self.mutex.lockUncancelable(syncIo());
    }

    fn unlock(self: *SerializedAllocator) void {
        self.mutex.unlock(syncIo());
    }

    fn alloc(context: *anyopaque, len: usize, alignment: std.mem.Alignment, ret_addr: usize) ?[*]u8 {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.lock(.alloc);
        defer self.unlock();
        return self.backing.rawAlloc(len, alignment, ret_addr);
    }

    fn resize(
        context: *anyopaque,
        memory: []u8,
        alignment: std.mem.Alignment,
        new_len: usize,
        ret_addr: usize,
    ) bool {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.lock(.resize);
        defer self.unlock();
        return self.backing.rawResize(memory, alignment, new_len, ret_addr);
    }

    fn remap(
        context: *anyopaque,
        memory: []u8,
        alignment: std.mem.Alignment,
        new_len: usize,
        ret_addr: usize,
    ) ?[*]u8 {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.lock(.remap);
        defer self.unlock();
        return self.backing.rawRemap(memory, alignment, new_len, ret_addr);
    }

    fn free(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, ret_addr: usize) void {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.lock(.free);
        defer self.unlock();
        self.backing.rawFree(memory, alignment, ret_addr);
    }
};

pub const Channel = struct {
    pub const Options = ChannelOptions;

    impl: *Impl,

    pub fn init(allocator: std.mem.Allocator, target: []const u8, options: ChannelOptions) !Channel {
        if (options.tls != null and !build_options.tls) return error.TlsUnavailable;
        if (options.tls) |tls_options| {
            if (tls_options.handshake_timeout_ns == 0) return error.InvalidTlsHandshakeTimeout;
        }
        if (options.reconnect) |reconnect| try validateReconnectOptions(reconnect);
        if (options.max_response_header_list_size == 0) {
            return error.InvalidMaxResponseHeaderListSize;
        }
        try validateTransportOptions(
            options.initial_stream_window_size,
            options.write_high_watermark_bytes,
            options.write_low_watermark_bytes,
        );
        fast_clock.warmup(syncIo());
        const parsed = try parseTarget(target);
        const literal_address = std.Io.net.IpAddress.parseIp4(parsed.host, parsed.port) catch null;
        if (literal_address == null) {
            const runtime = options.runtime orelse return error.RuntimeRequired;
            if (!runtime.isInitialized()) return error.RuntimeNotInitialized;
        }
        const impl = try allocator.create(Impl);
        errdefer allocator.destroy(impl);
        const serialized_allocator = try allocator.create(SerializedAllocator);
        errdefer allocator.destroy(serialized_allocator);
        serialized_allocator.* = .init(allocator);

        impl.* = .{
            .backing_allocator = allocator,
            .serialized_allocator = serialized_allocator,
            .allocator = undefined,
            .host = undefined,
            .port = parsed.port,
            .runtime = options.runtime,
            .literal_address = literal_address,
            .authority = undefined,
            .user_agent = undefined,
            .initial_stream_window_size = options.initial_stream_window_size,
            .max_response_header_list_size = options.max_response_header_list_size,
            .write_high_watermark_bytes = options.write_high_watermark_bytes,
            .write_low_watermark_bytes = options.write_low_watermark_bytes,
            .tls_config = null,
            .tls_handshake_timeout_ns = if (options.tls) |tls_options| tls_options.handshake_timeout_ns else 0,
            .reconnect_options = options.reconnect,
            .reconnect_backoff_ns = if (options.reconnect) |reconnect| reconnect.initial_backoff_ns else 0,
            .reconnect_random_state = std.hash.Wyhash.hash(nowNs(), target),
            .logger = options.logger,
            .async_handle = undefined,
            .deadline_timer = undefined,
        };
        impl.allocator = serialized_allocator.allocator();

        if (comptime build_options.tls) {
            if (options.tls) |tls_options| {
                impl.tls_config = try tls_record.Config.createClient(
                    impl.allocator,
                    tls_options.ca_certificates_pem,
                );
            }
        }
        errdefer if (comptime build_options.tls) {
            if (impl.tls_config) |config| config.destroy();
        };

        const host = try impl.allocator.dupeZ(u8, parsed.host);
        errdefer impl.allocator.free(host);
        const authority = try impl.allocator.dupe(u8, target);
        errdefer impl.allocator.free(authority);
        const user_agent = try impl.allocator.dupe(u8, options.user_agent);
        errdefer impl.allocator.free(user_agent);

        var async_handle = try xev.Async.init();
        errdefer async_handle.deinit();
        var deadline_timer = try xev.Timer.init();
        errdefer deadline_timer.deinit();

        impl.host = host;
        impl.authority = authority;
        impl.user_agent = user_agent;
        impl.async_handle = async_handle;
        impl.deadline_timer = deadline_timer;
        impl.thread = try std.Thread.spawn(.{}, runLoop, .{impl});

        impl.mutex.lockUncancelable(syncIo());
        while (impl.state == .starting) impl.condition.waitUncancelable(syncIo(), &impl.mutex);
        const running = impl.state == .running;
        impl.mutex.unlock(syncIo());
        if (!running) {
            impl.thread.?.join();
            impl.thread = null;
            impl.pending.deinit(impl.allocator);
            std.debug.assert(impl.waiting_operation_head == null);
            std.debug.assert(impl.waiting_operation_tail == null);
            impl.operations.deinit(impl.allocator);
            impl.stream_states.deinit(impl.allocator);
            impl.streams.deinit(impl.allocator);
            drainWriteRequestPool(impl);
            impl.writes.deinit(impl.allocator);
            std.debug.assert(impl.deadline_heap.items.len == 0);
            std.debug.assert(impl.deadline_heap_index == null);
            std.debug.assert(impl.reconnect_heap_index == null);
            impl.deadline_heap.deinit(impl.allocator);
            if (impl.resolved_addresses.len != 0) impl.allocator.free(impl.resolved_addresses);
            return error.ConnectionFailed;
        }

        return .{ .impl = impl };
    }

    /// May be called concurrently. Input slices are borrowed until this function returns.
    pub fn callUnary(
        self: *Channel,
        allocator: std.mem.Allocator,
        full_method_path: []const u8,
        request: []const u8,
        options: call.Options,
    ) !call.Result {
        if (!isValidMethodPath(full_method_path)) return error.InvalidMethodPath;
        if (options.max_response_size > std.math.maxInt(u32)) return error.InvalidMaxResponseSize;

        const operation = try Operation.init(self.impl, full_method_path, request, options);
        defer operation.deinit();

        const queued = try self.impl.enqueue(operation);
        if (!queued) operation.setOutcome(.unavailable, "channel is unavailable") catch {};
        if (!queued) operation.finish();
        operation.wait();

        var result = try call.Result.initWithCompression(
            allocator,
            .init(operation.response_code, operation.response_message),
            if (operation.response_payload) |payload| payload else &.{},
            operation.response_compression,
        );
        errdefer result.deinit();
        try copyMetadata(&result.initial_metadata, &operation.initial_metadata);
        try copyMetadata(&result.trailing_metadata, &operation.trailing_metadata);
        return result;
    }

    /// Starts an event-driven raw unary call and copies all inputs before return.
    ///
    /// A successful return means the call was accepted and `on_complete` will run
    /// exactly once on the transport loop thread. Result data is borrowed only for
    /// the callback. The callback must not block or call `Channel.deinit`.
    pub fn callUnaryAsync(
        self: *Channel,
        full_method_path: []const u8,
        request: []const u8,
        options: call.Options,
        callbacks: call.Callbacks,
    ) !void {
        if (!isValidMethodPath(full_method_path)) return error.InvalidMethodPath;
        if (options.max_response_size > std.math.maxInt(u32)) return error.InvalidMaxResponseSize;

        const operation = try Operation.initAsync(
            self.impl,
            full_method_path,
            request,
            options,
            callbacks,
        );
        errdefer operation.deinit();
        if (!try self.impl.enqueue(operation)) return error.ChannelUnavailable;
    }

    /// Opens an event-driven raw duplex stream. Input slices are copied before return.
    pub fn openStream(
        self: *Channel,
        full_method_path: []const u8,
        options: stream.Options,
        callbacks: stream.ClientCallbacks,
    ) !stream.ClientStream {
        if (!isValidMethodPath(full_method_path)) return error.InvalidMethodPath;
        try options.limits.validate();
        if (options.limits.max_inbound_buffer_size < self.impl.initial_stream_window_size) {
            return error.InvalidInboundBufferSize;
        }

        const client_stream = try ClientStreamState.init(
            self.impl,
            full_method_path,
            options,
            callbacks,
        );
        errdefer client_stream.destroyUnqueued();

        self.impl.mutex.lockUncancelable(syncIo());
        defer self.impl.mutex.unlock(syncIo());
        if (self.impl.state != .running or !self.impl.accepting_streams) {
            return error.ChannelUnavailable;
        }
        try self.impl.stream_states.put(self.impl.allocator, client_stream, {});
        errdefer _ = self.impl.stream_states.remove(client_stream);
        appendStreamWake(self.impl, client_stream);
        if (!self.impl.stream_wake_notify_pending) {
            self.impl.stream_wake_notify_pending = true;
            self.impl.async_handle.notify() catch {
                self.impl.stream_wake_notify_pending = false;
                removeStreamWake(self.impl, client_stream);
                return error.ChannelUnavailable;
            };
        }
        return client_stream.handle();
    }

    /// May be called concurrently with active calls and causes them to finish promptly.
    pub fn shutdown(self: *Channel) void {
        const impl = self.impl;
        impl.mutex.lockUncancelable(syncIo());
        var notify = false;
        if (impl.state == .running) {
            impl.state = .stopping;
            notify = true;
        }
        impl.mutex.unlock(syncIo());
        if (notify) {
            impl.logger.write(.info, "channel shutdown requested target={s}", .{impl.authority});
            impl.async_handle.notify() catch {};
        }
    }

    /// Waits for the channel event loop after shutdown.
    pub fn wait(self: *Channel) void {
        const impl = self.impl;
        impl.mutex.lockUncancelable(syncIo());
        const thread = impl.thread;
        impl.thread = null;
        if (thread == null) {
            while (impl.state != .stopped) impl.condition.waitUncancelable(syncIo(), &impl.mutex);
        }
        impl.mutex.unlock(syncIo());
        if (thread) |running_thread| running_thread.join();
    }

    /// Requires exclusive access after all concurrent calls have returned.
    pub fn deinit(self: *Channel) void {
        const impl = self.impl;
        self.shutdown();
        self.wait();

        impl.pending.deinit(impl.allocator);
        std.debug.assert(impl.waiting_operation_head == null);
        std.debug.assert(impl.waiting_operation_tail == null);
        impl.operations.deinit(impl.allocator);
        impl.stream_states.deinit(impl.allocator);
        impl.streams.deinit(impl.allocator);
        drainWriteRequestPool(impl);
        impl.writes.deinit(impl.allocator);
        std.debug.assert(impl.deadline_heap.items.len == 0);
        std.debug.assert(impl.deadline_heap_index == null);
        std.debug.assert(impl.reconnect_heap_index == null);
        impl.deadline_heap.deinit(impl.allocator);
        if (comptime build_options.tls) {
            if (impl.tls_config) |config| config.destroy();
        }
        if (impl.resolved_addresses.len != 0) impl.allocator.free(impl.resolved_addresses);
        impl.async_handle.deinit();
        impl.deadline_timer.deinit();
        impl.allocator.free(impl.user_agent);
        impl.allocator.free(impl.authority);
        impl.allocator.free(impl.host);
        const backing_allocator = impl.backing_allocator;
        const serialized_allocator = impl.serialized_allocator;
        backing_allocator.destroy(impl);
        serialized_allocator.release();
        self.* = undefined;
    }
};

const State = enum { starting, running, stopping, stopped };
const ConnectionState = enum { resolving, connecting, handshaking, active, draining, backing_off, closing };
const ResolveState = enum { idle, pending, ready, failed, cancelled };

const TestObserver = if (builtin.is_test) struct {
    write_requested: std.atomic.Value(bool) = .init(false),
    write_observed: std.atomic.Value(bool) = .init(false),
    write_observed_sem: std.Io.Semaphore = .{},
    connect_requested: std.atomic.Value(bool) = .init(false),
    connect_observed: std.atomic.Value(bool) = .init(false),
    connect_observed_sem: std.Io.Semaphore = .{},
    connect_release: std.Io.Semaphore = .{},
    connect_cancel_confirmed: std.atomic.Value(bool) = .init(false),
    reconnect_scheduled: std.atomic.Value(bool) = .init(false),
    operation_submitted: std.atomic.Value(bool) = .init(false),
    deadline_timer_callbacks: std.atomic.Value(usize) = .init(0),
    deadline_timer_armed: std.atomic.Value(bool) = .init(false),
    deadline_timer_target_ns: std.atomic.Value(u64) = .init(0),
} else struct {};

const Impl = struct {
    backing_allocator: std.mem.Allocator,
    serialized_allocator: *SerializedAllocator,
    allocator: std.mem.Allocator,
    host: [:0]u8,
    port: u16,
    runtime: ?*Runtime,
    literal_address: ?std.Io.net.IpAddress,
    authority: []u8,
    user_agent: []u8,
    initial_stream_window_size: u32,
    max_response_header_list_size: u32,
    write_high_watermark_bytes: usize,
    write_low_watermark_bytes: usize,
    tls_config: ?*tls_record.Config = null,
    tls_handshake_timeout_ns: u64 = 0,
    tls_handshake_deadline_ns: ?u64 = null,
    deadline_heap_index: ?usize = null,
    reconnect_options: ?ReconnectOptions,
    reconnect_attempt: u32 = 0,
    reconnect_backoff_ns: u64,
    reconnect_deadline_ns: ?u64 = null,
    reconnect_heap_index: ?usize = null,
    reconnect_after_close: bool = false,
    discard_writes_after_cancel: bool = false,
    reconnect_backoff_pending_reset: bool = false,
    ever_active: bool = false,
    reconnect_random_state: u64,
    logger: event_logger.Logger,
    tls_handshake_needs_write: bool = false,
    tls_session: ?*tls_record.Session = null,
    tls_plaintext: ?[]u8 = null,
    tls_plaintext_offset: usize = 0,
    mutex: std.Io.Mutex = .init,
    condition: std.Io.Condition = .init,
    state: State = .starting,
    thread: ?std.Thread = null,
    pending: std.ArrayList(*Operation) = .empty,
    waiting_operation_head: ?*Operation = null,
    waiting_operation_tail: ?*Operation = null,
    operations: std.AutoHashMapUnmanaged(i32, *Operation) = .empty,
    stream_states: std.AutoHashMapUnmanaged(*ClientStreamState, void) = .empty,
    streams: std.AutoHashMapUnmanaged(i32, *ClientStreamState) = .empty,
    stream_wake_head: ?*ClientStreamState = null,
    stream_wake_tail: ?*ClientStreamState = null,
    stream_wake_notify_pending: bool = false,
    accepting_streams: bool = false,
    loop: xev.Loop = undefined,
    tcp: xev.TCP = undefined,
    connect_completion: xev.Completion = .{},
    connect_cancel_completion: xev.Completion = .{},
    read_completion: xev.Completion = .{},
    read_cancel_completion: xev.Completion = .{},
    write_cancel_completion: xev.Completion = .{},
    close_completion: xev.Completion = .{},
    async_handle: xev.Async = undefined,
    async_completion: xev.Completion = .{},
    write_wake_queued: bool = false,
    deadline_timer: xev.Timer = undefined,
    deadline_completion: xev.Completion = .{},
    deadline_reset_completion: xev.Completion = .{},
    session: ?*c.nghttp2_session = null,
    loop_initialized: bool = false,
    tcp_initialized: bool = false,
    connect_active: bool = false,
    connect_cancel_submitted: bool = false,
    read_active: bool = false,
    read_cancel_submitted: bool = false,
    write_cancel_submitted: bool = false,
    write_cancel_target: ?*xev.Completion = null,
    close_submitted: bool = false,
    close_completed: bool = false,
    deadline_timer_armed: bool = false,
    deadline_timer_deadline_ns: ?u64 = null,
    deadline_heap: std.ArrayList(DeadlineEntry) = .empty,
    connected: bool = false,
    connection_state: ConnectionState = .connecting,
    resolver: cares_adapter.Adapter = undefined,
    resolver_initialized: bool = false,
    resolve_state: ResolveState = .idle,
    resolved_addresses: []std.Io.net.IpAddress = &.{},
    next_address: usize = 0,
    connection_generation: std.atomic.Value(usize) = .init(0),
    stopping_on_loop: bool = false,
    connect_count: std.atomic.Value(usize) = .init(0),
    read_buffer: [16 * 1024]u8 = undefined,
    plaintext_buffer: [16 * 1024]u8 = undefined,
    write_queue: xev.WriteQueue = .{},
    writes: std.AutoHashMapUnmanaged(*WriteRequest, void) = .empty,
    write_request_pool_head: ?*WriteRequest = null,
    write_request_pool_count: usize = 0,
    queued_write_bytes: usize = 0,
    test_observer: TestObserver = .{},

    fn enqueue(self: *Impl, operation: *Operation) !bool {
        self.mutex.lockUncancelable(syncIo());
        defer self.mutex.unlock(syncIo());
        if (self.state != .running) return false;
        try self.pending.append(self.allocator, operation);
        if (receiving_http2_impl == self) return true;
        if (self.async_handle.notify()) |_| {} else |_| {
            _ = self.pending.pop();
            return false;
        }
        return true;
    }

    fn signalStartup(self: *Impl, succeeded: bool) void {
        self.mutex.lockUncancelable(syncIo());
        if (self.state == .starting) self.state = if (succeeded) .running else .stopping;
        self.condition.broadcast(syncIo());
        self.mutex.unlock(syncIo());
    }

    fn markStopped(self: *Impl) void {
        self.mutex.lockUncancelable(syncIo());
        self.state = .stopped;
        self.condition.broadcast(syncIo());
        self.mutex.unlock(syncIo());
    }
};

const OutboundMessage = struct {
    frame_bytes: []u8,
    offset: usize = 0,
};

const InboundMessage = struct {
    payload: []u8,
    compression: Compression,
    buffered_size: usize,
};

threadlocal var active_client_callback: ?*ClientStreamState = null;

const ClientStreamState = struct {
    impl: ?*Impl,
    allocator: std.mem.Allocator,
    allocator_owner: *SerializedAllocator,
    path: []u8,
    request_metadata: metadata.Metadata,
    callbacks: stream.ClientCallbacks,
    limits: stream.BufferLimits,
    request_compression: Compression,
    deadline_ns: ?u64,
    deadline_heap_index: ?usize = null,
    timeout_header: [16]u8 = undefined,
    timeout_header_len: usize = 0,
    mutex: std.Io.Mutex = .init,
    callback_mutex: std.Io.Mutex = .init,
    app_owned: bool = true,
    loop_owned: bool = true,
    app_released: bool = false,
    terminal: bool = false,
    send_open: bool = true,
    cancel_requested: bool = false,
    resume_requested: bool = false,
    backpressure_requested: bool = false,
    outbound: std.ArrayList(OutboundMessage) = .empty,
    outbound_head: usize = 0,
    outbound_buffered: usize = 0,
    stream_id: i32 = -1,
    wake_queued: bool = false,
    wake_next: ?*ClientStreamState = null,
    provider_deferred: bool = false,
    provider_eof: bool = false,
    rst_submitted: bool = false,
    deadline_expired: bool = false,
    receive_paused: bool = false,
    deferred_stream_credit: usize = 0,
    inbound: std.ArrayList(InboundMessage) = .empty,
    inbound_head: usize = 0,
    inbound_buffered: usize = 0,
    decoder: frame.Decoder,
    response_compression: Compression = .identity,
    response_encoding_invalid: bool = false,
    response_metadata_invalid: bool = false,
    response_header_list_size: usize = 0,
    retained_response_metadata_size: usize = 0,
    response_headers_too_large: bool = false,
    saw_response_headers: bool = false,
    headers_called: bool = false,
    remote_end_seen: bool = false,
    remote_end_called: bool = false,
    transport_closed: bool = false,
    stream_error: u32 = c.NGHTTP2_NO_ERROR,
    http_status: ?u16 = null,
    content_type_grpc: bool = false,
    grpc_status: ?u32 = null,
    grpc_message: ?[]u8 = null,
    initial_metadata: metadata.Metadata,
    trailing_metadata: metadata.Metadata,
    block_kind: HeaderKind = .none,
    block_metadata: metadata.Metadata,
    block_grpc_status: ?u32 = null,
    block_grpc_message: ?[]u8 = null,
    forced_status: ?status.Status = null,

    fn init(
        impl: *Impl,
        path: []const u8,
        options: stream.Options,
        callbacks: stream.ClientCallbacks,
    ) !*ClientStreamState {
        const self = try impl.allocator.create(ClientStreamState);
        errdefer impl.allocator.destroy(self);
        impl.serialized_allocator.retain();
        errdefer impl.serialized_allocator.release();
        const owned_path = try impl.allocator.dupe(u8, path);
        errdefer impl.allocator.free(owned_path);
        self.* = .{
            .impl = impl,
            .allocator = impl.allocator,
            .allocator_owner = impl.serialized_allocator,
            .path = owned_path,
            .request_metadata = metadata.Metadata.init(impl.allocator),
            .callbacks = callbacks,
            .limits = options.limits,
            .request_compression = options.send_compression,
            .deadline_ns = if (options.timeout_ns) |timeout| nowNs() +| timeout else null,
            .decoder = frame.Decoder.init(impl.allocator, options.limits.max_message_size),
            .initial_metadata = metadata.Metadata.init(impl.allocator),
            .trailing_metadata = metadata.Metadata.init(impl.allocator),
            .block_metadata = metadata.Metadata.init(impl.allocator),
        };
        errdefer self.request_metadata.deinit();
        errdefer self.decoder.deinit();
        errdefer self.initial_metadata.deinit();
        errdefer self.trailing_metadata.deinit();
        errdefer self.block_metadata.deinit();
        for (options.metadata) |entry| {
            if (!isRequestMetadata(entry.key)) return error.InvalidMetadataKey;
            try self.request_metadata.append(entry.key, entry.value);
        }
        return self;
    }

    fn handle(self: *ClientStreamState) stream.ClientStream {
        return stream.ClientStream.init(
            self,
            clientStreamSend,
            clientStreamCloseSend,
            clientStreamCancel,
            clientStreamResumeReceive,
            clientStreamRelease,
        );
    }

    fn destroyUnqueued(self: *ClientStreamState) void {
        self.app_owned = false;
        self.loop_owned = false;
        self.destroy();
    }

    fn destroy(self: *ClientStreamState) void {
        std.debug.assert(self.deadline_heap_index == null);
        const allocator = self.allocator;
        const allocator_owner = self.allocator_owner;
        allocator.free(self.path);
        self.request_metadata.deinit();
        for (self.outbound.items[self.outbound_head..]) |item| allocator.free(item.frame_bytes);
        self.outbound.deinit(allocator);
        for (self.inbound.items[self.inbound_head..]) |item| allocator.free(item.payload);
        self.inbound.deinit(allocator);
        self.decoder.deinit();
        if (self.grpc_message) |value| allocator.free(value);
        if (self.block_grpc_message) |value| allocator.free(value);
        self.initial_metadata.deinit();
        self.trailing_metadata.deinit();
        self.block_metadata.deinit();
        allocator.destroy(self);
        allocator_owner.release();
    }

    fn resetHeaderBlock(self: *ClientStreamState, kind: HeaderKind) void {
        self.block_metadata.deinit();
        self.block_metadata = metadata.Metadata.init(self.allocator);
        if (self.block_grpc_message) |value| self.allocator.free(value);
        self.block_grpc_message = null;
        self.block_grpc_status = null;
        self.block_kind = kind;
    }
};

fn appendStreamWake(impl: *Impl, client_stream: *ClientStreamState) void {
    std.debug.assert(!client_stream.wake_queued);
    client_stream.wake_queued = true;
    client_stream.wake_next = null;
    if (impl.stream_wake_tail) |tail| {
        tail.wake_next = client_stream;
    } else {
        impl.stream_wake_head = client_stream;
    }
    impl.stream_wake_tail = client_stream;
}

fn removeStreamWake(impl: *Impl, client_stream: *ClientStreamState) void {
    var previous: ?*ClientStreamState = null;
    var current = impl.stream_wake_head;
    while (current) |item| : (current = item.wake_next) {
        if (item != client_stream) {
            previous = item;
            continue;
        }
        if (previous) |before| {
            before.wake_next = item.wake_next;
        } else {
            impl.stream_wake_head = item.wake_next;
        }
        if (impl.stream_wake_tail == item) impl.stream_wake_tail = previous;
        item.wake_queued = false;
        item.wake_next = null;
        return;
    }
}

fn wakeClientStreamLocked(client_stream: *ClientStreamState) void {
    const impl = client_stream.impl orelse return;
    if (!client_stream.loop_owned) return;
    impl.mutex.lockUncancelable(syncIo());
    defer impl.mutex.unlock(syncIo());
    if (impl.stream_states.contains(client_stream) and !client_stream.wake_queued) {
        appendStreamWake(impl, client_stream);
    }
    if (impl.stream_wake_head != null and !impl.stream_wake_notify_pending) {
        impl.stream_wake_notify_pending = true;
        impl.async_handle.notify() catch {
            impl.stream_wake_notify_pending = false;
        };
    }
}

fn clientStreamSend(context: *anyopaque, payload: []const u8, options: stream.SendOptions) !void {
    const client_stream: *ClientStreamState = @ptrCast(@alignCast(context));
    if (payload.len > client_stream.limits.max_message_size) return error.MessageTooLarge;
    if (options.compression != .identity and options.compression != client_stream.request_compression) {
        return error.CompressionNotConfigured;
    }
    client_stream.mutex.lockUncancelable(syncIo());
    if (client_stream.impl == null or !client_stream.app_owned or client_stream.app_released or
        client_stream.cancel_requested or client_stream.terminal)
    {
        client_stream.mutex.unlock(syncIo());
        return error.StreamClosed;
    }
    client_stream.mutex.unlock(syncIo());

    const encoded = try frame.encodeWithCompression(client_stream.allocator, payload, options.compression);
    var owned = true;
    defer if (owned) client_stream.allocator.free(encoded);

    client_stream.mutex.lockUncancelable(syncIo());
    defer client_stream.mutex.unlock(syncIo());
    if (client_stream.impl == null or !client_stream.app_owned or client_stream.app_released or
        client_stream.cancel_requested or client_stream.terminal) return error.StreamClosed;
    if (!client_stream.send_open) return error.SendClosed;
    if (encoded.len > client_stream.limits.max_outbound_buffer_size) return error.OutboundBufferLimitExceeded;
    if (encoded.len > client_stream.limits.max_outbound_buffer_size - client_stream.outbound_buffered) {
        client_stream.backpressure_requested = true;
        return error.WouldBlock;
    }
    try client_stream.outbound.append(client_stream.allocator, .{ .frame_bytes = encoded });
    client_stream.outbound_buffered += encoded.len;
    owned = false;
    wakeClientStreamLocked(client_stream);
}

fn clientStreamCloseSend(context: *anyopaque) !void {
    const client_stream: *ClientStreamState = @ptrCast(@alignCast(context));
    client_stream.mutex.lockUncancelable(syncIo());
    defer client_stream.mutex.unlock(syncIo());
    if (client_stream.impl == null or !client_stream.app_owned or client_stream.app_released or
        client_stream.cancel_requested or client_stream.terminal) return error.StreamClosed;
    if (!client_stream.send_open) return error.SendClosed;
    client_stream.send_open = false;
    wakeClientStreamLocked(client_stream);
}

fn clientStreamCancel(context: *anyopaque) void {
    const client_stream: *ClientStreamState = @ptrCast(@alignCast(context));
    client_stream.mutex.lockUncancelable(syncIo());
    defer client_stream.mutex.unlock(syncIo());
    if (client_stream.impl == null or !client_stream.app_owned or client_stream.app_released or client_stream.terminal) return;
    client_stream.cancel_requested = true;
    wakeClientStreamLocked(client_stream);
}

fn clientStreamResumeReceive(context: *anyopaque) !void {
    const client_stream: *ClientStreamState = @ptrCast(@alignCast(context));
    client_stream.mutex.lockUncancelable(syncIo());
    defer client_stream.mutex.unlock(syncIo());
    if (client_stream.impl == null or !client_stream.app_owned or client_stream.app_released or
        client_stream.cancel_requested or client_stream.terminal) return error.StreamClosed;
    client_stream.resume_requested = true;
    wakeClientStreamLocked(client_stream);
}

fn clientStreamRelease(context: *anyopaque) void {
    const client_stream: *ClientStreamState = @ptrCast(@alignCast(context));
    const called_from_callback = active_client_callback == client_stream;
    if (!called_from_callback) client_stream.callback_mutex.lockUncancelable(syncIo());
    client_stream.mutex.lockUncancelable(syncIo());
    std.debug.assert(client_stream.app_owned);
    client_stream.app_owned = false;
    client_stream.app_released = true;
    client_stream.cancel_requested = true;
    const loop_owned = client_stream.loop_owned;
    if (loop_owned) wakeClientStreamLocked(client_stream);
    client_stream.mutex.unlock(syncIo());
    if (!called_from_callback) client_stream.callback_mutex.unlock(syncIo());
    if (!loop_owned) client_stream.destroy();
}

const HeaderKind = enum { none, response, trailers };

const OperationOwner = union(enum) {
    blocking,
    callback: call.Callbacks,
};

const Operation = struct {
    impl: *Impl,
    path: []u8,
    request_frame: []u8,
    request_offset: usize = 0,
    request_metadata: metadata.Metadata,
    request_compression: Compression,
    max_response_size: usize,
    deadline_ns: ?u64,
    deadline_heap_index: ?usize = null,
    waiting_queued: bool = false,
    waiting_prev: ?*Operation = null,
    waiting_next: ?*Operation = null,
    timeout_header: [16]u8 = undefined,
    timeout_header_len: usize = 0,
    stream_id: i32 = -1,
    mutex: std.Io.Mutex = .init,
    condition: std.Io.Condition = .init,
    done: bool = false,
    finished: bool = false,
    owner: OperationOwner = .blocking,
    outcome_set: bool = false,
    deadline_expired: bool = false,
    response_code: status.Code = .unknown,
    response_message: []u8 = &.{},
    response_message_owned: bool = false,
    response_payload: ?[]u8 = null,
    response_compression: Compression = .identity,
    response_encoding_invalid: bool = false,
    response_metadata_invalid: bool = false,
    response_body: std.ArrayList(u8) = .empty,
    response_too_large: bool = false,
    response_header_list_size: usize = 0,
    retained_response_metadata_size: usize = 0,
    response_headers_too_large: bool = false,
    response_header_reset_submitted: bool = false,
    saw_response_headers: bool = false,
    http_status: ?u16 = null,
    content_type_grpc: bool = false,
    grpc_status: ?u32 = null,
    grpc_message: ?[]u8 = null,
    initial_metadata: metadata.Metadata,
    trailing_metadata: metadata.Metadata,
    block_kind: HeaderKind = .none,
    block_metadata: metadata.Metadata,
    block_grpc_status: ?u32 = null,
    block_grpc_message: ?[]u8 = null,

    fn init(impl: *Impl, path: []const u8, payload: []const u8, options: call.Options) !*Operation {
        const operation = try impl.allocator.create(Operation);
        errdefer impl.allocator.destroy(operation);
        const owned_path = try impl.allocator.dupe(u8, path);
        errdefer impl.allocator.free(owned_path);
        const request_frame = try frame.encodeWithCompression(
            impl.allocator,
            payload,
            options.request_compression,
        );
        errdefer impl.allocator.free(request_frame);

        operation.* = .{
            .impl = impl,
            .path = owned_path,
            .request_frame = request_frame,
            .request_metadata = metadata.Metadata.init(impl.allocator),
            .request_compression = options.request_compression,
            .max_response_size = options.max_response_size,
            .deadline_ns = if (options.timeout_ns) |timeout| nowNs() +| timeout else null,
            .initial_metadata = metadata.Metadata.init(impl.allocator),
            .trailing_metadata = metadata.Metadata.init(impl.allocator),
            .block_metadata = metadata.Metadata.init(impl.allocator),
        };
        errdefer operation.request_metadata.deinit();
        errdefer operation.initial_metadata.deinit();
        errdefer operation.trailing_metadata.deinit();
        errdefer operation.block_metadata.deinit();

        for (options.metadata) |entry| {
            if (!isRequestMetadata(entry.key)) return error.InvalidMetadataKey;
            try operation.request_metadata.append(entry.key, entry.value);
        }
        return operation;
    }

    fn initAsync(
        impl: *Impl,
        path: []const u8,
        payload: []const u8,
        options: call.Options,
        callbacks: call.Callbacks,
    ) !*Operation {
        const operation = try init(impl, path, payload, options);
        operation.owner = .{ .callback = callbacks };
        return operation;
    }

    fn deinit(self: *Operation) void {
        std.debug.assert(self.deadline_heap_index == null);
        std.debug.assert(!self.waiting_queued);
        std.debug.assert(self.waiting_prev == null);
        std.debug.assert(self.waiting_next == null);
        const allocator = self.impl.allocator;
        allocator.free(self.path);
        allocator.free(self.request_frame);
        self.request_metadata.deinit();
        self.response_body.deinit(allocator);
        if (self.response_message_owned) allocator.free(self.response_message);
        if (self.response_payload) |payload| allocator.free(payload);
        if (self.grpc_message) |value| allocator.free(value);
        if (self.block_grpc_message) |value| allocator.free(value);
        self.initial_metadata.deinit();
        self.trailing_metadata.deinit();
        self.block_metadata.deinit();
        allocator.destroy(self);
    }

    fn setOutcome(self: *Operation, code: status.Code, text: []const u8) !void {
        if (self.outcome_set) return;
        const owned = try self.impl.allocator.dupe(u8, text);
        if (self.response_message_owned) self.impl.allocator.free(self.response_message);
        self.response_message = owned;
        self.response_message_owned = true;
        self.response_code = code;
        self.outcome_set = true;
    }

    fn setStaticOutcome(self: *Operation, code: status.Code, text: []const u8) void {
        if (self.outcome_set) return;
        std.debug.assert(!self.response_message_owned);
        self.response_message = @constCast(text);
        self.response_code = code;
        self.outcome_set = true;
    }

    fn finish(self: *Operation) void {
        std.debug.assert(!self.finished);
        self.finished = true;
        switch (self.owner) {
            .blocking => {
                self.mutex.lockUncancelable(syncIo());
                self.done = true;
                self.condition.broadcast(syncIo());
                self.mutex.unlock(syncIo());
            },
            .callback => |callbacks| {
                callbacks.on_complete(callbacks.context, .{
                    .status = .init(self.response_code, self.response_message),
                    .payload = if (self.response_payload) |payload| payload else &.{},
                    .response_compression = self.response_compression,
                    .initial_metadata = &self.initial_metadata,
                    .trailing_metadata = &self.trailing_metadata,
                });
                self.deinit();
            },
        }
    }

    fn wait(self: *Operation) void {
        self.mutex.lockUncancelable(syncIo());
        while (!self.done) self.condition.waitUncancelable(syncIo(), &self.mutex);
        self.mutex.unlock(syncIo());
    }

    fn resetHeaderBlock(self: *Operation, kind: HeaderKind) void {
        self.block_metadata.deinit();
        self.block_metadata = metadata.Metadata.init(self.impl.allocator);
        if (self.block_grpc_message) |value| self.impl.allocator.free(value);
        self.block_grpc_message = null;
        self.block_grpc_status = null;
        self.block_kind = kind;
    }

    fn finishHeaderBlock(self: *Operation, end_stream: bool) !bool {
        const trailers_only = self.block_kind == .response and self.block_grpc_status != null;
        if (trailers_only and !end_stream) {
            self.block_kind = .none;
            return false;
        }
        const destination = if (self.block_kind == .trailers or trailers_only)
            &self.trailing_metadata
        else
            &self.initial_metadata;
        try copyMetadata(destination, &self.block_metadata);
        if (self.block_grpc_status) |code| self.grpc_status = code;
        if (self.block_grpc_message) |value| {
            if (self.grpc_message) |old| self.impl.allocator.free(old);
            self.grpc_message = value;
            self.block_grpc_message = null;
        }
        self.block_kind = .none;
        return true;
    }

    fn finalize(self: *Operation, stream_error: u32) void {
        if (self.outcome_set) {
            self.finish();
            return;
        }
        if (stream_error != c.NGHTTP2_NO_ERROR) {
            const mapped = streamErrorStatus(stream_error);
            self.setOutcome(mapped.code, mapped.message) catch {};
            self.finish();
            return;
        }
        if (!self.saw_response_headers) {
            self.setOutcome(.unknown, "missing response headers") catch {};
        } else if (self.http_status == null) {
            self.setOutcome(.unknown, "missing HTTP status") catch {};
        } else if (self.http_status.? != 200) {
            self.setOutcome(httpStatusCode(self.http_status.?), "HTTP request failed") catch {};
        } else if (!self.content_type_grpc) {
            self.setOutcome(.unknown, "invalid gRPC content-type") catch {};
        } else if (self.grpc_status == null) {
            self.setOutcome(.unknown, "missing grpc-status") catch {};
        } else if (self.response_encoding_invalid) {
            self.setOutcome(.unimplemented, "response compression is not supported") catch {};
        } else if (self.response_too_large) {
            self.setOutcome(.resource_exhausted, "response message too large") catch {};
        } else {
            const code = status.Code.fromInt(self.grpc_status.?);
            const allocator = self.impl.allocator;
            var decoded_message: ?[]u8 = null;
            defer if (decoded_message) |value| allocator.free(value);
            if (self.grpc_message) |encoded| {
                decoded_message = message.decode(allocator, encoded) catch {
                    self.setOutcome(.unknown, "invalid grpc-message") catch {};
                    self.finish();
                    return;
                };
            }
            if (code != .ok) {
                self.setOutcome(code, if (decoded_message) |value| value else "") catch {};
            } else {
                const payload = frame.decodeUnaryWithCompression(
                    self.impl.allocator,
                    self.response_body.items,
                    self.max_response_size,
                    self.response_compression,
                ) catch |err| {
                    const outcome: status.Status = switch (err) {
                        error.MessageTooLarge => .init(.resource_exhausted, "response message too large"),
                        else => .init(.internal, "malformed unary response"),
                    };
                    self.setOutcome(outcome.code, outcome.message) catch {};
                    self.finish();
                    return;
                };
                self.response_payload = payload;
                self.response_compression = if (self.response_body.items[0] == 1) .gzip else .identity;
                self.setOutcome(.ok, if (decoded_message) |value| value else "") catch {};
            }
        }
        if (!self.outcome_set) self.setOutcome(.unknown, "response failed") catch {};
        self.finish();
    }
};

const DeadlineTarget = union(enum) {
    operation: *Operation,
    client_stream: *ClientStreamState,
    tls_handshake: *Impl,
    reconnect: *Impl,
};

const DeadlineEntry = struct {
    expires_at_ns: u64,
    target: DeadlineTarget,
};

fn deadlineTargetIndex(target: DeadlineTarget) *?usize {
    return switch (target) {
        .operation => |operation| &operation.deadline_heap_index,
        .client_stream => |client_stream| &client_stream.deadline_heap_index,
        .tls_handshake => |impl| &impl.deadline_heap_index,
        .reconnect => |impl| &impl.reconnect_heap_index,
    };
}

fn deadlineTargetsEqual(a: DeadlineTarget, b: DeadlineTarget) bool {
    return switch (a) {
        .operation => |operation| switch (b) {
            .operation => |other| operation == other,
            else => false,
        },
        .client_stream => |client_stream| switch (b) {
            .client_stream => |other| client_stream == other,
            else => false,
        },
        .tls_handshake => |impl| switch (b) {
            .tls_handshake => |other| impl == other,
            else => false,
        },
        .reconnect => |impl| switch (b) {
            .reconnect => |other| impl == other,
            else => false,
        },
    };
}

fn deadlineHeapSwap(impl: *Impl, a: usize, b: usize) void {
    if (a == b) return;
    std.mem.swap(DeadlineEntry, &impl.deadline_heap.items[a], &impl.deadline_heap.items[b]);
    deadlineTargetIndex(impl.deadline_heap.items[a].target).* = a;
    deadlineTargetIndex(impl.deadline_heap.items[b].target).* = b;
}

fn deadlineHeapSiftUp(impl: *Impl, start: usize) usize {
    var index = start;
    while (index != 0) {
        const parent = (index - 1) / 2;
        if (impl.deadline_heap.items[parent].expires_at_ns <= impl.deadline_heap.items[index].expires_at_ns) break;
        deadlineHeapSwap(impl, parent, index);
        index = parent;
    }
    return index;
}

fn deadlineHeapSiftDown(impl: *Impl, start: usize) usize {
    var index = start;
    while (true) {
        const left = index * 2 + 1;
        if (left >= impl.deadline_heap.items.len) break;
        const right = left + 1;
        const child = if (right < impl.deadline_heap.items.len and
            impl.deadline_heap.items[right].expires_at_ns < impl.deadline_heap.items[left].expires_at_ns)
            right
        else
            left;
        if (impl.deadline_heap.items[index].expires_at_ns <= impl.deadline_heap.items[child].expires_at_ns) break;
        deadlineHeapSwap(impl, index, child);
        index = child;
    }
    return index;
}

fn deadlineHeapInsertOrUpdate(impl: *Impl, target: DeadlineTarget, expires_at_ns: u64) !void {
    const target_index = deadlineTargetIndex(target);
    if (target_index.*) |index| {
        std.debug.assert(index < impl.deadline_heap.items.len);
        std.debug.assert(deadlineTargetsEqual(impl.deadline_heap.items[index].target, target));
        const previous = impl.deadline_heap.items[index].expires_at_ns;
        impl.deadline_heap.items[index].expires_at_ns = expires_at_ns;
        if (expires_at_ns < previous) {
            _ = deadlineHeapSiftUp(impl, index);
        } else if (expires_at_ns > previous) {
            _ = deadlineHeapSiftDown(impl, index);
        }
        return;
    }

    try impl.deadline_heap.append(impl.allocator, .{
        .expires_at_ns = expires_at_ns,
        .target = target,
    });
    const index = impl.deadline_heap.items.len - 1;
    target_index.* = index;
    _ = deadlineHeapSiftUp(impl, index);
}

fn deadlineHeapRemove(impl: *Impl, target: DeadlineTarget) bool {
    const target_index = deadlineTargetIndex(target);
    const index = target_index.* orelse return false;
    std.debug.assert(index < impl.deadline_heap.items.len);
    std.debug.assert(deadlineTargetsEqual(impl.deadline_heap.items[index].target, target));

    const removed = impl.deadline_heap.items[index];
    const replacement = impl.deadline_heap.pop().?;
    deadlineTargetIndex(removed.target).* = null;
    if (index < impl.deadline_heap.items.len) {
        impl.deadline_heap.items[index] = replacement;
        deadlineTargetIndex(replacement.target).* = index;
        if (index != 0 and impl.deadline_heap.items[index].expires_at_ns <
            impl.deadline_heap.items[(index - 1) / 2].expires_at_ns)
        {
            _ = deadlineHeapSiftUp(impl, index);
        } else {
            _ = deadlineHeapSiftDown(impl, index);
        }
    }
    return true;
}

fn deadlineHeapPeek(impl: *const Impl) ?DeadlineEntry {
    if (impl.deadline_heap.items.len == 0) return null;
    return impl.deadline_heap.items[0];
}

fn deadlineHeapPop(impl: *Impl) ?DeadlineEntry {
    const entry = deadlineHeapPeek(impl) orelse return null;
    std.debug.assert(deadlineHeapRemove(impl, entry.target));
    return entry;
}

fn appendWaitingOperation(impl: *Impl, operation: *Operation) void {
    std.debug.assert(!operation.waiting_queued);
    std.debug.assert(operation.waiting_prev == null);
    std.debug.assert(operation.waiting_next == null);
    operation.waiting_queued = true;
    operation.waiting_prev = impl.waiting_operation_tail;
    if (impl.waiting_operation_tail) |tail| {
        tail.waiting_next = operation;
    } else {
        impl.waiting_operation_head = operation;
    }
    impl.waiting_operation_tail = operation;
}

fn removeWaitingOperation(impl: *Impl, operation: *Operation) bool {
    if (!operation.waiting_queued) return false;
    if (operation.waiting_prev) |previous| {
        previous.waiting_next = operation.waiting_next;
    } else {
        std.debug.assert(impl.waiting_operation_head == operation);
        impl.waiting_operation_head = operation.waiting_next;
    }
    if (operation.waiting_next) |next| {
        next.waiting_prev = operation.waiting_prev;
    } else {
        std.debug.assert(impl.waiting_operation_tail == operation);
        impl.waiting_operation_tail = operation.waiting_prev;
    }
    operation.waiting_queued = false;
    operation.waiting_prev = null;
    operation.waiting_next = null;
    return true;
}

fn popWaitingOperation(impl: *Impl) ?*Operation {
    const operation = impl.waiting_operation_head orelse return null;
    std.debug.assert(removeWaitingOperation(impl, operation));
    return operation;
}

fn removeOperationDeadline(impl: *Impl, operation: *Operation) void {
    _ = deadlineHeapRemove(impl, .{ .operation = operation });
}

fn removeStreamDeadline(impl: *Impl, client_stream: *ClientStreamState) void {
    _ = deadlineHeapRemove(impl, .{ .client_stream = client_stream });
}

fn clearTlsHandshakeDeadline(impl: *Impl) void {
    _ = deadlineHeapRemove(impl, .{ .tls_handshake = impl });
    impl.tls_handshake_deadline_ns = null;
}

fn clearReconnectDeadline(impl: *Impl) void {
    _ = deadlineHeapRemove(impl, .{ .reconnect = impl });
    impl.reconnect_deadline_ns = null;
}

const WriteRequest = struct {
    request: xev.WriteRequest = undefined,
    impl: *Impl,
    bytes: []u8,
    generation: usize,
    free_next: ?*WriteRequest = null,
    in_pool: bool = false,
};

fn acquireWriteRequest(impl: *Impl, bytes: []u8) !*WriteRequest {
    const write = if (impl.write_request_pool_head) |pooled| pooled else try impl.allocator.create(WriteRequest);
    if (impl.write_request_pool_head != null) {
        std.debug.assert(write.in_pool);
        std.debug.assert(impl.write_request_pool_count > 0);
        impl.write_request_pool_head = write.free_next;
        impl.write_request_pool_count -= 1;
    }
    write.* = .{
        .request = .{ .full_write_buffer = .{ .slice = &.{} } },
        .impl = impl,
        .bytes = bytes,
        .generation = impl.connection_generation.load(.monotonic),
    };
    return write;
}

fn releaseWriteRequest(impl: *Impl, write: *WriteRequest) void {
    std.debug.assert(write.impl == impl);
    std.debug.assert(!write.in_pool);
    std.debug.assert(!impl.writes.contains(write));
    std.debug.assert(!isWriteRequestQueued(impl, write));
    std.debug.assert(write.request.completion.state() == .dead);
    write.bytes = &.{};
    write.free_next = impl.write_request_pool_head;
    write.in_pool = true;
    impl.write_request_pool_head = write;
    impl.write_request_pool_count += 1;
}

fn drainWriteRequestPool(impl: *Impl) void {
    std.debug.assert(impl.writes.count() == 0);
    std.debug.assert(impl.write_queue.head == null);
    var count: usize = 0;
    var current = impl.write_request_pool_head;
    while (current) |write| {
        std.debug.assert(write.in_pool);
        std.debug.assert(!impl.writes.contains(write));
        std.debug.assert(write.request.completion.state() == .dead);
        count += 1;
        std.debug.assert(count <= impl.write_request_pool_count);
        current = write.free_next;
    }
    std.debug.assert(count == impl.write_request_pool_count);

    while (impl.write_request_pool_head) |write| {
        impl.write_request_pool_head = write.free_next;
        impl.write_request_pool_count -= 1;
        impl.allocator.destroy(write);
    }
    std.debug.assert(impl.write_request_pool_count == 0);
}

fn isWriteRequestQueued(impl: *const Impl, write: *const WriteRequest) bool {
    var current = impl.write_queue.head;
    while (current) |request| : (current = request.next) {
        if (request == &write.request) return true;
    }
    return false;
}

const CleartextWriteBatch = struct {
    bytes: std.ArrayList(u8) = .empty,

    fn deinit(self: *CleartextWriteBatch, allocator: std.mem.Allocator) void {
        self.bytes.deinit(allocator);
    }

    fn pendingBytes(self: *const CleartextWriteBatch) usize {
        return self.bytes.items.len;
    }

    fn append(
        self: *CleartextWriteBatch,
        allocator: std.mem.Allocator,
        chunk: []const u8,
        target: usize,
    ) !?[]u8 {
        var completed: ?[]u8 = null;
        if (self.bytes.items.len != 0 and
            (self.bytes.items.len >= target or chunk.len > target - self.bytes.items.len))
        {
            completed = (try self.take(allocator)).?;
        }
        errdefer if (completed) |owned| allocator.free(owned);
        try self.bytes.appendSlice(allocator, chunk);
        return completed;
    }

    fn ready(self: *const CleartextWriteBatch, target: usize) bool {
        return self.bytes.items.len >= target;
    }

    fn take(self: *CleartextWriteBatch, allocator: std.mem.Allocator) !?[]u8 {
        if (self.bytes.items.len == 0) return null;
        return try self.bytes.toOwnedSlice(allocator);
    }
};

fn runLoop(impl: *Impl) void {
    impl.loop = xev.Loop.init(.{}) catch {
        impl.logger.write(.err, "channel event loop initialization failed target={s}", .{impl.authority});
        impl.signalStartup(false);
        impl.markStopped();
        return;
    };
    impl.loop_initialized = true;
    if (impl.literal_address == null) {
        impl.resolver.init(impl.allocator, &impl.loop) catch {
            impl.logger.write(.err, "channel resolver initialization failed target={s}", .{impl.authority});
            impl.signalStartup(false);
            impl.loop.deinit();
            impl.loop_initialized = false;
            impl.markStopped();
            return;
        };
        impl.resolver_initialized = true;
    }
    impl.async_handle.wait(&impl.loop, &impl.async_completion, Impl, impl, onAsync);
    if (impl.reconnect_options) |options| {
        if (options.allow_initial_offline) impl.signalStartup(true);
    }
    startConnection(impl) catch {
        scheduleReconnect(impl, "connection failed");
    };
    impl.loop.run(.until_done) catch |err| {
        impl.logger.write(.err, "channel event loop failed target={s} error={s}", .{ impl.authority, @errorName(err) });
        beginStop(impl, "event loop failed");
        impl.loop.run(.until_done) catch @panic("event loop failed while stopping");
    };
    std.debug.assert(impl.writes.count() == 0);
    std.debug.assert(impl.write_queue.head == null);
    clearTlsConnection(impl);
    if (impl.session) |session| c.nghttp2_session_del(session);
    impl.session = null;
    impl.loop.deinit();
    impl.loop_initialized = false;
    if (impl.resolver_initialized) {
        impl.resolver.deinitAfterLoop();
        impl.resolver_initialized = false;
    }
    std.debug.assert(impl.deadline_heap.items.len == 0);
    std.debug.assert(impl.deadline_heap_index == null);
    std.debug.assert(impl.reconnect_heap_index == null);
    std.debug.assert(impl.waiting_operation_head == null);
    std.debug.assert(impl.waiting_operation_tail == null);
    impl.markStopped();
}

fn startConnection(impl: *Impl) !void {
    const address = impl.literal_address orelse address: {
        switch (impl.resolve_state) {
            .idle => {
                impl.resolve_state = .pending;
                impl.connection_state = .resolving;
                try impl.resolver.resolve(impl.host, impl.port, impl, onResolved);
                return;
            },
            .pending => return,
            .ready => {},
            .failed, .cancelled => return error.ConnectionFailed,
        }
        if (impl.next_address >= impl.resolved_addresses.len) return error.ConnectionFailed;
        const candidate = impl.resolved_addresses[impl.next_address];
        impl.next_address += 1;
        break :address candidate;
    };
    impl.tcp = try xev.TCP.init(address);
    impl.tcp_initialized = true;
    impl.close_submitted = false;
    impl.close_completed = false;
    impl.connected = false;
    impl.connection_state = .connecting;
    _ = impl.connection_generation.fetchAdd(1, .monotonic);

    initializeSession(impl) catch {
        beginStop(impl, "connection setup failed");
        return;
    };
    if (comptime build_options.tls) {
        if (impl.tls_config) |config| {
            impl.tls_session = tls_record.Session.create(impl.allocator, config, impl.host) catch {
                beginStop(impl, "TLS setup failed");
                return;
            };
        }
    }
    impl.connect_active = true;
    impl.tcp.connect(&impl.loop, &impl.connect_completion, address, Impl, impl, onConnect);
    observeTestIo(impl);
}

fn onResolved(context: ?*anyopaque, result: cares_adapter.ResolveResult) void {
    const impl: *Impl = @ptrCast(@alignCast(context.?));
    switch (result) {
        .addresses => |addresses| {
            if (impl.resolved_addresses.len != 0) impl.allocator.free(impl.resolved_addresses);
            impl.resolved_addresses = addresses;
            impl.next_address = 0;
            impl.resolve_state = .ready;
        },
        .failed => impl.resolve_state = .failed,
        .cancelled => impl.resolve_state = .cancelled,
    }
    impl.async_handle.notify() catch @panic("resolver wakeup failed");
}

fn continueResolvedConnection(impl: *Impl) void {
    if (impl.connection_state != .resolving) return;
    switch (impl.resolve_state) {
        .pending, .idle => return,
        .ready => startConnection(impl) catch {
            scheduleReconnect(impl, "name resolution failed");
        },
        .failed, .cancelled => {
            scheduleReconnect(impl, "name resolution failed");
        },
    }
}

fn clearResolvedAddresses(impl: *Impl) void {
    if (impl.resolved_addresses.len != 0) impl.allocator.free(impl.resolved_addresses);
    impl.resolved_addresses = &.{};
    impl.next_address = 0;
    impl.resolve_state = .idle;
}

fn initializeSession(impl: *Impl) !void {
    var callbacks: ?*c.nghttp2_session_callbacks = null;
    if (c.nghttp2_session_callbacks_new(&callbacks) != 0) return error.OutOfMemory;
    defer c.nghttp2_session_callbacks_del(callbacks);
    var options: ?*c.nghttp2_option = null;
    if (c.nghttp2_option_new(&options) != 0) return error.OutOfMemory;
    defer c.nghttp2_option_del(options);
    c.nghttp2_option_set_no_auto_window_update(options, 1);
    c.nghttp2_session_callbacks_set_on_begin_headers_callback(callbacks, onBeginHeaders);
    c.nghttp2_session_callbacks_set_on_header_callback(callbacks, onHeader);
    c.nghttp2_session_callbacks_set_on_invalid_header_callback(callbacks, onInvalidHeader);
    c.nghttp2_session_callbacks_set_on_data_chunk_recv_callback(callbacks, onDataChunk);
    c.nghttp2_session_callbacks_set_on_frame_recv_callback(callbacks, onFrameReceived);
    c.nghttp2_session_callbacks_set_on_stream_close_callback(callbacks, onStreamClose);
    if (c.nghttp2_session_client_new2(&impl.session, callbacks, impl, options) != 0) return error.OutOfMemory;
    const settings = [_]c.nghttp2_settings_entry{
        .{
            .settings_id = c.NGHTTP2_SETTINGS_INITIAL_WINDOW_SIZE,
            .value = impl.initial_stream_window_size,
        },
        .{
            .settings_id = c.NGHTTP2_SETTINGS_MAX_HEADER_LIST_SIZE,
            .value = impl.max_response_header_list_size,
        },
    };
    if (c.nghttp2_submit_settings(impl.session, c.NGHTTP2_FLAG_NONE, &settings, settings.len) != 0) {
        return error.NativeFailure;
    }
}

fn onConnect(impl_: ?*Impl, loop: *xev.Loop, _: *xev.Completion, _: xev.TCP, result: xev.ConnectError!void) xev.CallbackAction {
    const impl = impl_.?;
    impl.connect_active = false;
    defer submitCloseIfReady(impl, loop);
    result catch {
        if (impl.literal_address != null) {
            failConnectionCandidate(impl, "connection failed");
        } else {
            if (impl.session) |session| c.nghttp2_session_del(session);
            impl.session = null;
            clearTlsConnection(impl);
            impl.connection_state = .closing;
        }
        return .disarm;
    };
    socket_options.enableTcpNoDelay(impl.tcp.fd) catch {
        failConnectionCandidate(impl, "connection failed");
        return .disarm;
    };
    impl.mutex.lockUncancelable(syncIo());
    const stopping = impl.state == .stopping or impl.state == .stopped;
    impl.mutex.unlock(syncIo());
    if (stopping) {
        beginStop(impl, "channel closed");
        return .disarm;
    }
    impl.connected = true;
    _ = impl.connect_count.fetchAdd(1, .monotonic);
    impl.read_active = true;
    impl.tcp.read(&impl.loop, &impl.read_completion, .{ .slice = &impl.read_buffer }, Impl, impl, onRead);
    if (impl.tls_session != null) {
        impl.connection_state = .handshaking;
        const handshake_deadline = nowNs() +| impl.tls_handshake_timeout_ns;
        deadlineHeapInsertOrUpdate(impl, .{ .tls_handshake = impl }, handshake_deadline) catch {
            failConnectionCandidate(impl, "TLS handshake failed");
            return .disarm;
        };
        impl.tls_handshake_deadline_ns = handshake_deadline;
        driveTlsHandshake(impl) catch {
            failConnectionCandidate(impl, "TLS handshake failed");
        };
        scheduleDeadlineTimer(impl);
        return .disarm;
    }
    activateConnection(impl) catch {
        handleTransportFailure(impl, "connection failed");
    };
    return .disarm;
}

fn activateConnection(impl: *Impl) !void {
    clearTlsHandshakeDeadline(impl);
    clearReconnectDeadline(impl);
    clearResolvedAddresses(impl);
    impl.connection_state = .active;
    try flush(impl);
    impl.mutex.lockUncancelable(syncIo());
    impl.accepting_streams = true;
    impl.mutex.unlock(syncIo());
    impl.ever_active = true;
    impl.logger.write(.info, "channel connected target={s}", .{impl.authority});
    impl.reconnect_backoff_pending_reset = impl.reconnect_attempt != 0;
    impl.signalStartup(true);
    processPending(impl);
}

fn onAsync(impl_: ?*Impl, _: *xev.Loop, _: *xev.Completion, result: xev.Async.WaitError!void) xev.CallbackAction {
    const impl = impl_.?;
    impl.write_wake_queued = false;
    result catch {
        beginStop(impl, "channel wakeup failed");
        return .disarm;
    };
    observeTestIo(impl);
    impl.mutex.lockUncancelable(syncIo());
    impl.stream_wake_notify_pending = false;
    const stopping = impl.state == .stopping or impl.state == .stopped;
    impl.mutex.unlock(syncIo());
    if (stopping) {
        beginStop(impl, "channel closed");
        return .disarm;
    }
    processStreamWakes(impl);
    processPending(impl);
    switch (impl.connection_state) {
        .resolving => {
            continueResolvedConnection(impl);
            scheduleDeadlineTimer(impl);
        },
        .active => {},
        .draining => {
            if (impl.operations.count() == 0 and impl.streams.count() == 0) beginReconnect(impl);
            scheduleDeadlineTimer(impl);
        },
        .handshaking => {
            if (impl.tls_handshake_needs_write) {
                driveTlsHandshake(impl) catch failConnectionCandidate(impl, "TLS handshake failed");
            }
            scheduleDeadlineTimer(impl);
        },
        .connecting, .backing_off, .closing => scheduleDeadlineTimer(impl),
    }
    return .rearm;
}

fn processPending(impl: *Impl) void {
    var pending: std.ArrayList(*Operation) = .empty;
    impl.mutex.lockUncancelable(syncIo());
    std.mem.swap(std.ArrayList(*Operation), &pending, &impl.pending);
    impl.mutex.unlock(syncIo());
    defer pending.deinit(impl.allocator);

    for (pending.items, 0..) |operation, index| {
        pending.items[index] = undefined;
        appendWaitingOperation(impl, operation);
        if (operation.deadline_ns) |deadline| {
            deadlineHeapInsertOrUpdate(impl, .{ .operation = operation }, deadline) catch {
                std.debug.assert(removeWaitingOperation(impl, operation));
                operation.setOutcome(.unavailable, "request submission failed") catch {};
                operation.finish();
                continue;
            };
        }
    }

    if (impl.connection_state != .active) {
        scheduleDeadlineTimer(impl);
        return;
    }

    var now: ?u64 = null;
    while (popWaitingOperation(impl)) |operation| {
        if (operation.deadline_ns) |deadline| {
            const current = cachedNow(&now);
            if (deadline <= current) {
                removeOperationDeadline(impl, operation);
                operation.deadline_expired = true;
                operation.setOutcome(.deadline_exceeded, "deadline exceeded") catch {};
                operation.finish();
                continue;
            }
            operation.timeout_header_len = deadline_wire.formatTimeout(
                &operation.timeout_header,
                deadline - current,
            ).len;
        }
        submitOperation(impl, operation) catch {
            removeOperationDeadline(impl, operation);
            operation.setOutcome(.unavailable, "request submission failed") catch {};
            operation.finish();
            continue;
        };
    }
    flush(impl) catch {
        handleTransportFailure(impl, "connection failed");
        return;
    };
    scheduleDeadlineTimer(impl);
}

fn popStreamWake(impl: *Impl) ?*ClientStreamState {
    impl.mutex.lockUncancelable(syncIo());
    defer impl.mutex.unlock(syncIo());
    const client_stream = impl.stream_wake_head orelse return null;
    impl.stream_wake_head = client_stream.wake_next;
    if (impl.stream_wake_head == null) impl.stream_wake_tail = null;
    client_stream.wake_queued = false;
    client_stream.wake_next = null;
    return client_stream;
}

fn processStreamWakes(impl: *Impl) void {
    var now: ?u64 = null;
    while (popStreamWake(impl)) |client_stream| {
        if (client_stream.stream_id < 0) {
            if (client_stream.deadline_ns) |deadline| {
                deadlineHeapInsertOrUpdate(impl, .{ .client_stream = client_stream }, deadline) catch {
                    terminalizeStream(client_stream, .init(.unavailable, "stream submission failed"));
                    releaseStreamLoopOwnership(client_stream);
                    continue;
                };
            }
            client_stream.mutex.lockUncancelable(syncIo());
            const canceled = client_stream.cancel_requested or client_stream.app_released;
            client_stream.mutex.unlock(syncIo());
            if (canceled) {
                terminalizeStream(client_stream, .init(.cancelled, "stream cancelled"));
                releaseStreamLoopOwnership(client_stream);
                continue;
            }
            if (client_stream.deadline_ns) |deadline| {
                const current = cachedNow(&now);
                if (deadline <= current) {
                    client_stream.deadline_expired = true;
                    terminalizeStream(client_stream, .init(.deadline_exceeded, "deadline exceeded"));
                    releaseStreamLoopOwnership(client_stream);
                    continue;
                }
                client_stream.timeout_header_len = deadline_wire.formatTimeout(
                    &client_stream.timeout_header,
                    deadline - current,
                ).len;
            }
            if (impl.connection_state != .active) {
                terminalizeStream(client_stream, .init(.unavailable, "connection is unavailable"));
                releaseStreamLoopOwnership(client_stream);
                continue;
            }
            submitClientStream(impl, client_stream) catch {
                terminalizeStream(client_stream, .init(.unavailable, "stream submission failed"));
                releaseStreamLoopOwnership(client_stream);
                continue;
            };
        }
        processClientStreamCommands(client_stream) catch {
            handleTransportFailure(impl, "stream command failed");
            return;
        };
    }
    if (impl.connected and (impl.connection_state == .active or impl.connection_state == .draining)) flush(impl) catch {
        handleTransportFailure(impl, "connection failed");
        return;
    };
    scheduleDeadlineTimer(impl);
}

fn submitClientStream(impl: *Impl, client_stream: *ClientStreamState) !void {
    try impl.streams.ensureUnusedCapacity(impl.allocator, 1);
    var headers: HeaderBuilder = .{};
    defer headers.deinit(impl.allocator);
    var encoded_values: EncodedValueBuilder = .{};
    defer {
        for (encoded_values.items()) |value| value.deinit(impl.allocator);
        encoded_values.deinit(impl.allocator);
    }
    try headers.append(impl.allocator, nativeHeader(":method", "POST"));
    try headers.append(impl.allocator, nativeHeader(":scheme", if (impl.tls_config != null) "https" else "http"));
    try headers.append(impl.allocator, nativeHeader(":path", client_stream.path));
    try headers.append(impl.allocator, nativeHeader(":authority", impl.authority));
    try headers.append(impl.allocator, nativeHeader("content-type", "application/grpc"));
    try headers.append(impl.allocator, nativeHeader("te", "trailers"));
    try headers.append(impl.allocator, nativeHeader("grpc-encoding", client_stream.request_compression.name()));
    try headers.append(impl.allocator, nativeHeader("grpc-accept-encoding", "identity,gzip"));
    try headers.append(impl.allocator, nativeHeader("user-agent", impl.user_agent));
    if (client_stream.timeout_header_len != 0) {
        try headers.append(impl.allocator, nativeHeader(
            "grpc-timeout",
            client_stream.timeout_header[0..client_stream.timeout_header_len],
        ));
    }
    for (client_stream.request_metadata.items()) |entry| {
        try appendMetadataHeader(&headers, &encoded_values, impl.allocator, entry);
    }

    var provider: c.nghttp2_data_provider2 = .{
        .source = .{ .ptr = client_stream },
        .read_callback = readClientStreamData,
    };
    const stream_id = c.nghttp2_submit_request2(
        impl.session,
        null,
        headers.items().ptr,
        headers.items().len,
        &provider,
        client_stream,
    );
    if (stream_id < 0) return error.NativeFailure;
    client_stream.stream_id = stream_id;
    impl.streams.putAssumeCapacity(stream_id, client_stream);
}

fn processClientStreamCommands(client_stream: *ClientStreamState) !void {
    const impl = client_stream.impl orelse return;
    client_stream.mutex.lockUncancelable(syncIo());
    const app_released = client_stream.app_released;
    const cancel_requested = client_stream.cancel_requested or app_released;
    const should_resume_data = client_stream.provider_deferred and
        (client_stream.outbound_head < client_stream.outbound.items.len or !client_stream.send_open);
    const resume_receive = client_stream.resume_requested;
    client_stream.resume_requested = false;
    if (should_resume_data) client_stream.provider_deferred = false;
    client_stream.mutex.unlock(syncIo());

    if (cancel_requested) removeStreamDeadline(impl, client_stream);

    if (client_stream.transport_closed) {
        if (cancel_requested) {
            terminalizeStream(client_stream, .init(.cancelled, "stream cancelled"));
            releaseStreamLoopOwnership(client_stream);
        } else if (resume_receive) {
            try deliverInboundMessages(client_stream);
        }
        return;
    }

    if (cancel_requested and !client_stream.rst_submitted) {
        if (!app_released) setForcedStreamStatus(client_stream, .init(.cancelled, "stream cancelled"));
        if (c.nghttp2_submit_rst_stream(
            impl.session,
            c.NGHTTP2_FLAG_NONE,
            client_stream.stream_id,
            c.NGHTTP2_CANCEL,
        ) != 0) return error.NativeFailure;
        client_stream.rst_submitted = true;
    }
    if (should_resume_data and !client_stream.rst_submitted) {
        if (c.nghttp2_session_resume_data(impl.session, client_stream.stream_id) != 0) {
            return error.NativeFailure;
        }
    }
    if (resume_receive and !client_stream.rst_submitted) {
        try deliverInboundMessages(client_stream);
        if (canReturnDeferredStreamCredit(
            client_stream.receive_paused,
            client_stream.transport_closed,
        ) and client_stream.deferred_stream_credit != 0) {
            if (c.nghttp2_session_consume_stream(
                impl.session,
                client_stream.stream_id,
                client_stream.deferred_stream_credit,
            ) != 0) return error.NativeFailure;
            client_stream.deferred_stream_credit = 0;
        }
    }
}

fn readClientStreamData(
    _: ?*c.nghttp2_session,
    _: i32,
    output: [*c]u8,
    output_length: usize,
    data_flags: ?*u32,
    source: ?*c.nghttp2_data_source,
    _: ?*anyopaque,
) callconv(.c) c.nghttp2_ssize {
    const client_stream: *ClientStreamState = @ptrCast(@alignCast(source.?.*.ptr.?));
    client_stream.mutex.lockUncancelable(syncIo());
    if (client_stream.outbound_head == client_stream.outbound.items.len) {
        if (!client_stream.send_open) {
            client_stream.provider_eof = true;
            data_flags.?.* |= c.NGHTTP2_DATA_FLAG_EOF;
            client_stream.mutex.unlock(syncIo());
            return 0;
        }
        client_stream.provider_deferred = true;
        client_stream.mutex.unlock(syncIo());
        return c.NGHTTP2_ERR_DEFERRED;
    }

    var item = &client_stream.outbound.items[client_stream.outbound_head];
    const remaining = item.frame_bytes[item.offset..];
    const length = @min(remaining.len, output_length);
    @memcpy(output[0..length], remaining[0..length]);
    item.offset += length;
    var notify_writable = false;
    if (item.offset == item.frame_bytes.len) {
        client_stream.outbound_buffered -= item.frame_bytes.len;
        client_stream.allocator.free(item.frame_bytes);
        client_stream.outbound_head += 1;
        if (client_stream.outbound_head == client_stream.outbound.items.len) {
            client_stream.outbound.clearRetainingCapacity();
            client_stream.outbound_head = 0;
            if (!client_stream.send_open) {
                client_stream.provider_eof = true;
                data_flags.?.* |= c.NGHTTP2_DATA_FLAG_EOF;
            }
        }
        if (client_stream.backpressure_requested) {
            client_stream.backpressure_requested = false;
            notify_writable = true;
        }
    }
    const callbacks_enabled = client_stream.app_owned and !client_stream.app_released and !client_stream.terminal;
    client_stream.mutex.unlock(syncIo());
    if (notify_writable and callbacks_enabled) {
        invokeWritableCallback(client_stream);
    }
    return @intCast(length);
}

fn setForcedStreamStatus(client_stream: *ClientStreamState, value: status.Status) void {
    if (client_stream.forced_status == null) client_stream.forced_status = value;
}

fn callbacksEnabled(client_stream: *ClientStreamState) bool {
    client_stream.mutex.lockUncancelable(syncIo());
    defer client_stream.mutex.unlock(syncIo());
    return client_stream.app_owned and !client_stream.app_released and
        !client_stream.cancel_requested and !client_stream.terminal;
}

fn beginClientCallback(client_stream: *ClientStreamState) bool {
    client_stream.callback_mutex.lockUncancelable(syncIo());
    if (!callbacksEnabled(client_stream)) {
        client_stream.callback_mutex.unlock(syncIo());
        return false;
    }
    std.debug.assert(active_client_callback == null);
    active_client_callback = client_stream;
    return true;
}

fn endClientCallback(client_stream: *ClientStreamState) void {
    std.debug.assert(active_client_callback == client_stream);
    active_client_callback = null;
    client_stream.callback_mutex.unlock(syncIo());
}

fn beginTerminalCallback(client_stream: *ClientStreamState) bool {
    client_stream.callback_mutex.lockUncancelable(syncIo());
    client_stream.mutex.lockUncancelable(syncIo());
    const enabled = client_stream.app_owned and !client_stream.app_released;
    client_stream.mutex.unlock(syncIo());
    if (!enabled) {
        client_stream.callback_mutex.unlock(syncIo());
        return false;
    }
    std.debug.assert(active_client_callback == null);
    active_client_callback = client_stream;
    return true;
}

fn invokeWritableCallback(client_stream: *ClientStreamState) void {
    const callback = client_stream.callbacks.on_writable orelse return;
    if (!beginClientCallback(client_stream)) return;
    defer endClientCallback(client_stream);
    callback(client_stream.callbacks.context, client_stream.handle());
}

fn invokeHeadersCallback(client_stream: *ClientStreamState) void {
    const callback = client_stream.callbacks.on_headers orelse return;
    if (!beginClientCallback(client_stream)) return;
    defer endClientCallback(client_stream);
    callback(
        client_stream.callbacks.context,
        client_stream.handle(),
        &client_stream.initial_metadata,
    );
}

fn invokeMessageCallback(
    client_stream: *ClientStreamState,
    payload: []const u8,
    compression: Compression,
) ?stream.ReceiveAction {
    if (!beginClientCallback(client_stream)) return null;
    defer endClientCallback(client_stream);
    return client_stream.callbacks.on_message(
        client_stream.callbacks.context,
        client_stream.handle(),
        payload,
        compression,
    );
}

fn terminalizeStream(client_stream: *ClientStreamState, final_status: status.Status) void {
    if (client_stream.impl) |impl| removeStreamDeadline(impl, client_stream);
    client_stream.mutex.lockUncancelable(syncIo());
    if (client_stream.terminal) {
        client_stream.mutex.unlock(syncIo());
        return;
    }
    client_stream.terminal = true;
    const notify = client_stream.app_owned and !client_stream.app_released;
    client_stream.mutex.unlock(syncIo());
    if (notify) {
        if (!beginTerminalCallback(client_stream)) return;
        defer endClientCallback(client_stream);
        client_stream.callbacks.on_terminal(
            client_stream.callbacks.context,
            client_stream.handle(),
            final_status,
            &client_stream.trailing_metadata,
        );
    }
}

fn releaseStreamLoopOwnership(client_stream: *ClientStreamState) void {
    client_stream.mutex.lockUncancelable(syncIo());
    std.debug.assert(client_stream.loop_owned);
    const impl = client_stream.impl orelse unreachable;
    removeStreamDeadline(impl, client_stream);
    impl.mutex.lockUncancelable(syncIo());
    if (client_stream.wake_queued) removeStreamWake(impl, client_stream);
    _ = impl.stream_states.remove(client_stream);
    impl.mutex.unlock(syncIo());
    client_stream.loop_owned = false;
    client_stream.impl = null;
    const destroy = !client_stream.app_owned;
    client_stream.mutex.unlock(syncIo());
    if (destroy) {
        // Synchronize with an application release that still touches callback_mutex.
        client_stream.callback_mutex.lockUncancelable(syncIo());
        client_stream.callback_mutex.unlock(syncIo());
        client_stream.destroy();
    }
}

fn queueInboundMessage(client_stream: *ClientStreamState, decoded: frame.Decoder.DecodedMessage) !void {
    const buffered_size = std.math.add(usize, decoded.payload.len, frame.header_size) catch
        return error.BufferLimitExceeded;
    if (buffered_size > client_stream.limits.max_inbound_buffer_size -| client_stream.inbound_buffered) {
        return error.BufferLimitExceeded;
    }
    try client_stream.inbound.append(client_stream.allocator, .{
        .payload = decoded.payload,
        .compression = if (decoded.compressed) .gzip else .identity,
        .buffered_size = buffered_size,
    });
    client_stream.inbound_buffered += buffered_size;
}

fn decodeAvailableMessages(client_stream: *ClientStreamState) !void {
    while (try client_stream.decoder.nextMessage()) |decoded| {
        var owned = true;
        defer if (owned) client_stream.allocator.free(decoded.payload);
        if (!callbacksEnabled(client_stream)) continue;
        if (client_stream.receive_paused) {
            try queueInboundMessage(client_stream, decoded);
            owned = false;
            continue;
        }
        const action = invokeMessageCallback(
            client_stream,
            decoded.payload,
            if (decoded.compressed) .gzip else .identity,
        ) orelse continue;
        if (action == .pause) client_stream.receive_paused = true;
    }
}

fn deliverInboundMessages(client_stream: *ClientStreamState) !void {
    client_stream.receive_paused = false;
    while (client_stream.inbound_head < client_stream.inbound.items.len and
        !client_stream.receive_paused and callbacksEnabled(client_stream))
    {
        const item = client_stream.inbound.items[client_stream.inbound_head];
        client_stream.inbound_head += 1;
        client_stream.inbound_buffered -= item.buffered_size;
        const action = invokeMessageCallback(
            client_stream,
            item.payload,
            item.compression,
        ) orelse .continue_receiving;
        client_stream.allocator.free(item.payload);
        if (action == .pause) client_stream.receive_paused = true;
    }
    if (client_stream.inbound_head == client_stream.inbound.items.len) {
        client_stream.inbound.clearRetainingCapacity();
        client_stream.inbound_head = 0;
    }
    if (!client_stream.receive_paused) try decodeAvailableMessages(client_stream);
    if (client_stream.transport_closed and !client_stream.receive_paused and
        client_stream.inbound_head == client_stream.inbound.items.len)
    {
        finalizeClientStream(client_stream);
        releaseStreamLoopOwnership(client_stream);
    }
}

fn notifyRemoteEnd(client_stream: *ClientStreamState) void {
    if (!client_stream.remote_end_seen or client_stream.remote_end_called) return;
    client_stream.remote_end_called = true;
    const callback = client_stream.callbacks.on_remote_end orelse return;
    if (!beginClientCallback(client_stream)) return;
    defer endClientCallback(client_stream);
    callback(client_stream.callbacks.context, client_stream.handle());
}

fn finalizeClientStream(client_stream: *ClientStreamState) void {
    notifyRemoteEnd(client_stream);
    if (client_stream.forced_status) |forced| {
        terminalizeStream(client_stream, forced);
        return;
    }
    if (client_stream.stream_error != c.NGHTTP2_NO_ERROR) {
        terminalizeStream(client_stream, streamErrorStatus(client_stream.stream_error));
        return;
    }
    if (!client_stream.saw_response_headers) {
        terminalizeStream(client_stream, .init(.unknown, "missing response headers"));
    } else if (client_stream.http_status == null) {
        terminalizeStream(client_stream, .init(.unknown, "missing HTTP status"));
    } else if (client_stream.http_status.? != 200) {
        terminalizeStream(client_stream, .init(
            httpStatusCode(client_stream.http_status.?),
            "HTTP request failed",
        ));
    } else if (!client_stream.content_type_grpc) {
        terminalizeStream(client_stream, .init(.unknown, "invalid gRPC content-type"));
    } else if (client_stream.grpc_status == null) {
        terminalizeStream(client_stream, .init(.unknown, "missing grpc-status"));
    } else if (client_stream.response_encoding_invalid) {
        terminalizeStream(client_stream, .init(.unimplemented, "response compression is not supported"));
    } else if (client_stream.response_metadata_invalid) {
        terminalizeStream(client_stream, .init(.internal, "invalid response metadata"));
    } else if (client_stream.decoder.finish()) |_| {
        var decoded_message: ?[]u8 = null;
        defer if (decoded_message) |value| client_stream.allocator.free(value);
        if (client_stream.grpc_message) |encoded| {
            decoded_message = message.decode(client_stream.allocator, encoded) catch {
                terminalizeStream(client_stream, .init(.unknown, "invalid grpc-message"));
                return;
            };
        }
        terminalizeStream(client_stream, .init(
            status.Code.fromInt(client_stream.grpc_status.?),
            if (decoded_message) |value| value else "",
        ));
    } else |_| {
        terminalizeStream(client_stream, .init(.internal, "malformed streaming response"));
    }
}

fn submitOperation(impl: *Impl, operation: *Operation) !void {
    try impl.operations.ensureUnusedCapacity(impl.allocator, 1);
    var headers: HeaderBuilder = .{};
    defer headers.deinit(impl.allocator);
    var encoded_values: EncodedValueBuilder = .{};
    defer {
        for (encoded_values.items()) |value| value.deinit(impl.allocator);
        encoded_values.deinit(impl.allocator);
    }
    try headers.append(impl.allocator, nativeHeader(":method", "POST"));
    try headers.append(impl.allocator, nativeHeader(":scheme", if (impl.tls_config != null) "https" else "http"));
    try headers.append(impl.allocator, nativeHeader(":path", operation.path));
    try headers.append(impl.allocator, nativeHeader(":authority", impl.authority));
    try headers.append(impl.allocator, nativeHeader("content-type", "application/grpc"));
    try headers.append(impl.allocator, nativeHeader("te", "trailers"));
    try headers.append(impl.allocator, nativeHeader("grpc-encoding", operation.request_compression.name()));
    try headers.append(impl.allocator, nativeHeader("grpc-accept-encoding", "identity,gzip"));
    try headers.append(impl.allocator, nativeHeader("user-agent", impl.user_agent));
    if (operation.timeout_header_len != 0) {
        try headers.append(impl.allocator, nativeHeader("grpc-timeout", operation.timeout_header[0..operation.timeout_header_len]));
    }
    for (operation.request_metadata.items()) |entry| {
        try appendMetadataHeader(&headers, &encoded_values, impl.allocator, entry);
    }

    var provider: c.nghttp2_data_provider2 = .{
        .source = .{ .ptr = operation },
        .read_callback = readRequestData,
    };
    const stream_id = c.nghttp2_submit_request2(
        impl.session,
        null,
        headers.items().ptr,
        headers.items().len,
        &provider,
        operation,
    );
    if (stream_id < 0) return error.NativeFailure;
    operation.stream_id = stream_id;
    impl.operations.putAssumeCapacity(stream_id, operation);
    if (comptime builtin.is_test) impl.test_observer.operation_submitted.store(true, .release);
}

fn readRequestData(
    _: ?*c.nghttp2_session,
    _: i32,
    output: [*c]u8,
    output_length: usize,
    data_flags: ?*u32,
    source: ?*c.nghttp2_data_source,
    _: ?*anyopaque,
) callconv(.c) c.nghttp2_ssize {
    const operation: *Operation = @ptrCast(@alignCast(source.?.*.ptr.?));
    const remaining = operation.request_frame[operation.request_offset..];
    const length = @min(remaining.len, output_length);
    @memcpy(output[0..length], remaining[0..length]);
    operation.request_offset += length;
    if (operation.request_offset == operation.request_frame.len) data_flags.?.* |= c.NGHTTP2_DATA_FLAG_EOF;
    return @intCast(length);
}

fn flush(impl: *Impl) !void {
    if (comptime build_options.tls) {
        if (impl.tls_session != null) return flushTls(impl);
    }
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(impl.allocator);
    while (!impl.stopping_on_loop) {
        if (!canFlushWritesWithPending(
            impl.queued_write_bytes,
            batch.pendingBytes(),
            impl.write_high_watermark_bytes,
        )) break;
        var data: [*c]const u8 = null;
        const length = c.nghttp2_session_mem_send2(impl.session, &data);
        if (length < 0) return error.NativeFailure;
        if (length == 0) break;
        if (try batch.append(
            impl.allocator,
            data[0..@intCast(length)],
            socket_write_batch_target,
        )) |bytes| try queueOwnedSocketWrite(impl, bytes);
        if (batch.ready(socket_write_batch_target)) {
            try queueOwnedSocketWrite(impl, (try batch.take(impl.allocator)).?);
        }
    }
    if (try batch.take(impl.allocator)) |bytes| try queueOwnedSocketWrite(impl, bytes);
}

fn flushTls(impl: *Impl) !void {
    const tls_session = impl.tls_session.?;
    while (!impl.stopping_on_loop) {
        try drainTlsCiphertext(impl);
        if (tls_session.hasPendingWrite()) {
            const result = try tls_session.continueWrite();
            if (try finishTlsWrite(impl, result)) continue;
            return;
        }
        if (impl.tls_plaintext) |plaintext| {
            if (impl.tls_plaintext_offset == plaintext.len) {
                impl.allocator.free(plaintext);
                impl.tls_plaintext = null;
                impl.tls_plaintext_offset = 0;
                continue;
            }
            const result = try tls_session.beginWrite(plaintext[impl.tls_plaintext_offset..]);
            if (try finishTlsWrite(impl, result)) continue;
            return;
        }
        if (!canFlushWrites(impl.queued_write_bytes, impl.write_high_watermark_bytes)) return;
        var data: [*c]const u8 = null;
        const length = c.nghttp2_session_mem_send2(impl.session, &data);
        if (length < 0) return error.NativeFailure;
        if (length == 0) return;
        impl.tls_plaintext = try impl.allocator.dupe(u8, data[0..@intCast(length)]);
    }
}

fn finishTlsWrite(impl: *Impl, result: tls_record.Result) !bool {
    switch (result) {
        .bytes => |count| {
            impl.tls_plaintext_offset += count;
            try drainTlsCiphertext(impl);
            return true;
        },
        .want_write => {
            try drainTlsCiphertext(impl);
            return true;
        },
        .want_read => {
            try drainTlsCiphertext(impl);
            return false;
        },
        else => return error.TlsWriteFailed,
    }
}

fn queueSocketWrite(impl: *Impl, source: []const u8) !void {
    const bytes = try impl.allocator.dupe(u8, source);
    try queueOwnedSocketWrite(impl, bytes);
}

fn queueOwnedSocketWrite(impl: *Impl, bytes: []u8) !void {
    const write = acquireWriteRequest(impl, bytes) catch |err| {
        impl.allocator.free(bytes);
        return err;
    };
    errdefer releaseWriteRequest(impl, write);
    errdefer impl.allocator.free(bytes);
    try impl.writes.put(impl.allocator, write, {});
    errdefer {
        const removed = impl.writes.remove(write);
        std.debug.assert(removed);
    }
    impl.queued_write_bytes = try addQueuedWriteBytes(impl.queued_write_bytes, bytes.len);
    impl.tcp.queueWrite(
        &impl.loop,
        &impl.write_queue,
        &write.request,
        .{ .slice = bytes },
        WriteRequest,
        write,
        onWrite,
    );
}

fn drainTlsCiphertext(impl: *Impl) !void {
    if (comptime !build_options.tls) return;
    const tls_session = impl.tls_session orelse return;
    const ciphertext = tls_session.ciphertext();
    if (ciphertext.len == 0) return;
    try queueSocketWrite(impl, ciphertext);
    tls_session.consumeCiphertext(ciphertext.len);
}

fn driveTlsHandshake(impl: *Impl) !void {
    if (comptime !build_options.tls) return error.TlsUnavailable;
    const tls_session = impl.tls_session orelse return error.TlsUnavailable;
    while (true) {
        impl.tls_handshake_needs_write = false;
        const result = tls_session.handshake();
        try drainTlsCiphertext(impl);
        switch (result) {
            .complete => {
                try activateConnection(impl);
                if (impl.stopping_on_loop) return error.TlsHandshakeFailed;
                try receiveTlsPlaintext(impl);
                return;
            },
            .want_write => {
                impl.tls_handshake_needs_write = true;
                return;
            },
            .want_read => return,
            else => return error.TlsHandshakeFailed,
        }
    }
}

fn clearTlsConnection(impl: *Impl) void {
    if (comptime build_options.tls) {
        if (impl.tls_session) |session| session.destroy();
    }
    impl.tls_session = null;
    clearTlsHandshakeDeadline(impl);
    impl.tls_handshake_needs_write = false;
    if (impl.tls_plaintext) |plaintext| impl.allocator.free(plaintext);
    impl.tls_plaintext = null;
    impl.tls_plaintext_offset = 0;
}

fn onRead(impl_: ?*Impl, loop: *xev.Loop, _: *xev.Completion, _: xev.TCP, _: xev.ReadBuffer, result: xev.ReadError!usize) xev.CallbackAction {
    const impl = impl_.?;
    impl.read_active = false;
    defer submitCloseIfReady(impl, loop);
    const bytes_read = result catch {
        if (impl.connection_state == .closing or impl.stopping_on_loop) return .disarm;
        if (impl.connection_state == .handshaking) {
            failConnectionCandidate(impl, "TLS handshake failed");
            return .disarm;
        }
        closeOrReconnectAfterReadFailure(impl, "connection closed");
        return .disarm;
    };
    if (bytes_read == 0) {
        if (impl.connection_state == .closing or impl.stopping_on_loop) return .disarm;
        if (impl.connection_state == .handshaking) {
            failConnectionCandidate(impl, "TLS handshake failed");
            return .disarm;
        }
        if (comptime build_options.tls) {
            if (impl.tls_session) |tls_session| tls_session.markTransportEof();
        }
        closeOrReconnectAfterReadFailure(impl, "connection closed");
        return .disarm;
    }
    if (impl.connection_state == .closing or impl.stopping_on_loop) return .disarm;
    if (comptime build_options.tls) {
        if (impl.tls_session) |tls_session| {
            tls_session.feedCiphertext(impl.read_buffer[0..bytes_read]) catch {
                if (impl.connection_state == .handshaking)
                    failConnectionCandidate(impl, "TLS handshake failed")
                else
                    closeOrReconnectAfterReadFailure(impl, "TLS connection failed");
                return .disarm;
            };
            if (impl.connection_state == .handshaking) {
                driveTlsHandshake(impl) catch {
                    failConnectionCandidate(impl, "TLS handshake failed");
                    return .disarm;
                };
            } else {
                receiveTlsPlaintext(impl) catch {
                    closeOrReconnectAfterReadFailure(impl, "TLS connection failed");
                    return .disarm;
                };
            }
            if (impl.connection_state == .closing or impl.stopping_on_loop) return .disarm;
            impl.read_active = true;
            return .rearm;
        }
    }
    receiveHttp2(impl, impl.read_buffer[0..bytes_read]) catch {
        handleTransportFailure(impl, "HTTP/2 connection failed");
        return .disarm;
    };
    impl.read_active = true;
    return .rearm;
}

fn closeOrReconnectAfterReadFailure(impl: *Impl, reason: []const u8) void {
    if (impl.connection_state == .draining and impl.operations.count() == 0 and impl.streams.count() == 0) {
        beginReconnect(impl);
    } else {
        handleTransportFailure(impl, reason);
    }
}

fn receiveTlsPlaintext(impl: *Impl) !void {
    if (comptime !build_options.tls) return error.TlsUnavailable;
    const tls_session = impl.tls_session orelse return error.TlsUnavailable;
    while (true) {
        switch (try tls_session.read(&impl.plaintext_buffer)) {
            .bytes => |length| {
                try receiveHttp2(impl, impl.plaintext_buffer[0..length]);
            },
            .want_read => break,
            .want_write => try drainTlsCiphertext(impl),
            else => return error.TlsConnectionClosed,
        }
    }
}

fn receiveHttp2(impl: *Impl, input: []const u8) !void {
    std.debug.assert(receiving_http2_impl == null);
    const consumed = received: {
        receiving_http2_impl = impl;
        defer receiving_http2_impl = null;
        break :received c.nghttp2_session_mem_recv2(impl.session, input.ptr, input.len);
    };
    if (consumed < 0 or consumed != @as(c.nghttp2_ssize, @intCast(input.len))) {
        return error.Http2ConnectionFailed;
    }
    processPending(impl);
    if (impl.stopping_on_loop) return error.Http2ConnectionFailed;
}

fn onWrite(write_: ?*WriteRequest, loop: *xev.Loop, _: *xev.Completion, _: xev.TCP, _: xev.WriteBuffer, result: xev.WriteError!usize) xev.CallbackAction {
    const write = write_.?;
    const impl = write.impl;
    const generation = write.generation;
    const removed = impl.writes.remove(write);
    std.debug.assert(removed);
    impl.queued_write_bytes = completeQueuedWrite(impl.queued_write_bytes, write.bytes.len);
    // WriteQueue retries partial writes and calls back with the full buffer length.
    const completed: ?usize = result catch null;
    const write_succeeded = completed != null and completed.? == write.bytes.len;
    impl.allocator.free(write.bytes);
    releaseWriteRequest(impl, write);
    if (!write_succeeded and generation == impl.connection_generation.load(.monotonic) and impl.connection_state != .closing) {
        handleTransportFailure(impl, "connection write failed");
    }
    if (write_succeeded and
        generation == impl.connection_generation.load(.monotonic) and
        impl.connected and
        (impl.tls_handshake_needs_write or impl.queued_write_bytes < impl.write_low_watermark_bytes))
    {
        // WriteQueue schedules its next head after this callback returns.
        // Wake the async callback so queueWrite cannot reenter that ordering.
        if (!impl.write_wake_queued) {
            impl.write_wake_queued = true;
            impl.async_handle.notify() catch {
                impl.write_wake_queued = false;
                handleTransportFailure(impl, "connection write failed");
            };
        }
    }
    if (impl.stopping_on_loop or impl.discard_writes_after_cancel) {
        discardQueuedWrites(impl);
        impl.discard_writes_after_cancel = false;
    }
    submitCloseIfReady(impl, loop);
    return .disarm;
}

fn onBeginHeaders(_: ?*c.nghttp2_session, received_frame: ?*const c.nghttp2_frame, user_data: ?*anyopaque) callconv(.c) c_int {
    const native_frame = received_frame.?;
    if (native_frame.*.hd.type != c.NGHTTP2_HEADERS) return 0;
    const kind: HeaderKind = switch (native_frame.*.headers.cat) {
        c.NGHTTP2_HCAT_RESPONSE => .response,
        c.NGHTTP2_HCAT_HEADERS => .trailers,
        else => return 0,
    };
    const impl: *Impl = @ptrCast(@alignCast(user_data.?));
    if (impl.operations.get(native_frame.*.hd.stream_id)) |operation| {
        operation.resetHeaderBlock(kind);
        if (kind == .response) operation.saw_response_headers = true;
    } else if (impl.streams.get(native_frame.*.hd.stream_id)) |client_stream| {
        client_stream.resetHeaderBlock(kind);
        if (kind == .response) client_stream.saw_response_headers = true;
    }
    return 0;
}

const response_headers_too_large_message = "received metadata size exceeds hard limit";

fn accountResponseHeaderField(total: *usize, name_length: usize, value_length: usize, limit: u32) bool {
    const field_size = std.math.add(usize, name_length, value_length) catch return false;
    const charged_size = std.math.add(usize, field_size, 32) catch return false;
    const maximum: usize = limit;
    if (charged_size > maximum or total.* > maximum - charged_size) return false;
    total.* += charged_size;
    return true;
}

fn rejectOperationResponseHeaders(session: ?*c.nghttp2_session, operation: *Operation) c_int {
    operation.response_headers_too_large = true;
    operation.resetHeaderBlock(.none);
    operation.setStaticOutcome(.resource_exhausted, response_headers_too_large_message);
    if (!operation.response_header_reset_submitted) {
        if (c.nghttp2_submit_rst_stream(
            session,
            c.NGHTTP2_FLAG_NONE,
            operation.stream_id,
            c.NGHTTP2_CANCEL,
        ) != 0) return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        operation.response_header_reset_submitted = true;
    }
    return c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
}

fn rejectClientStreamResponseHeaders(client_stream: *ClientStreamState) c_int {
    client_stream.response_headers_too_large = true;
    client_stream.resetHeaderBlock(.none);

    // Serialize with application cancellation so the first local condition wins even
    // when its wake has not reached the transport loop yet.
    client_stream.mutex.lockUncancelable(syncIo());
    if (client_stream.forced_status == null) {
        client_stream.forced_status = if (client_stream.cancel_requested and !client_stream.app_released)
            .init(.cancelled, "stream cancelled")
        else
            .init(.resource_exhausted, response_headers_too_large_message);
    }
    client_stream.mutex.unlock(syncIo());

    if (!failClientStreamOnLoop(
        client_stream,
        .init(.resource_exhausted, response_headers_too_large_message),
    )) return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    return c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
}

fn accountResponseHeader(
    session: ?*c.nghttp2_session,
    impl: *Impl,
    stream_id: i32,
    name_length: usize,
    value_length: usize,
) ?c_int {
    if (impl.operations.get(stream_id)) |operation| {
        if (operation.response_headers_too_large) return 0;
        if (!accountResponseHeaderField(
            &operation.response_header_list_size,
            name_length,
            value_length,
            impl.max_response_header_list_size,
        )) return rejectOperationResponseHeaders(session, operation);
    } else if (impl.streams.get(stream_id)) |client_stream| {
        if (client_stream.response_headers_too_large) return 0;
        if (!accountResponseHeaderField(
            &client_stream.response_header_list_size,
            name_length,
            value_length,
            impl.max_response_header_list_size,
        )) return rejectClientStreamResponseHeaders(client_stream);
    }
    return null;
}

fn onHeader(
    session: ?*c.nghttp2_session,
    received_frame: ?*const c.nghttp2_frame,
    name_pointer: [*c]const u8,
    name_length: usize,
    value_pointer: [*c]const u8,
    value_length: usize,
    _: u8,
    user_data: ?*anyopaque,
) callconv(.c) c_int {
    const stream_id = received_frame.?.*.hd.stream_id;
    const impl: *Impl = @ptrCast(@alignCast(user_data.?));
    if (accountResponseHeader(session, impl, stream_id, name_length, value_length)) |result| {
        return result;
    }
    const name = name_pointer[0..name_length];
    const value = value_pointer[0..value_length];
    if (impl.operations.get(stream_id)) |operation| {
        if (std.mem.eql(u8, name, ":status")) {
            operation.http_status = std.fmt.parseInt(u16, value, 10) catch null;
        } else if (std.mem.eql(u8, name, "content-type")) {
            operation.content_type_grpc = std.mem.startsWith(u8, value, "application/grpc");
        } else if (std.mem.eql(u8, name, "grpc-status")) {
            operation.block_grpc_status = std.fmt.parseInt(u32, value, 10) catch std.math.maxInt(u32);
        } else if (std.mem.eql(u8, name, "grpc-message")) {
            if (operation.block_grpc_message) |old| operation.impl.allocator.free(old);
            operation.block_grpc_message = operation.impl.allocator.dupe(u8, value) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        } else if (std.mem.eql(u8, name, "grpc-encoding")) {
            operation.response_compression = Compression.parse(value) orelse {
                operation.response_encoding_invalid = true;
                return 0;
            };
        } else {
            processResponseMetadata(operation, name, value) catch |err| switch (err) {
                error.ResponseHeadersTooLarge => return rejectOperationResponseHeaders(session, operation),
                else => return c.NGHTTP2_ERR_CALLBACK_FAILURE,
            };
        }
    } else if (impl.streams.get(stream_id)) |client_stream| {
        if (std.mem.eql(u8, name, ":status")) {
            client_stream.http_status = std.fmt.parseInt(u16, value, 10) catch null;
        } else if (std.mem.eql(u8, name, "content-type")) {
            client_stream.content_type_grpc = std.mem.startsWith(u8, value, "application/grpc");
        } else if (std.mem.eql(u8, name, "grpc-status")) {
            client_stream.block_grpc_status = std.fmt.parseInt(u32, value, 10) catch std.math.maxInt(u32);
        } else if (std.mem.eql(u8, name, "grpc-message")) {
            if (client_stream.block_grpc_message) |old| client_stream.allocator.free(old);
            client_stream.block_grpc_message = client_stream.allocator.dupe(u8, value) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        } else if (std.mem.eql(u8, name, "grpc-encoding")) {
            client_stream.response_compression = Compression.parse(value) orelse {
                client_stream.response_encoding_invalid = true;
                return 0;
            };
            client_stream.decoder.compression = client_stream.response_compression;
        } else {
            processClientResponseMetadata(client_stream, name, value) catch |err| switch (err) {
                error.ResponseHeadersTooLarge => return rejectClientStreamResponseHeaders(client_stream),
                else => return c.NGHTTP2_ERR_CALLBACK_FAILURE,
            };
        }
    }
    return 0;
}

fn onInvalidHeader(
    session: ?*c.nghttp2_session,
    received_frame: ?*const c.nghttp2_frame,
    _: [*c]const u8,
    name_length: usize,
    _: [*c]const u8,
    value_length: usize,
    _: u8,
    user_data: ?*anyopaque,
) callconv(.c) c_int {
    const stream_id = received_frame.?.*.hd.stream_id;
    const impl: *Impl = @ptrCast(@alignCast(user_data.?));
    return accountResponseHeader(session, impl, stream_id, name_length, value_length) orelse 0;
}

fn onDataChunk(
    session: ?*c.nghttp2_session,
    _: u8,
    stream_id: i32,
    data_pointer: [*c]const u8,
    data_length: usize,
    user_data: ?*anyopaque,
) callconv(.c) c_int {
    const impl: *Impl = @ptrCast(@alignCast(user_data.?));
    if (impl.operations.get(stream_id)) |operation| {
        if (operation.response_headers_too_large) {
            if (c.nghttp2_session_consume_connection(session, data_length) != 0) {
                return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            }
            return 0;
        }
        const limit = wireMessageLimit(operation.max_response_size);
        if (data_length > limit -| operation.response_body.items.len) {
            operation.response_too_large = true;
        } else {
            operation.response_body.appendSlice(operation.impl.allocator, data_pointer[0..data_length]) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
        if (c.nghttp2_session_consume_connection(session, data_length) != 0 or
            c.nghttp2_session_consume_stream(session, stream_id, data_length) != 0)
        {
            return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
    } else if (impl.streams.get(stream_id)) |client_stream| {
        if (client_stream.response_headers_too_large) {
            if (c.nghttp2_session_consume_connection(session, data_length) != 0) {
                return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            }
            return 0;
        }
        const decoder_limit = client_stream.limits.max_inbound_buffer_size -| client_stream.inbound_buffered;
        client_stream.decoder.feedBounded(data_pointer[0..data_length], decoder_limit) catch |err| {
            const failure: status.Status = switch (err) {
                error.BufferLimitExceeded => .init(.resource_exhausted, "inbound stream buffer is full"),
                else => .init(.internal, "malformed streaming response"),
            };
            if (!failClientStreamOnLoop(client_stream, failure)) return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            if (c.nghttp2_session_consume_connection(session, data_length) != 0) {
                return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            }
            return 0;
        };
        if (c.nghttp2_session_consume_connection(session, data_length) != 0) {
            return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
        decodeAvailableMessages(client_stream) catch |err| {
            const failure: status.Status = switch (err) {
                error.MessageTooLarge, error.BufferLimitExceeded => .init(.resource_exhausted, "response message too large"),
                else => .init(.internal, "malformed streaming response"),
            };
            if (!failClientStreamOnLoop(client_stream, failure)) return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            return 0;
        };
        if (client_stream.receive_paused) {
            client_stream.deferred_stream_credit = std.math.add(
                usize,
                client_stream.deferred_stream_credit,
                data_length,
            ) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        } else if (c.nghttp2_session_consume_stream(session, stream_id, data_length) != 0) {
            return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
    }
    return 0;
}

fn onFrameReceived(session: ?*c.nghttp2_session, received_frame: ?*const c.nghttp2_frame, user_data: ?*anyopaque) callconv(.c) c_int {
    const native_frame = received_frame.?;
    const impl: *Impl = @ptrCast(@alignCast(user_data.?));
    if (native_frame.*.hd.type == c.NGHTTP2_SETTINGS and impl.reconnect_backoff_pending_reset) {
        impl.reconnect_attempt = 0;
        impl.reconnect_backoff_ns = impl.reconnect_options.?.initial_backoff_ns;
        impl.reconnect_backoff_pending_reset = false;
    }
    if (native_frame.*.hd.type == c.NGHTTP2_GOAWAY) {
        if (impl.connection_state == .active) {
            impl.logger.write(
                .info,
                "channel received GOAWAY target={s} last_stream_id={d} error_code={d}",
                .{ impl.authority, native_frame.*.goaway.last_stream_id, native_frame.*.goaway.error_code },
            );
            impl.connection_state = .draining;
            impl.mutex.lockUncancelable(syncIo());
            impl.accepting_streams = false;
            impl.mutex.unlock(syncIo());
            if (!terminalizeStreamsForGoAway(impl, native_frame.*.goaway.last_stream_id)) {
                return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            }
            impl.async_handle.notify() catch {};
        }
        return 0;
    }
    if (native_frame.*.hd.type == c.NGHTTP2_HEADERS) {
        const end_stream = native_frame.*.hd.flags & c.NGHTTP2_FLAG_END_STREAM != 0;
        if (impl.operations.get(native_frame.*.hd.stream_id)) |operation| {
            const valid_header_block = operation.finishHeaderBlock(end_stream) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            if ((!valid_header_block or operation.response_metadata_invalid) and !operation.outcome_set) {
                operation.setOutcome(
                    .internal,
                    if (valid_header_block) "invalid response metadata" else "grpc-status before end of stream",
                ) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
                if (!end_stream and
                    c.nghttp2_submit_rst_stream(session, c.NGHTTP2_FLAG_NONE, native_frame.*.hd.stream_id, c.NGHTTP2_CANCEL) != 0)
                {
                    return c.NGHTTP2_ERR_CALLBACK_FAILURE;
                }
            }
        } else if (impl.streams.get(native_frame.*.hd.stream_id)) |client_stream| {
            const valid_header_block = finishClientHeaderBlock(client_stream, end_stream) catch
                return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            if (!valid_header_block and !failClientStreamOnLoop(
                client_stream,
                .init(.internal, "grpc-status before end of stream"),
            )) return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
    }
    if (native_frame.*.hd.flags & c.NGHTTP2_FLAG_END_STREAM != 0) {
        if (impl.streams.get(native_frame.*.hd.stream_id)) |client_stream| {
            client_stream.remote_end_seen = true;
        }
    }
    return 0;
}

fn onStreamClose(_: ?*c.nghttp2_session, stream_id: i32, error_code: u32, user_data: ?*anyopaque) callconv(.c) c_int {
    const impl: *Impl = @ptrCast(@alignCast(user_data.?));
    if (impl.operations.fetchRemove(stream_id)) |entry| {
        removeOperationDeadline(impl, entry.value);
        entry.value.finalize(error_code);
        scheduleDeadlineTimer(impl);
        if (impl.connection_state == .draining and impl.operations.count() == 0 and impl.streams.count() == 0) {
            impl.async_handle.notify() catch {};
        }
    } else if (impl.streams.fetchRemove(stream_id)) |entry| {
        const client_stream = entry.value;
        removeStreamDeadline(impl, client_stream);
        client_stream.transport_closed = true;
        client_stream.stream_error = error_code;
        client_stream.mutex.lockUncancelable(syncIo());
        client_stream.send_open = false;
        const released = client_stream.app_released;
        const cancel_requested = client_stream.cancel_requested;
        client_stream.mutex.unlock(syncIo());
        if (cancel_requested and !released) {
            setForcedStreamStatus(client_stream, .init(.cancelled, "stream cancelled"));
        }
        if (client_stream.terminal or client_stream.forced_status != null or released or
            (!client_stream.receive_paused and client_stream.inbound_head == client_stream.inbound.items.len))
        {
            finalizeClientStream(client_stream);
            releaseStreamLoopOwnership(client_stream);
        }
        scheduleDeadlineTimer(impl);
        if (impl.connection_state == .draining and impl.operations.count() == 0 and impl.streams.count() == 0) {
            impl.async_handle.notify() catch {};
        }
    }
    return 0;
}

fn scheduleDeadlineTimer(impl: *Impl) void {
    if (impl.stopping_on_loop) return;
    var now: ?u64 = null;
    while (deadlineHeapPeek(impl)) |entry| {
        const earliest = entry.expires_at_ns;
        const current = cachedNow(&now);
        const delay_ms = deadlineDelayMs(earliest, current) orelse {
            expireDeadlines(impl, current) catch |err| {
                beginStop(impl, if (err == error.DeadlineCancellationFailed)
                    "deadline cancellation failed"
                else
                    "connection failed");
                return;
            };
            continue;
        };
        if (!impl.deadline_timer_armed) {
            impl.deadline_timer_armed = true;
            impl.deadline_timer_deadline_ns = earliest;
            observeDeadlineTimerScheduled(impl, earliest);
            impl.deadline_timer.run(
                &impl.loop,
                &impl.deadline_completion,
                delay_ms,
                Impl,
                impl,
                onDeadlineTimer,
            );
        } else if (earliest < impl.deadline_timer_deadline_ns.?) {
            impl.deadline_timer_deadline_ns = earliest;
            observeDeadlineTimerScheduled(impl, earliest);
            impl.deadline_timer.reset(
                &impl.loop,
                &impl.deadline_completion,
                &impl.deadline_reset_completion,
                delay_ms,
                Impl,
                impl,
                onDeadlineTimer,
            );
        }
        return;
    }
}

fn deadlineDelayMs(deadline_ns: u64, now_ns: u64) ?u64 {
    const remaining_ns = deadline_ns -| now_ns;
    if (remaining_ns == 0) return null;
    return @max(
        @as(u64, 1),
        std.math.divCeil(u64, remaining_ns, std.time.ns_per_ms) catch 1,
    );
}

fn onDeadlineTimer(impl_: ?*Impl, _: *xev.Loop, _: *xev.Completion, result: xev.Timer.RunError!void) xev.CallbackAction {
    const impl = impl_.?;
    impl.deadline_timer_armed = false;
    impl.deadline_timer_deadline_ns = null;
    if (comptime builtin.is_test) {
        _ = impl.test_observer.deadline_timer_callbacks.fetchAdd(1, .release);
        impl.test_observer.deadline_timer_target_ns.store(0, .release);
        impl.test_observer.deadline_timer_armed.store(false, .release);
    }
    result catch |err| switch (err) {
        error.Canceled => {
            scheduleDeadlineTimer(impl);
            return .disarm;
        },
        else => {
            beginStop(impl, "deadline timer failed");
            return .disarm;
        },
    };
    expireDeadlines(impl, fast_clock.validatedNow(syncIo())) catch |err| {
        beginStop(impl, if (err == error.DeadlineCancellationFailed)
            "deadline cancellation failed"
        else
            "connection failed");
        return .disarm;
    };
    scheduleDeadlineTimer(impl);
    return .disarm;
}

fn expireDeadlines(impl: *Impl, now: u64) !void {
    var flush_needed = false;
    while (deadlineHeapPeek(impl)) |next| {
        if (next.expires_at_ns > now) break;
        const entry = deadlineHeapPop(impl).?;
        switch (entry.target) {
            .tls_handshake => |target_impl| {
                std.debug.assert(target_impl == impl);
                if (impl.tls_handshake_deadline_ns != entry.expires_at_ns) continue;
                impl.tls_handshake_deadline_ns = null;
                if (impl.connection_state != .handshaking or impl.tls_session == null) continue;
                failConnectionCandidate(impl, "TLS handshake timed out");
            },
            .reconnect => |target_impl| {
                std.debug.assert(target_impl == impl);
                if (impl.reconnect_deadline_ns != entry.expires_at_ns) continue;
                impl.reconnect_deadline_ns = null;
                if (impl.connection_state != .backing_off) continue;
                startConnection(impl) catch scheduleReconnect(impl, "connection failed");
            },
            .operation => |operation| {
                if (operation.deadline_ns != entry.expires_at_ns or operation.deadline_expired) continue;
                if (operation.waiting_queued) {
                    std.debug.assert(removeWaitingOperation(impl, operation));
                    operation.deadline_expired = true;
                    operation.setOutcome(.deadline_exceeded, "deadline exceeded") catch {};
                    operation.finish();
                    continue;
                }
                if (operation.stream_id < 0 or impl.operations.get(operation.stream_id) != operation) continue;
                operation.deadline_expired = true;
                operation.setOutcome(.deadline_exceeded, "deadline exceeded") catch {};
                if (c.nghttp2_submit_rst_stream(impl.session, c.NGHTTP2_FLAG_NONE, operation.stream_id, c.NGHTTP2_CANCEL) != 0) {
                    return error.DeadlineCancellationFailed;
                }
                flush_needed = true;
            },
            .client_stream => |client_stream| {
                if (client_stream.deadline_ns != entry.expires_at_ns or
                    client_stream.deadline_expired or client_stream.terminal or
                    client_stream.impl != impl) continue;
                client_stream.deadline_expired = true;
                if (client_stream.stream_id < 0 or client_stream.transport_closed) {
                    terminalizeStream(client_stream, .init(.deadline_exceeded, "deadline exceeded"));
                    releaseStreamLoopOwnership(client_stream);
                    continue;
                }
                if (impl.streams.get(client_stream.stream_id) != client_stream) continue;
                setForcedStreamStatus(client_stream, .init(.deadline_exceeded, "deadline exceeded"));
                if (!client_stream.rst_submitted) {
                    if (c.nghttp2_submit_rst_stream(
                        impl.session,
                        c.NGHTTP2_FLAG_NONE,
                        client_stream.stream_id,
                        c.NGHTTP2_CANCEL,
                    ) != 0) return error.DeadlineCancellationFailed;
                    client_stream.rst_submitted = true;
                    flush_needed = true;
                }
            },
        }
    }
    if (flush_needed and impl.connected) try flush(impl);
}

fn beginReconnect(impl: *Impl) void {
    if (impl.connection_state != .draining or impl.operations.count() != 0 or impl.streams.count() != 0) return;
    impl.connection_state = .closing;
    impl.reconnect_after_close = false;
    impl.connected = false;
    clearResolvedAddresses(impl);
    if (impl.session) |session| {
        c.nghttp2_session_del(session);
        impl.session = null;
    }
    clearTlsConnection(impl);
    if (impl.read_active and !impl.read_cancel_submitted) {
        impl.read_cancel_submitted = true;
        impl.loop.cancel(
            &impl.read_completion,
            &impl.read_cancel_completion,
            Impl,
            impl,
            onReadCanceled,
        );
    }
    submitCloseIfReady(impl, &impl.loop);
}

fn handleTransportFailure(impl: *Impl, reason: []const u8) void {
    if (impl.stopping_on_loop or impl.connection_state == .closing or impl.connection_state == .backing_off) return;
    if (!canReconnect(impl)) {
        impl.logger.write(.err, "channel stopped target={s} reason={s}", .{ impl.authority, reason });
        beginStop(impl, reason);
        return;
    }

    impl.connection_state = .closing;
    impl.logger.write(.warn, "channel connection lost target={s} reason={s}", .{ impl.authority, reason });
    impl.reconnect_after_close = true;
    impl.connected = false;
    clearResolvedAddresses(impl);
    clearTlsHandshakeDeadline(impl);
    impl.mutex.lockUncancelable(syncIo());
    impl.accepting_streams = false;
    impl.mutex.unlock(syncIo());

    while (impl.operations.count() != 0) {
        var iterator = impl.operations.iterator();
        const entry = iterator.next().?;
        const operation = entry.value_ptr.*;
        std.debug.assert(impl.operations.remove(entry.key_ptr.*));
        removeOperationDeadline(impl, operation);
        operation.setOutcome(.unavailable, reason) catch {};
        operation.finish();
    }
    while (true) {
        impl.mutex.lockUncancelable(syncIo());
        var iterator = impl.stream_states.keyIterator();
        const client_stream = if (iterator.next()) |entry| entry.* else null;
        impl.mutex.unlock(syncIo());
        const target = client_stream orelse break;
        terminalizeStream(target, .init(.unavailable, reason));
        releaseStreamLoopOwnership(target);
    }
    impl.streams.clearRetainingCapacity();

    if (impl.session) |session| c.nghttp2_session_del(session);
    impl.session = null;
    clearTlsConnection(impl);
    if (impl.read_active and !impl.read_cancel_submitted) {
        impl.read_cancel_submitted = true;
        impl.loop.cancel(
            &impl.read_completion,
            &impl.read_cancel_completion,
            Impl,
            impl,
            onReadCanceled,
        );
    }
    if (impl.write_queue.head) |request| {
        if (request.completion.state() == .active) {
            if (!impl.write_cancel_submitted) {
                impl.discard_writes_after_cancel = true;
                impl.write_cancel_submitted = true;
                impl.write_cancel_target = &request.completion;
                impl.loop.cancel(
                    &request.completion,
                    &impl.write_cancel_completion,
                    Impl,
                    impl,
                    onWriteCanceled,
                );
            }
        } else {
            discardQueuedWrites(impl);
            impl.discard_writes_after_cancel = false;
        }
    }
    submitCloseIfReady(impl, &impl.loop);
    scheduleDeadlineTimer(impl);
}

fn canReconnect(impl: *const Impl) bool {
    const options = impl.reconnect_options orelse return false;
    return impl.ever_active or options.allow_initial_offline;
}

fn scheduleReconnect(impl: *Impl, reason: []const u8) void {
    if (!canReconnect(impl)) {
        impl.signalStartup(false);
        beginStop(impl, reason);
        return;
    }
    clearResolvedAddresses(impl);
    impl.connection_state = .backing_off;
    impl.reconnect_after_close = false;
    impl.signalStartup(true);
    const options = impl.reconnect_options.?;
    const delay = jitterReconnectBackoffNs(options, impl.reconnect_backoff_ns, nextReconnectRandom(impl));
    impl.reconnect_backoff_ns = nextReconnectBackoffNs(options, impl.reconnect_backoff_ns);
    impl.reconnect_attempt +|= 1;
    const deadline = nowNs() +| delay;
    impl.logger.write(
        .debug,
        "channel reconnect scheduled target={s} attempt={d} delay_ns={d} reason={s}",
        .{ impl.authority, impl.reconnect_attempt, delay, reason },
    );
    deadlineHeapInsertOrUpdate(impl, .{ .reconnect = impl }, deadline) catch {
        impl.logger.write(.err, "channel reconnect scheduling failed target={s}", .{impl.authority});
        beginStop(impl, "reconnect scheduling failed");
        return;
    };
    impl.reconnect_deadline_ns = deadline;
    if (comptime builtin.is_test) impl.test_observer.reconnect_scheduled.store(true, .release);
    scheduleDeadlineTimer(impl);
}

fn nextReconnectRandom(impl: *Impl) u64 {
    var value = impl.reconnect_random_state;
    if (value == 0) value = 0x9e3779b97f4a7c15;
    value ^= value << 13;
    value ^= value >> 7;
    value ^= value << 17;
    impl.reconnect_random_state = value;
    return value;
}

fn nextReconnectBackoffNs(options: ReconnectOptions, current: u64) u64 {
    const scaled = std.math.mul(u64, current, options.multiplier_millis) catch return options.max_backoff_ns;
    return @min(scaled / 1000, options.max_backoff_ns);
}

fn jitterReconnectBackoffNs(options: ReconnectOptions, base: u64, random: u64) u64 {
    const spread = base / 100 * options.jitter_percent;
    if (spread == 0) return base;
    const width = std.math.mul(u64, spread, 2) catch std.math.maxInt(u64);
    const offset = random % (width +| 1);
    return @min(base - spread +| offset, options.max_backoff_ns);
}

fn failConnectionCandidate(impl: *Impl, reason: []const u8) void {
    if (impl.literal_address != null or impl.next_address >= impl.resolved_addresses.len) {
        if (!canReconnect(impl)) {
            impl.signalStartup(false);
            beginStop(impl, reason);
            return;
        }
        impl.reconnect_after_close = true;
    } else {
        impl.reconnect_after_close = false;
    }
    impl.connection_state = .closing;
    impl.connected = false;
    if (impl.session) |session| {
        c.nghttp2_session_del(session);
        impl.session = null;
    }
    clearTlsConnection(impl);
    if (impl.read_active and !impl.read_cancel_submitted) {
        impl.read_cancel_submitted = true;
        impl.loop.cancel(
            &impl.read_completion,
            &impl.read_cancel_completion,
            Impl,
            impl,
            onReadCanceled,
        );
    }
    submitCloseIfReady(impl, &impl.loop);
}

fn submitCloseIfReady(impl: *Impl, loop: *xev.Loop) void {
    if (!impl.tcp_initialized) {
        if (impl.stopping_on_loop and
            !impl.connect_active and
            !impl.connect_cancel_submitted and
            !impl.read_active and
            !impl.read_cancel_submitted and
            !impl.write_cancel_submitted and
            impl.writes.count() == 0)
        {
            loop.stop();
        }
        return;
    }
    if (impl.close_submitted or
        impl.connect_active or
        impl.connect_cancel_submitted or
        impl.read_active or
        impl.read_cancel_submitted or
        impl.write_cancel_submitted or
        impl.writes.count() != 0) return;
    impl.close_submitted = true;
    impl.tcp.close(loop, &impl.close_completion, Impl, impl, onTcpClosed);
}

fn onTcpClosed(impl_: ?*Impl, loop: *xev.Loop, _: *xev.Completion, _: xev.TCP, _: xev.CloseError!void) xev.CallbackAction {
    const impl = impl_.?;
    impl.close_completed = true;
    impl.tcp_initialized = false;
    impl.connected = false;
    if (impl.stopping_on_loop) {
        loop.stop();
        return .disarm;
    }
    if (impl.connection_state != .closing) return .disarm;

    impl.mutex.lockUncancelable(syncIo());
    const running = impl.state == .starting or impl.state == .running;
    impl.mutex.unlock(syncIo());
    if (!running) return .disarm;

    if (impl.reconnect_after_close) {
        scheduleReconnect(impl, "connection failed");
    } else {
        startConnection(impl) catch scheduleReconnect(impl, "connection failed");
    }
    return .disarm;
}

fn onConnectCanceled(impl_: ?*Impl, loop: *xev.Loop, _: *xev.Completion, _: xev.CancelError!void) xev.CallbackAction {
    const impl = impl_.?;
    impl.connect_cancel_submitted = false;
    if (comptime builtin.is_test) impl.test_observer.connect_cancel_confirmed.store(true, .release);
    submitCloseIfReady(impl, loop);
    return .disarm;
}

fn onReadCanceled(impl_: ?*Impl, loop: *xev.Loop, _: *xev.Completion, _: xev.CancelError!void) xev.CallbackAction {
    const impl = impl_.?;
    impl.read_cancel_submitted = false;
    submitCloseIfReady(impl, loop);
    return .disarm;
}

fn onWriteCanceled(impl_: ?*Impl, loop: *xev.Loop, _: *xev.Completion, result: xev.CancelError!void) xev.CallbackAction {
    const impl = impl_.?;
    if (result) |_| {} else |err| {
        if (err == error.NotFound) {
            if (impl.write_cancel_target) |target| {
                if (target.state() == .active) return .rearm;
            }
        }
    }
    impl.write_cancel_submitted = false;
    impl.write_cancel_target = null;
    submitCloseIfReady(impl, loop);
    return .disarm;
}

fn beginStop(impl: *Impl, reason: []const u8) void {
    if (impl.stopping_on_loop) return;
    impl.stopping_on_loop = true;
    impl.connection_state = .closing;
    impl.connected = false;
    clearTlsHandshakeDeadline(impl);
    clearReconnectDeadline(impl);
    if (impl.resolver_initialized) impl.resolver.shutdown();
    impl.mutex.lockUncancelable(syncIo());
    impl.accepting_streams = false;
    if (impl.state == .starting) {
        impl.state = .stopping;
        impl.condition.broadcast(syncIo());
    } else if (impl.state == .running) {
        impl.state = .stopping;
    }
    var pending: std.ArrayList(*Operation) = .empty;
    std.mem.swap(std.ArrayList(*Operation), &pending, &impl.pending);
    impl.mutex.unlock(syncIo());

    while (pending.pop()) |operation| {
        operation.setOutcome(.unavailable, reason) catch {};
        operation.finish();
    }
    pending.deinit(impl.allocator);

    while (popWaitingOperation(impl)) |operation| {
        removeOperationDeadline(impl, operation);
        operation.setOutcome(.unavailable, reason) catch {};
        operation.finish();
    }

    if (impl.session) |session| {
        c.nghttp2_session_del(session);
        impl.session = null;
    }
    while (impl.operations.count() != 0) {
        var iterator = impl.operations.iterator();
        const entry = iterator.next().?;
        const stream_id = entry.key_ptr.*;
        const operation = entry.value_ptr.*;
        std.debug.assert(impl.operations.remove(stream_id));
        removeOperationDeadline(impl, operation);
        operation.setOutcome(.unavailable, reason) catch {};
        operation.finish();
    }

    while (impl.streams.count() != 0) {
        var stream_iterator = impl.streams.iterator();
        const entry = stream_iterator.next().?;
        const stream_id = entry.key_ptr.*;
        const client_stream = entry.value_ptr.*;
        _ = impl.streams.remove(stream_id);
        client_stream.transport_closed = true;
        client_stream.stream_error = c.NGHTTP2_CANCEL;
        terminalizeStream(client_stream, .init(.unavailable, reason));
        releaseStreamLoopOwnership(client_stream);
    }
    while (true) {
        var pending_stream: ?*ClientStreamState = null;
        impl.mutex.lockUncancelable(syncIo());
        var state_iterator = impl.stream_states.keyIterator();
        while (state_iterator.next()) |client_stream_ptr| {
            pending_stream = client_stream_ptr.*;
            break;
        }
        impl.mutex.unlock(syncIo());
        const client_stream = pending_stream orelse break;
        terminalizeStream(client_stream, .init(.unavailable, reason));
        releaseStreamLoopOwnership(client_stream);
    }
    std.debug.assert(impl.deadline_heap.items.len == 0);

    if (impl.connect_active and !impl.connect_cancel_submitted) {
        impl.connect_cancel_submitted = true;
        impl.loop.cancel(
            &impl.connect_completion,
            &impl.connect_cancel_completion,
            Impl,
            impl,
            onConnectCanceled,
        );
    }
    if (impl.read_active and !impl.read_cancel_submitted) {
        impl.read_cancel_submitted = true;
        impl.loop.cancel(
            &impl.read_completion,
            &impl.read_cancel_completion,
            Impl,
            impl,
            onReadCanceled,
        );
    }
    if (impl.write_queue.head) |request| {
        if (request.completion.state() == .active) {
            if (!impl.write_cancel_submitted) {
                impl.write_cancel_submitted = true;
                impl.write_cancel_target = &request.completion;
                impl.loop.cancel(
                    &request.completion,
                    &impl.write_cancel_completion,
                    Impl,
                    impl,
                    onWriteCanceled,
                );
            }
        } else {
            discardQueuedWrites(impl);
        }
    }
    submitCloseIfReady(impl, &impl.loop);
}

fn discardQueuedWrites(impl: *Impl) void {
    while (impl.write_queue.pop()) |request| {
        const write: *WriteRequest = @fieldParentPtr("request", request);
        const removed = impl.writes.remove(write);
        std.debug.assert(removed);
        impl.queued_write_bytes = completeQueuedWrite(impl.queued_write_bytes, write.bytes.len);
        impl.allocator.free(write.bytes);
        releaseWriteRequest(impl, write);
    }
}

fn observeTestIo(impl: *Impl) void {
    if (comptime builtin.is_test) {
        if (impl.test_observer.write_requested.swap(false, .acq_rel)) {
            if (impl.write_queue.head) |request| {
                if (request.completion.state() == .active) {
                    impl.test_observer.write_observed.store(true, .release);
                    impl.test_observer.write_observed_sem.post(std.testing.io);
                }
            }
        }
        if (impl.test_observer.connect_requested.load(.acquire) and
            impl.connect_active and
            impl.connect_completion.state() == .active)
        {
            impl.test_observer.connect_requested.store(false, .release);
            impl.test_observer.connect_observed.store(true, .release);
            impl.test_observer.connect_observed_sem.post(std.testing.io);
            impl.test_observer.connect_release.waitUncancelable(std.testing.io);
        }
    }
}

fn observeDeadlineTimerScheduled(impl: *Impl, deadline_ns: u64) void {
    if (comptime builtin.is_test) {
        impl.test_observer.deadline_timer_target_ns.store(deadline_ns, .release);
        impl.test_observer.deadline_timer_armed.store(true, .release);
    }
}

fn syncIo() std.Io {
    return std.Io.Threaded.global_single_threaded.io();
}

fn nowNs() u64 {
    return fast_clock.now(syncIo());
}

fn cachedNow(cached: *?u64) u64 {
    if (cached.*) |value| return value;
    const value = nowNs();
    cached.* = value;
    return value;
}

fn waitForTestFlag(flag: *const std.atomic.Value(bool), timeout_ns: u64) bool {
    const deadline = nowNs() +| timeout_ns;
    while (!flag.load(.acquire)) {
        if (nowNs() >= deadline) return false;
        std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake) catch return false;
    }
    return true;
}

fn nativeHeader(name: []const u8, value: []const u8) c.nghttp2_nv {
    return .{
        .name = @ptrCast(@constCast(name.ptr)),
        .value = @ptrCast(@constCast(value.ptr)),
        .namelen = name.len,
        .valuelen = value.len,
        .flags = c.NGHTTP2_NV_FLAG_NONE,
    };
}

fn appendMetadataHeader(
    headers: *HeaderBuilder,
    encoded_values: *EncodedValueBuilder,
    allocator: std.mem.Allocator,
    entry: metadata.Entry,
) !void {
    const encoded = try metadata.encodeOutboundValue(allocator, entry.key, entry.value);
    encoded_values.append(allocator, encoded) catch |err| {
        encoded.deinit(allocator);
        return err;
    };
    try headers.append(allocator, nativeHeader(entry.key, encoded.bytes()));
}

fn testHeaderBuilderAllocations(allocator: std.mem.Allocator) !void {
    var headers: HeaderBuilder = .{};
    defer headers.deinit(allocator);
    for (0..request_header_stack_capacity * 4) |_| {
        try headers.append(allocator, nativeHeader("x-test", "value"));
    }
}

fn testMixedMetadataHeaderCleanup(allocator: std.mem.Allocator) !void {
    var headers: HeaderBuilder = .{};
    defer headers.deinit(allocator);
    var encoded_values: EncodedValueBuilder = .{};
    defer {
        for (encoded_values.items()) |value| value.deinit(allocator);
        encoded_values.deinit(allocator);
    }
    for (0..request_header_stack_capacity) |_| {
        try headers.append(allocator, nativeHeader("x-fixed", "value"));
    }
    const entries = [_]metadata.Entry{
        .{ .key = "x-first", .value = "one" },
        .{ .key = "first-bin", .value = "first" },
        .{ .key = "x-second", .value = "two" },
        .{ .key = "second-bin", .value = "second" },
        .{ .key = "x-third", .value = "three" },
    };
    for (entries) |entry| try appendMetadataHeader(&headers, &encoded_values, allocator, entry);
}

test "client header builder stack path does not allocate" {
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{
        .fail_index = 0,
    });
    var headers: HeaderBuilder = .{};
    defer headers.deinit(failing.allocator());

    for (0..request_header_stack_capacity) |_| {
        try headers.append(failing.allocator(), nativeHeader("x-test", "value"));
    }
    try std.testing.expect(!headers.overflowed);
    try std.testing.expectEqual(request_header_stack_capacity, headers.items().len);
}

test "client header builder overflow preserves order" {
    const values = [_][]const u8{ "0", "1", "2", "3", "4", "5", "6", "7", "8", "9", "10" };
    var headers: HeaderBuilder = .{};
    defer headers.deinit(std.testing.allocator);

    for (values) |value| try headers.append(std.testing.allocator, nativeHeader("x-test", value));
    try std.testing.expect(headers.overflowed);
    for (headers.items(), values) |header, value| {
        try std.testing.expectEqualStrings(value, header.value[0..header.valuelen]);
    }
}

test "client header builder handles every overflow allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testHeaderBuilderAllocations,
        .{},
    );
}

test "client mixed metadata header cleanup handles every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testMixedMetadataHeaderCleanup,
        .{},
    );
}

fn copyMetadata(destination: *metadata.Metadata, source: *const metadata.Metadata) !void {
    for (source.items()) |entry| try destination.append(entry.key, entry.value);
}

fn parseTarget(target: []const u8) !struct { host: []const u8, port: u16 } {
    const separator = std.mem.lastIndexOfScalar(u8, target, ':') orelse return error.InvalidTarget;
    const host = target[0..separator];
    if (host.len == 0 or std.mem.indexOfScalar(u8, host, ':') != null) return error.InvalidTarget;
    const port = std.fmt.parseInt(u16, target[separator + 1 ..], 10) catch return error.InvalidTarget;
    if (port == 0) return error.InvalidTarget;
    return .{ .host = host, .port = port };
}

fn validateTransportOptions(initial_stream_window_size: u32, high: usize, low: usize) !void {
    if (initial_stream_window_size == 0 or initial_stream_window_size > std.math.maxInt(i32)) {
        return error.InvalidInitialStreamWindowSize;
    }
    if (low == 0 or low >= high) return error.InvalidWriteWatermarks;
}

fn validateReconnectOptions(options: ReconnectOptions) !void {
    if (options.initial_backoff_ns == 0 or
        options.max_backoff_ns < options.initial_backoff_ns or
        options.multiplier_millis < 1000 or options.jitter_percent > 100)
    {
        return error.InvalidReconnectOptions;
    }
}

fn canFlushWrites(queued: usize, high: usize) bool {
    return queued < high;
}

fn canFlushWritesWithPending(queued: usize, pending: usize, high: usize) bool {
    return queued < high and pending < high - queued;
}

fn addQueuedWriteBytes(queued: usize, length: usize) !usize {
    return std.math.add(usize, queued, length) catch error.WriteQueueSizeOverflow;
}

fn completeQueuedWrite(queued: usize, length: usize) usize {
    std.debug.assert(length <= queued);
    return queued - length;
}

fn canReturnDeferredStreamCredit(receive_paused: bool, transport_closed: bool) bool {
    return !receive_paused and !transport_closed;
}

fn isValidMethodPath(path: []const u8) bool {
    if (path.len < 4 or path[0] != '/') return false;
    const separator = std.mem.indexOfScalarPos(u8, path, 1, '/') orelse return false;
    return separator > 1 and separator + 1 < path.len and std.mem.indexOfScalarPos(u8, path, separator + 1, '/') == null;
}

fn isRequestMetadata(name: []const u8) bool {
    return metadata.isApplicationKey(name) and !isReservedRequestHeader(name);
}

fn isResponseMetadata(name: []const u8) bool {
    return metadata.isApplicationKey(name) and !isReservedResponseHeader(name);
}

fn isReservedRequestHeader(name: []const u8) bool {
    const protocol_headers = [_][]const u8{ "content-type", "te", "user-agent" };
    for (protocol_headers) |header| if (std.mem.eql(u8, name, header)) return true;
    return std.mem.startsWith(u8, name, "grpc-");
}

fn isReservedResponseHeader(name: []const u8) bool {
    if (std.mem.eql(u8, name, "content-type")) return true;
    return std.mem.startsWith(u8, name, "grpc-");
}

fn isMalformedResponseMetadataName(name: []const u8) bool {
    if (name.len == 0 or name[0] == ':' or isReservedResponseHeader(name)) return false;
    return !metadata.isValidKey(name);
}

fn retainedMetadataFieldSize(name: []const u8, value: []const u8) !usize {
    if (!metadata.isBinaryKey(name)) {
        if (!metadata.isValidAsciiValue(value)) return 0;
        const field_size = try std.math.add(usize, name.len, value.len);
        return std.math.add(usize, field_size, 32);
    }

    var total: usize = 0;
    var values = std.mem.splitScalar(u8, value, ',');
    while (values.next()) |encoded| {
        const decoder = if (std.mem.endsWith(u8, encoded, "="))
            std.base64.standard.Decoder
        else
            std.base64.standard_no_pad.Decoder;
        const decoded_length = try decoder.calcSizeForSlice(encoded);
        const entry_size = try std.math.add(usize, name.len, decoded_length);
        const charged_size = try std.math.add(usize, entry_size, 32);
        total = try std.math.add(usize, total, charged_size);
    }
    return total;
}

fn preflightRetainedMetadata(
    total: *usize,
    name: []const u8,
    value: []const u8,
    limit: u32,
) !void {
    const charged_size = retainedMetadataFieldSize(name, value) catch |err| switch (err) {
        error.Overflow => return error.ResponseHeadersTooLarge,
        else => return err,
    };
    const maximum: usize = limit;
    if (charged_size > maximum or total.* > maximum - charged_size) {
        return error.ResponseHeadersTooLarge;
    }
    total.* += charged_size;
}

fn processResponseMetadata(operation: *Operation, name: []const u8, value: []const u8) !void {
    if (isResponseMetadata(name)) {
        preflightRetainedMetadata(
            &operation.retained_response_metadata_size,
            name,
            value,
            operation.impl.max_response_header_list_size,
        ) catch |err| switch (err) {
            error.ResponseHeadersTooLarge => return error.ResponseHeadersTooLarge,
            else => {
                operation.response_metadata_invalid = true;
                return;
            },
        };
        _ = operation.block_metadata.appendDecoded(name, value) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => {
                operation.response_metadata_invalid = true;
                return;
            },
        };
    } else if (isMalformedResponseMetadataName(name)) {
        operation.response_metadata_invalid = true;
    }
}

fn processClientResponseMetadata(
    client_stream: *ClientStreamState,
    name: []const u8,
    value: []const u8,
) !void {
    if (isResponseMetadata(name)) {
        preflightRetainedMetadata(
            &client_stream.retained_response_metadata_size,
            name,
            value,
            client_stream.impl.?.max_response_header_list_size,
        ) catch |err| switch (err) {
            error.ResponseHeadersTooLarge => return error.ResponseHeadersTooLarge,
            else => {
                client_stream.response_metadata_invalid = true;
                return;
            },
        };
        _ = client_stream.block_metadata.appendDecoded(name, value) catch |err| switch (err) {
            error.OutOfMemory => return error.OutOfMemory,
            else => {
                client_stream.response_metadata_invalid = true;
                return;
            },
        };
    } else if (isMalformedResponseMetadataName(name)) {
        client_stream.response_metadata_invalid = true;
    }
}

fn finishClientHeaderBlock(client_stream: *ClientStreamState, end_stream: bool) !bool {
    const trailers_only = client_stream.block_kind == .response and
        client_stream.block_grpc_status != null;
    if (trailers_only and !end_stream) {
        client_stream.block_kind = .none;
        return false;
    }
    const initial = client_stream.block_kind == .response and !trailers_only;
    const destination = if (initial)
        &client_stream.initial_metadata
    else
        &client_stream.trailing_metadata;
    try copyMetadata(destination, &client_stream.block_metadata);
    if (client_stream.block_grpc_status) |code| client_stream.grpc_status = code;
    if (client_stream.block_grpc_message) |value| {
        if (client_stream.grpc_message) |old| client_stream.allocator.free(old);
        client_stream.grpc_message = value;
        client_stream.block_grpc_message = null;
    }
    client_stream.block_kind = .none;
    if (initial and !client_stream.headers_called) {
        client_stream.headers_called = true;
        invokeHeadersCallback(client_stream);
    }
    return true;
}

fn failClientStreamOnLoop(client_stream: *ClientStreamState, failure: status.Status) bool {
    setForcedStreamStatus(client_stream, failure);
    if (client_stream.rst_submitted or client_stream.transport_closed) return true;
    if (c.nghttp2_submit_rst_stream(
        client_stream.impl.?.session,
        c.NGHTTP2_FLAG_NONE,
        client_stream.stream_id,
        c.NGHTTP2_CANCEL,
    ) != 0) return false;
    client_stream.rst_submitted = true;
    return true;
}

fn goAwayRejectsStream(stream_id: i32, last_stream_id: i32) bool {
    return stream_id < 0 or stream_id > last_stream_id;
}

fn terminalizeStreamsForGoAway(impl: *Impl, last_stream_id: i32) bool {
    var active = impl.streams.valueIterator();
    while (active.next()) |client_stream_ptr| {
        const client_stream = client_stream_ptr.*;
        if (!goAwayRejectsStream(client_stream.stream_id, last_stream_id)) continue;
        setForcedStreamStatus(client_stream, .init(.unavailable, "connection received GOAWAY"));
        terminalizeStream(client_stream, client_stream.forced_status.?);
        if (!client_stream.rst_submitted) {
            if (c.nghttp2_submit_rst_stream(
                impl.session,
                c.NGHTTP2_FLAG_NONE,
                client_stream.stream_id,
                c.NGHTTP2_CANCEL,
            ) != 0) return false;
            client_stream.rst_submitted = true;
        }
    }
    while (true) {
        var pending: ?*ClientStreamState = null;
        impl.mutex.lockUncancelable(syncIo());
        var states = impl.stream_states.keyIterator();
        while (states.next()) |client_stream_ptr| {
            if (client_stream_ptr.*.stream_id < 0) {
                pending = client_stream_ptr.*;
                break;
            }
        }
        impl.mutex.unlock(syncIo());
        const client_stream = pending orelse break;
        terminalizeStream(client_stream, .init(.unavailable, "connection received GOAWAY"));
        releaseStreamLoopOwnership(client_stream);
    }
    return true;
}

fn httpStatusCode(http_status: u16) status.Code {
    return switch (http_status) {
        400 => .internal,
        401 => .unauthenticated,
        403 => .permission_denied,
        404 => .unimplemented,
        429, 502, 503, 504 => .unavailable,
        else => .unknown,
    };
}

fn streamErrorStatus(error_code: u32) status.Status {
    return switch (error_code) {
        c.NGHTTP2_CANCEL => .init(.cancelled, "stream cancelled"),
        c.NGHTTP2_ENHANCE_YOUR_CALM => .init(.resource_exhausted, "stream rejected by peer"),
        else => .init(.unavailable, "stream closed"),
    };
}

fn wireMessageLimit(max_message_size: usize) usize {
    const overhead = std.math.add(usize, max_message_size / 8, 1024) catch return std.math.maxInt(usize);
    const total_overhead = std.math.add(usize, overhead, frame.header_size) catch return std.math.maxInt(usize);
    return std.math.add(usize, max_message_size, total_overhead) catch std.math.maxInt(usize);
}

const StreamTestCallbacks = struct {
    fn onMessage(
        context: ?*anyopaque,
        _: stream.ClientStream,
        payload: []const u8,
        _: Compression,
    ) stream.ReceiveAction {
        const count: *usize = @ptrCast(@alignCast(context.?));
        count.* += payload.len;
        return .continue_receiving;
    }

    fn onTerminal(
        _: ?*anyopaque,
        _: stream.ClientStream,
        _: status.Status,
        _: *const metadata.Metadata,
    ) void {}
};

fn initTestImpl(impl: *Impl, serialized_allocator: *SerializedAllocator, host: [:0]u8) void {
    serialized_allocator.* = .init(std.testing.allocator);
    impl.* = .{
        .backing_allocator = std.testing.allocator,
        .serialized_allocator = serialized_allocator,
        .allocator = undefined,
        .host = host,
        .port = 1,
        .runtime = null,
        .literal_address = std.Io.net.IpAddress.parseIp4("127.0.0.1", 1) catch unreachable,
        .authority = &.{},
        .user_agent = &.{},
        .initial_stream_window_size = 64 * 1024,
        .max_response_header_list_size = 64 * 1024,
        .write_high_watermark_bytes = 1024 * 1024,
        .write_low_watermark_bytes = 512 * 1024,
        .reconnect_options = null,
        .reconnect_backoff_ns = 0,
        .reconnect_random_state = 1,
        .logger = .{},
    };
    impl.allocator = serialized_allocator.allocator();
}

test "channel rejects a zero response header list size" {
    try std.testing.expectError(error.InvalidMaxResponseHeaderListSize, Channel.init(
        std.testing.allocator,
        "invalid",
        .{ .max_response_header_list_size = 0 },
    ));
}

test "client stream open validates its synchronous inputs" {
    var channel: Channel = undefined;
    var received: usize = 0;
    const callbacks: stream.ClientCallbacks = .{
        .context = &received,
        .on_message = StreamTestCallbacks.onMessage,
        .on_terminal = StreamTestCallbacks.onTerminal,
    };
    try std.testing.expectError(
        error.InvalidMethodPath,
        channel.openStream("invalid", .{}, callbacks),
    );
    try std.testing.expectError(
        error.InvalidMaxMessageSize,
        channel.openStream(
            "/test.Stream/Duplex",
            .{ .limits = .{ .max_message_size = 0 } },
            callbacks,
        ),
    );
}

test "flush watermarks permit one chunk of overshoot and resume below low" {
    const high = 10;
    const low = 4;
    var queued: usize = 9;
    try std.testing.expect(canFlushWrites(queued, high));

    const chunk = 5;
    queued = try addQueuedWriteBytes(queued, chunk);
    try std.testing.expectEqual(@as(usize, 14), queued);
    try std.testing.expect(queued - high <= chunk);
    try std.testing.expect(!canFlushWrites(queued, high));

    queued = completeQueuedWrite(queued, chunk);
    try std.testing.expect(!(queued < low));
    queued = completeQueuedWrite(queued, 6);
    try std.testing.expect(queued < low);
    try std.testing.expectError(error.WriteQueueSizeOverflow, addQueuedWriteBytes(std.math.maxInt(usize), 1));
}

test "client write request pool reuses descriptors without allocation" {
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    var host = [_:0]u8{'x'};
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    const owner_allocator = impl.allocator;
    defer {
        impl.allocator = owner_allocator;
        drainWriteRequestPool(&impl);
        impl.writes.deinit(impl.allocator);
    }

    const first = try acquireWriteRequest(&impl, &.{});
    releaseWriteRequest(&impl, first);

    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{ .fail_index = 0 });
    impl.allocator = failing.allocator();
    const reused = try acquireWriteRequest(&impl, &.{});
    try std.testing.expect(reused == first);
    try std.testing.expectEqual(@as(usize, 0), impl.write_request_pool_count);
    releaseWriteRequest(&impl, reused);
    try std.testing.expectEqual(@as(usize, 1), impl.write_request_pool_count);
}

test "client write request pool is LIFO without duplicates" {
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    var host = [_:0]u8{'x'};
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer {
        drainWriteRequestPool(&impl);
        impl.writes.deinit(impl.allocator);
    }

    const first = try acquireWriteRequest(&impl, &.{});
    const second = try acquireWriteRequest(&impl, &.{});
    const third = try acquireWriteRequest(&impl, &.{});
    releaseWriteRequest(&impl, first);
    releaseWriteRequest(&impl, second);
    releaseWriteRequest(&impl, third);

    const reused_third = try acquireWriteRequest(&impl, &.{});
    const reused_second = try acquireWriteRequest(&impl, &.{});
    const reused_first = try acquireWriteRequest(&impl, &.{});
    try std.testing.expect(reused_third == third);
    try std.testing.expect(reused_second == second);
    try std.testing.expect(reused_first == first);
    try std.testing.expect(reused_third != reused_second);
    try std.testing.expect(reused_second != reused_first);
    releaseWriteRequest(&impl, reused_third);
    releaseWriteRequest(&impl, reused_second);
    releaseWriteRequest(&impl, reused_first);

    drainWriteRequestPool(&impl);
    try std.testing.expect(impl.write_request_pool_head == null);
    try std.testing.expectEqual(@as(usize, 0), impl.write_request_pool_count);
}

test "client write queue setup failure returns descriptor to owner pool" {
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    var host = [_:0]u8{'x'};
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    const owner_allocator = impl.allocator;
    defer {
        impl.allocator = owner_allocator;
        drainWriteRequestPool(&impl);
        impl.writes.deinit(impl.allocator);
    }

    const pooled = try acquireWriteRequest(&impl, &.{});
    releaseWriteRequest(&impl, pooled);
    const bytes = try std.testing.allocator.dupe(u8, "owned");
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{ .fail_index = 0 });
    impl.allocator = failing.allocator();
    try std.testing.expectError(error.OutOfMemory, queueOwnedSocketWrite(&impl, bytes));
    try std.testing.expectEqual(@as(usize, 1), impl.write_request_pool_count);
    try std.testing.expect(impl.write_request_pool_head == pooled);
    try std.testing.expectEqual(@as(usize, 0), impl.writes.count());
    try std.testing.expectEqual(@as(usize, 0), impl.queued_write_bytes);
    try std.testing.expectEqual(@as(usize, 1), failing.deallocations);
}

test "client write callback and discard each release once" {
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    var host = [_:0]u8{'x'};
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer {
        drainWriteRequestPool(&impl);
        impl.writes.deinit(impl.allocator);
    }

    const callback_bytes = try impl.allocator.dupe(u8, "callback");
    const callback_write = try acquireWriteRequest(&impl, callback_bytes);
    try impl.writes.put(impl.allocator, callback_write, {});
    impl.queued_write_bytes = callback_bytes.len;
    var loop: xev.Loop = undefined;
    _ = onWrite(
        callback_write,
        &loop,
        &callback_write.request.completion,
        undefined,
        .{ .slice = callback_bytes },
        callback_bytes.len,
    );
    try std.testing.expectEqual(@as(usize, 1), impl.write_request_pool_count);

    const discarded_bytes = try impl.allocator.dupe(u8, "discarded");
    const discarded_write = try acquireWriteRequest(&impl, discarded_bytes);
    try impl.writes.put(impl.allocator, discarded_write, {});
    impl.queued_write_bytes = discarded_bytes.len;
    impl.write_queue.push(&discarded_write.request);
    discardQueuedWrites(&impl);
    try std.testing.expectEqual(@as(usize, 1), impl.write_request_pool_count);
    try std.testing.expect(impl.write_request_pool_head == discarded_write);
    try std.testing.expectEqual(@as(usize, 0), impl.writes.count());
    try std.testing.expectEqual(@as(usize, 0), impl.queued_write_bytes);
}

test "cleartext write batch coalesces chunks below target" {
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(std.testing.allocator);

    try std.testing.expectEqual(null, try batch.append(std.testing.allocator, "abc", 8));
    try std.testing.expectEqual(null, try batch.append(std.testing.allocator, "def", 8));
    const owned = (try batch.take(std.testing.allocator)).?;
    defer std.testing.allocator.free(owned);
    try std.testing.expectEqualStrings("abcdef", owned);
}

test "cleartext write batch splits before crossing target" {
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(std.testing.allocator);

    try std.testing.expectEqual(null, try batch.append(std.testing.allocator, "12345", 8));
    const completed = (try batch.append(std.testing.allocator, "6789", 8)).?;
    defer std.testing.allocator.free(completed);
    try std.testing.expectEqualStrings("12345", completed);
    try std.testing.expectEqualStrings("6789", batch.bytes.items);
}

test "cleartext write batch keeps an oversized chunk intact" {
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(std.testing.allocator);

    try std.testing.expectEqual(null, try batch.append(std.testing.allocator, "123456789", 8));
    try std.testing.expect(batch.ready(8));
    const owned = (try batch.take(std.testing.allocator)).?;
    defer std.testing.allocator.free(owned);
    try std.testing.expectEqualStrings("123456789", owned);
}

test "empty cleartext write batch has nothing to submit" {
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(std.testing.allocator);
    try std.testing.expectEqual(null, try batch.take(std.testing.allocator));
}

test "cleartext write batch transfers ownership only once" {
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(std.testing.allocator);

    try std.testing.expectEqual(null, try batch.append(std.testing.allocator, "owned", 8));
    const owned = (try batch.take(std.testing.allocator)).?;
    try std.testing.expectEqual(null, try batch.take(std.testing.allocator));
    std.testing.allocator.free(owned);
}

test "flush watermark includes pending cleartext batch bytes" {
    const high = 10;
    var batch: CleartextWriteBatch = .{};
    defer batch.deinit(std.testing.allocator);

    try std.testing.expect(canFlushWritesWithPending(8, batch.pendingBytes(), high));
    try std.testing.expectEqual(null, try batch.append(std.testing.allocator, "abc", 8));
    try std.testing.expect(!canFlushWritesWithPending(8, batch.pendingBytes(), high));
    try std.testing.expectEqual(@as(usize, 1), 8 + batch.pendingBytes() - high);
}

test "channel transport options reject unsafe windows and watermarks" {
    try std.testing.expectError(
        error.InvalidInitialStreamWindowSize,
        Channel.init(std.testing.allocator, "127.0.0.1:1", .{ .initial_stream_window_size = 0 }),
    );
    try std.testing.expectError(
        error.InvalidWriteWatermarks,
        Channel.init(std.testing.allocator, "127.0.0.1:1", .{
            .write_high_watermark_bytes = 8,
            .write_low_watermark_bytes = 8,
        }),
    );
}

test "reconnect backoff is bounded exponential with deterministic jitter" {
    const options: ReconnectOptions = .{
        .initial_backoff_ns = 100,
        .max_backoff_ns = 1_000,
        .multiplier_millis = 2000,
        .jitter_percent = 20,
    };
    try std.testing.expectEqual(@as(u64, 80), jitterReconnectBackoffNs(options, 100, 0));
    try std.testing.expectEqual(@as(u64, 120), jitterReconnectBackoffNs(options, 100, 40));
    try std.testing.expectEqual(@as(u64, 200), nextReconnectBackoffNs(options, 100));
    try std.testing.expectEqual(@as(u64, 1_000), nextReconnectBackoffNs(options, 800));
    try std.testing.expectEqual(@as(u64, 1_000), jitterReconnectBackoffNs(options, 1_000, 400));
    try std.testing.expectError(error.InvalidReconnectOptions, validateReconnectOptions(.{
        .initial_backoff_ns = 2,
        .max_backoff_ns = 1,
    }));
}

test "allow_initial_offline queues unary until the server becomes available" {
    const server = @import("server.zig");
    const service = @import("service.zig");
    const Handler = struct {
        calls: std.atomic.Value(usize) = .init(0),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            _ = self.calls.fetchAdd(1, .monotonic);
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Worker = struct {
        channel: *Channel,
        done: std.atomic.Value(bool) = .init(false),
        code: status.Code = .unknown,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Reconnect/InitialOffline",
                "queued",
                .{ .timeout_ns = 5 * std.time.ns_per_s },
            ) catch {
                self.done.store(true, .release);
                return;
            };
            defer result.deinit();
            self.code = result.status.code;
            self.done.store(true, .release);
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
    var channel = try Channel.init(std.testing.allocator, target, .{
        .reconnect = .{
            .allow_initial_offline = true,
            .initial_backoff_ns = std.time.ns_per_s,
            .max_backoff_ns = std.time.ns_per_s,
            .jitter_percent = 0,
        },
    });
    defer channel.deinit();
    // Let the initial socket close before starting the server loop. TSan cannot
    // distinguish an io_uring fd reuse from the socket it replaced.
    try std.testing.expect(waitForTestFlag(
        &channel.impl.test_observer.reconnect_scheduled,
        5 * std.time.ns_per_s,
    ));

    var worker = Worker{ .channel = &channel };
    const worker_thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});
    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{ .port = port });
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Reconnect/InitialOffline",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();
    const completed = waitForTestFlag(&worker.done, 5 * std.time.ns_per_s);
    if (!completed) channel.shutdown();
    worker_thread.join();

    try std.testing.expect(completed);
    try std.testing.expectEqual(status.Code.ok, worker.code);
    try std.testing.expectEqual(@as(usize, 1), handler.calls.load(.acquire));
    channel.shutdown();
    channel.wait();
    try std.testing.expectEqual(@as(u32, 0), channel.impl.reconnect_attempt);
}

test "ordinary connection loss reconnects without replaying a submitted unary" {
    const server = @import("server.zig");
    const service = @import("service.zig");
    const Handler = struct {
        calls: std.atomic.Value(usize) = .init(0),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            _ = self.calls.fetchAdd(1, .monotonic);
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Worker = struct {
        channel: *Channel,
        done: std.atomic.Value(bool) = .init(false),
        code: status.Code = .unknown,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Reconnect/NoReplay",
                "first",
                .{ .timeout_ns = 5 * std.time.ns_per_s },
            ) catch {
                self.done.store(true, .release);
                return;
            };
            defer result.deinit();
            self.code = result.status.code;
            self.done.store(true, .release);
        }
    };

    var address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var listener = try address.listen(std.testing.io, .{});
    var listener_open = true;
    defer if (listener_open) listener.deinit(std.testing.io);
    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(
        listener.socket.handle,
        @ptrCast(&local_address),
        &address_length,
    )) != .SUCCESS) return error.AddressQueryFailed;
    const port = std.mem.bigToNative(u16, local_address.port);
    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{port});

    const InitWorker = struct {
        target: []const u8,
        channel: ?Channel = null,

        fn run(self: *@This()) void {
            self.channel = Channel.init(std.testing.allocator, self.target, .{
                .reconnect = .{
                    .initial_backoff_ns = 10 * std.time.ns_per_ms,
                    .max_backoff_ns = 10 * std.time.ns_per_ms,
                    .jitter_percent = 0,
                },
            }) catch null;
        }
    };
    var init_worker = InitWorker{ .target = target };
    const init_thread = try std.Thread.spawn(.{}, InitWorker.run, .{&init_worker});
    var peer = try listener.accept(std.testing.io);
    init_thread.join();
    var channel = init_worker.channel orelse return error.ChannelInitFailed;
    defer channel.deinit();

    var worker = Worker{ .channel = &channel };
    const worker_thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});
    try std.testing.expect(waitForTestFlag(
        &channel.impl.test_observer.operation_submitted,
        5 * std.time.ns_per_s,
    ));
    peer.close(std.testing.io);
    const first_completed = waitForTestFlag(&worker.done, 5 * std.time.ns_per_s);
    if (!first_completed) channel.shutdown();
    worker_thread.join();
    try std.testing.expect(first_completed);
    try std.testing.expectEqual(status.Code.unavailable, worker.code);

    listener.deinit(std.testing.io);
    listener_open = false;
    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{ .port = port });
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Reconnect/NoReplay",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var second = try channel.callUnary(
        std.testing.allocator,
        "/test.Reconnect/NoReplay",
        "second",
        .{ .timeout_ns = 5 * std.time.ns_per_s },
    );
    defer second.deinit();
    try std.testing.expect(second.status.isOk());
    try std.testing.expectEqualStrings("second", second.payload);
    try std.testing.expectEqual(@as(usize, 1), handler.calls.load(.acquire));
}

test "queued unary deadline expires during reconnect backoff" {
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
    var channel = try Channel.init(std.testing.allocator, target, .{
        .reconnect = .{
            .allow_initial_offline = true,
            .initial_backoff_ns = std.time.ns_per_s,
            .max_backoff_ns = std.time.ns_per_s,
            .jitter_percent = 0,
        },
    });
    defer channel.deinit();

    var result = try channel.callUnary(
        std.testing.allocator,
        "/test.Reconnect/Deadline",
        "expires",
        .{ .timeout_ns = 10 * std.time.ns_per_ms },
    );
    defer result.deinit();
    try std.testing.expectEqual(status.Code.deadline_exceeded, result.status.code);
}

test "channel shutdown cancels reconnect backoff and drains" {
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
    var channel = try Channel.init(std.testing.allocator, target, .{
        .reconnect = .{
            .allow_initial_offline = true,
            .initial_backoff_ns = std.time.ns_per_hour,
            .max_backoff_ns = std.time.ns_per_hour,
            .jitter_percent = 0,
        },
    });
    defer channel.deinit();

    channel.shutdown();
    const first_waiter = try std.Thread.spawn(.{}, Channel.wait, .{&channel});
    const second_waiter = try std.Thread.spawn(.{}, Channel.wait, .{&channel});
    first_waiter.join();
    second_waiter.join();
    try std.testing.expectEqual(State.stopped, channel.impl.state);
    try std.testing.expectEqual(@as(usize, 0), channel.impl.deadline_heap.items.len);
    try std.testing.expect(channel.impl.reconnect_heap_index == null);
}

test "client stream handle outlives channel teardown" {
    const server = @import("server.zig");
    const TerminalState = struct {
        terminals: usize = 0,

        fn onMessage(
            _: ?*anyopaque,
            _: stream.ClientStream,
            _: []const u8,
            _: Compression,
        ) stream.ReceiveAction {
            return .continue_receiving;
        }

        fn onTerminal(
            context: ?*anyopaque,
            _: stream.ClientStream,
            _: status.Status,
            _: *const metadata.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.terminals += 1;
        }
    };

    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    var terminal_state = TerminalState{};
    var client_stream = try channel.openStream(
        "/test.Stream/OutlivesChannel",
        .{},
        .{
            .context = &terminal_state,
            .on_message = TerminalState.onMessage,
            .on_terminal = TerminalState.onTerminal,
        },
    );

    channel.deinit();
    const client_state: *ClientStreamState = @ptrCast(@alignCast(client_stream.context));
    try std.testing.expect(client_state.impl == null);
    try std.testing.expectEqual(@as(usize, 1), terminal_state.terminals);
    try std.testing.expectError(error.StreamClosed, client_stream.send("late", .{}));
    try std.testing.expectError(error.StreamClosed, client_stream.closeSend());
    try std.testing.expectError(error.StreamClosed, client_stream.resumeReceive());
    client_stream.cancel();
    client_stream.deinit();
}

test "client stream cancellation closes command surface" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Cancel",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    client_stream.loop_owned = false;
    var handle = client_stream.handle();
    defer handle.deinit();

    handle.cancel();
    try std.testing.expectError(error.StreamClosed, handle.send("late", .{}));
    try std.testing.expectError(error.StreamClosed, handle.closeSend());
    try std.testing.expectError(error.StreamClosed, handle.resumeReceive());
}

test "client stream requires configured message compression" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    var received: usize = 0;

    const identity_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Identity",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    identity_stream.loop_owned = false;
    var identity_handle = identity_stream.handle();
    defer identity_handle.deinit();
    try std.testing.expectError(
        error.CompressionNotConfigured,
        identity_handle.send("compressed", .{ .compression = .gzip }),
    );

    const gzip_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Gzip",
        .{ .send_compression = .gzip },
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    gzip_stream.loop_owned = false;
    var gzip_handle = gzip_stream.handle();
    defer gzip_handle.deinit();
    try gzip_handle.send("compressed", .{ .compression = .gzip });
}

test "GOAWAY rejects only unaccepted client stream IDs" {
    try std.testing.expect(goAwayRejectsStream(-1, 3));
    try std.testing.expect(!goAwayRejectsStream(1, 3));
    try std.testing.expect(!goAwayRejectsStream(3, 3));
    try std.testing.expect(goAwayRejectsStream(5, 3));
}

test "initial grpc-status requires trailers-only END_STREAM" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Duplex",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    client_stream.loop_owned = false;
    var handle = client_stream.handle();
    defer handle.deinit();

    client_stream.resetHeaderBlock(.response);
    try client_stream.block_metadata.append("x-invalid", "present");
    client_stream.block_grpc_status = @intFromEnum(status.Code.ok);
    try std.testing.expect(!try finishClientHeaderBlock(client_stream, false));
    try std.testing.expectEqual(@as(usize, 0), client_stream.initial_metadata.items().len);
    try std.testing.expectEqual(@as(usize, 0), client_stream.trailing_metadata.items().len);

    client_stream.resetHeaderBlock(.response);
    try client_stream.block_metadata.append("x-trailers-only", "present");
    client_stream.block_grpc_status = @intFromEnum(status.Code.ok);
    try std.testing.expect(try finishClientHeaderBlock(client_stream, true));
    try std.testing.expectEqualStrings(
        "present",
        client_stream.trailing_metadata.getFirst("x-trailers-only").?,
    );
}

test "RST error codes map to gRPC status" {
    try std.testing.expectEqual(status.Code.cancelled, streamErrorStatus(c.NGHTTP2_CANCEL).code);
    try std.testing.expectEqual(
        status.Code.resource_exhausted,
        streamErrorStatus(c.NGHTTP2_ENHANCE_YOUR_CALM).code,
    );
    try std.testing.expectEqual(
        status.Code.unavailable,
        streamErrorStatus(c.NGHTTP2_REFUSED_STREAM).code,
    );
    try std.testing.expectEqual(
        status.Code.unavailable,
        streamErrorStatus(c.NGHTTP2_INTERNAL_ERROR).code,
    );
}

test "client stream command state owns bounded outbound frames" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Duplex",
        .{ .limits = .{
            .max_message_size = 16,
            .max_inbound_buffer_size = 21,
            .max_outbound_buffer_size = 21,
        } },
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    client_stream.loop_owned = false;
    var handle = client_stream.handle();
    defer handle.deinit();

    try handle.send("payload", .{});
    try std.testing.expectEqual(@as(usize, frame.header_size + 7), client_stream.outbound_buffered);
    try std.testing.expectError(error.WouldBlock, handle.send("12345678", .{}));
    try std.testing.expect(client_stream.backpressure_requested);
    try handle.closeSend();
    try std.testing.expectError(error.SendClosed, handle.send("later", .{}));
}

test "client stream provider defers and emits EOF only after drain" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Duplex",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    defer client_stream.destroyUnqueued();

    var source: c.nghttp2_data_source = .{ .ptr = client_stream };
    var output: [64]u8 = undefined;
    var data_flags: u32 = 0;
    try std.testing.expectEqual(
        @as(c.nghttp2_ssize, c.NGHTTP2_ERR_DEFERRED),
        readClientStreamData(null, 1, &output, output.len, &data_flags, &source, null),
    );
    try std.testing.expectEqual(@as(u32, 0), data_flags);

    const encoded = try frame.encode(impl.allocator, "request");
    try client_stream.outbound.append(impl.allocator, .{ .frame_bytes = encoded });
    client_stream.outbound_buffered = encoded.len;
    client_stream.send_open = false;
    data_flags = 0;
    try std.testing.expectEqual(
        @as(c.nghttp2_ssize, @intCast(encoded.len)),
        readClientStreamData(null, 1, &output, output.len, &data_flags, &source, null),
    );
    try std.testing.expect(data_flags & c.NGHTTP2_DATA_FLAG_EOF != 0);
    try std.testing.expectEqual(@as(usize, 0), client_stream.outbound_buffered);
}

test "client receive resume retains credit when delivery pauses again" {
    const ReceiveState = struct {
        messages: usize = 0,

        fn onMessage(
            context: ?*anyopaque,
            _: stream.ClientStream,
            payload: []const u8,
            _: Compression,
        ) stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.messages += payload.len;
            return if (self.messages == payload.len) .pause else .continue_receiving;
        }
    };

    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    var receive_state = ReceiveState{};
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Stream/Duplex",
        .{ .limits = .{
            .max_message_size = 16,
            .max_inbound_buffer_size = 42,
            .max_outbound_buffer_size = 21,
        } },
        .{
            .context = &receive_state,
            .on_message = ReceiveState.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    client_stream.loop_owned = false;
    var handle = client_stream.handle();
    defer handle.deinit();

    const first = try frame.encode(impl.allocator, "one");
    defer impl.allocator.free(first);
    const second = try frame.encode(impl.allocator, "two");
    defer impl.allocator.free(second);
    try client_stream.decoder.feed(first);
    try client_stream.decoder.feed(second);
    client_stream.receive_paused = true;
    client_stream.deferred_stream_credit = first.len + second.len;
    try decodeAvailableMessages(client_stream);
    try std.testing.expectEqual(@as(usize, 2), client_stream.inbound.items.len);
    try std.testing.expectEqual(@as(usize, 0), receive_state.messages);

    try deliverInboundMessages(client_stream);
    try std.testing.expect(client_stream.receive_paused);
    try std.testing.expectEqual(@as(usize, 3), receive_state.messages);
    try std.testing.expectEqual(first.len + second.len, client_stream.deferred_stream_credit);
    try std.testing.expect(!canReturnDeferredStreamCredit(
        client_stream.receive_paused,
        client_stream.transport_closed,
    ));

    try deliverInboundMessages(client_stream);
    try std.testing.expect(!client_stream.receive_paused);
    try std.testing.expectEqual(@as(usize, 6), receive_state.messages);
    try std.testing.expectEqual(@as(usize, 0), client_stream.inbound.items.len);
    try std.testing.expect(canReturnDeferredStreamCredit(
        client_stream.receive_paused,
        client_stream.transport_closed,
    ));
    try std.testing.expect(!canReturnDeferredStreamCredit(false, true));
    try client_stream.decoder.finish();
}

fn expectDeadlineHeapConsistent(impl: *const Impl) !void {
    for (impl.deadline_heap.items, 0..) |entry, index| {
        try std.testing.expectEqual(@as(?usize, index), deadlineTargetIndex(entry.target).*);
        if (index != 0) {
            try std.testing.expect(impl.deadline_heap.items[(index - 1) / 2].expires_at_ns <= entry.expires_at_ns);
        }
    }
}

test "client deadline heap orders updates and mixed targets" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.deadline_heap.deinit(impl.allocator);
    const first = try Operation.init(&impl, "/test.Deadline/First", "", .{});
    defer first.deinit();
    const second = try Operation.init(&impl, "/test.Deadline/Second", "", .{});
    defer second.deinit();
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Deadline/Stream",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    defer client_stream.destroyUnqueued();

    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = first }, 40);
    try deadlineHeapInsertOrUpdate(&impl, .{ .tls_handshake = &impl }, 20);
    try deadlineHeapInsertOrUpdate(&impl, .{ .client_stream = client_stream }, 30);
    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = second }, 10);
    try expectDeadlineHeapConsistent(&impl);
    try std.testing.expect(deadlineTargetsEqual(deadlineHeapPeek(&impl).?.target, .{ .operation = second }));

    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = first }, 5);
    try expectDeadlineHeapConsistent(&impl);
    try std.testing.expect(deadlineTargetsEqual(deadlineHeapPeek(&impl).?.target, .{ .operation = first }));

    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = first }, 50);
    try expectDeadlineHeapConsistent(&impl);
    try std.testing.expect(deadlineTargetsEqual(deadlineHeapPeek(&impl).?.target, .{ .operation = second }));

    const expected = [_]u64{ 10, 20, 30, 50 };
    for (expected) |expiry| {
        const entry = deadlineHeapPop(&impl).?;
        try std.testing.expectEqual(expiry, entry.expires_at_ns);
        try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(entry.target).*);
        try expectDeadlineHeapConsistent(&impl);
    }
}

test "client deadline heap removes root middle and last" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.deadline_heap.deinit(impl.allocator);
    var operations: [7]*Operation = undefined;
    var initialized: usize = 0;
    defer for (operations[0..initialized]) |operation| operation.deinit();
    for (&operations, 0..) |*operation, index| {
        operation.* = try Operation.init(&impl, "/test.Deadline/Remove", "", .{});
        initialized += 1;
        try deadlineHeapInsertOrUpdate(&impl, .{ .operation = operation.* }, @intCast((index + 1) * 10));
    }

    const root = impl.deadline_heap.items[0].target;
    try std.testing.expect(deadlineHeapRemove(&impl, root));
    try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(root).*);
    try expectDeadlineHeapConsistent(&impl);
    const middle = impl.deadline_heap.items[impl.deadline_heap.items.len / 2].target;
    try std.testing.expect(deadlineHeapRemove(&impl, middle));
    try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(middle).*);
    try expectDeadlineHeapConsistent(&impl);
    const last = impl.deadline_heap.items[impl.deadline_heap.items.len - 1].target;
    try std.testing.expect(deadlineHeapRemove(&impl, last));
    try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(last).*);
    try expectDeadlineHeapConsistent(&impl);
    while (deadlineHeapPop(&impl)) |entry| {
        try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(entry.target).*);
    }
}

test "waiting operation queue preserves FIFO and middle unlink" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    const first = try Operation.init(&impl, "/test.Waiting/First", "", .{});
    defer first.deinit();
    const middle = try Operation.init(&impl, "/test.Waiting/Middle", "", .{});
    defer middle.deinit();
    const last = try Operation.init(&impl, "/test.Waiting/Last", "", .{});
    defer last.deinit();

    appendWaitingOperation(&impl, first);
    appendWaitingOperation(&impl, middle);
    appendWaitingOperation(&impl, last);
    try std.testing.expect(impl.waiting_operation_head == first);
    try std.testing.expect(impl.waiting_operation_tail == last);
    try std.testing.expect(first.waiting_prev == null);
    try std.testing.expect(first.waiting_next == middle);
    try std.testing.expect(middle.waiting_prev == first);
    try std.testing.expect(middle.waiting_next == last);
    try std.testing.expect(last.waiting_prev == middle);
    try std.testing.expect(last.waiting_next == null);

    try std.testing.expect(removeWaitingOperation(&impl, middle));
    try std.testing.expect(!middle.waiting_queued);
    try std.testing.expect(middle.waiting_prev == null);
    try std.testing.expect(middle.waiting_next == null);
    try std.testing.expect(first.waiting_next == last);
    try std.testing.expect(last.waiting_prev == first);
    try std.testing.expect(impl.waiting_operation_head == first);
    try std.testing.expect(impl.waiting_operation_tail == last);

    try std.testing.expect(popWaitingOperation(&impl) == first);
    try std.testing.expect(impl.waiting_operation_head == last);
    try std.testing.expect(impl.waiting_operation_tail == last);
    try std.testing.expect(last.waiting_prev == null);
    try std.testing.expect(popWaitingOperation(&impl) == last);
    try std.testing.expect(popWaitingOperation(&impl) == null);
    try std.testing.expect(impl.waiting_operation_head == null);
    try std.testing.expect(impl.waiting_operation_tail == null);
}

test "waiting unary deadline removes ownership before completion" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.deadline_heap.deinit(impl.allocator);
    const operation = try Operation.init(&impl, "/test.Deadline/Waiting", "", .{});
    operation.deadline_ns = 100;
    appendWaitingOperation(&impl, operation);
    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = operation }, operation.deadline_ns.?);

    try expireDeadlines(&impl, 100);
    try std.testing.expect(operation.done);
    try std.testing.expect(operation.deadline_expired);
    try std.testing.expectEqual(status.Code.deadline_exceeded, operation.response_code);
    try std.testing.expect(!operation.waiting_queued);
    try std.testing.expect(operation.waiting_prev == null);
    try std.testing.expect(operation.waiting_next == null);
    try std.testing.expectEqual(@as(?usize, null), operation.deadline_heap_index);
    try std.testing.expect(impl.waiting_operation_head == null);
    try std.testing.expect(impl.waiting_operation_tail == null);
    try std.testing.expectEqual(@as(usize, 0), impl.deadline_heap.items.len);
    operation.deinit();
}

test "pre-submit stream deadline removes loop ownership" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.stream_states.deinit(impl.allocator);
    defer impl.deadline_heap.deinit(impl.allocator);
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Deadline/PreSubmit",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    client_stream.deadline_ns = 100;
    try impl.stream_states.put(impl.allocator, client_stream, {});
    try deadlineHeapInsertOrUpdate(&impl, .{ .client_stream = client_stream }, client_stream.deadline_ns.?);

    try expireDeadlines(&impl, 100);
    try std.testing.expect(client_stream.deadline_expired);
    try std.testing.expect(client_stream.terminal);
    try std.testing.expect(!client_stream.loop_owned);
    try std.testing.expect(client_stream.impl == null);
    try std.testing.expectEqual(@as(usize, 0), impl.stream_states.count());
    try std.testing.expectEqual(@as(usize, 0), impl.deadline_heap.items.len);
    var handle = client_stream.handle();
    handle.deinit();
}

test "active unary deadline pops before submitting reset" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.operations.deinit(impl.allocator);
    defer impl.deadline_heap.deinit(impl.allocator);
    try initializeSession(&impl);
    const operation = try Operation.init(&impl, "/test.Deadline/ActiveUnary", "", .{});
    defer {
        c.nghttp2_session_del(impl.session);
        impl.session = null;
        operation.deinit();
    }
    operation.deadline_ns = 100;
    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = operation }, operation.deadline_ns.?);
    try submitOperation(&impl, operation);

    try expireDeadlines(&impl, 100);
    try std.testing.expect(operation.deadline_expired);
    try std.testing.expectEqual(status.Code.deadline_exceeded, operation.response_code);
    try std.testing.expectEqual(@as(?usize, null), operation.deadline_heap_index);
    try std.testing.expectEqual(@as(usize, 0), impl.deadline_heap.items.len);
    _ = impl.operations.remove(operation.stream_id);
}

test "active stream deadline pops before submitting reset" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.stream_states.deinit(impl.allocator);
    defer impl.streams.deinit(impl.allocator);
    defer impl.deadline_heap.deinit(impl.allocator);
    try initializeSession(&impl);
    defer if (impl.session) |session| c.nghttp2_session_del(session);
    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Deadline/ActiveStream",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    client_stream.deadline_ns = 100;
    try impl.stream_states.put(impl.allocator, client_stream, {});
    try deadlineHeapInsertOrUpdate(&impl, .{ .client_stream = client_stream }, client_stream.deadline_ns.?);
    try submitClientStream(&impl, client_stream);

    try expireDeadlines(&impl, 100);
    try std.testing.expect(client_stream.deadline_expired);
    try std.testing.expect(client_stream.rst_submitted);
    try std.testing.expectEqual(status.Code.deadline_exceeded, client_stream.forced_status.?.code);
    try std.testing.expectEqual(@as(?usize, null), client_stream.deadline_heap_index);
    try std.testing.expectEqual(@as(usize, 0), impl.deadline_heap.items.len);
    _ = impl.streams.remove(client_stream.stream_id);
    client_stream.transport_closed = true;
    terminalizeStream(client_stream, client_stream.forced_status.?);
    releaseStreamLoopOwnership(client_stream);
    c.nghttp2_session_del(impl.session);
    impl.session = null;
    var handle = client_stream.handle();
    handle.deinit();
}

test "deadline expiration pops only an expired root among many waiting calls" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    defer impl.deadline_heap.deinit(impl.allocator);
    const expired = try Operation.init(&impl, "/test.Deadline/Expired", "", .{});
    expired.deadline_ns = 10;
    appendWaitingOperation(&impl, expired);
    try deadlineHeapInsertOrUpdate(&impl, .{ .operation = expired }, expired.deadline_ns.?);
    var future: [256]*Operation = undefined;
    var initialized: usize = 0;
    defer for (future[0..initialized]) |operation| operation.deinit();
    for (&future) |*operation| {
        operation.* = try Operation.init(&impl, "/test.Deadline/Future", "", .{});
        initialized += 1;
        operation.*.deadline_ns = 1000;
        appendWaitingOperation(&impl, operation.*);
        try deadlineHeapInsertOrUpdate(&impl, .{ .operation = operation.* }, operation.*.deadline_ns.?);
    }

    try expireDeadlines(&impl, 10);
    try std.testing.expect(expired.done);
    try std.testing.expectEqual(future.len, impl.deadline_heap.items.len);
    for (&future) |operation| {
        try std.testing.expect(operation.deadline_heap_index != null);
        try std.testing.expect(operation.waiting_queued);
    }
    try std.testing.expect(impl.waiting_operation_head == future[0]);
    try std.testing.expect(impl.waiting_operation_tail == future[future.len - 1]);
    expired.deinit();
    while (popWaitingOperation(&impl)) |operation| {
        removeOperationDeadline(&impl, operation);
    }
    try expectDeadlineHeapConsistent(&impl);
}

test "deadline timer delay rounds up and handles boundaries" {
    try std.testing.expectEqual(@as(?u64, null), deadlineDelayMs(100, 100));
    try std.testing.expectEqual(@as(?u64, null), deadlineDelayMs(99, 100));
    try std.testing.expectEqual(@as(?u64, 1), deadlineDelayMs(101, 100));
    try std.testing.expectEqual(@as(?u64, 1), deadlineDelayMs(std.time.ns_per_ms, 0));
    try std.testing.expectEqual(@as(?u64, 2), deadlineDelayMs(std.time.ns_per_ms + 1, 0));
    try std.testing.expectEqual(
        @as(?u64, std.math.divCeil(u64, std.math.maxInt(u64), std.time.ns_per_ms) catch unreachable),
        deadlineDelayMs(std.math.maxInt(u64), 0),
    );
}

test "serialized allocator forwards every vtable operation" {
    const OperationKind = enum { none, alloc, resize, remap, free };
    const Probe = struct {
        storage: [64]u8 align(16) = undefined,
        operation: OperationKind = .none,
        len: usize = 0,
        memory_address: usize = 0,
        alignment: std.mem.Alignment = .@"1",
        new_len: usize = 0,
        ret_addr: usize = 0,

        fn allocator(self: *@This()) std.mem.Allocator {
            return .{
                .ptr = self,
                .vtable = &.{
                    .alloc = alloc,
                    .resize = resize,
                    .remap = remap,
                    .free = free,
                },
            };
        }

        fn alloc(context: *anyopaque, len: usize, alignment: std.mem.Alignment, ret_addr: usize) ?[*]u8 {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.operation = .alloc;
            self.len = len;
            self.alignment = alignment;
            self.ret_addr = ret_addr;
            return self.storage[0..].ptr;
        }

        fn resize(
            context: *anyopaque,
            memory: []u8,
            alignment: std.mem.Alignment,
            new_len: usize,
            ret_addr: usize,
        ) bool {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.operation = .resize;
            self.memory_address = @intFromPtr(memory.ptr);
            self.len = memory.len;
            self.alignment = alignment;
            self.new_len = new_len;
            self.ret_addr = ret_addr;
            return true;
        }

        fn remap(
            context: *anyopaque,
            memory: []u8,
            alignment: std.mem.Alignment,
            new_len: usize,
            ret_addr: usize,
        ) ?[*]u8 {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.operation = .remap;
            self.memory_address = @intFromPtr(memory.ptr);
            self.len = memory.len;
            self.alignment = alignment;
            self.new_len = new_len;
            self.ret_addr = ret_addr;
            return self.storage[16..].ptr;
        }

        fn free(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, ret_addr: usize) void {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.operation = .free;
            self.memory_address = @intFromPtr(memory.ptr);
            self.len = memory.len;
            self.alignment = alignment;
            self.ret_addr = ret_addr;
        }
    };

    var probe = Probe{};
    var serialized = SerializedAllocator.init(probe.allocator());
    const allocator = serialized.allocator();
    const memory = probe.storage[0..8];

    const allocated = allocator.rawAlloc(8, .@"16", 11).?;
    try std.testing.expectEqual(@intFromPtr(probe.storage[0..].ptr), @intFromPtr(allocated));
    try std.testing.expectEqual(OperationKind.alloc, probe.operation);
    try std.testing.expectEqual(@as(usize, 8), probe.len);
    try std.testing.expectEqual(std.mem.Alignment.@"16", probe.alignment);
    try std.testing.expectEqual(@as(usize, 11), probe.ret_addr);

    try std.testing.expect(allocator.rawResize(memory, .@"8", 12, 22));
    try std.testing.expectEqual(OperationKind.resize, probe.operation);
    try std.testing.expectEqual(@intFromPtr(memory.ptr), probe.memory_address);
    try std.testing.expectEqual(memory.len, probe.len);
    try std.testing.expectEqual(std.mem.Alignment.@"8", probe.alignment);
    try std.testing.expectEqual(@as(usize, 12), probe.new_len);
    try std.testing.expectEqual(@as(usize, 22), probe.ret_addr);

    const remapped = allocator.rawRemap(memory, .@"4", 24, 33).?;
    try std.testing.expectEqual(@intFromPtr(probe.storage[16..].ptr), @intFromPtr(remapped));
    try std.testing.expectEqual(OperationKind.remap, probe.operation);
    try std.testing.expectEqual(@intFromPtr(memory.ptr), probe.memory_address);
    try std.testing.expectEqual(memory.len, probe.len);
    try std.testing.expectEqual(std.mem.Alignment.@"4", probe.alignment);
    try std.testing.expectEqual(@as(usize, 24), probe.new_len);
    try std.testing.expectEqual(@as(usize, 33), probe.ret_addr);

    allocator.rawFree(memory, .@"2", 44);
    try std.testing.expectEqual(OperationKind.free, probe.operation);
    try std.testing.expectEqual(@intFromPtr(memory.ptr), probe.memory_address);
    try std.testing.expectEqual(memory.len, probe.len);
    try std.testing.expectEqual(std.mem.Alignment.@"2", probe.alignment);
    try std.testing.expectEqual(@as(usize, 44), probe.ret_addr);
}

test "response header accounting is exact cumulative and overflow safe" {
    var total: usize = 0;
    try std.testing.expect(accountResponseHeaderField(&total, 20, 48, 100));
    try std.testing.expectEqual(@as(usize, 100), total);
    try std.testing.expect(!accountResponseHeaderField(&total, 0, 0, 100));
    try std.testing.expectEqual(@as(usize, 100), total);

    total = 0;
    try std.testing.expect(accountResponseHeaderField(&total, 1, 1, 100));
    try std.testing.expect(accountResponseHeaderField(&total, 1, 33, 100));
    try std.testing.expectEqual(@as(usize, 100), total);
    try std.testing.expect(!accountResponseHeaderField(
        &total,
        std.math.maxInt(usize),
        1,
        std.math.maxInt(u32),
    ));
}

test "response header block reset preserves RPC accounting" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    const operation = try Operation.init(&impl, "/test.Metadata/Reset", "", .{});
    defer operation.deinit();

    operation.response_header_list_size = 91;
    operation.retained_response_metadata_size = 47;
    operation.resetHeaderBlock(.response);
    operation.resetHeaderBlock(.trailers);
    try std.testing.expectEqual(@as(usize, 91), operation.response_header_list_size);
    try std.testing.expectEqual(@as(usize, 47), operation.retained_response_metadata_size);
}

test "unary response header quota crossing records allocation-free local outcome" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    impl.max_response_header_list_size = 64;
    defer impl.operations.deinit(impl.allocator);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);

    const operation = try Operation.init(&impl, "/test.Metadata/UnaryLimit", "", .{});
    defer operation.deinit();
    try submitOperation(&impl, operation);
    operation.resetHeaderBlock(.response);
    try operation.block_metadata.append("x-before", "discarded");
    var native_frame: c.nghttp2_frame = undefined;
    native_frame.hd.stream_id = operation.stream_id;
    const encoded_unicode_message = "%E2%98%83" ** 4;
    try std.testing.expectEqual(
        @as(c_int, c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE),
        onHeader(
            impl.session,
            &native_frame,
            "grpc-message".ptr,
            "grpc-message".len,
            encoded_unicode_message.ptr,
            encoded_unicode_message.len,
            0,
            &impl,
        ),
    );
    try std.testing.expect(operation.response_headers_too_large);
    try std.testing.expect(operation.response_header_reset_submitted);
    try std.testing.expect(operation.outcome_set);
    try std.testing.expectEqual(status.Code.resource_exhausted, operation.response_code);
    try std.testing.expectEqualStrings(response_headers_too_large_message, operation.response_message);
    try std.testing.expect(!operation.response_message_owned);
    try std.testing.expectEqual(HeaderKind.none, operation.block_kind);
    try std.testing.expectEqual(@as(usize, 0), operation.block_metadata.items().len);
    try std.testing.expect(operation.block_grpc_message == null);

    const rejected_size = operation.response_header_list_size;
    for ([_][2][]const u8{
        .{ "x-after", "hidden" },
        .{ "grpc-status", "0" },
    }) |header| {
        try std.testing.expectEqual(
            @as(c_int, 0),
            onHeader(
                impl.session,
                &native_frame,
                header[0].ptr,
                header[0].len,
                header[1].ptr,
                header[1].len,
                0,
                &impl,
            ),
        );
        try std.testing.expectEqual(rejected_size, operation.response_header_list_size);
        try std.testing.expectEqual(@as(usize, 0), operation.block_metadata.items().len);
        try std.testing.expect(operation.block_grpc_status == null);
    }
    try std.testing.expectEqual(@as(usize, 0), operation.initial_metadata.items().len);
    try std.testing.expectEqual(@as(usize, 0), operation.trailing_metadata.items().len);
    _ = impl.operations.remove(operation.stream_id);
}

test "invalid response headers share the aggregate quota" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    impl.max_response_header_list_size = 64;
    defer impl.operations.deinit(impl.allocator);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);

    const operation = try Operation.init(&impl, "/test.Metadata/InvalidLimit", "", .{});
    defer operation.deinit();
    try submitOperation(&impl, operation);
    operation.resetHeaderBlock(.response);
    var native_frame: c.nghttp2_frame = undefined;
    native_frame.hd.stream_id = operation.stream_id;
    try std.testing.expectEqual(
        @as(c_int, 0),
        onInvalidHeader(
            impl.session,
            &native_frame,
            "x!invalid".ptr,
            "x!invalid".len,
            "value".ptr,
            "value".len,
            0,
            &impl,
        ),
    );
    try std.testing.expectEqual(@as(usize, "x!invalid".len + "value".len + 32), operation.response_header_list_size);
    try std.testing.expectEqual(
        @as(c_int, c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE),
        onInvalidHeader(
            impl.session,
            &native_frame,
            "x".ptr,
            1,
            "".ptr,
            0,
            0,
            &impl,
        ),
    );
    try std.testing.expect(operation.response_headers_too_large);
    try std.testing.expectEqual(status.Code.resource_exhausted, operation.response_code);
    _ = impl.operations.remove(operation.stream_id);
}

test "streaming response header quota terminalizes once and closes sends" {
    const TerminalState = struct {
        count: usize = 0,
        code: status.Code = .unknown,

        fn onMessage(
            _: ?*anyopaque,
            _: stream.ClientStream,
            _: []const u8,
            _: Compression,
        ) stream.ReceiveAction {
            return .continue_receiving;
        }

        fn onTerminal(
            context: ?*anyopaque,
            _: stream.ClientStream,
            final_status: status.Status,
            _: *const metadata.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.count += 1;
            self.code = final_status.code;
        }
    };

    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    impl.max_response_header_list_size = 64;
    defer impl.stream_states.deinit(impl.allocator);
    defer impl.streams.deinit(impl.allocator);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);
    var terminal_state = TerminalState{};
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Metadata/StreamingLimit",
        .{},
        .{
            .context = &terminal_state,
            .on_message = TerminalState.onMessage,
            .on_terminal = TerminalState.onTerminal,
        },
    );
    try impl.stream_states.put(impl.allocator, client_stream, {});
    try submitClientStream(&impl, client_stream);
    client_stream.resetHeaderBlock(.response);
    var native_frame: c.nghttp2_frame = undefined;
    native_frame.hd.stream_id = client_stream.stream_id;
    const large_value = "x" ** 33;
    try std.testing.expectEqual(
        @as(c_int, c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE),
        onHeader(
            impl.session,
            &native_frame,
            "grpc-message".ptr,
            "grpc-message".len,
            large_value.ptr,
            large_value.len,
            0,
            &impl,
        ),
    );
    try std.testing.expect(client_stream.response_headers_too_large);
    try std.testing.expect(client_stream.rst_submitted);
    try std.testing.expectEqual(status.Code.resource_exhausted, client_stream.forced_status.?.code);
    try std.testing.expect(!client_stream.headers_called);
    try std.testing.expect(!client_stream.remote_end_seen);

    const rejected_size = client_stream.response_header_list_size;
    for ([_][2][]const u8{
        .{ "x-after", "hidden" },
        .{ "grpc-status", "0" },
    }) |header| {
        try std.testing.expectEqual(
            @as(c_int, 0),
            onHeader(
                impl.session,
                &native_frame,
                header[0].ptr,
                header[0].len,
                header[1].ptr,
                header[1].len,
                0,
                &impl,
            ),
        );
        try std.testing.expectEqual(rejected_size, client_stream.response_header_list_size);
        try std.testing.expectEqual(@as(usize, 0), client_stream.block_metadata.items().len);
        try std.testing.expect(client_stream.block_grpc_status == null);
    }
    try std.testing.expectEqual(@as(usize, 0), client_stream.initial_metadata.items().len);
    try std.testing.expectEqual(@as(usize, 0), client_stream.trailing_metadata.items().len);

    const stream_id = client_stream.stream_id;
    _ = onStreamClose(impl.session, stream_id, c.NGHTTP2_CANCEL, &impl);
    try std.testing.expectEqual(@as(usize, 1), terminal_state.count);
    try std.testing.expectEqual(status.Code.resource_exhausted, terminal_state.code);
    var handle = client_stream.handle();
    try std.testing.expectError(error.StreamClosed, handle.send("late", .{}));
    try std.testing.expectError(error.StreamClosed, handle.closeSend());
    handle.deinit();
}

test "streaming oversized trailers preserve accepted initial metadata" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    impl.max_response_header_list_size = 160;
    defer impl.stream_states.deinit(impl.allocator);
    defer impl.streams.deinit(impl.allocator);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);

    var received: usize = 0;
    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Metadata/StreamingTrailersLimit",
        .{},
        .{
            .context = &received,
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    try impl.stream_states.put(impl.allocator, client_stream, {});
    try submitClientStream(&impl, client_stream);
    var native_frame: c.nghttp2_frame = undefined;
    native_frame.hd.stream_id = client_stream.stream_id;

    client_stream.resetHeaderBlock(.response);
    for ([_][2][]const u8{
        .{ ":status", "200" },
        .{ "content-type", "application/grpc" },
        .{ "x-a", "v" },
    }) |header| {
        try std.testing.expectEqual(
            @as(c_int, 0),
            onHeader(
                impl.session,
                &native_frame,
                header[0].ptr,
                header[0].len,
                header[1].ptr,
                header[1].len,
                0,
                &impl,
            ),
        );
    }
    try std.testing.expect(try finishClientHeaderBlock(client_stream, false));
    try std.testing.expectEqualStrings("v", client_stream.initial_metadata.getFirst("x-a").?);
    const encoded = try frame.encode(impl.allocator, "before");
    defer impl.allocator.free(encoded);
    try client_stream.decoder.feed(encoded);
    try decodeAvailableMessages(client_stream);
    try std.testing.expectEqual("before".len, received);

    client_stream.resetHeaderBlock(.trailers);
    try std.testing.expectEqual(
        @as(c_int, c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE),
        onHeader(
            impl.session,
            &native_frame,
            "grpc-status".ptr,
            "grpc-status".len,
            "0".ptr,
            1,
            0,
            &impl,
        ),
    );
    try std.testing.expectEqual(status.Code.resource_exhausted, client_stream.forced_status.?.code);
    try std.testing.expectEqualStrings("v", client_stream.initial_metadata.getFirst("x-a").?);
    try std.testing.expectEqual(@as(usize, 0), client_stream.trailing_metadata.items().len);
    try std.testing.expectEqual(HeaderKind.none, client_stream.block_kind);

    const stream_id = client_stream.stream_id;
    _ = onStreamClose(impl.session, stream_id, c.NGHTTP2_CANCEL, &impl);
    var handle = client_stream.handle();
    handle.deinit();
}

test "pending streaming cancellation wins over response header quota" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    impl.max_response_header_list_size = 64;
    impl.stream_wake_notify_pending = true;
    defer impl.stream_states.deinit(impl.allocator);
    defer impl.streams.deinit(impl.allocator);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);

    const client_stream = try ClientStreamState.init(
        &impl,
        "/test.Metadata/CancelBeforeLimit",
        .{},
        .{
            .on_message = StreamTestCallbacks.onMessage,
            .on_terminal = StreamTestCallbacks.onTerminal,
        },
    );
    try impl.stream_states.put(impl.allocator, client_stream, {});
    try submitClientStream(&impl, client_stream);
    client_stream.resetHeaderBlock(.response);

    var handle = client_stream.handle();
    handle.cancel();
    try std.testing.expect(client_stream.cancel_requested);
    try std.testing.expect(client_stream.wake_queued);

    var native_frame: c.nghttp2_frame = undefined;
    native_frame.hd.stream_id = client_stream.stream_id;
    const large_value = "x" ** 33;
    try std.testing.expectEqual(
        @as(c_int, c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE),
        onHeader(
            impl.session,
            &native_frame,
            "grpc-message".ptr,
            "grpc-message".len,
            large_value.ptr,
            large_value.len,
            0,
            &impl,
        ),
    );
    try std.testing.expect(client_stream.response_headers_too_large);
    try std.testing.expect(client_stream.rst_submitted);
    try std.testing.expectEqual(status.Code.cancelled, client_stream.forced_status.?.code);

    _ = onStreamClose(impl.session, client_stream.stream_id, c.NGHTTP2_CANCEL, &impl);
    handle.deinit();
}

test "retained response metadata accounting bounds binary comma amplification" {
    try std.testing.expectEqual(
        @as(usize, "x-ascii".len + "value".len + 32),
        try retainedMetadataFieldSize("x-ascii", "value"),
    );
    try std.testing.expectEqual(
        @as(usize, 2 * ("x-bin".len + 1 + 32)),
        try retainedMetadataFieldSize("x-bin", "YQ==,Yg"),
    );

    var total: usize = 0;
    const one_entry = "very-long-binary-metadata-key-bin".len + 32;
    try preflightRetainedMetadata(
        &total,
        "very-long-binary-metadata-key-bin",
        ",,,",
        @intCast(4 * one_entry),
    );
    try std.testing.expectEqual(4 * one_entry, total);
    try std.testing.expectError(error.ResponseHeadersTooLarge, preflightRetainedMetadata(
        &total,
        "very-long-binary-metadata-key-bin",
        "",
        @intCast(4 * one_entry),
    ));
}

fn transferNghttp2Sessions(source: ?*c.nghttp2_session, destination: ?*c.nghttp2_session) !usize {
    var transferred: usize = 0;
    while (true) {
        var data: [*c]const u8 = null;
        const length = c.nghttp2_session_mem_send2(source, &data);
        if (length < 0) return error.NativeFailure;
        if (length == 0) return transferred;
        const consumed = c.nghttp2_session_mem_recv2(destination, data, @intCast(length));
        if (consumed != length) return error.Http2ConnectionFailed;
        transferred += @intCast(length);
    }
}

fn transferClientPrefaceWithUnboundedPeerSetting(
    source: ?*c.nghttp2_session,
    destination: ?*c.nghttp2_session,
) !void {
    var data: [*c]const u8 = null;
    const preface_length = c.nghttp2_session_mem_send2(source, &data);
    if (preface_length <= 0 or
        c.nghttp2_session_mem_recv2(destination, data, @intCast(preface_length)) != preface_length)
    {
        return error.Http2ConnectionFailed;
    }
    const settings_length = c.nghttp2_session_mem_send2(source, &data);
    if (settings_length < 9) return error.Http2ConnectionFailed;
    const settings = try std.testing.allocator.dupe(u8, data[0..@intCast(settings_length)]);
    defer std.testing.allocator.free(settings);
    var offset: usize = 9;
    while (offset + 6 <= settings.len) : (offset += 6) {
        const id = (@as(u16, settings[offset]) << 8) | settings[offset + 1];
        if (id != c.NGHTTP2_SETTINGS_MAX_HEADER_LIST_SIZE) continue;
        @memset(settings[offset + 2 .. offset + 6], 0xff);
    }
    if (c.nghttp2_session_mem_recv2(destination, settings.ptr, settings.len) != settings.len) {
        return error.Http2ConnectionFailed;
    }
    _ = try transferNghttp2Sessions(source, destination);
}

test "raw nghttp2 peer covers response header boundaries isolation and HPACK reuse" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    impl.max_response_header_list_size = 300;
    defer impl.operations.deinit(impl.allocator);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);

    const PeerState = struct {
        closes: usize = 0,

        fn onStreamClose(
            _: ?*c.nghttp2_session,
            _: i32,
            _: u32,
            user_data: ?*anyopaque,
        ) callconv(.c) c_int {
            const self: *@This() = @ptrCast(@alignCast(user_data.?));
            self.closes += 1;
            return 0;
        }
    };
    var peer_state = PeerState{};
    var callbacks: ?*c.nghttp2_session_callbacks = null;
    try std.testing.expectEqual(@as(c_int, 0), c.nghttp2_session_callbacks_new(&callbacks));
    defer c.nghttp2_session_callbacks_del(callbacks);
    c.nghttp2_session_callbacks_set_on_stream_close_callback(callbacks, PeerState.onStreamClose);
    var peer_options: ?*c.nghttp2_option = null;
    try std.testing.expectEqual(@as(c_int, 0), c.nghttp2_option_new(&peer_options));
    defer c.nghttp2_option_del(peer_options);
    c.nghttp2_option_set_no_http_messaging(peer_options, 1);
    var peer: ?*c.nghttp2_session = null;
    try std.testing.expectEqual(
        @as(c_int, 0),
        c.nghttp2_session_server_new2(&peer, callbacks, &peer_state, peer_options),
    );
    defer c.nghttp2_session_del(peer);
    try std.testing.expectEqual(
        @as(c_int, 0),
        c.nghttp2_submit_settings(peer, c.NGHTTP2_FLAG_NONE, null, 0),
    );

    const exact = try Operation.init(&impl, "/test.Metadata/ExactBoundary", "", .{});
    try submitOperation(&impl, exact);
    const over = try Operation.init(&impl, "/test.Metadata/OverBoundary", "", .{});
    try submitOperation(&impl, over);
    try transferClientPrefaceWithUnboundedPeerSetting(impl.session, peer);
    try std.testing.expectEqual(
        std.math.maxInt(u32),
        c.nghttp2_session_get_remote_settings(peer, c.NGHTTP2_SETTINGS_MAX_HEADER_LIST_SIZE),
    );
    try std.testing.expectEqual(@as(usize, 0), peer_state.closes);

    // These trailers-only responses total exactly 300 and 301 charged bytes.
    const exact_boundary_value = "b" ** 112;
    const over_boundary_value = "b" ** 113;
    const exact_headers = [_]c.nghttp2_nv{
        nativeHeader(":status", "200"),
        nativeHeader("content-type", "application/grpc"),
        nativeHeader("x-boundary", exact_boundary_value),
        nativeHeader("grpc-status", "5"),
    };
    const over_headers = [_]c.nghttp2_nv{
        nativeHeader(":status", "200"),
        nativeHeader("content-type", "application/grpc"),
        nativeHeader("x-boundary", over_boundary_value),
        nativeHeader("grpc-status", "5"),
    };
    try std.testing.expectEqual(
        @as(c_int, 0),
        c.nghttp2_submit_headers(
            peer,
            c.NGHTTP2_FLAG_END_STREAM,
            over.stream_id,
            null,
            &over_headers,
            over_headers.len,
            null,
        ),
    );
    try std.testing.expectEqual(
        @as(c_int, 0),
        c.nghttp2_submit_headers(
            peer,
            c.NGHTTP2_FLAG_END_STREAM,
            exact.stream_id,
            null,
            &exact_headers,
            exact_headers.len,
            null,
        ),
    );
    _ = try transferNghttp2Sessions(peer, impl.session);
    _ = try transferNghttp2Sessions(impl.session, peer);
    try std.testing.expect(over.done);
    try std.testing.expect(over.response_headers_too_large);
    try std.testing.expectEqual(status.Code.resource_exhausted, over.response_code);
    try std.testing.expect(exact.done);
    try std.testing.expectEqual(@as(usize, 300), exact.response_header_list_size);
    try std.testing.expectEqual(status.Code.not_found, exact.response_code);
    try std.testing.expectEqualStrings(
        exact_boundary_value,
        exact.trailing_metadata.getFirst("x-boundary").?,
    );
    try std.testing.expectEqual(@as(usize, 0), impl.operations.count());
    over.deinit();
    exact.deinit();

    const rejected = try Operation.init(&impl, "/test.Metadata/Rejected", "", .{});
    try submitOperation(&impl, rejected);
    _ = try transferNghttp2Sessions(impl.session, peer);

    // The preceding fields total 266 bytes. The large field is completed in a
    // CONTINUATION callback and crosses the configured 300-byte limit there.
    const large_value = "x" ** (20 * 1024);
    const rejected_headers = [_]c.nghttp2_nv{
        nativeHeader(":status", "200"),
        nativeHeader("content-type", "application/grpc"),
        // Repeated values compress to indexed fields but each decoded field is charged.
        nativeHeader("x-repeat", "v"),
        nativeHeader("x-repeat", "v"),
        nativeHeader("x-repeat", "v"),
        nativeHeader("x-repeat", "v"),
        nativeHeader("x-large", large_value),
        // This insertion follows the rejected field. A later stream references it.
        nativeHeader("x-dynamic", "shared"),
        nativeHeader("grpc-status", "8"),
    };
    try std.testing.expectEqual(
        @as(c_int, 0),
        c.nghttp2_submit_headers(
            peer,
            c.NGHTTP2_FLAG_END_STREAM,
            rejected.stream_id,
            null,
            &rejected_headers,
            rejected_headers.len,
            null,
        ),
    );
    _ = try transferNghttp2Sessions(peer, impl.session);
    _ = try transferNghttp2Sessions(impl.session, peer);
    try std.testing.expect(rejected.done);
    try std.testing.expect(rejected.response_headers_too_large);
    try std.testing.expectEqual(status.Code.resource_exhausted, rejected.response_code);
    try std.testing.expectEqualStrings(response_headers_too_large_message, rejected.response_message);
    try std.testing.expectEqual(@as(usize, 0), impl.operations.count());
    rejected.deinit();

    const accepted = try Operation.init(&impl, "/test.Metadata/Accepted", "", .{});
    try submitOperation(&impl, accepted);
    _ = try transferNghttp2Sessions(impl.session, peer);
    const accepted_headers = [_]c.nghttp2_nv{
        nativeHeader(":status", "200"),
        nativeHeader("content-type", "application/grpc"),
        nativeHeader("x-dynamic", "shared"),
        nativeHeader("grpc-status", "5"),
    };
    try std.testing.expectEqual(
        @as(c_int, 0),
        c.nghttp2_submit_headers(
            peer,
            c.NGHTTP2_FLAG_END_STREAM,
            accepted.stream_id,
            null,
            &accepted_headers,
            accepted_headers.len,
            null,
        ),
    );
    _ = try transferNghttp2Sessions(peer, impl.session);
    _ = try transferNghttp2Sessions(impl.session, peer);
    try std.testing.expect(accepted.done);
    try std.testing.expectEqual(status.Code.not_found, accepted.response_code);
    try std.testing.expectEqualStrings("shared", accepted.trailing_metadata.getFirst("x-dynamic").?);
    try std.testing.expectEqual(@as(usize, 0), impl.operations.count());
    accepted.deinit();
}

test "client session advertises the default and reconnect response header list size" {
    const Reader = struct {
        fn advertised(session: ?*c.nghttp2_session) !u32 {
            var data: [*c]const u8 = null;
            const preface_length = c.nghttp2_session_mem_send2(session, &data);
            try std.testing.expect(preface_length > 0);
            try std.testing.expectEqualStrings(
                "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
                data[0..@intCast(preface_length)],
            );
            const settings_length = c.nghttp2_session_mem_send2(session, &data);
            try std.testing.expect(settings_length >= 9 + 12);
            const settings = data[0..@intCast(settings_length)];
            try std.testing.expectEqual(@as(u8, c.NGHTTP2_SETTINGS), settings[3]);
            try std.testing.expectEqual(@as(u8, 0), settings[9]);
            try std.testing.expectEqual(@as(u8, c.NGHTTP2_SETTINGS_INITIAL_WINDOW_SIZE), settings[10]);
            try std.testing.expectEqual(@as(u8, 0), settings[15]);
            try std.testing.expectEqual(@as(u8, c.NGHTTP2_SETTINGS_MAX_HEADER_LIST_SIZE), settings[16]);
            return (@as(u32, settings[17]) << 24) |
                (@as(u32, settings[18]) << 16) |
                (@as(u32, settings[19]) << 8) |
                settings[20];
        }
    };

    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    try initializeSession(&impl);
    defer c.nghttp2_session_del(impl.session);
    try std.testing.expectEqual(@as(u32, 64 * 1024), try Reader.advertised(impl.session));

    c.nghttp2_session_del(impl.session);
    impl.session = null;
    impl.max_response_header_list_size = 123;
    try initializeSession(&impl);
    try std.testing.expectEqual(@as(u32, 123), try Reader.advertised(impl.session));

    c.nghttp2_session_del(impl.session);
    impl.session = null;
    try initializeSession(&impl);
    try std.testing.expectEqual(@as(u32, 123), try Reader.advertised(impl.session));

    defer impl.operations.deinit(impl.allocator);
    const operation = try Operation.init(&impl, "/test.Metadata/ReconnectedLimit", "", .{});
    defer operation.deinit();
    try submitOperation(&impl, operation);
    operation.resetHeaderBlock(.response);
    var native_frame: c.nghttp2_frame = undefined;
    native_frame.hd.stream_id = operation.stream_id;
    const oversized_value = "x" ** 91;
    try std.testing.expectEqual(
        @as(c_int, c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE),
        onHeader(
            impl.session,
            &native_frame,
            "x".ptr,
            1,
            oversized_value.ptr,
            oversized_value.len,
            0,
            &impl,
        ),
    );
    try std.testing.expect(operation.response_headers_too_large);
    try std.testing.expectEqual(status.Code.resource_exhausted, operation.response_code);
    _ = impl.operations.remove(operation.stream_id);
}

test "target parsing" {
    const target = try parseTarget("127.0.0.1:50051");
    try std.testing.expectEqualStrings("127.0.0.1", target.host);
    try std.testing.expectEqual(@as(u16, 50051), target.port);
    try std.testing.expectError(error.InvalidTarget, parseTarget("localhost"));
    try std.testing.expectError(error.InvalidTarget, parseTarget("[::1]:50051"));
}

test "outbound metadata rejects invalid application values before queuing" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);

    try std.testing.expectError(error.InvalidMetadataValue, Operation.init(
        &impl,
        "/test.Echo/Unary",
        "request",
        .{ .metadata = &.{.{ .key = "x-control", .value = "bad\tvalue" }} },
    ));
    try std.testing.expectError(error.InvalidMetadataKey, Operation.init(
        &impl,
        "/test.Echo/Unary",
        "request",
        .{ .metadata = &.{.{ .key = "grpc-future", .value = "value" }} },
    ));

    var binary = try Operation.init(
        &impl,
        "/test.Echo/Unary",
        "request",
        .{ .metadata = &.{.{ .key = "trace-bin", .value = "\x00\xff" }} },
    );
    binary.deinit();
}

test "malformed binary response metadata marks only the operation invalid" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);

    const operation = try Operation.init(&impl, "/test.Echo/Unary", "request", .{});
    defer operation.deinit();
    try processResponseMetadata(operation, "grpc-future", "ignored");
    try processResponseMetadata(operation, "x-control", "bad\tvalue");
    try std.testing.expect(!operation.response_metadata_invalid);
    try std.testing.expectEqual(@as(usize, 0), operation.block_metadata.items().len);
    try processResponseMetadata(operation, "trace-bin", "qw,not base64!");
    try std.testing.expect(operation.response_metadata_invalid);
    try std.testing.expectEqual(@as(usize, 0), operation.block_metadata.items().len);
}

test "response headers with grpc-status are trailers-only metadata" {
    var host = [_:0]u8{'x'};
    var serialized_allocator: SerializedAllocator = undefined;
    var impl: Impl = undefined;
    initTestImpl(&impl, &serialized_allocator, host[0..1 :0]);
    const operation = try Operation.init(&impl, "/test.Echo/Unary", "", .{});
    defer operation.deinit();

    operation.resetHeaderBlock(.response);
    try operation.block_metadata.append("x-trailers-only", "present");
    operation.block_grpc_status = @intFromEnum(status.Code.not_found);
    try std.testing.expect(try operation.finishHeaderBlock(true));

    try std.testing.expectEqual(@as(usize, 0), operation.initial_metadata.items().len);
    try std.testing.expectEqualStrings("present", operation.trailing_metadata.getFirst("x-trailers-only").?);
    try std.testing.expectEqual(@as(?u32, @intFromEnum(status.Code.not_found)), operation.grpc_status);
}

test "channel connection refusal cleans up startup resources" {
    const address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    const socket = try xev.TCP.init(address);
    defer _ = std.posix.system.close(socket.fd);
    try socket.bind(address);

    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(
        socket.fd,
        @ptrCast(&local_address),
        &address_length,
    )) != .SUCCESS) return error.AddressQueryFailed;

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(
        &target_buffer,
        "127.0.0.1:{d}",
        .{std.mem.bigToNative(u16, local_address.port)},
    );
    for (0..4) |_| {
        try std.testing.expectError(
            error.ConnectionFailed,
            Channel.init(std.testing.allocator, target, .{}),
        );
    }
}

test "binary request initial and trailing metadata round trip as raw duplicate values" {
    const server = @import("server.zig");
    const service = @import("service.zig");
    const binary_value = [_]u8{ 0xab, 0xab, 0xab };
    const second_value = [_]u8{0xab};

    const Handler = struct {
        request_matches: std.atomic.Value(bool) = .init(false),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            const entries = context.request_metadata.items();
            self.request_matches.store(entries.len == 3 and
                std.mem.eql(u8, entries[0].key, "x-request") and
                std.mem.eql(u8, entries[0].value, "plain") and
                std.mem.eql(u8, entries[1].key, "x-request-bin") and
                std.mem.eql(u8, entries[1].value, &binary_value) and
                std.mem.eql(u8, entries[2].key, "x-request-bin") and
                std.mem.eql(u8, entries[2].value, &second_value), .release);

            try context.addInitialMetadata("x-initial", "plain");
            try context.addInitialMetadata("x-initial-bin", &binary_value);
            try context.addInitialMetadata("x-initial-bin", &second_value);
            try context.addTrailingMetadata("x-trailing", "plain");
            try context.addTrailingMetadata("x-trailing-bin", &binary_value);
            try context.addTrailingMetadata("x-trailing-bin", &second_value);
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Metadata/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var result = try channel.callUnary(
        std.testing.allocator,
        "/test.Metadata/Unary",
        "payload",
        .{ .metadata = &.{
            .{ .key = "x-request", .value = "plain" },
            .{ .key = "x-request-bin", .value = &binary_value },
            .{ .key = "x-request-bin", .value = &second_value },
        } },
    );
    defer result.deinit();

    try std.testing.expect(result.status.isOk());
    try std.testing.expect(handler.request_matches.load(.acquire));
    const initial = result.initial_metadata.items();
    try std.testing.expectEqual(@as(usize, 3), initial.len);
    try std.testing.expectEqualStrings("plain", initial[0].value);
    try std.testing.expectEqualSlices(u8, &binary_value, initial[1].value);
    try std.testing.expectEqualSlices(u8, &second_value, initial[2].value);
    try std.testing.expectEqualStrings(initial[1].key, initial[2].key);

    const trailing = result.trailing_metadata.items();
    try std.testing.expectEqual(@as(usize, 3), trailing.len);
    try std.testing.expectEqualStrings("plain", trailing[0].value);
    try std.testing.expectEqualSlices(u8, &binary_value, trailing[1].value);
    try std.testing.expectEqualSlices(u8, &second_value, trailing[2].value);
    try std.testing.expectEqualStrings(trailing[1].key, trailing[2].key);
}

test "malformed response metadata fails one call and preserves the channel" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        calls: std.atomic.Value(usize) = .init(0),

        fn appendUnchecked(target: *metadata.Metadata, key: []const u8, value: []const u8) !void {
            const owned_key = try target.allocator.dupe(u8, key);
            errdefer target.allocator.free(owned_key);
            const owned_value = try target.allocator.dupe(u8, value);
            errdefer target.allocator.free(owned_value);
            try target.entries.append(target.allocator, .{ .key = owned_key, .value = owned_value });
        }

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            _ = self.calls.fetchAdd(1, .monotonic);
            if (std.mem.eql(u8, request, "bad-key")) {
                try appendUnchecked(&context.initial_metadata, "x!invalid", "value");
            } else if (std.mem.eql(u8, request, "bad-trailer")) {
                try appendUnchecked(&context.trailing_metadata, "x!invalid", "value");
            } else if (std.mem.eql(u8, request, "bad-ascii")) {
                try appendUnchecked(&context.initial_metadata, "x-control", "bad\tvalue");
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Metadata/InvalidResponse",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    const Worker = struct {
        channel: *Channel,
        request: []const u8,
        expected: status.Code,
        succeeded: bool = false,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Metadata/InvalidResponse",
                self.request,
                .{},
            ) catch return;
            defer result.deinit();
            self.succeeded = result.status.code == self.expected and
                (self.expected != .ok or std.mem.eql(u8, self.request, result.payload));
        }
    };
    var workers = [_]Worker{
        .{ .channel = &channel, .request = "bad-key", .expected = .internal },
        .{ .channel = &channel, .request = "concurrent", .expected = .ok },
    };
    var threads: [workers.len]std.Thread = undefined;
    for (&workers, &threads) |*worker, *thread| thread.* = try std.Thread.spawn(.{}, Worker.run, .{worker});
    for (&threads) |thread| thread.join();
    for (&workers) |worker| try std.testing.expect(worker.succeeded);

    var discarded = try channel.callUnary(std.testing.allocator, "/test.Metadata/InvalidResponse", "bad-ascii", .{});
    defer discarded.deinit();
    try std.testing.expect(discarded.status.isOk());
    try std.testing.expectEqualStrings("bad-ascii", discarded.payload);
    try std.testing.expect(discarded.initial_metadata.getFirst("x-control") == null);

    var bad_trailer = try channel.callUnary(std.testing.allocator, "/test.Metadata/InvalidResponse", "bad-trailer", .{});
    defer bad_trailer.deinit();
    try std.testing.expectEqual(status.Code.internal, bad_trailer.status.code);

    var reused = try channel.callUnary(std.testing.allocator, "/test.Metadata/InvalidResponse", "reused", .{});
    defer reused.deinit();
    try std.testing.expect(reused.status.isOk());
    try std.testing.expectEqualStrings("reused", reused.payload);
    try std.testing.expectEqual(@as(usize, 5), handler.calls.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), channel.impl.connect_count.load(.monotonic));
}

test "oversized response metadata is block atomic and preserves channel reuse" {
    const server = @import("server.zig");
    const service = @import("service.zig");
    const large_value = "x" ** 2048;

    const Handler = struct {
        calls: std.atomic.Value(usize) = .init(0),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            _ = self.calls.fetchAdd(1, .monotonic);
            if (std.mem.eql(u8, request, "large-initial")) {
                try context.addInitialMetadata("x-large", large_value);
            } else if (std.mem.eql(u8, request, "large-trailers")) {
                try context.addInitialMetadata("x-accepted", "yes");
                try context.addTrailingMetadata("x-large", large_value);
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const AsyncState = struct {
        done: std.atomic.Value(bool) = .init(false),
        valid: std.atomic.Value(bool) = .init(false),

        fn onComplete(context: ?*anyopaque, result: call.AsyncResult) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.valid.store(
                result.status.code == .resource_exhausted and
                    std.mem.eql(u8, result.status.message, response_headers_too_large_message) and
                    result.payload.len == 0 and
                    result.initial_metadata.items().len == 0 and
                    result.trailing_metadata.items().len == 0,
                .release,
            );
            self.done.store(true, .release);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Metadata/ResponseLimit",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{
        .max_response_header_list_size = 1024,
    });
    defer channel.deinit();

    var initial = try channel.callUnary(
        std.testing.allocator,
        "/test.Metadata/ResponseLimit",
        "large-initial",
        .{},
    );
    defer initial.deinit();
    try std.testing.expectEqual(status.Code.resource_exhausted, initial.status.code);
    try std.testing.expectEqualStrings(response_headers_too_large_message, initial.status.message);
    try std.testing.expectEqual(@as(usize, 0), initial.payload.len);
    try std.testing.expectEqual(@as(usize, 0), initial.initial_metadata.items().len);
    try std.testing.expectEqual(@as(usize, 0), initial.trailing_metadata.items().len);

    var trailers = try channel.callUnary(
        std.testing.allocator,
        "/test.Metadata/ResponseLimit",
        "large-trailers",
        .{},
    );
    defer trailers.deinit();
    try std.testing.expectEqual(status.Code.resource_exhausted, trailers.status.code);
    try std.testing.expectEqual(@as(usize, 0), trailers.payload.len);
    try std.testing.expectEqualStrings("yes", trailers.initial_metadata.getFirst("x-accepted").?);
    try std.testing.expectEqual(@as(usize, 0), trailers.trailing_metadata.items().len);

    var async_state = AsyncState{};
    try channel.callUnaryAsync(
        "/test.Metadata/ResponseLimit",
        "large-initial",
        .{},
        .{ .context = &async_state, .on_complete = AsyncState.onComplete },
    );
    const async_deadline = nowNs() +| 5 * std.time.ns_per_s;
    while (!async_state.done.load(.acquire)) {
        if (nowNs() >= async_deadline) return error.AsyncTimeout;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.testing.expect(async_state.valid.load(.acquire));

    var reused = try channel.callUnary(
        std.testing.allocator,
        "/test.Metadata/ResponseLimit",
        "reused",
        .{},
    );
    defer reused.deinit();
    try std.testing.expect(reused.status.isOk());
    try std.testing.expectEqualStrings("reused", reused.payload);
    try std.testing.expectEqual(@as(usize, 1), channel.impl.connect_count.load(.monotonic));
    try std.testing.expectEqual(@as(usize, 4), handler.calls.load(.acquire));
}

test "channel and server exchange gzip-compressed unary messages" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        saw_gzip_request: bool = false,

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            self.saw_gzip_request = context.request_compression == .gzip;
            context.setResponseCompression(.gzip);
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Compression/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var result = try channel.callUnary(
        std.testing.allocator,
        "/test.Compression/Unary",
        "compressible compressible compressible",
        .{ .request_compression = .gzip },
    );
    defer result.deinit();
    try std.testing.expect(result.status.isOk());
    try std.testing.expect(handler.saw_gzip_request);
    try std.testing.expectEqual(Compression.gzip, result.response_compression);
    try std.testing.expectEqualStrings("compressible compressible compressible", result.payload);

    var limited = try channel.callUnary(
        std.testing.allocator,
        "/test.Compression/Unary",
        "123456789",
        .{ .request_compression = .gzip, .max_response_size = 8 },
    );
    defer limited.deinit();
    try std.testing.expectEqual(status.Code.resource_exhausted, limited.status.code);
}

test "channel resolves an IPv4 hostname with c-ares" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    var runtime = try Runtime.init();
    defer runtime.deinit();

    const Handler = struct {
        fn handle(
            _: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Dns/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "localhost:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{ .runtime = &runtime });
    defer channel.deinit();

    var result = try channel.callUnary(std.testing.allocator, "/test.Dns/Unary", "resolved", .{});
    defer result.deinit();
    try std.testing.expect(result.status.isOk());
    try std.testing.expectEqualStrings("resolved", result.payload);
}

test "manual receive flow control preserves unary messages larger than the initial window" {
    const server = @import("server.zig");
    const service = @import("service.zig");
    const Handler = struct {
        fn handle(
            _: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Flow/LargeUnary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    const payload = try std.testing.allocator.alloc(u8, 128 * 1024);
    defer std.testing.allocator.free(payload);
    for (payload, 0..) |*byte, index| byte.* = @truncate(index);

    var result = try channel.callUnary(
        std.testing.allocator,
        "/test.Flow/LargeUnary",
        payload,
        .{ .max_response_size = payload.len },
    );
    defer result.deinit();
    try std.testing.expect(result.status.isOk());
    try std.testing.expectEqualSlices(u8, payload, result.payload);
}

test "channel replaces a connection after GOAWAY without replaying calls" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        server: *server.Server,
        calls: std.atomic.Value(usize) = .init(0),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            const calls = self.calls.fetchAdd(1, .acq_rel) + 1;
            if (calls == 1) {
                const connection = self.server.impl.connections.items[0];
                try connection.submitGoAway(1, c.NGHTTP2_NO_ERROR);
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    var handler = Handler{ .server = &test_server };
    try test_server.registerUnary(
        "/test.GoAway/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var first = try channel.callUnary(
        std.testing.allocator,
        "/test.GoAway/Unary",
        "first",
        .{ .timeout_ns = std.time.ns_per_hour },
    );
    defer first.deinit();
    try std.testing.expect(first.status.isOk());
    try std.testing.expectEqualStrings("first", first.payload);

    var second = try channel.callUnary(
        std.testing.allocator,
        "/test.GoAway/Unary",
        "second",
        .{ .timeout_ns = std.time.ns_per_hour },
    );
    defer second.deinit();
    try std.testing.expect(second.status.isOk());
    try std.testing.expectEqualStrings("second", second.payload);
    try std.testing.expectEqual(@as(usize, 2), channel.impl.connect_count.load(.monotonic));
    try std.testing.expectEqual(@as(usize, 2), handler.calls.load(.acquire));
    try std.testing.expectEqual(@as(usize, 0), channel.impl.deadline_heap.items.len);
}

test "server drain finishes an accepted RPC and rejects a replacement connection" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        server: *server.Server,
        local_address_available: std.atomic.Value(bool) = .init(false),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            self.server.shutdownGracefully(5 * std.time.ns_per_s);
            _ = try self.server.localAddress();
            self.local_address_available.store(true, .release);
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    var handler = Handler{ .server = &test_server };
    try test_server.registerUnary(
        "/test.Drain/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var accepted = try channel.callUnary(std.testing.allocator, "/test.Drain/Unary", "accepted", .{});
    defer accepted.deinit();
    try std.testing.expect(accepted.status.isOk());
    try std.testing.expectEqualStrings("accepted", accepted.payload);
    try std.testing.expect(handler.local_address_available.load(.acquire));

    const reconnect_deadline = nowNs() +| 5 * std.time.ns_per_s;
    while (channel.impl.connection_generation.load(.monotonic) < 2 and nowNs() < reconnect_deadline) {
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.testing.expectEqual(@as(usize, 2), channel.impl.connection_generation.load(.monotonic));

    var rejected = try channel.callUnary(std.testing.allocator, "/test.Drain/Unary", "later", .{});
    defer rejected.deinit();
    try std.testing.expectEqual(status.Code.unavailable, rejected.status.code);
    test_server.wait();
}

test "server drain timeout closes a transport-active stream" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        server: *server.Server,
        called: bool = false,

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            _: []const u8,
        ) !service.UnaryResponse {
            self.called = true;
            const payload = try allocator.alloc(u8, 8 * 1024 * 1024);
            defer allocator.free(payload);
            @memset(payload, 'x');
            self.server.shutdownGracefully(0);
            return service.UnaryResponse.ok(allocator, payload);
        }
    };

    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    var handler = Handler{ .server = &test_server };
    try test_server.registerUnary(
        "/test.Drain/Timeout",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var result = try channel.callUnary(
        std.testing.allocator,
        "/test.Drain/Timeout",
        "request",
        .{ .max_response_size = 16 * 1024 * 1024 },
    );
    defer result.deinit();
    try std.testing.expect(handler.called);
    try std.testing.expectEqual(status.Code.unavailable, result.status.code);
    test_server.wait();
}

test "immediate shutdown escalates an active graceful drain" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        fn handle(_: *@This(), allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Drain/Escalate",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    test_server.shutdownGracefully(std.time.ns_per_hour);
    test_server.shutdown();
    test_server.shutdown();
    test_server.wait();

    var result = try channel.callUnary(std.testing.allocator, "/test.Drain/Escalate", "later", .{});
    defer result.deinit();
    try std.testing.expectEqual(status.Code.unavailable, result.status.code);
}

test "channel shutdown safely completes an active call before exclusive deinit" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        entered: std.Io.Semaphore = .{},
        release: std.Io.Semaphore = .{},

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            self.entered.post(std.testing.io);
            self.release.waitUncancelable(std.testing.io);
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Lifecycle/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    const Worker = struct {
        channel: *Channel,
        done: std.Io.Semaphore = .{},
        code: status.Code = .unknown,
        returned_result: bool = false,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Lifecycle/Unary",
                "request",
                .{ .timeout_ns = std.time.ns_per_hour },
            ) catch {
                self.done.post(std.testing.io);
                return;
            };
            defer result.deinit();
            self.code = result.status.code;
            self.returned_result = true;
            self.done.post(std.testing.io);
        }
    };

    var worker = Worker{ .channel = &channel };
    const thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});
    handler.entered.waitUncancelable(std.testing.io);
    channel.shutdown();
    worker.done.waitUncancelable(std.testing.io);
    handler.release.post(std.testing.io);
    thread.join();

    try std.testing.expect(worker.returned_result);
    try std.testing.expectEqual(status.Code.unavailable, worker.code);
    try std.testing.expectEqual(@as(usize, 0), channel.impl.deadline_heap.items.len);
}

test "channel shutdown cancels an active blocked write" {
    var listen_address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var listener = try listen_address.listen(std.testing.io, .{});
    defer listener.deinit(std.testing.io);

    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(
        listener.socket.handle,
        @ptrCast(&local_address),
        &address_length,
    )) != .SUCCESS) return error.AddressQueryFailed;

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(
        &target_buffer,
        "127.0.0.1:{d}",
        .{std.mem.bigToNative(u16, local_address.port)},
    );
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var peer = try listener.accept(std.testing.io);
    var peer_open = true;
    defer if (peer_open) peer.close(std.testing.io);

    const send_buffer: c_int = 4096;
    try std.posix.setsockopt(
        channel.impl.tcp.fd,
        std.posix.SOL.SOCKET,
        std.posix.SO.SNDBUF,
        std.mem.asBytes(&send_buffer),
    );

    const flow_control_frames = [_]u8{
        // SETTINGS_INITIAL_WINDOW_SIZE = 16 MiB - 1.
        0x00, 0x00, 0x06, 0x04, 0x00, 0x00, 0x00, 0x00, 0x00,
        0x00, 0x04, 0x00, 0xff, 0xff, 0xff,
        // Increase the connection window by another 16 MiB - 1.
        0x00, 0x00, 0x04,
        0x08, 0x00, 0x00, 0x00, 0x00, 0x00, 0x00, 0xff, 0xff,
        0xff,
    };
    var peer_write_buffer: [64]u8 = undefined;
    var peer_writer = peer.writer(std.testing.io, &peer_write_buffer);
    try peer_writer.interface.writeAll(&flow_control_frames);
    try peer_writer.interface.flush();

    const payload = try std.testing.allocator.alloc(u8, 16 * 1024 * 1024);
    defer std.testing.allocator.free(payload);
    @memset(payload, 'x');

    const Worker = struct {
        channel: *Channel,
        payload: []const u8,
        done: std.atomic.Value(bool) = .init(false),
        code: status.Code = .unknown,
        returned_result: bool = false,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Lifecycle/BlockedWrite",
                self.payload,
                .{},
            ) catch {
                self.done.store(true, .release);
                return;
            };
            defer result.deinit();
            self.code = result.status.code;
            self.returned_result = true;
            self.done.store(true, .release);
        }
    };
    const Waiter = struct {
        channel: *Channel,
        done: std.atomic.Value(bool) = .init(false),

        fn run(self: *@This()) void {
            self.channel.wait();
            self.done.store(true, .release);
        }
    };

    var worker = Worker{ .channel = &channel, .payload = payload };
    const worker_thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});

    const observe_deadline = nowNs() +| 5 * std.time.ns_per_s;
    while (!channel.impl.test_observer.write_observed.load(.acquire) and nowNs() < observe_deadline) {
        channel.impl.test_observer.write_requested.store(true, .release);
        channel.impl.async_handle.notify() catch break;
        std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake) catch break;
    }
    const write_observed = channel.impl.test_observer.write_observed.load(.acquire);
    if (!write_observed) {
        channel.shutdown();
        peer.close(std.testing.io);
        peer_open = false;
        worker_thread.join();
        channel.wait();
        try std.testing.expect(write_observed);
        return;
    }
    channel.impl.test_observer.write_observed_sem.waitUncancelable(std.testing.io);

    channel.shutdown();
    var waiter = Waiter{ .channel = &channel };
    const waiter_thread = std.Thread.spawn(.{}, Waiter.run, .{&waiter}) catch |err| {
        peer.close(std.testing.io);
        peer_open = false;
        worker_thread.join();
        channel.wait();
        return err;
    };
    const worker_finished = waitForTestFlag(&worker.done, 5 * std.time.ns_per_s);
    const waiter_finished = waitForTestFlag(&waiter.done, 5 * std.time.ns_per_s);
    if (!worker_finished or !waiter_finished) {
        peer.close(std.testing.io);
        peer_open = false;
    }
    worker_thread.join();
    waiter_thread.join();

    try std.testing.expect(worker_finished);
    try std.testing.expect(waiter_finished);
    try std.testing.expect(worker.returned_result);
    try std.testing.expectEqual(status.Code.unavailable, worker.code);
}

test "channel shutdown drains an active reconnect completion" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        server: *server.Server,

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            const connection = self.server.impl.connections.items[0];
            try connection.submitGoAway(1, c.NGHTTP2_NO_ERROR);
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Waiter = struct {
        channel: *Channel,
        done: std.atomic.Value(bool) = .init(false),

        fn run(self: *@This()) void {
            self.channel.wait();
            self.done.store(true, .release);
        }
    };

    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    var handler = Handler{ .server = &test_server };
    try test_server.registerUnary(
        "/test.Lifecycle/Reconnect",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    channel.impl.test_observer.connect_requested.store(true, .release);
    var connect_released = false;
    defer if (!connect_released) channel.impl.test_observer.connect_release.post(std.testing.io);
    var result = try channel.callUnary(
        std.testing.allocator,
        "/test.Lifecycle/Reconnect",
        "request",
        .{ .timeout_ns = std.time.ns_per_hour },
    );
    defer result.deinit();
    try std.testing.expect(result.status.isOk());

    const connect_observed = waitForTestFlag(
        &channel.impl.test_observer.connect_observed,
        5 * std.time.ns_per_s,
    );
    if (!connect_observed) {
        channel.shutdown();
        channel.impl.test_observer.connect_release.post(std.testing.io);
        connect_released = true;
        channel.wait();
        try std.testing.expect(connect_observed);
        return;
    }
    channel.impl.test_observer.connect_observed_sem.waitUncancelable(std.testing.io);

    channel.shutdown();
    channel.impl.test_observer.connect_release.post(std.testing.io);
    connect_released = true;
    var waiter = Waiter{ .channel = &channel };
    const waiter_thread = std.Thread.spawn(.{}, Waiter.run, .{&waiter}) catch |err| {
        channel.wait();
        return err;
    };
    const waiter_finished = waitForTestFlag(&waiter.done, 5 * std.time.ns_per_s);
    if (!waiter_finished) test_server.shutdown();
    waiter_thread.join();

    try std.testing.expect(waiter_finished);
    try std.testing.expect(channel.impl.test_observer.connect_cancel_confirmed.load(.acquire));
    try std.testing.expect(!channel.impl.connect_active);
    try std.testing.expect(!channel.impl.connect_cancel_submitted);
    try std.testing.expect(channel.impl.close_completed);
    try std.testing.expectEqual(@as(usize, 0), channel.impl.deadline_heap.items.len);
}

test "server context observes a wire deadline and overrides a late handler response" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const FakeClock = struct {
        now_ns: u64 = 100,

        fn now(context: ?*anyopaque) u64 {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            return self.now_ns;
        }
    };
    const Handler = struct {
        clock: *FakeClock,
        saw_deadline: std.atomic.Value(bool) = .init(false),
        saw_no_deadline: std.atomic.Value(bool) = .init(false),
        calls: std.atomic.Value(usize) = .init(0),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            _ = self.calls.fetchAdd(1, .monotonic);
            if (context.hasDeadline()) {
                self.saw_deadline.store(context.remainingTimeNs().? > 0 and !context.isDeadlineExceeded(), .release);
                self.clock.now_ns +|= 20 * std.time.ns_per_s;
            } else {
                self.saw_no_deadline.store(true, .release);
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var fake_clock = FakeClock{};
    var handler = Handler{ .clock = &fake_clock };
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    test_server.impl.clock = .{ .context = &fake_clock, .now_fn = FakeClock.now };
    try test_server.registerUnary(
        "/test.Deadline/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var expired = try channel.callUnary(
        std.testing.allocator,
        "/test.Deadline/Unary",
        "late",
        .{ .timeout_ns = 10 * std.time.ns_per_s },
    );
    defer expired.deinit();
    try std.testing.expectEqual(status.Code.deadline_exceeded, expired.status.code);
    try std.testing.expect(handler.saw_deadline.load(.acquire));

    var reused = try channel.callUnary(std.testing.allocator, "/test.Deadline/Unary", "reused", .{});
    defer reused.deinit();
    try std.testing.expect(reused.status.isOk());
    try std.testing.expectEqualStrings("reused", reused.payload);
    try std.testing.expect(handler.saw_no_deadline.load(.acquire));
    try std.testing.expectEqual(@as(usize, 2), handler.calls.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), channel.impl.connect_count.load(.monotonic));
}

test "channel deadline timer does not poll before a distant deadline" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        entered: std.Io.Semaphore = .{},
        release: std.Io.Semaphore = .{},

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            self.entered.post(std.testing.io);
            self.release.waitUncancelable(std.testing.io);
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Worker = struct {
        channel: *Channel,
        returned_result: bool = false,
        code: status.Code = .unknown,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Deadline/NoPolling",
                "request",
                .{ .timeout_ns = 2 * std.time.ns_per_s },
            ) catch return;
            defer result.deinit();
            self.returned_result = true;
            self.code = result.status.code;
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Deadline/NoPolling",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var worker = Worker{ .channel = &channel };
    const thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});
    handler.entered.waitUncancelable(std.testing.io);
    const armed = waitForTestFlag(
        &channel.impl.test_observer.deadline_timer_armed,
        5 * std.time.ns_per_s,
    );
    const baseline = channel.impl.test_observer.deadline_timer_callbacks.load(.acquire);
    std.Io.sleep(std.testing.io, .fromMilliseconds(100), .awake) catch {};
    const observed = channel.impl.test_observer.deadline_timer_callbacks.load(.acquire);
    handler.release.post(std.testing.io);
    thread.join();

    try std.testing.expect(armed);
    try std.testing.expectEqual(@as(usize, 0), observed - baseline);
    try std.testing.expect(worker.returned_result);
    try std.testing.expectEqual(status.Code.ok, worker.code);
}

test "channel resets its timer when an earlier deadline is added" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        entered: std.Io.Semaphore = .{},
        release: std.Io.Semaphore = .{},

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            if (std.mem.eql(u8, request, "first")) {
                self.entered.post(std.testing.io);
                self.release.waitUncancelable(std.testing.io);
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Worker = struct {
        channel: *Channel,
        request: []const u8,
        timeout_ns: u64,
        returned_result: bool = false,
        code: status.Code = .unknown,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Deadline/Earlier",
                self.request,
                .{ .timeout_ns = self.timeout_ns },
            ) catch return;
            defer result.deinit();
            self.returned_result = true;
            self.code = result.status.code;
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Deadline/Earlier",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var first = Worker{
        .channel = &channel,
        .request = "first",
        .timeout_ns = 10 * std.time.ns_per_s,
    };
    const first_thread = try std.Thread.spawn(.{}, Worker.run, .{&first});
    handler.entered.waitUncancelable(std.testing.io);
    const initially_armed = waitForTestFlag(
        &channel.impl.test_observer.deadline_timer_armed,
        5 * std.time.ns_per_s,
    );
    const first_target = channel.impl.test_observer.deadline_timer_target_ns.load(.acquire);
    if (!initially_armed or first_target == 0) {
        handler.release.post(std.testing.io);
        first_thread.join();
        try std.testing.expect(initially_armed and first_target != 0);
        return;
    }

    var second = Worker{
        .channel = &channel,
        .request = "second",
        .timeout_ns = 200 * std.time.ns_per_ms,
    };
    const second_thread = std.Thread.spawn(.{}, Worker.run, .{&second}) catch |err| {
        handler.release.post(std.testing.io);
        first_thread.join();
        return err;
    };
    const reset_observe_deadline = nowNs() +| 5 * std.time.ns_per_s;
    var reset_observed = false;
    while (nowNs() < reset_observe_deadline) {
        const current_target = channel.impl.test_observer.deadline_timer_target_ns.load(.acquire);
        if (current_target != 0 and current_target < first_target) {
            reset_observed = true;
            break;
        }
        std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake) catch break;
    }
    if (!reset_observed) {
        handler.release.post(std.testing.io);
        first_thread.join();
        second_thread.join();
        try std.testing.expect(reset_observed);
        return;
    }

    second_thread.join();
    handler.release.post(std.testing.io);
    first_thread.join();

    try std.testing.expect(second.returned_result);
    try std.testing.expectEqual(status.Code.deadline_exceeded, second.code);
    try std.testing.expect(first.returned_result);
    try std.testing.expectEqual(status.Code.ok, first.code);
}

test "completed deadline leaves at most one stale timer callback" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        entered: std.Io.Semaphore = .{},
        release: std.Io.Semaphore = .{},

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            if (std.mem.eql(u8, request, "deadline")) {
                self.entered.post(std.testing.io);
                self.release.waitUncancelable(std.testing.io);
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Worker = struct {
        channel: *Channel,
        returned_result: bool = false,
        code: status.Code = .unknown,

        fn run(self: *@This()) void {
            var result = self.channel.callUnary(
                std.testing.allocator,
                "/test.Deadline/Stale",
                "deadline",
                .{ .timeout_ns = std.time.ns_per_s },
            ) catch return;
            defer result.deinit();
            self.returned_result = true;
            self.code = result.status.code;
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Deadline/Stale",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var worker = Worker{ .channel = &channel };
    const thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});
    handler.entered.waitUncancelable(std.testing.io);
    const armed = waitForTestFlag(
        &channel.impl.test_observer.deadline_timer_armed,
        5 * std.time.ns_per_s,
    );
    handler.release.post(std.testing.io);
    thread.join();

    const baseline = channel.impl.test_observer.deadline_timer_callbacks.load(.acquire);
    const stale_wait_deadline = nowNs() +| 5 * std.time.ns_per_s;
    while (channel.impl.test_observer.deadline_timer_armed.load(.acquire) and
        nowNs() < stale_wait_deadline)
    {
        std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake) catch break;
    }
    const after_stale = channel.impl.test_observer.deadline_timer_callbacks.load(.acquire);
    std.Io.sleep(std.testing.io, .fromMilliseconds(100), .awake) catch {};
    const after_observation = channel.impl.test_observer.deadline_timer_callbacks.load(.acquire);

    var reused = try channel.callUnary(std.testing.allocator, "/test.Deadline/Stale", "reused", .{});
    defer reused.deinit();

    try std.testing.expect(armed);
    try std.testing.expect(worker.returned_result);
    try std.testing.expectEqual(status.Code.ok, worker.code);
    try std.testing.expect(!channel.impl.test_observer.deadline_timer_armed.load(.acquire));
    try std.testing.expect(after_stale - baseline <= 1);
    try std.testing.expectEqual(after_stale, after_observation);
    try std.testing.expect(reused.status.isOk());
    try std.testing.expectEqualStrings("reused", reused.payload);
    try std.testing.expectEqual(@as(usize, 1), channel.impl.connect_count.load(.monotonic));
}

test "channel serializes a non-thread-safe backing allocator" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Probe = struct {
        backing: std.mem.Allocator,
        armed: std.atomic.Value(bool) = .init(false),
        active: std.atomic.Value(usize) = .init(0),
        max_active: std.atomic.Value(usize) = .init(0),
        alloc_count: std.atomic.Value(usize) = .init(0),
        first_alloc_blocked: std.atomic.Value(bool) = .init(false),
        first_alloc_entered: std.Io.Semaphore = .{},
        first_alloc_release: std.Io.Semaphore = .{},

        fn allocator(self: *@This()) std.mem.Allocator {
            return .{
                .ptr = self,
                .vtable = &.{
                    .alloc = alloc,
                    .resize = resize,
                    .remap = remap,
                    .free = free,
                },
            };
        }

        fn resetAndArm(self: *@This()) void {
            self.active.store(0, .release);
            self.max_active.store(0, .release);
            self.alloc_count.store(0, .release);
            self.first_alloc_blocked.store(false, .release);
            self.armed.store(true, .release);
        }

        fn enter(self: *@This(), is_alloc: bool) bool {
            if (!self.armed.load(.acquire)) return false;
            const active = self.active.fetchAdd(1, .acq_rel) + 1;
            _ = self.max_active.fetchMax(active, .acq_rel);
            if (is_alloc and self.alloc_count.fetchAdd(1, .acq_rel) == 0) {
                self.first_alloc_blocked.store(true, .release);
                self.first_alloc_entered.post(std.testing.io);
                self.first_alloc_release.waitUncancelable(std.testing.io);
            }
            return true;
        }

        fn leave(self: *@This()) void {
            _ = self.active.fetchSub(1, .acq_rel);
        }

        fn alloc(context: *anyopaque, len: usize, alignment: std.mem.Alignment, ret_addr: usize) ?[*]u8 {
            const self: *@This() = @ptrCast(@alignCast(context));
            const tracked = self.enter(true);
            defer if (tracked) self.leave();
            return self.backing.rawAlloc(len, alignment, ret_addr);
        }

        fn resize(
            context: *anyopaque,
            memory: []u8,
            alignment: std.mem.Alignment,
            new_len: usize,
            ret_addr: usize,
        ) bool {
            const self: *@This() = @ptrCast(@alignCast(context));
            const tracked = self.enter(false);
            defer if (tracked) self.leave();
            return self.backing.rawResize(memory, alignment, new_len, ret_addr);
        }

        fn remap(
            context: *anyopaque,
            memory: []u8,
            alignment: std.mem.Alignment,
            new_len: usize,
            ret_addr: usize,
        ) ?[*]u8 {
            const self: *@This() = @ptrCast(@alignCast(context));
            const tracked = self.enter(false);
            defer if (tracked) self.leave();
            return self.backing.rawRemap(memory, alignment, new_len, ret_addr);
        }

        fn free(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, ret_addr: usize) void {
            const self: *@This() = @ptrCast(@alignCast(context));
            const tracked = self.enter(false);
            defer if (tracked) self.leave();
            self.backing.rawFree(memory, alignment, ret_addr);
        }
    };
    const LockHook = struct {
        probe: *Probe,
        notified: std.atomic.Value(bool) = .init(false),
        second_before_lock: std.Io.Semaphore = .{},

        fn beforeLock(context: ?*anyopaque, operation: AllocatorOperation) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            if (operation == .alloc and
                self.probe.first_alloc_blocked.load(.acquire) and
                !self.notified.swap(true, .acq_rel))
            {
                self.second_before_lock.post(std.testing.io);
            }
        }
    };
    const Handler = struct {
        fn handle(
            _: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const Worker = struct {
        channel: *Channel,
        index: usize,
        succeeded: bool = false,
        result_allocator_ok: bool = false,

        fn run(self: *@This()) void {
            var result_allocator: std.heap.DebugAllocator(.{ .thread_safe = false }) = .init;
            defer self.result_allocator_ok = result_allocator.deinit() == .ok;
            var request_buffer: [16]u8 = undefined;
            const request = std.fmt.bufPrint(&request_buffer, "request-{d}", .{self.index}) catch return;
            var result = self.channel.callUnary(
                result_allocator.allocator(),
                "/test.Allocator/Unary",
                request,
                .{},
            ) catch return;
            defer result.deinit();
            self.succeeded = result.status.isOk() and std.mem.eql(u8, request, result.payload);
        }
    };

    var backing_allocator: std.heap.DebugAllocator(.{ .thread_safe = false }) = .init;
    var backing_allocator_active = true;
    defer if (backing_allocator_active) {
        std.testing.expectEqual(std.heap.Check.ok, backing_allocator.deinit()) catch @panic("backing allocator leak");
    };
    var probe = Probe{ .backing = backing_allocator.allocator() };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Allocator/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(probe.allocator(), target, .{});
    var channel_active = true;
    defer if (channel_active) channel.deinit();

    var lock_hook = LockHook{ .probe = &probe };
    channel.impl.serialized_allocator.test_hook_context.store(&lock_hook, .release);
    channel.impl.serialized_allocator.test_before_lock.store(LockHook.beforeLock, .release);
    probe.resetAndArm();

    var workers: [8]Worker = undefined;
    var threads: [8]std.Thread = undefined;
    for (&workers, 0..) |*worker, index| worker.* = .{ .channel = &channel, .index = index };

    threads[0] = try std.Thread.spawn(.{}, Worker.run, .{&workers[0]});
    probe.first_alloc_entered.waitUncancelable(std.testing.io);
    for (threads[1..], workers[1..]) |*thread, *worker| {
        thread.* = try std.Thread.spawn(.{}, Worker.run, .{worker});
    }
    lock_hook.second_before_lock.waitUncancelable(std.testing.io);
    probe.first_alloc_release.post(std.testing.io);

    for (&threads) |*thread| thread.join();
    for (&workers) |worker| {
        try std.testing.expect(worker.succeeded);
        try std.testing.expect(worker.result_allocator_ok);
    }

    channel.deinit();
    channel_active = false;
    try std.testing.expect(lock_hook.notified.load(.acquire));
    try std.testing.expectEqual(@as(usize, 0), probe.active.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), probe.max_active.load(.acquire));
    try std.testing.expectEqual(std.heap.Check.ok, backing_allocator.deinit());
    backing_allocator_active = false;
}

test "channel performs reusable concurrent unary calls end to end" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        calls: std.atomic.Value(usize) = .init(0),
        saw_request_metadata: std.atomic.Value(bool) = .init(false),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            _ = self.calls.fetchAdd(1, .monotonic);
            if (context.request_metadata.getFirst("x-request-id")) |value| {
                self.saw_request_metadata.store(std.mem.eql(u8, value, "request-1"), .release);
            }
            try context.addInitialMetadata("x-initial", "present");
            try context.addTrailingMetadata("x-trailing", "present");
            if (std.mem.eql(u8, request, "fail")) {
                return service.UnaryResponse.fail(
                    allocator,
                    .init(.invalid_argument, "bad % value\n"),
                );
            }
            if (std.mem.eql(u8, request, "slow")) try std.Io.sleep(std.testing.io, .fromMilliseconds(50), .awake);
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Echo/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();

    var success = try channel.callUnary(
        std.testing.allocator,
        "/test.Echo/Unary",
        "hello",
        .{ .metadata = &.{.{ .key = "x-request-id", .value = "request-1" }} },
    );
    defer success.deinit();
    try std.testing.expect(success.status.isOk());
    try std.testing.expectEqualStrings("hello", success.payload);
    try std.testing.expectEqualStrings("present", success.initial_metadata.getFirst("x-initial").?);
    try std.testing.expectEqualStrings("present", success.trailing_metadata.getFirst("x-trailing").?);
    try std.testing.expect(handler.saw_request_metadata.load(.acquire));

    const binary_payload = [_]u8{ 0, 1, 0xff, 0, 42 };
    var binary = try channel.callUnary(std.testing.allocator, "/test.Echo/Unary", &binary_payload, .{});
    defer binary.deinit();
    try std.testing.expectEqualSlices(u8, &binary_payload, binary.payload);

    var application_error = try channel.callUnary(std.testing.allocator, "/test.Echo/Unary", "fail", .{});
    defer application_error.deinit();
    try std.testing.expectEqual(status.Code.invalid_argument, application_error.status.code);
    try std.testing.expectEqualStrings("bad % value\n", application_error.status.message);
    try std.testing.expectEqualStrings("present", application_error.trailing_metadata.getFirst("x-trailing").?);

    var missing_method = try channel.callUnary(std.testing.allocator, "/test.Echo/Missing", "request", .{});
    defer missing_method.deinit();
    try std.testing.expectEqual(status.Code.unimplemented, missing_method.status.code);

    var limited = try channel.callUnary(
        std.testing.allocator,
        "/test.Echo/Unary",
        "too large",
        .{ .max_response_size = 3 },
    );
    defer limited.deinit();
    try std.testing.expectEqual(status.Code.resource_exhausted, limited.status.code);

    var reused = try channel.callUnary(std.testing.allocator, "/test.Echo/Unary", "again", .{});
    defer reused.deinit();
    try std.testing.expectEqualStrings("again", reused.payload);
    try std.testing.expectEqual(@as(usize, 1), channel.impl.connect_count.load(.monotonic));

    const Worker = struct {
        channel: *Channel,
        index: usize,
        succeeded: bool = false,

        fn run(self: *@This()) void {
            var request_buffer: [16]u8 = undefined;
            const request = std.fmt.bufPrint(&request_buffer, "request-{d}", .{self.index}) catch return;
            var result = self.channel.callUnary(std.testing.allocator, "/test.Echo/Unary", request, .{}) catch return;
            defer result.deinit();
            self.succeeded = result.status.isOk() and std.mem.eql(u8, request, result.payload);
        }
    };
    var workers: [8]Worker = undefined;
    var threads: [8]std.Thread = undefined;
    for (&workers, &threads, 0..) |*worker, *thread, index| {
        worker.* = .{ .channel = &channel, .index = index };
        thread.* = try std.Thread.spawn(.{}, Worker.run, .{worker});
    }
    for (&threads) |*thread| thread.join();
    for (&workers) |worker| try std.testing.expect(worker.succeeded);
    try std.testing.expectEqual(@as(usize, 1), channel.impl.connect_count.load(.monotonic));

    var deadline = try channel.callUnary(
        std.testing.allocator,
        "/test.Echo/Unary",
        "slow",
        .{ .timeout_ns = 5 * std.time.ns_per_ms },
    );
    defer deadline.deinit();
    try std.testing.expectEqual(status.Code.deadline_exceeded, deadline.status.code);
    try std.testing.expectEqual(@as(usize, 0), channel.impl.deadline_heap.items.len);

    test_server.shutdown();
    test_server.wait();
    var unavailable = try channel.callUnary(std.testing.allocator, "/test.Echo/Unary", "after-close", .{});
    defer unavailable.deinit();
    try std.testing.expectEqual(status.Code.unavailable, unavailable.status.code);
}

test "event-driven unary calls complete exactly once and release operations" {
    const server = @import("server.zig");
    const service = @import("service.zig");

    const Handler = struct {
        blocked_entered: std.Io.Semaphore = .{},
        blocked_release: std.Io.Semaphore = .{},
        copied_metadata_seen: std.atomic.Value(bool) = .init(false),

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            try context.addInitialMetadata("x-initial", "present");
            try context.addTrailingMetadata("x-trailing", "present");
            if (context.request_metadata.getFirst("x-copy")) |value| {
                self.copied_metadata_seen.store(std.mem.eql(u8, value, "original"), .release);
            }
            if (std.mem.eql(u8, request, "status")) {
                return service.UnaryResponse.fail(allocator, .init(.invalid_argument, "bad request"));
            }
            if (std.mem.eql(u8, request, "slow")) {
                try std.Io.sleep(std.testing.io, .fromMilliseconds(50), .awake);
            } else if (std.mem.eql(u8, request, "blocked")) {
                self.blocked_entered.post(std.testing.io);
                self.blocked_release.waitUncancelable(std.testing.io);
            }
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    const Completion = struct {
        expected_code: status.Code,
        expected_message: []const u8 = "",
        expected_payload: []const u8 = "",
        require_metadata: bool = false,
        caller_thread_id: std.Thread.Id,
        calls: std.atomic.Value(usize) = .init(0),
        done: std.atomic.Value(bool) = .init(false),
        valid: std.atomic.Value(bool) = .init(false),

        fn onComplete(context: ?*anyopaque, result: call.AsyncResult) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            const first = self.calls.fetchAdd(1, .acq_rel) == 0;
            const metadata_valid = !self.require_metadata or
                (std.mem.eql(u8, result.initial_metadata.getFirst("x-initial") orelse "", "present") and
                    std.mem.eql(u8, result.trailing_metadata.getFirst("x-trailing") orelse "", "present"));
            self.valid.store(
                first and
                    std.Thread.getCurrentId() != self.caller_thread_id and
                    result.status.code == self.expected_code and
                    std.mem.eql(u8, result.status.message, self.expected_message) and
                    std.mem.eql(u8, result.payload, self.expected_payload) and
                    result.response_compression == .identity and
                    metadata_valid,
                .release,
            );
            self.done.store(true, .release);
        }
    };

    const ManyState = struct {
        remaining: std.atomic.Value(usize),
        valid: std.atomic.Value(bool) = .init(true),
        done: std.atomic.Value(bool) = .init(false),
    };
    const ManyCompletion = struct {
        shared: *ManyState,
        calls: std.atomic.Value(usize) = .init(0),

        fn onComplete(context: ?*anyopaque, result: call.AsyncResult) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            if (self.calls.fetchAdd(1, .acq_rel) != 0 or
                !result.status.isOk() or
                !std.mem.eql(u8, result.payload, "many"))
            {
                self.shared.valid.store(false, .release);
            }
            if (self.shared.remaining.fetchSub(1, .acq_rel) == 1) {
                self.shared.done.store(true, .release);
            }
        }
    };

    const Reentrant = struct {
        channel: *Channel,
        calls: usize = 0,
        valid: bool = true,
        done: std.atomic.Value(bool) = .init(false),

        fn onComplete(context: ?*anyopaque, result: call.AsyncResult) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.calls += 1;
            self.valid = self.valid and result.status.isOk() and
                std.mem.eql(u8, result.payload, "reentrant");
            if (self.calls == 4) {
                self.done.store(true, .release);
                return;
            }
            self.channel.callUnaryAsync(
                "/test.Async/Unary",
                "reentrant",
                .{},
                .{ .context = self, .on_complete = onComplete },
            ) catch {
                self.valid = false;
                self.done.store(true, .release);
            };
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{});
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Async/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try test_server.port()});
    var channel_allocator: std.heap.DebugAllocator(.{ .thread_safe = false }) = .init;
    var allocator_active = true;
    defer if (allocator_active) {
        std.testing.expectEqual(std.heap.Check.ok, channel_allocator.deinit()) catch
            @panic("async channel allocator leak");
    };
    var channel = try Channel.init(channel_allocator.allocator(), target, .{});
    var channel_active = true;
    defer if (channel_active) channel.deinit();
    const caller_thread_id = std.Thread.getCurrentId();

    var copied_path = "/test.Async/Unary".*;
    var copied_request = "copied".*;
    var copied_key = "x-copy".*;
    var copied_value = "original".*;
    var copied = Completion{
        .expected_code = .ok,
        .expected_payload = "copied",
        .require_metadata = true,
        .caller_thread_id = caller_thread_id,
    };
    try channel.callUnaryAsync(&copied_path, &copied_request, .{
        .metadata = &.{.{ .key = &copied_key, .value = &copied_value }},
    }, .{ .context = &copied, .on_complete = Completion.onComplete });
    @memset(&copied_path, 'x');
    @memset(&copied_request, 'x');
    @memset(&copied_key, 'x');
    @memset(&copied_value, 'x');
    try std.testing.expect(waitForTestFlag(&copied.done, 5 * std.time.ns_per_s));
    try std.testing.expect(copied.valid.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), copied.calls.load(.acquire));
    try std.testing.expect(handler.copied_metadata_seen.load(.acquire));

    var application_error = Completion{
        .expected_code = .invalid_argument,
        .expected_message = "bad request",
        .caller_thread_id = caller_thread_id,
    };
    try channel.callUnaryAsync(
        "/test.Async/Unary",
        "status",
        .{},
        .{ .context = &application_error, .on_complete = Completion.onComplete },
    );
    try std.testing.expect(waitForTestFlag(&application_error.done, 5 * std.time.ns_per_s));
    try std.testing.expect(application_error.valid.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), application_error.calls.load(.acquire));

    var pre_submit_deadline = Completion{
        .expected_code = .deadline_exceeded,
        .expected_message = "deadline exceeded",
        .caller_thread_id = caller_thread_id,
    };
    try channel.callUnaryAsync(
        "/test.Async/Unary",
        "never submitted",
        .{ .timeout_ns = 1 },
        .{ .context = &pre_submit_deadline, .on_complete = Completion.onComplete },
    );
    try std.testing.expect(waitForTestFlag(&pre_submit_deadline.done, 5 * std.time.ns_per_s));
    try std.testing.expect(pre_submit_deadline.valid.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), pre_submit_deadline.calls.load(.acquire));

    var active_deadline = Completion{
        .expected_code = .deadline_exceeded,
        .expected_message = "deadline exceeded",
        .caller_thread_id = caller_thread_id,
    };
    try channel.callUnaryAsync(
        "/test.Async/Unary",
        "slow",
        .{ .timeout_ns = 5 * std.time.ns_per_ms },
        .{ .context = &active_deadline, .on_complete = Completion.onComplete },
    );
    try std.testing.expect(waitForTestFlag(&active_deadline.done, 5 * std.time.ns_per_s));
    try std.testing.expect(active_deadline.valid.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), active_deadline.calls.load(.acquire));

    const concurrent_count = 128;
    var many_state = ManyState{ .remaining = .init(concurrent_count) };
    var many: [concurrent_count]ManyCompletion = undefined;
    for (&many) |*completion| {
        completion.* = .{ .shared = &many_state };
        try channel.callUnaryAsync(
            "/test.Async/Unary",
            "many",
            .{},
            .{ .context = completion, .on_complete = ManyCompletion.onComplete },
        );
    }
    try std.testing.expect(waitForTestFlag(&many_state.done, 5 * std.time.ns_per_s));
    try std.testing.expect(many_state.valid.load(.acquire));
    for (&many) |*completion| {
        try std.testing.expectEqual(@as(usize, 1), completion.calls.load(.acquire));
    }

    var reentrant = Reentrant{ .channel = &channel };
    try channel.callUnaryAsync(
        "/test.Async/Unary",
        "reentrant",
        .{},
        .{ .context = &reentrant, .on_complete = Reentrant.onComplete },
    );
    try std.testing.expect(waitForTestFlag(&reentrant.done, 5 * std.time.ns_per_s));
    try std.testing.expect(reentrant.valid);
    try std.testing.expectEqual(@as(usize, 4), reentrant.calls);

    var shutdown_completion = Completion{
        .expected_code = .unavailable,
        .expected_message = "channel closed",
        .caller_thread_id = caller_thread_id,
    };
    try channel.callUnaryAsync(
        "/test.Async/Unary",
        "blocked",
        .{},
        .{ .context = &shutdown_completion, .on_complete = Completion.onComplete },
    );
    handler.blocked_entered.waitUncancelable(std.testing.io);
    channel.shutdown();
    try std.testing.expectError(error.ChannelUnavailable, channel.callUnaryAsync(
        "/test.Async/Unary",
        "rejected",
        .{},
        .{ .context = &shutdown_completion, .on_complete = Completion.onComplete },
    ));
    channel.wait();
    try std.testing.expect(shutdown_completion.done.load(.acquire));
    try std.testing.expect(shutdown_completion.valid.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), shutdown_completion.calls.load(.acquire));
    handler.blocked_release.post(std.testing.io);

    channel.deinit();
    channel_active = false;
    try std.testing.expectEqual(std.heap.Check.ok, channel_allocator.deinit());
    allocator_active = false;
}

test "channel and server exchange a unary call over TLS" {
    if (!build_options.tls) return error.SkipZigTest;
    const server = @import("server.zig");
    const service = @import("service.zig");
    const certificate = @embedFile("testdata/localhost-cert.pem");
    const private_key = @embedFile("testdata/localhost-key.pem");

    const Handler = struct {
        fn handle(
            _: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{
        .tls = .{
            .certificate_chain_pem = certificate,
            .private_key_pem = private_key,
        },
    });
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Tls/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var runtime = try Runtime.init();
    defer runtime.deinit();
    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "localhost:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{
        .runtime = &runtime,
        .tls = .{ .ca_certificates_pem = certificate },
    });
    defer channel.deinit();

    var result = try channel.callUnary(std.testing.allocator, "/test.Tls/Unary", "secure", .{});
    defer result.deinit();
    try std.testing.expect(result.status.isOk());
    try std.testing.expectEqualStrings("secure", result.payload);
}

test "TLS channel replaces a server-idle connection for a later RPC" {
    if (!build_options.tls) return error.SkipZigTest;
    const server = @import("server.zig");
    const service = @import("service.zig");
    const certificate = @embedFile("testdata/localhost-cert.pem");
    const private_key = @embedFile("testdata/localhost-key.pem");

    const Diagnostics = struct {
        mutex: std.Io.Mutex = .init,
        started_ns: u64,
        buffer: [16 * 1024]u8 = undefined,
        len: usize = 0,
        dropped: usize = 0,

        fn log(context: ?*anyopaque, level: u32, log_message: event_logger.BytesView) callconv(.c) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.mutex.lockUncancelable(syncIo());
            defer self.mutex.unlock(syncIo());
            const line = std.fmt.bufPrint(self.buffer[self.len..], "+{d}us level={d} {s}\n", .{
                (nowNs() -| self.started_ns) / std.time.ns_per_us,
                level,
                if (log_message.data) |data| data[0..log_message.size] else "",
            }) catch {
                self.dropped += 1;
                return;
            };
            self.len += line.len;
        }

        fn dump(self: *@This(), err: anyerror) void {
            self.mutex.lockUncancelable(syncIo());
            defer self.mutex.unlock(syncIo());
            std.debug.print("TLS idle replacement failed: {s} (idle=20ms sleep=100ms high=16 low=8 dropped={d})\n{s}", .{
                @errorName(err), self.dropped, self.buffer[0..self.len],
            });
        }
    };
    var diagnostics: Diagnostics = .{ .started_ns = nowNs() };
    // Registered before transport teardown so callbacks cannot outlive the capture.
    errdefer |err| diagnostics.dump(err);
    const logger: event_logger.Logger = .{ .context = &diagnostics, .callback = Diagnostics.log };
    logger.write(.debug, "test: initializing server", .{});

    const Handler = struct {
        fn handle(
            _: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var test_server = try server.Server.init(std.testing.allocator, .{
        .logger = logger,
        .connection_idle_timeout_ns = 20 * std.time.ns_per_ms,
        .write_high_watermark_bytes = 16,
        .write_low_watermark_bytes = 8,
        .tls = .{
            .certificate_chain_pem = certificate,
            .private_key_pem = private_key,
        },
    });
    defer test_server.deinit();
    try test_server.registerUnary(
        "/test.Tls/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try test_server.start();

    var runtime = try Runtime.init();
    defer runtime.deinit();
    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "localhost:{d}", .{try test_server.port()});
    logger.write(.debug, "test: initializing channel", .{});
    var channel = try Channel.init(std.testing.allocator, target, .{
        .runtime = &runtime,
        .logger = logger,
        .tls = .{ .ca_certificates_pem = certificate },
    });
    defer channel.deinit();

    logger.write(.debug, "test: first RPC start generation={d}", .{channel.impl.connection_generation.load(.monotonic)});
    var first = try channel.callUnary(std.testing.allocator, "/test.Tls/Unary", "first", .{});
    defer first.deinit();
    logger.write(.debug, "test: first RPC status={s} message={s} generation={d}", .{
        @tagName(first.status.code), first.status.message, channel.impl.connection_generation.load(.monotonic),
    });
    try std.testing.expect(first.status.isOk());
    try std.Io.sleep(std.testing.io, .fromMilliseconds(100), .awake);
    logger.write(.debug, "test: second RPC start generation={d}", .{channel.impl.connection_generation.load(.monotonic)});
    var second = try channel.callUnary(std.testing.allocator, "/test.Tls/Unary", "second", .{});
    defer second.deinit();
    logger.write(.debug, "test: second RPC status={s} message={s} payload_len={d} generation={d}", .{
        @tagName(second.status.code), second.status.message, second.payload.len, channel.impl.connection_generation.load(.monotonic),
    });
    try std.testing.expect(second.status.isOk());
    try std.testing.expectEqualStrings("second", second.payload);
}

test "TLS channel initialization times out against a silent peer" {
    if (!build_options.tls) return error.SkipZigTest;
    const certificate = @embedFile("testdata/localhost-cert.pem");
    var listen_address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var listener = try listen_address.listen(std.testing.io, .{});
    defer listener.deinit(std.testing.io);

    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(
        listener.socket.handle,
        @ptrCast(&local_address),
        &address_length,
    )) != .SUCCESS) return error.AddressQueryFailed;

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(
        &target_buffer,
        "127.0.0.1:{d}",
        .{std.mem.bigToNative(u16, local_address.port)},
    );
    try std.testing.expectError(error.ConnectionFailed, Channel.init(std.testing.allocator, target, .{
        .tls = .{
            .ca_certificates_pem = certificate,
            .handshake_timeout_ns = 10 * std.time.ns_per_ms,
        },
    }));
}

test "channel and server exchange a bidirectional stream over TLS" {
    if (!build_options.tls) return error.SkipZigTest;
    const server = @import("server.zig");
    const service = @import("service.zig");
    const certificate = @embedFile("testdata/localhost-cert.pem");
    const private_key = @embedFile("testdata/localhost-key.pem");

    const ServerHandler = struct {
        fn onStart(_: ?*anyopaque, _: stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            _: ?*anyopaque,
            server_stream: stream.ServerStream,
            _: *service.ServerContext,
            payload: []const u8,
            _: Compression,
        ) !stream.ReceiveAction {
            try server_stream.send(payload, .{});
            return .continue_receiving;
        }

        fn onRemoteEnd(
            _: ?*anyopaque,
            server_stream: stream.ServerStream,
            _: *service.ServerContext,
        ) !void {
            try server_stream.finish(.ok);
        }
    };
    const ClientState = struct {
        messages: std.atomic.Value(usize) = .init(0),
        done: std.atomic.Value(bool) = .init(false),
        succeeded: std.atomic.Value(bool) = .init(false),

        fn onMessage(
            context: ?*anyopaque,
            _: stream.ClientStream,
            payload: []const u8,
            _: Compression,
        ) stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            if (std.mem.eql(u8, payload, "one") or std.mem.eql(u8, payload, "two")) {
                _ = self.messages.fetchAdd(1, .monotonic);
            }
            return .continue_receiving;
        }

        fn onTerminal(
            context: ?*anyopaque,
            _: stream.ClientStream,
            final_status: status.Status,
            _: *const metadata.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.succeeded.store(final_status.isOk(), .release);
            self.done.store(true, .release);
        }
    };

    var test_server = try server.Server.init(std.testing.allocator, .{
        .tls = .{
            .certificate_chain_pem = certificate,
            .private_key_pem = private_key,
        },
    });
    defer test_server.deinit();
    try test_server.registerStream("/test.Tls/Bidi", .{
        .on_start = ServerHandler.onStart,
        .on_message = ServerHandler.onMessage,
        .on_remote_end = ServerHandler.onRemoteEnd,
    });
    try test_server.start();

    var runtime = try Runtime.init();
    defer runtime.deinit();
    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "localhost:{d}", .{try test_server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{
        .runtime = &runtime,
        .tls = .{ .ca_certificates_pem = certificate },
    });
    defer channel.deinit();

    var state: ClientState = .{};
    var client_stream = try channel.openStream("/test.Tls/Bidi", .{}, .{
        .context = &state,
        .on_message = ClientState.onMessage,
        .on_terminal = ClientState.onTerminal,
    });
    defer client_stream.deinit();
    try client_stream.send("one", .{});
    try client_stream.send("two", .{});
    try client_stream.closeSend();

    const deadline_ns = nowNs() +| 5 * std.time.ns_per_s;
    while (!state.done.load(.acquire)) {
        if (nowNs() >= deadline_ns) return error.StreamTimeout;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    try std.testing.expect(state.succeeded.load(.acquire));
    try std.testing.expectEqual(@as(usize, 2), state.messages.load(.acquire));
}
