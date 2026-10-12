const std = @import("std");
const builtin = @import("builtin");
const build_options = @import("grpc_lite_options");
const xev = @import("xev");
const c = @import("c.zig").api;
const Compression = @import("compression.zig").Compression;
const deadline = @import("deadline.zig");
const fast_clock = @import("fast_clock.zig");
const frame = @import("frame.zig");
const message = @import("message.zig");
const metadata = @import("metadata.zig");
const event_logger = @import("logger.zig");
const service = @import("service.zig");
const socket_options = @import("socket_options.zig");
const status = @import("status.zig");
const raw_stream = @import("stream.zig");
const tls_record = if (build_options.tls) @import("tls_record.zig") else @import("tls_disabled.zig");

// Bounds socket-write aggregation only; HTTP/2 frame boundaries remain unchanged.
const socket_write_batch_target = 64 * 1024;
const response_header_stack_capacity = 4;
const encoded_value_stack_capacity = 4;
const local_page_size = @max(std.heap.page_size_max, 128 * 1024);
const local_page_alignment = std.mem.Alignment.fromByteUnits(local_page_size);

const LocalDebugAllocator = std.heap.DebugAllocator(.{
    .thread_safe = false,
    .stack_trace_frames = 0,
    .backing_allocator_zeroes = false,
    .page_size = local_page_size,
});

const ReactorLocalAllocator = struct {
    const FreePage = struct {
        next: ?*FreePage,
    };

    shared_allocator: std.mem.Allocator,
    free_page_head: ?*FreePage = null,
    gpa: LocalDebugAllocator,
    owner_thread_id: std.Thread.Id = undefined,
    loop_running: std.atomic.Value(bool) = .init(false),

    fn init(shared_allocator: std.mem.Allocator) ReactorLocalAllocator {
        return .{
            .shared_allocator = shared_allocator,
            .gpa = .init,
        };
    }

    fn bindBacking(self: *ReactorLocalAllocator) void {
        self.gpa.backing_allocator = self.pageAllocator();
    }

    fn allocator(self: *ReactorLocalAllocator) std.mem.Allocator {
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

    fn enterLoop(self: *ReactorLocalAllocator) void {
        self.owner_thread_id = std.Thread.getCurrentId();
        self.loop_running.store(true, .release);
    }

    fn leaveLoop(self: *ReactorLocalAllocator) void {
        const was_running = self.loop_running.swap(false, .acq_rel);
        std.debug.assert(!was_running or self.owner_thread_id == std.Thread.getCurrentId());
    }

    fn assertOwner(self: *const ReactorLocalAllocator) void {
        if (std.debug.runtime_safety or builtin.is_test) {
            if (self.loop_running.load(.acquire)) {
                std.debug.assert(self.owner_thread_id == std.Thread.getCurrentId());
            }
        }
    }

    fn deinit(self: *ReactorLocalAllocator) void {
        std.debug.assert(!self.loop_running.load(.acquire));
        const check = self.gpa.deinit();
        std.debug.assert(check == .ok);
        while (self.free_page_head) |page| {
            self.free_page_head = page.next;
            self.shared_allocator.rawFree(
                @as([*]u8, @ptrCast(page))[0..local_page_size],
                local_page_alignment,
                @returnAddress(),
            );
        }
        self.* = undefined;
    }

    fn pageAllocator(self: *ReactorLocalAllocator) std.mem.Allocator {
        return .{
            .ptr = self,
            .vtable = &.{
                .alloc = pageAlloc,
                .resize = pageResize,
                .remap = pageRemap,
                .free = pageFree,
            },
        };
    }

    fn alloc(context: *anyopaque, len: usize, alignment: std.mem.Alignment, ret_addr: usize) ?[*]u8 {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        self.assertOwner();
        return self.gpa.allocator().rawAlloc(len, alignment, ret_addr);
    }

    fn resize(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, new_len: usize, ret_addr: usize) bool {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        self.assertOwner();
        return self.gpa.allocator().rawResize(memory, alignment, new_len, ret_addr);
    }

    fn remap(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, new_len: usize, ret_addr: usize) ?[*]u8 {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        self.assertOwner();
        return self.gpa.allocator().rawRemap(memory, alignment, new_len, ret_addr);
    }

    fn free(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, ret_addr: usize) void {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        self.assertOwner();
        if (memory.len == 0) return;
        self.gpa.allocator().rawFree(memory, alignment, ret_addr);
    }

    fn pageAlloc(context: *anyopaque, len: usize, alignment: std.mem.Alignment, ret_addr: usize) ?[*]u8 {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        if (len == local_page_size and alignment == local_page_alignment) {
            if (self.free_page_head) |page| {
                self.free_page_head = page.next;
                return @ptrCast(page);
            }
        }
        return self.shared_allocator.rawAlloc(len, alignment, ret_addr);
    }

    fn pageResize(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, new_len: usize, ret_addr: usize) bool {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        return self.shared_allocator.rawResize(memory, alignment, new_len, ret_addr);
    }

    fn pageRemap(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, new_len: usize, ret_addr: usize) ?[*]u8 {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        return self.shared_allocator.rawRemap(memory, alignment, new_len, ret_addr);
    }

    fn pageFree(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, ret_addr: usize) void {
        const self: *ReactorLocalAllocator = @ptrCast(@alignCast(context));
        if (memory.len == local_page_size and alignment == local_page_alignment) {
            const page: *FreePage = @ptrCast(@alignCast(memory.ptr));
            page.* = .{ .next = self.free_page_head };
            self.free_page_head = page;
            return;
        }
        self.shared_allocator.rawFree(memory, alignment, ret_addr);
    }
};

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

const HeaderBuilder = StackFirstBuilder(c.nghttp2_nv, response_header_stack_capacity);
const EncodedValueBuilder = StackFirstBuilder(metadata.OutboundValue, encoded_value_stack_capacity);

pub const TlsOptions = struct {
    certificate_chain_pem: []const u8,
    private_key_pem: []const u8,
    ca_certificates_pem: ?[]const u8 = null,
    require_client_certificate: bool = false,
    handshake_timeout_ns: u64 = 10 * std.time.ns_per_s,
};

pub const Options = struct {
    host: []const u8 = "127.0.0.1",
    port: u16 = 0,
    reactor_count: usize = 1,
    max_request_size: usize = 4 * 1024 * 1024,
    stream_limits: raw_stream.BufferLimits = .{},
    initial_stream_window_size: u32 = 64 * 1024,
    write_high_watermark_bytes: usize = 1024 * 1024,
    write_low_watermark_bytes: usize = 512 * 1024,
    tls: ?TlsOptions = null,
    logger: event_logger.Logger = .{},
    max_connections: usize = 1024,
    max_concurrent_streams_per_connection: u32 = 100,
    cleartext_preface_timeout_ns: u64 = 10 * std.time.ns_per_s,
    connection_idle_timeout_ns: u64 = 5 * 60 * std.time.ns_per_s,
};

pub const LocalAddress = struct {
    host: []const u8,
    port: u16,
};

pub const Server = struct {
    impl: *Impl,
    coordinator: *Coordinator,

    pub fn init(allocator: std.mem.Allocator, options: Options) !Server {
        if (options.reactor_count == 0) return error.InvalidReactorCount;
        if (options.max_connections == 0) return error.InvalidMaxConnections;
        if (options.max_concurrent_streams_per_connection == 0) return error.InvalidMaxConcurrentStreams;
        if (options.cleartext_preface_timeout_ns == 0) return error.InvalidCleartextPrefaceTimeout;
        if (options.connection_idle_timeout_ns == 0) return error.InvalidConnectionIdleTimeout;
        if (options.tls != null and !build_options.tls) return error.TlsUnavailable;
        if (options.tls) |tls_options| {
            if (tls_options.handshake_timeout_ns == 0) return error.InvalidTlsHandshakeTimeout;
        }
        try options.stream_limits.validate();
        try validateTransportOptions(
            options.initial_stream_window_size,
            options.write_high_watermark_bytes,
            options.write_low_watermark_bytes,
        );
        fast_clock.warmup(syncIo());
        if (options.stream_limits.max_inbound_buffer_size < options.initial_stream_window_size) {
            return error.InvalidInboundBufferSize;
        }
        const serialized_allocator = try allocator.create(SerializedAllocator);
        errdefer allocator.destroy(serialized_allocator);
        serialized_allocator.* = .init(allocator);
        const reactors = try allocator.alloc(*Impl, options.reactor_count);
        errdefer allocator.free(reactors);
        const coordinator = try allocator.create(Coordinator);
        errdefer allocator.destroy(coordinator);
        coordinator.* = .{
            .backing_allocator = allocator,
            .serialized_allocator = serialized_allocator,
            .reactors = reactors,
            .max_connections = options.max_connections,
        };

        var initialized: usize = 0;
        errdefer for (reactors[0..initialized]) |reactor| destroyImpl(reactor);
        while (initialized < reactors.len) : (initialized += 1) {
            reactors[initialized] = try initImpl(serialized_allocator.allocator(), coordinator, options);
        }

        return .{ .impl = reactors[0], .coordinator = coordinator };
    }

    pub fn registerUnary(self: *Server, full_method_path: []const u8, handler: service.UnaryHandler) !void {
        const coordinator = self.coordinator;
        coordinator.lock();
        defer coordinator.unlock();
        if (coordinator.state != .initialized) return error.ServerAlreadyStarted;
        if (!isValidMethodPath(full_method_path)) return error.InvalidMethodPath;
        if (self.impl.handlers.contains(full_method_path) or self.impl.stream_handlers.contains(full_method_path)) return error.MethodAlreadyRegistered;

        var inserted: usize = 0;
        errdefer rollbackUnaryRegistration(coordinator.reactors[0..inserted], full_method_path);
        while (inserted < coordinator.reactors.len) : (inserted += 1) {
            const reactor = coordinator.reactors[inserted];
            const local_allocator = reactor.localAllocator();
            const owned_path = try local_allocator.dupe(u8, full_method_path);
            errdefer local_allocator.free(owned_path);
            try reactor.handlers.put(local_allocator, owned_path, handler);
        }
    }

    pub fn registerStream(self: *Server, full_method_path: []const u8, handler: raw_stream.ServerHandler) !void {
        const coordinator = self.coordinator;
        coordinator.lock();
        defer coordinator.unlock();
        if (coordinator.state != .initialized) return error.ServerAlreadyStarted;
        if (!isValidMethodPath(full_method_path)) return error.InvalidMethodPath;
        if (self.impl.handlers.contains(full_method_path) or self.impl.stream_handlers.contains(full_method_path)) return error.MethodAlreadyRegistered;

        var inserted: usize = 0;
        errdefer rollbackStreamRegistration(coordinator.reactors[0..inserted], full_method_path);
        while (inserted < coordinator.reactors.len) : (inserted += 1) {
            const reactor = coordinator.reactors[inserted];
            const local_allocator = reactor.localAllocator();
            const owned_path = try local_allocator.dupe(u8, full_method_path);
            errdefer local_allocator.free(owned_path);
            try reactor.stream_handlers.put(local_allocator, owned_path, handler);
        }
    }

    pub fn start(self: *Server) !void {
        const coordinator = self.coordinator;
        coordinator.lock();
        if (coordinator.state != .initialized) {
            coordinator.unlock();
            return error.ServerAlreadyStarted;
        }
        coordinator.state = .starting;
        coordinator.start_in_progress = true;
        coordinator.unlock();

        for (coordinator.reactors, 0..) |reactor, index| {
            if (index != 0 and self.impl.configured_port == 0) {
                reactor.configured_port = self.impl.local_port;
            }
            startReactor(coordinator, reactor, index) catch |err| {
                rollbackStart(coordinator);
                return err;
            };
            waitForReactorStartup(reactor) catch |err| {
                rollbackStart(coordinator);
                return err;
            };
            if (reactor.local_port != self.impl.local_port or
                !std.mem.eql(u8, reactor.local_host[0..reactor.local_host_len], self.impl.local_host[0..self.impl.local_host_len]))
            {
                rollbackStart(coordinator);
                return error.ReactorAddressMismatch;
            }
        }

        coordinator.lock();
        coordinator.start_in_progress = false;
        coordinator.state = switch (coordinator.shutdown_request) {
            .none => .running,
            .graceful => .draining,
            .immediate => .stopping,
        };
        const log_drain = coordinator.state == .draining;
        const drain_timeout_ns = coordinator.drain_timeout_ns;
        coordinator.log_mutex.lockUncancelable(coordinator.io());
        coordinator.condition.broadcast(coordinator.io());
        coordinator.unlock();
        self.impl.logger.write(
            .info,
            "server started address={s}:{d}",
            .{ self.impl.local_host[0..self.impl.local_host_len], self.impl.local_port },
        );
        if (log_drain) {
            self.impl.logger.write(
                .info,
                "server drain requested address={s}:{d} timeout_ns={d}",
                .{
                    self.impl.local_host[0..self.impl.local_host_len],
                    self.impl.local_port,
                    drain_timeout_ns,
                },
            );
        }
        coordinator.log_mutex.unlock(coordinator.io());
    }

    pub fn localAddress(self: *const Server) !LocalAddress {
        const impl = self.impl;
        impl.lock();
        defer impl.unlock();
        if (impl.state != .running and impl.state != .draining and impl.state != .stopping) return error.ServerNotRunning;
        return .{
            .host = impl.local_host[0..impl.local_host_len],
            .port = impl.local_port,
        };
    }

    pub fn port(self: *const Server) !u16 {
        return (try self.localAddress()).port;
    }

    pub fn shutdown(self: *Server) void {
        self.requestShutdown(.immediate, 0);
    }

    pub fn shutdownGracefully(self: *Server, timeout_ns: u64) void {
        self.requestShutdown(.graceful, timeout_ns);
    }

    pub fn wait(self: *Server) void {
        const coordinator = self.coordinator;
        coordinator.lock();
        while (coordinator.start_in_progress) coordinator.condition.waitUncancelable(coordinator.io(), &coordinator.mutex);
        const launched_count = coordinator.launched_count;
        coordinator.unlock();
        for (coordinator.reactors[0..launched_count]) |reactor| waitForReactor(reactor);
        std.debug.assert(coordinator.admitted_connections.load(.acquire) == 0);
        coordinator.lock();
        const log_stopped = coordinator.state != .initialized and coordinator.state != .stopped;
        if (log_stopped) coordinator.state = .stopped;
        if (log_stopped) coordinator.log_mutex.lockUncancelable(coordinator.io());
        coordinator.unlock();
        if (log_stopped) {
            self.impl.logger.write(
                .info,
                "server stopped address={s}:{d}",
                .{ self.impl.local_host[0..self.impl.local_host_len], self.impl.local_port },
            );
            coordinator.log_mutex.unlock(coordinator.io());
        }
    }

    pub fn deinit(self: *Server) void {
        self.shutdown();
        self.wait();
        const coordinator = self.coordinator;
        for (coordinator.reactors) |reactor| destroyImpl(reactor);
        const backing_allocator = coordinator.backing_allocator;
        const serialized_allocator = coordinator.serialized_allocator;
        backing_allocator.free(coordinator.reactors);
        backing_allocator.destroy(coordinator);
        backing_allocator.destroy(serialized_allocator);
        self.* = undefined;
    }

    fn requestShutdown(self: *Server, request: ShutdownRequest, timeout_ns: u64) void {
        const coordinator = self.coordinator;
        coordinator.lock();
        var log_drain = false;
        switch (coordinator.state) {
            .initialized => coordinator.state = .stopped,
            .starting => {
                coordinator.state = if (request == .graceful) .draining else .stopping;
            },
            .running => {
                coordinator.state = if (request == .graceful) .draining else .stopping;
                log_drain = request == .graceful;
            },
            .draining => {
                if (request == .immediate) coordinator.state = .stopping;
            },
            .stopping, .stopped => {},
        }
        if (request == .immediate or coordinator.shutdown_request == .none) {
            coordinator.shutdown_request = request;
            coordinator.drain_timeout_ns = timeout_ns;
        }
        const launched_count = coordinator.launched_count;
        const initialized = coordinator.state == .stopped and !coordinator.start_in_progress and launched_count == 0;
        if (log_drain) coordinator.log_mutex.lockUncancelable(coordinator.io());
        coordinator.unlock();

        if (log_drain) {
            self.impl.logger.write(
                .info,
                "server drain requested address={s}:{d} timeout_ns={d}",
                .{ self.impl.local_host[0..self.impl.local_host_len], self.impl.local_port, timeout_ns },
            );
            coordinator.log_mutex.unlock(coordinator.io());
        }

        if (initialized) {
            for (coordinator.reactors) |reactor| shutdownReactor(reactor, request, timeout_ns);
        } else {
            for (coordinator.reactors[0..launched_count]) |reactor| shutdownReactor(reactor, request, timeout_ns);
        }
    }
};

const SerializedAllocator = struct {
    backing: std.mem.Allocator,
    mutex: std.Io.Mutex = .init,
    operation_count: if (builtin.is_test) std.atomic.Value(usize) else void = if (builtin.is_test) .init(0) else {},

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

    fn lock(self: *SerializedAllocator) void {
        self.mutex.lockUncancelable(syncIo());
    }

    fn unlock(self: *SerializedAllocator) void {
        self.mutex.unlock(syncIo());
    }

    fn recordOperation(self: *SerializedAllocator) void {
        if (comptime builtin.is_test) _ = self.operation_count.fetchAdd(1, .monotonic);
    }

    fn alloc(context: *anyopaque, len: usize, alignment: std.mem.Alignment, ret_addr: usize) ?[*]u8 {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.recordOperation();
        self.lock();
        defer self.unlock();
        return self.backing.rawAlloc(len, alignment, ret_addr);
    }

    fn resize(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, new_len: usize, ret_addr: usize) bool {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.recordOperation();
        self.lock();
        defer self.unlock();
        return self.backing.rawResize(memory, alignment, new_len, ret_addr);
    }

    fn remap(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, new_len: usize, ret_addr: usize) ?[*]u8 {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.recordOperation();
        self.lock();
        defer self.unlock();
        return self.backing.rawRemap(memory, alignment, new_len, ret_addr);
    }

    fn free(context: *anyopaque, memory: []u8, alignment: std.mem.Alignment, ret_addr: usize) void {
        const self: *SerializedAllocator = @ptrCast(@alignCast(context));
        self.recordOperation();
        self.lock();
        defer self.unlock();
        self.backing.rawFree(memory, alignment, ret_addr);
    }
};

const Coordinator = struct {
    backing_allocator: std.mem.Allocator,
    serialized_allocator: *SerializedAllocator,
    reactors: []*Impl,
    mutex: std.Io.Mutex = .init,
    condition: std.Io.Condition = .init,
    log_mutex: std.Io.Mutex = .init,
    state: State = .initialized,
    shutdown_request: ShutdownRequest = .none,
    drain_timeout_ns: u64 = 0,
    launched_count: usize = 0,
    start_in_progress: bool = false,
    max_connections: usize,
    admitted_connections: std.atomic.Value(usize) = .init(0),

    fn io(_: *Coordinator) std.Io {
        return syncIo();
    }

    fn lock(self: *Coordinator) void {
        self.mutex.lockUncancelable(self.io());
    }

    fn unlock(self: *Coordinator) void {
        self.mutex.unlock(self.io());
    }

    fn tryClaimConnection(self: *Coordinator) bool {
        var current = self.admitted_connections.load(.acquire);
        while (current < self.max_connections) {
            if (self.admitted_connections.cmpxchgWeak(
                current,
                current + 1,
                .acq_rel,
                .acquire,
            )) |actual| {
                current = actual;
            } else return true;
        }
        return false;
    }

    fn releaseConnection(self: *Coordinator) void {
        const previous = self.admitted_connections.fetchSub(1, .acq_rel);
        std.debug.assert(previous > 0 and previous <= self.max_connections);
    }
};

const State = enum { initialized, starting, running, draining, stopping, stopped };
const ShutdownRequest = enum { none, graceful, immediate };
const StartupError = error{
    LoopInitializationFailed,
    InvalidAddress,
    ListenerInitializationFailed,
    BindFailed,
    ListenFailed,
    AddressQueryFailed,
    AsyncInitializationFailed,
    TimerInitializationFailed,
};

const Impl = struct {
    // Shared storage may be touched by application and reactor threads.
    shared_allocator: std.mem.Allocator,
    coordinator: *Coordinator,
    // Transport state uses this only on the owner reactor, or before start/after join.
    local_allocator_state: ReactorLocalAllocator,
    host: [:0]u8,
    configured_port: u16,
    reuse_port: bool,
    max_request_size: usize,
    stream_limits: raw_stream.BufferLimits,
    initial_stream_window_size: u32,
    write_high_watermark_bytes: usize,
    write_low_watermark_bytes: usize,
    max_concurrent_streams_per_connection: u32,
    cleartext_preface_timeout_ns: u64,
    connection_idle_timeout_ns: u64,
    logger: event_logger.Logger,
    handlers: std.StringHashMapUnmanaged(service.UnaryHandler) = .empty,
    stream_handlers: std.StringHashMapUnmanaged(raw_stream.ServerHandler) = .empty,
    stream_commands: std.ArrayList(StreamCommand) = .empty,
    connections: std.ArrayList(*Connection) = .empty,
    deadline_heap: std.ArrayList(DeadlineEntry) = .empty,
    io_threaded: std.Io.Threaded,
    mutex: std.Io.Mutex = .init,
    condition: std.Io.Condition = .init,
    state: State = .initialized,
    shutdown_request: ShutdownRequest = .none,
    drain_timeout_ns: u64 = 0,
    startup_error: ?StartupError = null,
    thread: ?std.Thread = null,
    loop: xev.Loop = undefined,
    loop_initialized: bool = false,
    listener: xev.TCP = undefined,
    listener_initialized: bool = false,
    listener_accept_completion: xev.Completion = .{},
    listener_accept_cancel_completion: xev.Completion = .{},
    listener_accept_active: bool = false,
    listener_accept_cancel_submitted: bool = false,
    listener_close_completion: xev.Completion = .{},
    listener_close_submitted: bool = false,
    listener_closed: bool = false,
    shutdown_async: xev.Async = undefined,
    shutdown_async_initialized: bool = false,
    shutdown_completion: xev.Completion = .{},
    stream_async: xev.Async = undefined,
    stream_async_initialized: bool = false,
    stream_async_completion: xev.Completion = .{},
    dirty_connection_head: ?*Connection = null,
    dirty_connection_tail: ?*Connection = null,
    drain_timer: xev.Timer = undefined,
    drain_timer_initialized: bool = false,
    drain_timer_completion: xev.Completion = .{},
    deadline_timer: xev.Timer = undefined,
    deadline_timer_initialized: bool = false,
    deadline_timer_completion: xev.Completion = .{},
    deadline_timer_cancel_completion: xev.Completion = .{},
    deadline_timer_armed: bool = false,
    deadline_timer_target_ns: ?u64 = null,
    drain_started: bool = false,
    clock: deadline.Clock = undefined,
    local_host: [15]u8 = undefined,
    local_host_len: usize = 0,
    local_port: u16 = 0,
    tls_config: ?*tls_record.Config = null,
    tls_handshake_timeout_ns: u64 = 0,
    accepted_connections: if (builtin.is_test) std.atomic.Value(usize) else void = if (builtin.is_test) .init(0) else {},
    test_fail_startup: if (builtin.is_test) bool else void = if (builtin.is_test) false else {},

    fn localAllocator(self: *Impl) std.mem.Allocator {
        return self.local_allocator_state.allocator();
    }

    fn lock(self: *Impl) void {
        self.mutex.lockUncancelable(self.io());
    }

    fn unlock(self: *Impl) void {
        self.mutex.unlock(self.io());
    }

    fn waitForSignal(self: *Impl) void {
        self.condition.waitUncancelable(self.io(), &self.mutex);
    }

    fn signalStarted(self: *Impl, result: ?StartupError) void {
        self.lock();
        self.startup_error = result;
        self.state = if (result != null)
            .stopped
        else switch (self.shutdown_request) {
            .none => .running,
            .graceful => .draining,
            .immediate => .stopping,
        };
        self.condition.broadcast(self.io());
        const should_shutdown = result == null and self.shutdown_request != .none;
        self.unlock();
        if (should_shutdown) self.notifyShutdown();
    }

    fn io(self: *Impl) std.Io {
        return self.io_threaded.io();
    }

    fn notifyShutdown(self: *Impl) void {
        if (self.shutdown_async_initialized) self.shutdown_async.notify() catch {};
    }

    fn enqueueDirtyConnection(self: *Impl, connection: *Connection) bool {
        if (connection.dirty_queued) return false;
        const was_empty = self.dirty_connection_head == null;
        connection.dirty_queued = true;
        connection.dirty_next = null;
        if (self.dirty_connection_tail) |tail|
            tail.dirty_next = connection
        else
            self.dirty_connection_head = connection;
        self.dirty_connection_tail = connection;
        return was_empty;
    }

    fn popDirtyConnection(self: *Impl) ?*Connection {
        const connection = self.dirty_connection_head orelse return null;
        self.dirty_connection_head = connection.dirty_next;
        if (self.dirty_connection_head == null) self.dirty_connection_tail = null;
        connection.dirty_next = null;
        connection.dirty_queued = false;
        return connection;
    }

    fn removeDirtyConnection(self: *Impl, connection: *Connection) void {
        if (!connection.dirty_queued) return;
        var previous: ?*Connection = null;
        var current = self.dirty_connection_head;
        while (current) |item| {
            if (item == connection) {
                if (previous) |before|
                    before.dirty_next = item.dirty_next
                else
                    self.dirty_connection_head = item.dirty_next;
                if (self.dirty_connection_tail == item) self.dirty_connection_tail = previous;
                item.dirty_next = null;
                item.dirty_queued = false;
                return;
            }
            previous = item;
            current = item.dirty_next;
        }
        unreachable;
    }
};

fn initImpl(shared_allocator: std.mem.Allocator, coordinator: *Coordinator, options: Options) !*Impl {
    const impl = try shared_allocator.create(Impl);
    errdefer shared_allocator.destroy(impl);
    const host = try shared_allocator.dupeZ(u8, options.host);
    errdefer shared_allocator.free(host);
    var io_threaded = std.Io.Threaded.init(shared_allocator, .{});
    errdefer io_threaded.deinit();
    impl.* = .{
        .shared_allocator = shared_allocator,
        .coordinator = coordinator,
        .local_allocator_state = .init(shared_allocator),
        .host = host,
        .configured_port = options.port,
        .reuse_port = options.reactor_count > 1,
        .max_request_size = options.max_request_size,
        .stream_limits = options.stream_limits,
        .initial_stream_window_size = options.initial_stream_window_size,
        .write_high_watermark_bytes = options.write_high_watermark_bytes,
        .write_low_watermark_bytes = options.write_low_watermark_bytes,
        .max_concurrent_streams_per_connection = options.max_concurrent_streams_per_connection,
        .cleartext_preface_timeout_ns = options.cleartext_preface_timeout_ns,
        .connection_idle_timeout_ns = options.connection_idle_timeout_ns,
        .logger = options.logger,
        .io_threaded = io_threaded,
        .tls_config = null,
        .tls_handshake_timeout_ns = if (options.tls) |tls_options| tls_options.handshake_timeout_ns else 0,
    };
    impl.local_allocator_state.bindBacking();
    errdefer impl.local_allocator_state.deinit();
    if (comptime build_options.tls) {
        if (options.tls) |tls_options| {
            impl.tls_config = try tls_record.Config.createServer(
                shared_allocator,
                tls_options.certificate_chain_pem,
                tls_options.private_key_pem,
                tls_options.ca_certificates_pem,
                tls_options.require_client_certificate,
            );
        }
    }
    errdefer if (comptime build_options.tls) {
        if (impl.tls_config) |config| config.destroy();
    };
    impl.clock = .{ .context = impl, .now_fn = ioNow };
    return impl;
}

fn destroyImpl(impl: *Impl) void {
    const local_allocator = impl.localAllocator();
    var iterator = impl.handlers.iterator();
    while (iterator.next()) |entry| local_allocator.free(entry.key_ptr.*);
    impl.handlers.deinit(local_allocator);
    var stream_iterator = impl.stream_handlers.iterator();
    while (stream_iterator.next()) |entry| local_allocator.free(entry.key_ptr.*);
    impl.stream_handlers.deinit(local_allocator);
    for (impl.stream_commands.items) |*command| command.deinit(impl.shared_allocator);
    impl.stream_commands.deinit(impl.shared_allocator);
    impl.connections.deinit(local_allocator);
    std.debug.assert(impl.deadline_heap.items.len == 0);
    impl.deadline_heap.deinit(local_allocator);
    if (comptime build_options.tls) {
        if (impl.tls_config) |config| config.destroy();
    }
    impl.shared_allocator.free(impl.host);
    impl.io_threaded.deinit();
    impl.local_allocator_state.deinit();
    const shared_allocator = impl.shared_allocator;
    shared_allocator.destroy(impl);
}

fn rollbackUnaryRegistration(reactors: []*Impl, path: []const u8) void {
    for (reactors) |reactor| {
        const removed = reactor.handlers.fetchRemove(path).?;
        reactor.localAllocator().free(removed.key);
    }
}

fn rollbackStreamRegistration(reactors: []*Impl, path: []const u8) void {
    for (reactors) |reactor| {
        const removed = reactor.stream_handlers.fetchRemove(path).?;
        reactor.localAllocator().free(removed.key);
    }
}

fn startReactor(coordinator: *Coordinator, reactor: *Impl, index: usize) !void {
    coordinator.lock();
    defer coordinator.unlock();
    reactor.lock();
    if (reactor.state != .initialized) {
        reactor.unlock();
        return error.ServerAlreadyStarted;
    }
    reactor.state = .starting;
    reactor.thread = std.Thread.spawn(.{}, runLoop, .{reactor}) catch |err| {
        reactor.state = .initialized;
        reactor.unlock();
        return err;
    };
    std.debug.assert(coordinator.launched_count == index);
    coordinator.launched_count = index + 1;
    const request = coordinator.shutdown_request;
    const timeout_ns = coordinator.drain_timeout_ns;
    reactor.unlock();
    if (request != .none) shutdownReactor(reactor, request, timeout_ns);
}

fn waitForReactorStartup(reactor: *Impl) StartupError!void {
    reactor.lock();
    defer reactor.unlock();
    while (reactor.state == .starting) reactor.waitForSignal();
    if (reactor.startup_error) |err| return err;
}

fn shutdownReactor(impl: *Impl, request: ShutdownRequest, timeout_ns: u64) void {
    impl.lock();
    defer impl.unlock();
    switch (impl.state) {
        .initialized => impl.state = .stopped,
        .starting => if (request == .immediate or impl.shutdown_request == .none) {
            impl.shutdown_request = request;
            impl.drain_timeout_ns = timeout_ns;
        },
        .running => {
            impl.shutdown_request = request;
            impl.drain_timeout_ns = timeout_ns;
            impl.state = if (request == .graceful) .draining else .stopping;
            impl.notifyShutdown();
        },
        .draining => if (request == .immediate) {
            impl.shutdown_request = .immediate;
            impl.state = .stopping;
            impl.notifyShutdown();
        },
        .stopping, .stopped => {},
    }
}

fn waitForReactor(impl: *Impl) void {
    impl.lock();
    const thread = impl.thread;
    impl.thread = null;
    impl.unlock();
    if (thread) |running_thread| running_thread.join();
}

fn rollbackStart(coordinator: *Coordinator) void {
    coordinator.lock();
    coordinator.shutdown_request = .immediate;
    coordinator.state = .stopped;
    coordinator.start_in_progress = false;
    const launched_count = coordinator.launched_count;
    coordinator.condition.broadcast(coordinator.io());
    coordinator.unlock();
    for (coordinator.reactors[0..launched_count]) |reactor| shutdownReactor(reactor, .immediate, 0);
    for (coordinator.reactors[0..launched_count]) |reactor| waitForReactor(reactor);
}

const StreamCommand = struct {
    target: *Stream,
    action: union(enum) {
        initial_metadata: struct {
            entries: metadata.Metadata,
            compression: Compression,
        },
        send: []u8,
        finish: struct {
            code: status.Code,
            message: []const u8,
            trailing_metadata: ?metadata.Metadata,
        },
        resume_receive,
    },

    fn deinit(self: *StreamCommand, allocator: std.mem.Allocator) void {
        switch (self.action) {
            .initial_metadata => |*value| value.entries.deinit(),
            .send => |bytes| allocator.free(bytes),
            .finish => |*value| {
                if (value.message.len != 0) allocator.free(value.message);
                if (value.trailing_metadata) |*entries| entries.deinit();
            },
            .resume_receive => {},
        }
        self.* = undefined;
    }
};

/// Consumes command-owned storage on both success and failure. The server lock
/// must be held so notification rollback always removes this exact command.
fn enqueueStreamCommandLocked(server: *Impl, target: *Stream, command: StreamCommand) !void {
    var owned_command = command;
    var command_owned = true;
    defer if (command_owned) owned_command.deinit(server.shared_allocator);

    const notify = server.stream_commands.items.len == 0;
    try server.stream_commands.append(server.shared_allocator, command);
    command_owned = false;
    target.command_refs += 1;
    if (notify) server.stream_async.notify() catch |err| {
        var queued_command = server.stream_commands.pop().?;
        target.command_refs -= 1;
        queued_command.deinit(server.shared_allocator);
        return err;
    };
}

const ServerCallControl = struct {
    allocator: std.mem.Allocator,
    server: *Impl,
    target: ?*Stream,
    references: std.atomic.Value(usize) = .init(1),
    cancelled: std.atomic.Value(bool) = .init(false),
    terminal: std.atomic.Value(bool) = .init(false),

    fn handle(self: *ServerCallControl) raw_stream.ServerCall {
        return raw_stream.ServerCall.initAbortable(
            self,
            serverCallId,
            serverCallIsCancelled,
            serverCallIsTerminal,
            serverCallAbort,
            serverCallSendInitialMetadata,
            serverCallSend,
            serverCallFinish,
            serverCallResumeReceive,
            serverCallRetain,
            serverCallRelease,
        );
    }
};

const OutboundMessage = struct {
    bytes: []u8,
    allocator: std.mem.Allocator,
    offset: usize = 0,
};

const StreamingState = struct {
    handler: raw_stream.ServerHandler,
    decoder: frame.Decoder,
    context: service.ServerContext,
    outbound: std.ArrayList(OutboundMessage) = .empty,
    outbound_head: usize = 0,
    remote_end_received: bool = false,
    remote_end_called: bool = false,

    fn nextOutbound(self: *StreamingState) ?*OutboundMessage {
        std.debug.assert(self.outbound_head <= self.outbound.items.len);
        if (self.outbound_head == self.outbound.items.len) return null;
        return &self.outbound.items[self.outbound_head];
    }

    fn finishOutbound(self: *StreamingState) void {
        const item = self.nextOutbound().?;
        item.allocator.free(item.bytes);
        self.outbound_head += 1;
        if (self.outbound_head == self.outbound.items.len) {
            self.outbound.clearRetainingCapacity();
            self.outbound_head = 0;
        }
    }

    fn clearOutbound(self: *StreamingState) usize {
        std.debug.assert(self.outbound_head <= self.outbound.items.len);
        var reserved_bytes: usize = 0;
        for (self.outbound.items[self.outbound_head..]) |item| {
            reserved_bytes += item.bytes.len - item.offset;
            item.allocator.free(item.bytes);
        }
        self.outbound.clearRetainingCapacity();
        self.outbound_head = 0;
        return reserved_bytes;
    }

    fn deinit(self: *StreamingState, allocator: std.mem.Allocator) void {
        self.decoder.deinit();
        self.context.deinit();
        _ = self.clearOutbound();
        self.outbound.deinit(allocator);
        self.* = undefined;
    }
};

const Stream = struct {
    allocator: std.mem.Allocator,
    connection: *Connection,
    id: i32,
    path: ?[]u8 = null,
    method_post: bool = false,
    content_type_grpc: bool = false,
    request_compression: ?Compression = .identity,
    accepts_response_gzip: bool = false,
    response_compression: Compression = .identity,
    timeout_seen: bool = false,
    timeout_invalid: bool = false,
    deadline: ?deadline.Deadline = null,
    deadline_heap_index: ?usize = null,
    request_metadata_invalid: bool = false,
    header_too_large: bool = false,
    request_too_large: bool = false,
    responded: bool = false,
    trailer_submitted: bool = false,
    header_bytes: usize = 0,
    request_body: std.ArrayList(u8) = .empty,
    request_metadata: metadata.Metadata,
    response_body: []u8 = &.{},
    response_offset: usize = 0,
    trailing_metadata: metadata.Metadata,
    response_code: status.Code = .ok,
    response_message: []const u8 = &.{},
    response_message_allocator: ?std.mem.Allocator = null,
    streaming: ?StreamingState = null,
    streaming_active: bool = false,
    response_finished: bool = false,
    finish_queued: bool = false,
    receive_paused: bool = false,
    resume_queued: bool = false,
    resume_requested: bool = false,
    message_callback_active: bool = false,
    deferred_stream_credit: usize = 0,
    response_gzip_requested: bool = false,
    response_headers_queued: bool = false,
    response_headers_submitted: bool = false,
    outbound_reserved_bytes: usize = 0,
    writable_requested: bool = false,
    cancel_called: bool = false,
    terminal_reason: raw_stream.ServerTerminalReason = .completed,
    call_control: ?*ServerCallControl = null,
    reset_after_trailers: bool = false,
    reset_submitted: bool = false,
    // Queued and currently processing commands each retain this stream once.
    command_refs: usize = 0,
    transport_closed: bool = false,
    force_abort_requested: bool = false,

    fn init(allocator: std.mem.Allocator, connection: *Connection, id: i32) Stream {
        return .{
            .allocator = allocator,
            .connection = connection,
            .id = id,
            .request_metadata = metadata.Metadata.init(allocator),
            .trailing_metadata = metadata.Metadata.init(allocator),
        };
    }

    fn deinit(self: *Stream) void {
        removeStreamDeadline(self);
        if (self.streaming) |*streaming| streaming.deinit(self.allocator);
        if (self.path) |path| self.allocator.free(path);
        self.request_body.deinit(self.allocator);
        self.request_metadata.deinit();
        if (self.response_body.len != 0) self.allocator.free(self.response_body);
        if (self.response_message_allocator) |owner| owner.free(self.response_message);
        self.trailing_metadata.deinit();
        self.* = undefined;
    }

    fn setStatus(self: *Stream, response_status: status.Status) !void {
        const owned_message = if (response_status.message.len == 0)
            &.{}
        else
            try self.allocator.dupe(u8, response_status.message);
        if (self.response_message_allocator) |owner| owner.free(self.response_message);
        self.response_code = response_status.code;
        self.response_message = owned_message;
        self.response_message_allocator = if (owned_message.len == 0) null else self.allocator;
    }

    fn setOwnedStatus(
        self: *Stream,
        code: status.Code,
        owned_message: []const u8,
        owner: std.mem.Allocator,
    ) void {
        if (self.response_message_allocator) |old_owner| old_owner.free(self.response_message);
        self.response_code = code;
        self.response_message = owned_message;
        self.response_message_allocator = if (owned_message.len == 0) null else owner;
    }

    fn serverHandle(self: *Stream) raw_stream.ServerStream {
        return raw_stream.ServerStream.initRetainable(
            self,
            streamSend,
            streamFinish,
            streamResumeReceive,
            serverStreamCallId,
            serverStreamRetain,
        );
    }
};

fn serverCallId(context: *anyopaque) raw_stream.ServerCallId {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    return @enumFromInt(@intFromPtr(control));
}

fn serverCallIsCancelled(context: *anyopaque) bool {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    return control.cancelled.load(.acquire);
}

fn serverCallIsTerminal(context: *anyopaque) bool {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    return control.terminal.load(.acquire);
}

fn serverCallAbort(context: *anyopaque) void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const server = control.server;
    server.lock();
    const notify = if (control.target) |target| blk: {
        if (!target.streaming_active or target.transport_closed or target.force_abort_requested) break :blk false;
        target.force_abort_requested = true;
        target.finish_queued = true;
        break :blk true;
    } else false;
    if (notify and server.stream_async_initialized) server.stream_async.notify() catch {};
    server.unlock();
}

fn serverCallRetain(context: *anyopaque) void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const previous = control.references.fetchAdd(1, .monotonic);
    std.debug.assert(previous != 0 and previous != std.math.maxInt(usize));
}

fn serverCallRelease(context: *anyopaque) void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const previous = control.references.fetchSub(1, .acq_rel);
    std.debug.assert(previous != 0);
    if (previous == 1) control.allocator.destroy(control);
}

fn serverStreamCallId(context: *anyopaque) raw_stream.ServerCallId {
    const target: *Stream = @ptrCast(@alignCast(context));
    const control = target.call_control orelse return @enumFromInt(@intFromPtr(target));
    return serverCallId(control);
}

fn serverStreamRetain(context: *anyopaque) !raw_stream.ServerCall {
    const target: *Stream = @ptrCast(@alignCast(context));
    const control = target.call_control orelse return error.ServerCallUnavailable;
    serverCallRetain(control);
    return control.handle();
}

fn cloneMetadata(allocator: std.mem.Allocator, entries: []const metadata.Entry) !metadata.Metadata {
    var result = metadata.Metadata.init(allocator);
    errdefer result.deinit();
    for (entries) |entry| try result.append(entry.key, entry.value);
    return result;
}

fn serverCallSendInitialMetadata(
    context: *anyopaque,
    entries: []const metadata.Entry,
    response_compression: Compression,
) !void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const server = control.server;
    const owned_entries = try cloneMetadata(server.shared_allocator, entries);
    var command = StreamCommand{ .target = undefined, .action = .{ .initial_metadata = .{
        .entries = owned_entries,
        .compression = response_compression,
    } } };
    var command_owned = true;
    errdefer if (command_owned) command.deinit(server.shared_allocator);

    server.lock();
    defer server.unlock();
    const target = control.target orelse return error.CallClosed;
    command.target = target;
    if (!target.streaming_active or target.finish_queued or target.transport_closed) return error.CallClosed;
    if (target.streaming.?.handler.initial_metadata_mode != .explicit) return error.InitialMetadataNotExplicit;
    if (target.response_headers_queued) return error.InitialMetadataAlreadySent;
    target.response_headers_queued = true;
    target.response_compression = if (target.accepts_response_gzip and response_compression == .gzip)
        .gzip
    else
        .identity;
    errdefer {
        target.response_headers_queued = false;
        target.response_compression = .identity;
    }
    command_owned = false;
    try enqueueStreamCommandLocked(server, target, command);
}

fn serverCallSend(context: *anyopaque, payload: []const u8, options: raw_stream.SendOptions) !void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const server = control.server;
    if (payload.len > server.stream_limits.max_message_size) return error.MessageTooLarge;

    const encoded = try frame.encodeWithCompression(server.shared_allocator, payload, options.compression);
    var command = StreamCommand{ .target = undefined, .action = .{ .send = encoded } };
    var command_owned = true;
    errdefer if (command_owned) command.deinit(server.shared_allocator);

    server.lock();
    defer server.unlock();
    const target = control.target orelse return error.CallClosed;
    command.target = target;
    if (!target.streaming_active or target.finish_queued or target.transport_closed) return error.CallClosed;
    if (!target.response_headers_queued) return error.InitialMetadataRequired;
    if (options.compression == .gzip and !target.accepts_response_gzip) return error.CompressionNotAccepted;
    if (target.streaming.?.handler.initial_metadata_mode == .explicit and
        options.compression == .gzip and target.response_compression != .gzip)
    {
        return error.ResponseCompressionNotEnabled;
    }
    const outbound_limit = server.stream_limits.max_outbound_buffer_size;
    if (encoded.len > outbound_limit) return error.OutboundBufferLimitExceeded;
    if (target.outbound_reserved_bytes > outbound_limit or encoded.len > outbound_limit - target.outbound_reserved_bytes) {
        target.writable_requested = true;
        return error.WouldBlock;
    }
    target.outbound_reserved_bytes += encoded.len;
    errdefer target.outbound_reserved_bytes -= encoded.len;
    if (options.compression == .gzip) target.response_gzip_requested = true;
    command_owned = false;
    try enqueueStreamCommandLocked(server, target, command);
}

fn serverCallFinish(
    context: *anyopaque,
    final_status: status.Status,
    trailing_metadata: []const metadata.Entry,
) !void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const server = control.server;
    const owned_message = if (final_status.message.len == 0)
        &.{}
    else
        try server.shared_allocator.dupe(u8, final_status.message);
    const owned_metadata = cloneMetadata(server.shared_allocator, trailing_metadata) catch |err| {
        if (owned_message.len != 0) server.shared_allocator.free(owned_message);
        return err;
    };
    var command = StreamCommand{ .target = undefined, .action = .{ .finish = .{
        .code = final_status.code,
        .message = owned_message,
        .trailing_metadata = owned_metadata,
    } } };
    var command_owned = true;
    errdefer if (command_owned) command.deinit(server.shared_allocator);

    server.lock();
    defer server.unlock();
    const target = control.target orelse return error.CallClosed;
    command.target = target;
    if (!target.streaming_active or target.finish_queued or target.transport_closed) return error.CallClosed;
    if (!target.response_headers_queued) return error.InitialMetadataRequired;
    target.finish_queued = true;
    errdefer target.finish_queued = false;
    command_owned = false;
    try enqueueStreamCommandLocked(server, target, command);
}

fn serverCallResumeReceive(context: *anyopaque) !void {
    const control: *ServerCallControl = @ptrCast(@alignCast(context));
    const server = control.server;
    server.lock();
    defer server.unlock();
    const target = control.target orelse return error.CallClosed;
    if (!target.streaming_active or target.finish_queued or target.transport_closed) return error.CallClosed;
    if (target.resume_queued or target.resume_requested) return;
    if (!target.receive_paused) {
        if (!target.message_callback_active) return error.ReceiveNotPaused;
        target.resume_requested = true;
        return;
    }
    target.resume_queued = true;
    errdefer target.resume_queued = false;
    try enqueueStreamCommandLocked(server, target, .{
        .target = target,
        .action = .resume_receive,
    });
}

fn streamSend(context: *anyopaque, payload: []const u8, options: raw_stream.SendOptions) !void {
    const target: *Stream = @ptrCast(@alignCast(context));
    const server = target.connection.server;
    if (payload.len > server.stream_limits.max_message_size) return error.MessageTooLarge;
    if (options.compression == .gzip and !target.accepts_response_gzip) return error.CompressionNotAccepted;

    const encoded = try frame.encodeWithCompression(server.shared_allocator, payload, options.compression);
    var command = StreamCommand{ .target = target, .action = .{ .send = encoded } };
    var command_owned = true;
    errdefer if (command_owned) command.deinit(server.shared_allocator);

    server.lock();
    defer server.unlock();
    if (!target.streaming_active or target.finish_queued) return error.StreamFinished;
    if (target.streaming.?.handler.initial_metadata_mode == .explicit and !target.response_headers_queued) {
        return error.InitialMetadataRequired;
    }
    if (target.streaming.?.handler.initial_metadata_mode == .explicit and
        options.compression == .gzip and target.response_compression != .gzip)
    {
        return error.ResponseCompressionNotEnabled;
    }
    const outbound_limit = server.stream_limits.max_outbound_buffer_size;
    if (encoded.len > outbound_limit) return error.OutboundBufferLimitExceeded;
    if (target.outbound_reserved_bytes > outbound_limit or encoded.len > outbound_limit - target.outbound_reserved_bytes) {
        target.writable_requested = true;
        return error.WouldBlock;
    }
    target.outbound_reserved_bytes += encoded.len;
    errdefer target.outbound_reserved_bytes -= encoded.len;
    if (options.compression == .gzip) target.response_gzip_requested = true;
    command_owned = false;
    try enqueueStreamCommandLocked(server, target, command);
}

fn streamFinish(context: *anyopaque, final_status: status.Status) !void {
    const target: *Stream = @ptrCast(@alignCast(context));
    const server = target.connection.server;
    const owned_message = if (final_status.message.len == 0)
        &.{}
    else
        try server.shared_allocator.dupe(u8, final_status.message);
    var command = StreamCommand{ .target = target, .action = .{ .finish = .{
        .code = final_status.code,
        .message = owned_message,
        .trailing_metadata = null,
    } } };
    var command_owned = true;
    errdefer if (command_owned) command.deinit(server.shared_allocator);

    server.lock();
    defer server.unlock();
    if (!target.streaming_active or target.finish_queued) return error.StreamFinished;
    if (target.streaming.?.handler.initial_metadata_mode == .explicit and !target.response_headers_queued) {
        return error.InitialMetadataRequired;
    }
    target.finish_queued = true;
    errdefer target.finish_queued = false;
    command_owned = false;
    try enqueueStreamCommandLocked(server, target, command);
}

fn streamResumeReceive(context: *anyopaque) !void {
    const target: *Stream = @ptrCast(@alignCast(context));
    const server = target.connection.server;
    server.lock();
    defer server.unlock();
    if (!target.streaming_active) return error.StreamFinished;
    if (target.resume_queued or target.resume_requested) return;
    if (!target.receive_paused) {
        if (!target.message_callback_active) return error.ReceiveNotPaused;
        target.resume_requested = true;
        return;
    }
    target.resume_queued = true;
    errdefer target.resume_queued = false;
    try enqueueStreamCommandLocked(server, target, .{
        .target = target,
        .action = .resume_receive,
    });
}

const ConnectionDeadlineKind = enum {
    tls_handshake,
    cleartext_preface,
    idle,
};

const ConnectionDeadline = struct {
    kind: ConnectionDeadlineKind,
    expires_at_ns: u64,
};

const Connection = struct {
    server: *Impl,
    owns_admission: bool = false,
    tcp: xev.TCP = undefined,
    session: ?*c.nghttp2_session = null,
    streams: std.AutoHashMapUnmanaged(i32, *Stream) = .empty,
    highest_accepted_stream_id: i32 = 0,
    local_goaway_submitted: bool = false,
    pending_writes: usize = 0,
    queued_write_bytes: usize = 0,
    draining: bool = false,
    close_after_writes: bool = false,
    closing: bool = false,
    read_active: bool = false,
    read_cancel_submitted: bool = false,
    write_cancel_submitted: bool = false,
    write_cancel_target: ?*xev.Completion = null,
    close_submitted: bool = false,
    close_completed: bool = false,
    read_completion: xev.Completion = .{},
    read_cancel_completion: xev.Completion = .{},
    write_cancel_completion: xev.Completion = .{},
    close_completion: xev.Completion = .{},
    write_queue: xev.WriteQueue = .{},
    write_request_pool_head: ?*WriteRequest = null,
    write_request_pool_count: usize = 0,
    read_buffer: []u8 = &.{},
    plaintext_buffer: [16 * 1024]u8 = undefined,
    tls_session: ?*tls_record.Session = null,
    tls_handshaking: bool = false,
    connection_deadline: ?ConnectionDeadline = null,
    deadline_heap_index: ?usize = null,
    tls_handshake_needs_write: bool = false,
    tls_plaintext: ?[]u8 = null,
    tls_plaintext_offset: usize = 0,
    dirty_next: ?*Connection = null,
    dirty_queued: bool = false,

    fn allocator(self: *Connection) std.mem.Allocator {
        return self.server.localAllocator();
    }

    fn initializeSession(self: *Connection) !void {
        var callbacks: ?*c.nghttp2_session_callbacks = null;
        if (c.nghttp2_session_callbacks_new(&callbacks) != 0) return error.OutOfMemory;
        defer c.nghttp2_session_callbacks_del(callbacks);
        var options: ?*c.nghttp2_option = null;
        if (c.nghttp2_option_new(&options) != 0) return error.OutOfMemory;
        defer c.nghttp2_option_del(options);
        c.nghttp2_option_set_no_auto_window_update(options, 1);

        c.nghttp2_session_callbacks_set_on_begin_headers_callback(callbacks, onBeginHeaders);
        c.nghttp2_session_callbacks_set_on_header_callback(callbacks, onHeader);
        c.nghttp2_session_callbacks_set_on_data_chunk_recv_callback(callbacks, onDataChunk);
        c.nghttp2_session_callbacks_set_on_frame_recv_callback(callbacks, onFrameReceived);
        c.nghttp2_session_callbacks_set_on_frame_send_callback(callbacks, onFrameSent);
        c.nghttp2_session_callbacks_set_on_stream_close_callback(callbacks, onStreamClose);
        if (c.nghttp2_session_server_new2(&self.session, callbacks, self, options) != 0) return error.OutOfMemory;
        const settings = [_]c.nghttp2_settings_entry{
            .{
                .settings_id = c.NGHTTP2_SETTINGS_INITIAL_WINDOW_SIZE,
                .value = self.server.initial_stream_window_size,
            },
            .{
                .settings_id = c.NGHTTP2_SETTINGS_MAX_CONCURRENT_STREAMS,
                .value = self.server.max_concurrent_streams_per_connection,
            },
        };
        if (c.nghttp2_submit_settings(self.session, c.NGHTTP2_FLAG_NONE, &settings, settings.len) != 0) {
            return error.NativeFailure;
        }
    }

    fn flush(self: *Connection) !void {
        if (comptime build_options.tls) {
            if (self.tls_session != null) return self.flushTls();
        }
        var batch: CleartextWriteBatch = .{};
        defer batch.deinit(self.allocator());
        while (!self.closing) {
            if (!canFlushWritesWithPending(
                self.queued_write_bytes,
                batch.pendingBytes(),
                self.server.write_high_watermark_bytes,
            )) break;
            var data: [*c]const u8 = null;
            const length = c.nghttp2_session_mem_send2(self.session, &data);
            if (length < 0) return error.NativeFailure;
            if (length == 0) break;
            if (try batch.append(
                self.allocator(),
                data[0..@intCast(length)],
                socket_write_batch_target,
            )) |bytes| try self.queueOwnedSocketWrite(bytes);
            if (batch.ready(socket_write_batch_target)) {
                try self.queueOwnedSocketWrite((try batch.take(self.allocator())).?);
            }
        }
        if (try batch.take(self.allocator())) |bytes| try self.queueOwnedSocketWrite(bytes);
    }

    fn flushTls(self: *Connection) !void {
        const tls_session = self.tls_session.?;
        while (!self.closing) {
            try self.drainTlsCiphertext();
            if (tls_session.hasPendingWrite()) {
                const result = try tls_session.continueWrite();
                if (try self.finishTlsWrite(result)) continue;
                return;
            }
            if (self.tls_plaintext) |plaintext| {
                if (self.tls_plaintext_offset == plaintext.len) {
                    self.allocator().free(plaintext);
                    self.tls_plaintext = null;
                    self.tls_plaintext_offset = 0;
                    continue;
                }
                const result = try tls_session.beginWrite(plaintext[self.tls_plaintext_offset..]);
                if (try self.finishTlsWrite(result)) continue;
                return;
            }
            if (!canFlushWrites(self.queued_write_bytes, self.server.write_high_watermark_bytes)) return;
            var data: [*c]const u8 = null;
            const length = c.nghttp2_session_mem_send2(self.session, &data);
            if (length < 0) return error.NativeFailure;
            if (length == 0) return;
            self.tls_plaintext = try self.allocator().dupe(u8, data[0..@intCast(length)]);
        }
    }

    fn flushIdleGoAway(self: *Connection) !bool {
        try self.flush();
        if (c.nghttp2_session_want_write(self.session) != 0) return false;
        if (comptime build_options.tls) {
            if (self.tls_session) |tls_session| {
                return self.tls_plaintext == null and
                    !tls_session.hasPendingWrite() and
                    tls_session.ciphertext().len == 0;
            }
        }
        return true;
    }

    fn finishTlsWrite(self: *Connection, result: tls_record.Result) !bool {
        switch (result) {
            .bytes => |count| {
                self.tls_plaintext_offset += count;
                try self.drainTlsCiphertext();
                return true;
            },
            .want_write => {
                try self.drainTlsCiphertext();
                return true;
            },
            .want_read => {
                try self.drainTlsCiphertext();
                return false;
            },
            else => return error.TlsWriteFailed,
        }
    }

    fn queueSocketWrite(self: *Connection, source: []const u8) !void {
        const bytes = try self.allocator().dupe(u8, source);
        try self.queueOwnedSocketWrite(bytes);
    }

    fn queueOwnedSocketWrite(self: *Connection, bytes: []u8) !void {
        const write = acquireWriteRequest(self, bytes) catch |err| {
            self.allocator().free(bytes);
            return err;
        };
        errdefer releaseWriteRequest(self, write);
        errdefer self.allocator().free(bytes);
        self.pending_writes = std.math.add(usize, self.pending_writes, 1) catch {
            return error.WriteQueueSizeOverflow;
        };
        errdefer self.pending_writes -= 1;
        self.queued_write_bytes = try addQueuedWriteBytes(self.queued_write_bytes, bytes.len);
        self.tcp.queueWrite(
            &self.server.loop,
            &self.write_queue,
            &write.request,
            .{ .slice = bytes },
            WriteRequest,
            write,
            onWrite,
        );
    }

    fn drainTlsCiphertext(self: *Connection) !void {
        if (comptime !build_options.tls) return;
        const tls_session = self.tls_session orelse return;
        const ciphertext = tls_session.ciphertext();
        if (ciphertext.len == 0) return;
        try self.queueSocketWrite(ciphertext);
        tls_session.consumeCiphertext(ciphertext.len);
    }

    fn driveTlsHandshake(self: *Connection) !void {
        if (comptime !build_options.tls) return error.TlsUnavailable;
        const tls_session = self.tls_session orelse return error.TlsUnavailable;
        while (true) {
            self.tls_handshake_needs_write = false;
            const result = tls_session.handshake();
            try self.drainTlsCiphertext();
            switch (result) {
                .complete => {
                    self.tls_handshaking = false;
                    clearConnectionDeadline(self);
                    try armIdleDeadline(self);
                    try self.flush();
                    try self.receiveTlsPlaintext();
                    return;
                },
                .want_write => {
                    self.tls_handshake_needs_write = true;
                    return;
                },
                .want_read => return,
                else => return error.TlsHandshakeFailed,
            }
        }
    }

    fn receiveTlsPlaintext(self: *Connection) !void {
        if (comptime !build_options.tls) return error.TlsUnavailable;
        const tls_session = self.tls_session orelse return error.TlsUnavailable;
        while (true) {
            switch (try tls_session.read(&self.plaintext_buffer)) {
                .bytes => |length| {
                    const consumed = c.nghttp2_session_mem_recv2(
                        self.session,
                        self.plaintext_buffer[0..length].ptr,
                        length,
                    );
                    if (consumed < 0 or consumed != @as(c.nghttp2_ssize, @intCast(length)))
                        return error.Http2ConnectionFailed;
                },
                .want_read => break,
                .want_write => try self.drainTlsCiphertext(),
                .peer_closed => return error.TlsPeerClosed,
                else => return error.TlsConnectionClosed,
            }
        }
        try self.flush();
    }

    fn clearTls(self: *Connection) void {
        if (comptime build_options.tls) {
            if (self.tls_session) |session| session.destroy();
        }
        self.tls_session = null;
        clearConnectionDeadline(self);
        self.tls_handshake_needs_write = false;
        if (self.tls_plaintext) |plaintext| self.allocator().free(plaintext);
        self.tls_plaintext = null;
        self.tls_plaintext_offset = 0;
    }

    fn startRead(self: *Connection, loop: *xev.Loop) !void {
        if (self.close_after_writes or self.closing or self.read_active) return;
        if (self.read_buffer.len == 0) self.read_buffer = try self.allocator().alloc(u8, 16 * 1024);
        self.read_active = true;
        self.tcp.read(loop, &self.read_completion, .{ .slice = self.read_buffer }, Connection, self, onRead);
    }

    fn closeTerminatedSession(self: *Connection, loop: *xev.Loop) void {
        if (self.local_goaway_submitted or
            self.closing or
            c.nghttp2_session_want_read(self.session) != 0 or
            c.nghttp2_session_want_write(self.session) != 0) return;
        self.closeGracefully(loop);
    }

    fn closeGracefully(self: *Connection, loop: *xev.Loop) void {
        if (self.closing or self.close_after_writes) return;
        if (comptime build_options.tls) {
            if (self.tls_session) |tls_session| {
                while (true) {
                    const result = tls_session.closeNotify() catch {
                        self.closeOnLoop(loop);
                        return;
                    };
                    self.drainTlsCiphertext() catch {
                        self.closeOnLoop(loop);
                        return;
                    };
                    switch (result) {
                        .want_write => continue,
                        .complete, .want_read => break,
                        else => {
                            self.closeOnLoop(loop);
                            return;
                        },
                    }
                }
            }
        }
        self.close_after_writes = true;
        if (self.pending_writes == 0) self.closeOnLoop(loop);
    }

    pub fn submitGoAway(self: *Connection, last_stream_id: i32, error_code: u32) !void {
        self.local_goaway_submitted = true;
        if (c.nghttp2_submit_goaway(
            self.session,
            c.NGHTTP2_FLAG_NONE,
            last_stream_id,
            error_code,
            null,
            0,
        ) != 0) return error.NativeFailure;
    }

    fn close(self: *Connection) void {
        if (!self.server.loop_initialized) {
            clearConnectionDeadline(self);
            self.closing = true;
            return;
        }
        self.closeOnLoop(&self.server.loop);
    }

    fn closeOnLoop(self: *Connection, loop: *xev.Loop) void {
        self.server.removeDirtyConnection(self);
        clearConnectionDeadline(self);
        if (self.closing) return;
        self.closing = true;
        var stream_iterator = self.streams.valueIterator();
        while (stream_iterator.next()) |stream_ptr| {
            const stream = stream_ptr.*;
            if (stream.streaming != null and !stream.trailer_submitted) {
                cancelStreaming(stream, connectionTerminalReason(self));
            }
        }
        if (self.read_active) {
            self.read_cancel_submitted = true;
            loop.cancel(
                &self.read_completion,
                &self.read_cancel_completion,
                Connection,
                self,
                onReadCanceled,
            );
        }
        if (self.write_queue.head) |request| {
            if (request.completion.state() == .active) {
                self.write_cancel_submitted = true;
                self.write_cancel_target = &request.completion;
                loop.cancel(
                    &request.completion,
                    &self.write_cancel_completion,
                    Connection,
                    self,
                    onWriteCanceled,
                );
            } else {
                self.discardQueuedWrites();
            }
        }
        self.submitCloseIfReady(loop);
    }

    fn submitCloseIfReady(self: *Connection, loop: *xev.Loop) void {
        if (self.close_submitted or self.read_active or self.pending_writes != 0) return;
        self.close_submitted = true;
        self.tcp.close(loop, &self.close_completion, Connection, self, onConnectionClosed);
    }

    fn discardQueuedWrites(self: *Connection) void {
        while (self.write_queue.pop()) |request| {
            const write: *WriteRequest = @fieldParentPtr("request", request);
            self.pending_writes -= 1;
            self.queued_write_bytes = completeQueuedWrite(self.queued_write_bytes, write.bytes.len);
            self.allocator().free(write.bytes);
            releaseWriteRequest(self, write);
        }
    }

    fn finishCloseIfReady(self: *Connection) void {
        if (!self.close_completed or self.read_active or self.pending_writes != 0 or self.read_cancel_submitted or self.write_cancel_submitted) return;
        const server = self.server;
        server.removeDirtyConnection(self);
        clearConnectionDeadline(self);
        var cancel_iterator = self.streams.valueIterator();
        while (cancel_iterator.next()) |stream_ptr| {
            const stream = stream_ptr.*;
            if (stream.streaming != null and !stream.trailer_submitted) {
                cancelStreaming(stream, connectionTerminalReason(self));
            }
        }
        if (self.session) |session| c.nghttp2_session_del(session);
        self.clearTls();
        var iterator = self.streams.iterator();
        while (iterator.next()) |entry| {
            const stream = entry.value_ptr.*;
            if (stream.streaming != null and !stream.trailer_submitted) {
                cancelStreaming(stream, connectionTerminalReason(self));
            }
            std.debug.assert(retireStream(stream));
        }
        self.streams.deinit(self.allocator());
        if (self.read_buffer.len != 0) self.allocator().free(self.read_buffer);
        std.debug.assert(self.pending_writes == 0);
        std.debug.assert(self.write_queue.head == null);
        drainWriteRequestPool(self);
        var removed = false;
        for (server.connections.items, 0..) |item, index| {
            if (item == self) {
                _ = server.connections.swapRemove(index);
                removed = true;
                break;
            }
        }
        std.debug.assert(removed);
        if (self.owns_admission) {
            self.owns_admission = false;
            server.coordinator.releaseConnection();
        }
        server.localAllocator().destroy(self);
        finishDrainIfIdle(server);
        maybeStopLoop(server);
    }
};

const DeadlineTarget = union(enum) {
    stream: *Stream,
    connection: *Connection,
};

const DeadlineEntry = struct {
    expires_at_ns: u64,
    target: DeadlineTarget,
};

fn deadlineTargetIndex(target: DeadlineTarget) *?usize {
    return switch (target) {
        .stream => |stream| &stream.deadline_heap_index,
        .connection => |connection| &connection.deadline_heap_index,
    };
}

fn deadlineTargetsEqual(a: DeadlineTarget, b: DeadlineTarget) bool {
    return switch (a) {
        .stream => |stream| switch (b) {
            .stream => |other| stream == other,
            .connection => false,
        },
        .connection => |connection| switch (b) {
            .stream => false,
            .connection => |other| connection == other,
        },
    };
}

fn deadlineHeapSwap(server: *Impl, a: usize, b: usize) void {
    if (a == b) return;
    std.mem.swap(DeadlineEntry, &server.deadline_heap.items[a], &server.deadline_heap.items[b]);
    deadlineTargetIndex(server.deadline_heap.items[a].target).* = a;
    deadlineTargetIndex(server.deadline_heap.items[b].target).* = b;
}

fn deadlineHeapSiftUp(server: *Impl, start: usize) usize {
    var index = start;
    while (index != 0) {
        const parent = (index - 1) / 2;
        if (server.deadline_heap.items[parent].expires_at_ns <= server.deadline_heap.items[index].expires_at_ns) break;
        deadlineHeapSwap(server, parent, index);
        index = parent;
    }
    return index;
}

fn deadlineHeapSiftDown(server: *Impl, start: usize) usize {
    var index = start;
    while (true) {
        const left = index * 2 + 1;
        if (left >= server.deadline_heap.items.len) break;
        const right = left + 1;
        const child = if (right < server.deadline_heap.items.len and
            server.deadline_heap.items[right].expires_at_ns < server.deadline_heap.items[left].expires_at_ns)
            right
        else
            left;
        if (server.deadline_heap.items[index].expires_at_ns <= server.deadline_heap.items[child].expires_at_ns) break;
        deadlineHeapSwap(server, index, child);
        index = child;
    }
    return index;
}

fn deadlineHeapInsertOrUpdate(server: *Impl, target: DeadlineTarget, expires_at_ns: u64) !void {
    const target_index = deadlineTargetIndex(target);
    if (target_index.*) |index| {
        std.debug.assert(index < server.deadline_heap.items.len);
        std.debug.assert(deadlineTargetsEqual(server.deadline_heap.items[index].target, target));
        const previous = server.deadline_heap.items[index].expires_at_ns;
        server.deadline_heap.items[index].expires_at_ns = expires_at_ns;
        if (expires_at_ns < previous) {
            _ = deadlineHeapSiftUp(server, index);
        } else if (expires_at_ns > previous) {
            _ = deadlineHeapSiftDown(server, index);
        }
        return;
    }

    try server.deadline_heap.append(server.localAllocator(), .{
        .expires_at_ns = expires_at_ns,
        .target = target,
    });
    const index = server.deadline_heap.items.len - 1;
    target_index.* = index;
    _ = deadlineHeapSiftUp(server, index);
}

fn deadlineHeapRemove(server: *Impl, target: DeadlineTarget) bool {
    const target_index = deadlineTargetIndex(target);
    const index = target_index.* orelse return false;
    std.debug.assert(index < server.deadline_heap.items.len);
    std.debug.assert(deadlineTargetsEqual(server.deadline_heap.items[index].target, target));

    const removed = server.deadline_heap.items[index];
    const replacement = server.deadline_heap.pop().?;
    deadlineTargetIndex(removed.target).* = null;
    if (index < server.deadline_heap.items.len) {
        server.deadline_heap.items[index] = replacement;
        deadlineTargetIndex(replacement.target).* = index;
        if (index != 0 and server.deadline_heap.items[index].expires_at_ns <
            server.deadline_heap.items[(index - 1) / 2].expires_at_ns)
        {
            _ = deadlineHeapSiftUp(server, index);
        } else {
            _ = deadlineHeapSiftDown(server, index);
        }
    }
    return true;
}

fn deadlineHeapPeek(server: *const Impl) ?DeadlineEntry {
    if (server.deadline_heap.items.len == 0) return null;
    return server.deadline_heap.items[0];
}

fn deadlineHeapPop(server: *Impl) ?DeadlineEntry {
    const entry = deadlineHeapPeek(server) orelse return null;
    std.debug.assert(deadlineHeapRemove(server, entry.target));
    return entry;
}

fn setStreamDeadline(stream: *Stream, value: deadline.Deadline) !void {
    try deadlineHeapInsertOrUpdate(stream.connection.server, .{ .stream = stream }, value.expires_at_ns);
    stream.deadline = value;
    scheduleDeadlineTimer(stream.connection.server);
}

fn clearStreamDeadline(stream: *Stream) void {
    const server = stream.connection.server;
    const removed = deadlineHeapRemove(server, .{ .stream = stream });
    stream.deadline = null;
    if (removed) scheduleDeadlineTimer(server);
}

fn removeStreamDeadline(stream: *Stream) void {
    const server = stream.connection.server;
    if (deadlineHeapRemove(server, .{ .stream = stream })) scheduleDeadlineTimer(server);
}

fn setConnectionDeadline(connection: *Connection, kind: ConnectionDeadlineKind, expires_at_ns: u64) !void {
    try deadlineHeapInsertOrUpdate(connection.server, .{ .connection = connection }, expires_at_ns);
    connection.connection_deadline = .{ .kind = kind, .expires_at_ns = expires_at_ns };
    scheduleDeadlineTimer(connection.server);
}

fn clearConnectionDeadline(connection: *Connection) void {
    const server = connection.server;
    const removed = deadlineHeapRemove(server, .{ .connection = connection });
    connection.connection_deadline = null;
    if (removed) scheduleDeadlineTimer(server);
}

fn serverAcceptsIdleDeadline(server: *Impl) bool {
    server.lock();
    defer server.unlock();
    return server.state == .running and server.shutdown_request == .none;
}

fn armIdleDeadline(connection: *Connection) !void {
    if (connection.closing or connection.draining or connection.streams.count() != 0 or
        !serverAcceptsIdleDeadline(connection.server)) return;
    try setConnectionDeadline(
        connection,
        .idle,
        connection.server.clock.now() +| connection.server.connection_idle_timeout_ns,
    );
}

const WriteRequest = struct {
    request: xev.WriteRequest = undefined,
    connection: *Connection,
    bytes: []u8,
    free_next: ?*WriteRequest = null,
    in_pool: bool = false,
};

fn acquireWriteRequest(connection: *Connection, bytes: []u8) !*WriteRequest {
    const write = if (connection.write_request_pool_head) |pooled| pooled else try connection.allocator().create(WriteRequest);
    if (connection.write_request_pool_head != null) {
        std.debug.assert(write.in_pool);
        std.debug.assert(connection.write_request_pool_count > 0);
        connection.write_request_pool_head = write.free_next;
        connection.write_request_pool_count -= 1;
    }
    write.* = .{
        .request = .{ .full_write_buffer = .{ .slice = &.{} } },
        .connection = connection,
        .bytes = bytes,
    };
    return write;
}

fn releaseWriteRequest(connection: *Connection, write: *WriteRequest) void {
    std.debug.assert(write.connection == connection);
    std.debug.assert(!write.in_pool);
    std.debug.assert(!isWriteRequestQueued(connection, write));
    std.debug.assert(write.request.completion.state() == .dead);
    write.bytes = &.{};
    write.free_next = connection.write_request_pool_head;
    write.in_pool = true;
    connection.write_request_pool_head = write;
    connection.write_request_pool_count += 1;
}

fn drainWriteRequestPool(connection: *Connection) void {
    std.debug.assert(connection.pending_writes == 0);
    std.debug.assert(connection.write_queue.head == null);
    var count: usize = 0;
    var current = connection.write_request_pool_head;
    while (current) |write| {
        std.debug.assert(write.in_pool);
        std.debug.assert(write.request.completion.state() == .dead);
        count += 1;
        std.debug.assert(count <= connection.write_request_pool_count);
        current = write.free_next;
    }
    std.debug.assert(count == connection.write_request_pool_count);

    while (connection.write_request_pool_head) |write| {
        connection.write_request_pool_head = write.free_next;
        connection.write_request_pool_count -= 1;
        connection.allocator().destroy(write);
    }
    std.debug.assert(connection.write_request_pool_count == 0);
}

fn isWriteRequestQueued(connection: *const Connection, write: *const WriteRequest) bool {
    var current = connection.write_queue.head;
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

fn runLoop(server: *Impl) void {
    server.local_allocator_state.enterLoop();
    defer server.local_allocator_state.leaveLoop();
    const setup_result = setupLoop(server);
    if (setup_result) |_| {
        server.signalStarted(null);
        server.loop.run(.until_done) catch |err| {
            server.logger.write(
                .err,
                "server event loop failed address={s}:{d} error={s}",
                .{ server.local_host[0..server.local_host_len], server.local_port, @errorName(err) },
            );
        };
        server.lock();
        server.stream_async_initialized = false;
        server.shutdown_async_initialized = false;
        server.unlock();
        server.stream_async.deinit();
        server.shutdown_async.deinit();
        server.loop.deinit();
        server.loop_initialized = false;
        server.lock();
        server.state = .stopped;
        server.condition.broadcast(server.io());
        server.unlock();
    } else |err| {
        server.logger.write(.err, "server startup failed address={s}:{d} error={s}", .{ server.host, server.configured_port, @errorName(err) });
        server.signalStarted(err);
    }
}

fn setupLoop(server: *Impl) StartupError!void {
    server.loop = xev.Loop.init(.{}) catch return error.LoopInitializationFailed;
    server.loop_initialized = true;
    errdefer {
        server.loop.deinit();
        server.loop_initialized = false;
    }

    errdefer {
        if (server.stream_async_initialized) {
            server.stream_async.deinit();
            server.stream_async_initialized = false;
        }
        if (server.shutdown_async_initialized) {
            server.shutdown_async.deinit();
            server.shutdown_async_initialized = false;
        }
        if (server.listener_initialized) closeFd(server.listener.fd);
    }

    const address = std.Io.net.IpAddress.parseIp4(server.host, server.configured_port) catch return error.InvalidAddress;
    server.listener = xev.TCP.init(address) catch return error.ListenerInitializationFailed;
    server.listener_initialized = true;
    if (server.reuse_port) socket_options.enableReusePort(server.listener.fd) catch return error.ListenerInitializationFailed;
    server.listener.bind(address) catch return error.BindFailed;
    server.listener.listen(128) catch return error.ListenFailed;

    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(server.listener.fd, @ptrCast(&local_address), &address_length)) != .SUCCESS) return error.AddressQueryFailed;
    @memcpy(server.local_host[0..server.host.len], server.host);
    server.local_host_len = server.host.len;
    server.local_port = std.mem.bigToNative(u16, local_address.port);
    if (comptime builtin.is_test) {
        if (server.test_fail_startup) return error.AsyncInitializationFailed;
    }

    server.shutdown_async = xev.Async.init() catch return error.AsyncInitializationFailed;
    server.shutdown_async_initialized = true;
    server.shutdown_async.wait(&server.loop, &server.shutdown_completion, Impl, server, onShutdown);
    server.stream_async = xev.Async.init() catch return error.AsyncInitializationFailed;
    server.stream_async_initialized = true;
    server.stream_async.wait(&server.loop, &server.stream_async_completion, Impl, server, onStreamAsync);
    server.drain_timer = xev.Timer.init() catch return error.TimerInitializationFailed;
    server.drain_timer_initialized = true;
    server.deadline_timer = xev.Timer.init() catch return error.TimerInitializationFailed;
    server.deadline_timer_initialized = true;
    server.listener_accept_active = true;
    server.listener.accept(&server.loop, &server.listener_accept_completion, Impl, server, onConnection);
}

fn onConnection(server: ?*Impl, loop: *xev.Loop, _: *xev.Completion, result: xev.AcceptError!xev.TCP) xev.CallbackAction {
    const impl = server orelse return .disarm;
    impl.listener_accept_active = false;
    const tcp = result catch {
        if (isAccepting(impl)) return rearmListener(impl);
        closeListener(impl);
        return .disarm;
    };
    const server_ptr = impl;
    if (!isAccepting(server_ptr)) {
        closeFd(tcp.fd);
        closeListener(server_ptr);
        return .disarm;
    }
    if (!server_ptr.coordinator.tryClaimConnection()) {
        closeFd(tcp.fd);
        return rearmListener(server_ptr);
    }
    socket_options.enableTcpNoDelay(tcp.fd) catch {
        server_ptr.coordinator.releaseConnection();
        closeFd(tcp.fd);
        return rearmListener(server_ptr);
    };
    if (comptime builtin.is_test) _ = server_ptr.accepted_connections.fetchAdd(1, .monotonic);

    const connection = server_ptr.localAllocator().create(Connection) catch {
        server_ptr.coordinator.releaseConnection();
        closeFd(tcp.fd);
        return rearmListener(server_ptr);
    };
    connection.* = .{ .server = server_ptr, .tcp = tcp };
    server_ptr.connections.append(server_ptr.localAllocator(), connection) catch {
        server_ptr.coordinator.releaseConnection();
        closeFd(tcp.fd);
        drainWriteRequestPool(connection);
        server_ptr.localAllocator().destroy(connection);
        return rearmListener(server_ptr);
    };
    connection.owns_admission = true;
    connection.initializeSession() catch {
        connection.closeOnLoop(loop);
        return rearmListener(server_ptr);
    };
    if (comptime build_options.tls) {
        if (server_ptr.tls_config) |config| {
            connection.tls_session = tls_record.Session.create(server_ptr.localAllocator(), config, null) catch {
                connection.closeOnLoop(loop);
                return rearmListener(server_ptr);
            };
            connection.tls_handshaking = true;
            setConnectionDeadline(
                connection,
                .tls_handshake,
                server_ptr.clock.now() +| server_ptr.tls_handshake_timeout_ns,
            ) catch {
                connection.closeOnLoop(loop);
                return rearmListener(server_ptr);
            };
        }
    }
    if (!connection.tls_handshaking) {
        setConnectionDeadline(
            connection,
            .cleartext_preface,
            server_ptr.clock.now() +| server_ptr.cleartext_preface_timeout_ns,
        ) catch {
            connection.closeOnLoop(loop);
            return rearmListener(server_ptr);
        };
    }
    connection.startRead(loop) catch connection.closeOnLoop(loop);
    if (connection.tls_handshaking) {
        connection.driveTlsHandshake() catch connection.closeOnLoop(loop);
    } else {
        connection.flush() catch connection.closeOnLoop(loop);
    }
    return rearmListener(server_ptr);
}

fn rearmListener(server: *Impl) xev.CallbackAction {
    server.listener_accept_active = true;
    return .rearm;
}

fn isAccepting(server: *Impl) bool {
    server.lock();
    defer server.unlock();
    return server.state == .running;
}

fn closeFd(fd: std.posix.fd_t) void {
    _ = std.posix.system.close(fd);
}

fn onRead(connection: ?*Connection, loop: *xev.Loop, _: *xev.Completion, _: xev.TCP, buffer: xev.ReadBuffer, result: xev.ReadError!usize) xev.CallbackAction {
    const conn = connection orelse return .disarm;
    conn.read_active = false;
    const length = result catch {
        conn.closeOnLoop(loop);
        conn.submitCloseIfReady(loop);
        conn.finishCloseIfReady();
        return .disarm;
    };
    const bytes = switch (buffer) {
        .slice => |slice| slice[0..length],
        .array => unreachable,
    };
    if (comptime build_options.tls) {
        if (conn.tls_session) |tls_session| {
            if (bytes.len == 0) {
                tls_session.markTransportEof();
                conn.closeOnLoop(loop);
            } else {
                tls_session.feedCiphertext(bytes) catch {
                    conn.closeOnLoop(loop);
                    conn.submitCloseIfReady(loop);
                    conn.finishCloseIfReady();
                    return .disarm;
                };
                if (conn.tls_handshaking) {
                    conn.driveTlsHandshake() catch conn.closeOnLoop(loop);
                } else {
                    conn.receiveTlsPlaintext() catch |err| {
                        if (err == error.TlsPeerClosed)
                            conn.closeGracefully(loop)
                        else
                            conn.closeOnLoop(loop);
                    };
                }
            }
            if (!conn.tls_handshaking and !conn.closing) conn.closeTerminatedSession(loop);
            if (!conn.closing) conn.startRead(loop) catch conn.closeOnLoop(loop);
            if (conn.closing) conn.submitCloseIfReady(loop);
            conn.finishCloseIfReady();
            return .disarm;
        }
    }
    const consumed = c.nghttp2_session_mem_recv2(conn.session, bytes.ptr, bytes.len);
    if (consumed < 0 or consumed != bytes.len) {
        conn.closeOnLoop(loop);
    } else {
        conn.flush() catch conn.closeOnLoop(loop);
        conn.closeTerminatedSession(loop);
        if (!conn.close_after_writes) conn.startRead(loop) catch conn.closeOnLoop(loop);
    }
    if (conn.closing) conn.submitCloseIfReady(loop);
    conn.finishCloseIfReady();
    return .disarm;
}

fn onWrite(write: ?*WriteRequest, loop: *xev.Loop, _: *xev.Completion, _: xev.TCP, _: xev.WriteBuffer, result: xev.WriteError!usize) xev.CallbackAction {
    const request = write orelse return .disarm;
    const connection = request.connection;
    connection.pending_writes -= 1;
    connection.queued_write_bytes = completeQueuedWrite(connection.queued_write_bytes, request.bytes.len);
    // WriteQueue retries partial writes and calls back with the full buffer length.
    const completed: ?usize = result catch null;
    const write_succeeded = completed != null and completed.? == request.bytes.len;
    connection.allocator().free(request.bytes);
    releaseWriteRequest(connection, request);
    if (connection.closing) {
        connection.discardQueuedWrites();
        connection.submitCloseIfReady(loop);
        connection.finishCloseIfReady();
        return .disarm;
    }
    if (!write_succeeded) {
        connection.closeOnLoop(loop);
        connection.finishCloseIfReady();
        return .disarm;
    }
    if (connection.close_after_writes and connection.pending_writes == 0) {
        connection.closeOnLoop(loop);
        connection.finishCloseIfReady();
        return .disarm;
    }
    if (connection.tls_handshake_needs_write or connection.queued_write_bytes < connection.server.write_low_watermark_bytes) {
        // WriteQueue adds its next completion after this callback returns.
        // Wake the async callback so queueWrite cannot reenter that ordering.
        if (connection.server.enqueueDirtyConnection(connection)) {
            connection.server.stream_async.notify() catch {
                connection.server.removeDirtyConnection(connection);
                connection.closeOnLoop(loop);
                connection.finishCloseIfReady();
                return .disarm;
            };
        }
        return .disarm;
    }
    maybeCloseDrainedConnection(connection);
    connection.finishCloseIfReady();
    return .disarm;
}

fn onReadCanceled(connection: ?*Connection, loop: *xev.Loop, _: *xev.Completion, _: xev.CancelError!void) xev.CallbackAction {
    const conn = connection orelse return .disarm;
    conn.read_cancel_submitted = false;
    conn.submitCloseIfReady(loop);
    conn.finishCloseIfReady();
    return .disarm;
}

fn onWriteCanceled(connection: ?*Connection, loop: *xev.Loop, _: *xev.Completion, result: xev.CancelError!void) xev.CallbackAction {
    const conn = connection orelse return .disarm;
    if (result) |_| {} else |err| {
        if (err == error.NotFound) {
            if (conn.write_cancel_target) |target| {
                if (target.state() == .active) return .rearm;
            }
        }
    }
    conn.write_cancel_submitted = false;
    conn.write_cancel_target = null;
    conn.submitCloseIfReady(loop);
    conn.finishCloseIfReady();
    return .disarm;
}

fn onConnectionClosed(connection: ?*Connection, _: *xev.Loop, _: *xev.Completion, _: xev.TCP, _: xev.CloseError!void) xev.CallbackAction {
    const conn = connection orelse return .disarm;
    conn.close_completed = true;
    conn.finishCloseIfReady();
    return .disarm;
}

fn onShutdown(server: ?*Impl, _: *xev.Loop, _: *xev.Completion, _: xev.Async.WaitError!void) xev.CallbackAction {
    const impl = server orelse return .disarm;
    impl.lock();
    const state = impl.state;
    impl.unlock();
    switch (state) {
        .draining => beginDrain(impl),
        .stopping => stopImmediately(impl),
        else => {},
    }
    return .rearm;
}

fn onStreamAsync(server: ?*Impl, _: *xev.Loop, _: *xev.Completion, result: xev.Async.WaitError!void) xev.CallbackAction {
    const impl = server orelse return .disarm;
    result catch {
        stopImmediately(impl);
        return .disarm;
    };
    processStreamCommands(impl);
    return .rearm;
}

fn processStreamCommands(server: *Impl) void {
    processForcedServerCallAborts(server);
    var commands: std.ArrayList(StreamCommand) = .empty;
    server.lock();
    std.mem.swap(std.ArrayList(StreamCommand), &commands, &server.stream_commands);
    server.unlock();
    defer commands.deinit(server.shared_allocator);

    for (commands.items) |command| {
        const connection = command.target.connection;
        processStreamCommand(server, command);
        _ = server.enqueueDirtyConnection(connection);
        releaseStreamCommand(command.target);
    }

    drainDirtyConnections(server);
}

fn processForcedServerCallAborts(server: *Impl) void {
    for (server.connections.items) |connection| {
        var iterator = connection.streams.valueIterator();
        while (iterator.next()) |target_ptr| {
            const target = target_ptr.*;
            server.lock();
            const requested = target.force_abort_requested;
            server.unlock();
            if (requested) forceAbortStreaming(target);
        }
    }
}

fn drainDirtyConnections(server: *Impl) void {
    while (server.popDirtyConnection()) |connection| {
        if (connection.closing or connection.session == null) continue;
        if (connection.tls_handshaking) {
            if (connection.tls_handshake_needs_write) {
                connection.driveTlsHandshake() catch connection.closeOnLoop(&server.loop);
            }
            continue;
        }
        connection.flush() catch {
            connection.closeOnLoop(&server.loop);
            continue;
        };
        connection.closeTerminatedSession(&server.loop);
        if (!connection.closing) maybeCloseDrainedConnection(connection);
    }
}

fn processStreamCommand(server: *Impl, command: StreamCommand) void {
    const target = command.target;
    switch (command.action) {
        .initial_metadata => |value| {
            var entries = value.entries;
            defer entries.deinit();
            server.lock();
            const active = target.streaming_active and !target.transport_closed and !target.response_headers_submitted;
            if (active) {
                target.response_compression = if (target.accepts_response_gzip and value.compression == .gzip)
                    .gzip
                else
                    .identity;
                target.response_headers_submitted = true;
            }
            server.unlock();
            if (!active or target.streaming == null) return;
            const session = target.connection.session orelse return;
            submitStreamingResponse(session, target, entries.items()) catch target.connection.close();
        },
        .send => |bytes| {
            server.lock();
            const active = target.streaming_active and !target.transport_closed;
            server.unlock();
            if (!active or target.streaming == null) {
                releaseOutboundReservation(target, bytes.len, false);
                server.shared_allocator.free(bytes);
                return;
            }
            const streaming = &target.streaming.?;
            streaming.outbound.append(target.allocator, .{
                .bytes = bytes,
                .allocator = server.shared_allocator,
            }) catch {
                releaseOutboundReservation(target, bytes.len, false);
                server.shared_allocator.free(bytes);
                failStreaming(target, .internal, "response allocation failed");
                return;
            };
            resumeStreamingResponse(target);
        },
        .finish => |value| {
            var trailing_metadata = value.trailing_metadata;
            defer if (trailing_metadata) |*entries| entries.deinit();
            server.lock();
            const active = target.streaming_active and !target.transport_closed;
            if (active) target.response_finished = true;
            server.unlock();
            if (!active or target.streaming == null) {
                if (value.message.len != 0) server.shared_allocator.free(value.message);
                return;
            }
            target.setOwnedStatus(value.code, value.message, server.shared_allocator);
            const metadata_result = if (trailing_metadata) |*entries|
                copyMetadataEntries(&target.trailing_metadata, entries.items())
            else
                copyStreamingTrailers(target);
            metadata_result catch {
                target.setStatus(.init(.internal, "metadata allocation failed")) catch {
                    target.connection.close();
                    return;
                };
            };
            resumeStreamingResponse(target);
        },
        .resume_receive => {
            server.lock();
            const active = target.streaming_active and !target.transport_closed;
            target.resume_queued = false;
            if (active) target.receive_paused = false;
            server.unlock();
            if (active) {
                deliverStreamingMessages(target);
                server.lock();
                const return_credit = canReturnDeferredStreamCredit(
                    target.receive_paused,
                    target.transport_closed,
                ) and target.streaming_active;
                server.unlock();
                if (return_credit and target.deferred_stream_credit != 0) {
                    if (c.nghttp2_session_consume_stream(
                        target.connection.session,
                        target.id,
                        target.deferred_stream_credit,
                    ) != 0) {
                        target.connection.close();
                        return;
                    }
                    target.deferred_stream_credit = 0;
                }
            }
        },
    }
}

fn releaseStreamCommand(target: *Stream) void {
    const server = target.connection.server;
    server.lock();
    std.debug.assert(target.command_refs != 0);
    target.command_refs -= 1;
    const destroy = target.transport_closed and target.command_refs == 0;
    server.unlock();
    if (destroy) destroyStream(target);
}

fn releaseOutboundReservation(target: *Stream, length: usize, notify_writable: bool) void {
    const server = target.connection.server;
    server.lock();
    target.outbound_reserved_bytes -= length;
    const writable = notify_writable and
        target.writable_requested and
        target.streaming_active and
        !target.finish_queued and
        !target.transport_closed and
        target.outbound_reserved_bytes <= outboundLowWatermark(server);
    if (writable) target.writable_requested = false;
    server.unlock();
    if (writable) {
        const streaming = &(target.streaming orelse return);
        if (streaming.handler.on_writable) |callback| {
            callback(streaming.handler.context, target.serverHandle(), &streaming.context);
        }
    }
}

fn outboundLowWatermark(server: *const Impl) usize {
    return server.stream_limits.max_outbound_buffer_size / 2;
}

fn copyStreamingTrailers(target: *Stream) !void {
    const streaming = &(target.streaming orelse return);
    try copyMetadataEntries(&target.trailing_metadata, streaming.context.trailing_metadata.items());
}

fn copyMetadataEntries(destination: *metadata.Metadata, entries: []const metadata.Entry) !void {
    for (entries) |entry| try destination.append(entry.key, entry.value);
}

fn resumeStreamingResponse(target: *Stream) void {
    if (!target.response_headers_submitted or target.connection.session == null or target.connection.closing) return;
    _ = c.nghttp2_session_resume_data(target.connection.session, target.id);
}

fn failStreaming(target: *Stream, code: status.Code, text: []const u8) void {
    if (target.streaming == null) return;
    removeStreamDeadline(target);
    const server = target.connection.server;
    server.lock();
    target.streaming_active = false;
    target.response_finished = true;
    target.finish_queued = true;
    target.reset_after_trailers = true;
    if (target.terminal_reason == .completed) target.terminal_reason = .local_error;
    const trailers_submitted = target.trailer_submitted;
    server.unlock();
    if (trailers_submitted) {
        submitStreamingFailureReset(target) catch {};
        return;
    }
    target.setStatus(.init(code, text)) catch {
        target.connection.close();
        return;
    };
    copyStreamingTrailers(target) catch {};
    if (!target.response_headers_submitted) {
        const session = target.connection.session orelse return;
        server.lock();
        target.response_compression = .identity;
        target.response_headers_queued = true;
        target.response_headers_submitted = true;
        server.unlock();
        submitStreamingResponse(session, target, &.{}) catch {
            target.connection.close();
            return;
        };
    }
    resumeStreamingResponse(target);
}

fn forceAbortStreaming(target: *Stream) void {
    const server = target.connection.server;
    server.lock();
    const active = target.force_abort_requested and target.streaming_active and !target.transport_closed;
    target.force_abort_requested = false;
    if (active) {
        target.streaming_active = false;
        target.response_finished = true;
        target.finish_queued = true;
        target.receive_paused = false;
        target.resume_queued = false;
        target.reset_after_trailers = true;
        target.terminal_reason = .local_error;
    }
    server.unlock();
    if (!active) return;

    removeStreamDeadline(target);
    discardStreamCommands(target);
    const reserved_bytes = target.streaming.?.clearOutbound();
    releaseOutboundReservation(target, reserved_bytes, false);
    const session = target.connection.session orelse {
        target.connection.close();
        return;
    };
    if (c.nghttp2_submit_rst_stream(session, c.NGHTTP2_FLAG_NONE, target.id, c.NGHTTP2_INTERNAL_ERROR) != 0) {
        target.connection.close();
        return;
    }
    target.reset_submitted = true;
    _ = server.enqueueDirtyConnection(target.connection);
}

fn discardStreamCommands(target: *Stream) void {
    const server = target.connection.server;
    server.lock();
    defer server.unlock();
    var index: usize = 0;
    while (index < server.stream_commands.items.len) {
        if (server.stream_commands.items[index].target != target) {
            index += 1;
            continue;
        }
        var command = server.stream_commands.orderedRemove(index);
        std.debug.assert(target.command_refs != 0);
        target.command_refs -= 1;
        switch (command.action) {
            .send => |bytes| target.outbound_reserved_bytes -= bytes.len,
            else => {},
        }
        command.deinit(server.shared_allocator);
    }
}

fn retireStream(target: *Stream) bool {
    removeStreamDeadline(target);
    const server = target.connection.server;
    server.lock();
    target.transport_closed = true;
    target.streaming_active = false;
    if (target.call_control) |control| control.target = null;
    server.unlock();
    discardStreamCommands(target);
    server.lock();
    const destroy = target.command_refs == 0;
    server.unlock();
    notifyServerCallTerminal(target);
    if (destroy) destroyStream(target);
    return destroy;
}

fn destroyStream(target: *Stream) void {
    const allocator = target.allocator;
    target.deinit();
    allocator.destroy(target);
}

fn notifyServerCallTerminal(target: *Stream) void {
    const control = target.call_control orelse return;
    const notify = !control.terminal.swap(true, .acq_rel);
    if (notify) {
        if (target.streaming) |*streaming| {
            if (streaming.handler.on_terminal) |callback| {
                callback(streaming.handler.context, serverCallId(control), target.terminal_reason);
            }
        }
    }
    target.call_control = null;
    serverCallRelease(control);
}

fn connectionTerminalReason(connection: *const Connection) raw_stream.ServerTerminalReason {
    const server = connection.server;
    server.lock();
    defer server.unlock();
    return if (server.shutdown_request == .none)
        .transport_error
    else
        .server_shutdown;
}

fn cancelStreaming(target: *Stream, reason: raw_stream.ServerTerminalReason) void {
    const streaming = &(target.streaming orelse return);
    removeStreamDeadline(target);
    const server = target.connection.server;
    server.lock();
    if (target.cancel_called) {
        server.unlock();
        return;
    }
    target.cancel_called = true;
    if (target.terminal_reason == .completed) target.terminal_reason = reason;
    if (target.call_control) |control| control.cancelled.store(true, .release);
    target.streaming_active = false;
    target.finish_queued = true;
    target.receive_paused = false;
    target.resume_queued = false;
    target.resume_requested = false;
    target.message_callback_active = false;
    server.unlock();
    discardStreamCommands(target);
    const reserved_bytes = streaming.clearOutbound();
    releaseOutboundReservation(target, reserved_bytes, false);
    if (streaming.handler.on_cancel) |callback| {
        callback(streaming.handler.context, target.serverHandle(), &streaming.context);
    }
}

fn beginDrain(server: *Impl) void {
    for (server.connections.items) |connection| {
        if (connection.connection_deadline) |value| {
            if (value.kind != .tls_handshake) clearConnectionDeadline(connection);
        }
    }
    if (!server.drain_started) {
        server.drain_started = true;
        closeListener(server);

        const timeout_ms = if (server.drain_timeout_ns == 0)
            0
        else
            std.math.divCeil(u64, server.drain_timeout_ns, std.time.ns_per_ms) catch std.math.maxInt(u64);
        server.drain_timer.run(&server.loop, &server.drain_timer_completion, timeout_ms, Impl, server, onDrainTimeout);
    }

    // Do not advertise GOAWAY until replacement connections can no longer enter
    // the listener backlog and appear connected without ever being accepted.
    if (!server.listener_closed) return;

    for (server.connections.items) |connection| {
        if (connection.draining or connection.closing or connection.session == null) continue;
        if (connection.tls_handshaking) {
            connection.closeOnLoop(&server.loop);
            continue;
        }
        connection.draining = true;
        connection.submitGoAway(
            connection.highest_accepted_stream_id,
            c.NGHTTP2_NO_ERROR,
        ) catch {
            connection.closeOnLoop(&server.loop);
            continue;
        };
        connection.flush() catch connection.closeOnLoop(&server.loop);
    }

    for (server.connections.items) |connection| maybeCloseDrainedConnection(connection);
    finishDrainIfIdle(server);
}

fn maybeCloseDrainedConnection(connection: *Connection) void {
    if (connection.draining and !connection.closing and connection.streams.count() == 0 and connection.pending_writes == 0) {
        if (connection.server.loop_initialized)
            connection.closeGracefully(&connection.server.loop)
        else
            connection.close();
    }
}

fn finishDrainIfIdle(server: *Impl) void {
    server.lock();
    const draining = server.state == .draining;
    server.unlock();
    if (!draining or server.connections.items.len != 0) return;
    server.lock();
    server.state = .stopping;
    server.unlock();
    maybeStopLoop(server);
}

fn onDrainTimeout(server: ?*Impl, _: *xev.Loop, _: *xev.Completion, result: xev.Timer.RunError!void) xev.CallbackAction {
    const impl = server orelse return .disarm;
    result catch return .disarm;
    impl.lock();
    if (impl.state == .draining) impl.state = .stopping;
    impl.unlock();
    stopImmediately(impl);
    return .disarm;
}

fn stopImmediately(server: *Impl) void {
    closeListener(server);
    for (server.connections.items) |connection| connection.closeOnLoop(&server.loop);
    maybeStopLoop(server);
}

fn closeListener(server: *Impl) void {
    if (!server.listener_initialized or server.listener_close_submitted) return;
    if (server.listener_accept_active) {
        if (server.listener_accept_cancel_submitted) return;
        server.listener_accept_cancel_submitted = true;
        server.loop.cancel(
            &server.listener_accept_completion,
            &server.listener_accept_cancel_completion,
            Impl,
            server,
            onListenerAcceptCanceled,
        );
        return;
    }
    server.listener_close_submitted = true;
    server.listener.close(&server.loop, &server.listener_close_completion, Impl, server, onListenerClosed);
}

fn onListenerAcceptCanceled(
    server: ?*Impl,
    _: *xev.Loop,
    _: *xev.Completion,
    _: xev.CancelError!void,
) xev.CallbackAction {
    const impl = server orelse return .disarm;
    impl.listener_accept_active = false;
    impl.listener_accept_cancel_submitted = false;
    closeListener(impl);
    return .disarm;
}

fn onListenerClosed(server: ?*Impl, _: *xev.Loop, _: *xev.Completion, _: xev.TCP, _: xev.CloseError!void) xev.CallbackAction {
    const impl = server orelse return .disarm;
    impl.listener_closed = true;
    impl.lock();
    const draining = impl.state == .draining;
    impl.unlock();
    if (draining) beginDrain(impl) else maybeStopLoop(impl);
    return .disarm;
}

fn maybeStopLoop(server: *Impl) void {
    server.lock();
    const stopping = server.state == .stopping;
    server.unlock();
    if (stopping and server.listener_closed and server.connections.items.len == 0) server.loop.stop();
}

fn refuseStream(session: ?*c.nghttp2_session, stream_id: i32) c_int {
    if (c.nghttp2_submit_rst_stream(
        session,
        c.NGHTTP2_FLAG_NONE,
        stream_id,
        c.NGHTTP2_REFUSED_STREAM,
    ) != 0) return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    return c.NGHTTP2_ERR_TEMPORAL_CALLBACK_FAILURE;
}

fn onBeginHeaders(session: ?*c.nghttp2_session, received_frame: ?*const c.nghttp2_frame, user_data: ?*anyopaque) callconv(.c) c_int {
    const native_frame = received_frame.?;
    if (native_frame.*.hd.type != c.NGHTTP2_HEADERS or native_frame.*.headers.cat != c.NGHTTP2_HCAT_REQUEST) return 0;
    const connection: *Connection = @ptrCast(@alignCast(user_data.?));
    if (connection.draining or
        connection.streams.count() >= connection.server.max_concurrent_streams_per_connection)
    {
        return refuseStream(session, native_frame.*.hd.stream_id);
    }
    const local_allocator = connection.allocator();
    const stream = local_allocator.create(Stream) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    stream.* = Stream.init(local_allocator, connection, native_frame.*.hd.stream_id);
    connection.streams.put(local_allocator, stream.id, stream) catch {
        stream.deinit();
        local_allocator.destroy(stream);
        return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    };
    if (connection.connection_deadline) |value| {
        if (value.kind == .idle) clearConnectionDeadline(connection);
    }
    connection.highest_accepted_stream_id = @max(connection.highest_accepted_stream_id, stream.id);
    return 0;
}

fn onHeader(
    _: ?*c.nghttp2_session,
    received_frame: ?*const c.nghttp2_frame,
    name_pointer: [*c]const u8,
    name_length: usize,
    value_pointer: [*c]const u8,
    value_length: usize,
    _: u8,
    user_data: ?*anyopaque,
) callconv(.c) c_int {
    const native_frame = received_frame.?;
    if (native_frame.*.headers.cat != c.NGHTTP2_HCAT_REQUEST) return 0;
    const connection: *Connection = @ptrCast(@alignCast(user_data.?));
    const stream = connection.streams.get(native_frame.*.hd.stream_id) orelse return 0;
    const name = name_pointer[0..name_length];
    const value = value_pointer[0..value_length];
    const field_size = std.math.add(usize, name.len, value.len) catch {
        stream.header_too_large = true;
        return 0;
    };
    stream.header_bytes = std.math.add(usize, stream.header_bytes, field_size) catch {
        stream.header_too_large = true;
        return 0;
    };
    if (stream.header_bytes > 64 * 1024) {
        stream.header_too_large = true;
        return 0;
    }

    if (std.mem.eql(u8, name, ":path")) {
        if (stream.path) |old_path| stream.allocator.free(old_path);
        stream.path = stream.allocator.dupe(u8, value) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    } else if (std.mem.eql(u8, name, ":method")) {
        stream.method_post = std.mem.eql(u8, value, "POST");
    } else if (std.mem.eql(u8, name, "content-type")) {
        stream.content_type_grpc = std.mem.startsWith(u8, value, "application/grpc");
    } else if (std.mem.eql(u8, name, "grpc-encoding")) {
        stream.request_compression = Compression.parse(value);
    } else if (std.mem.eql(u8, name, "grpc-accept-encoding")) {
        stream.accepts_response_gzip = stream.accepts_response_gzip or acceptsEncoding(value, .gzip);
    } else if (std.mem.eql(u8, name, "grpc-timeout")) {
        if (stream.timeout_seen) {
            stream.timeout_invalid = true;
            clearStreamDeadline(stream);
        } else {
            stream.timeout_seen = true;
            const timeout_ns = deadline.parseTimeout(value) catch {
                stream.timeout_invalid = true;
                return 0;
            };
            setStreamDeadline(
                stream,
                deadline.Deadline.initAfter(connection.server.clock, timeout_ns),
            ) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
    } else if (isRequestMetadata(name)) {
        _ = stream.request_metadata.appendDecoded(name, value) catch |err| switch (err) {
            error.OutOfMemory => return c.NGHTTP2_ERR_CALLBACK_FAILURE,
            else => {
                stream.request_metadata_invalid = true;
                return 0;
            },
        };
    } else if (isMalformedRequestMetadataName(name)) {
        stream.request_metadata_invalid = true;
    }
    return 0;
}

fn onDataChunk(
    session: ?*c.nghttp2_session,
    _: u8,
    stream_id: i32,
    data_pointer: [*c]const u8,
    data_length: usize,
    user_data: ?*anyopaque,
) callconv(.c) c_int {
    const connection: *Connection = @ptrCast(@alignCast(user_data.?));
    const stream = connection.streams.get(stream_id) orelse return 0;
    if (stream.streaming != null) {
        const copied = receiveStreamingData(stream, data_pointer[0..data_length]);
        if (c.nghttp2_session_consume_connection(session, data_length) != 0) {
            return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
        if (copied and stream.streaming_active) {
            if (stream.receive_paused) {
                stream.deferred_stream_credit = std.math.add(
                    usize,
                    stream.deferred_stream_credit,
                    data_length,
                ) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            } else if (c.nghttp2_session_consume_stream(session, stream_id, data_length) != 0) {
                return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            }
        }
        return 0;
    }
    if (!stream.responded) {
        const body_limit = wireMessageLimit(connection.server.max_request_size);
        if (data_length > body_limit -| stream.request_body.items.len) {
            stream.request_too_large = true;
        } else {
            stream.request_body.appendSlice(stream.allocator, data_pointer[0..data_length]) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
    }
    if (c.nghttp2_session_consume_connection(session, data_length) != 0 or
        c.nghttp2_session_consume_stream(session, stream_id, data_length) != 0)
    {
        return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    }
    return 0;
}

fn onFrameReceived(session: ?*c.nghttp2_session, received_frame: ?*const c.nghttp2_frame, user_data: ?*anyopaque) callconv(.c) c_int {
    const native_frame = received_frame.?;
    const connection: *Connection = @ptrCast(@alignCast(user_data.?));
    if (native_frame.*.hd.stream_id == 0) {
        if (native_frame.*.hd.type == c.NGHTTP2_SETTINGS and
            native_frame.*.hd.flags & c.NGHTTP2_FLAG_ACK == 0)
        {
            if (connection.connection_deadline) |value| {
                if (value.kind == .cleartext_preface) {
                    clearConnectionDeadline(connection);
                    armIdleDeadline(connection) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
                }
            }
        }
        return 0;
    }
    const stream = connection.streams.get(native_frame.*.hd.stream_id) orelse return 0;
    if (native_frame.*.hd.type == c.NGHTTP2_HEADERS and stream.request_metadata_invalid and !stream.responded) {
        stream.responded = true;
        submitFailure(session.?, stream, .invalid_argument, "invalid request metadata");
        return 0;
    }
    if (native_frame.*.hd.type == c.NGHTTP2_HEADERS and !stream.responded) {
        if (stream.path) |path| {
            if (connection.server.stream_handlers.get(path)) |handler| startStreaming(session.?, stream, handler);
        }
    }
    if ((native_frame.*.hd.type != c.NGHTTP2_HEADERS and native_frame.*.hd.type != c.NGHTTP2_DATA) or
        native_frame.*.hd.flags & c.NGHTTP2_FLAG_END_STREAM == 0)
    {
        return 0;
    }
    if (stream.streaming != null) {
        receiveStreamingEnd(stream);
    } else if (!stream.responded) {
        finishRequest(session.?, stream);
    }
    return 0;
}

fn startStreaming(session: *c.nghttp2_session, target: *Stream, handler: raw_stream.ServerHandler) void {
    target.responded = true;
    if (target.header_too_large) {
        submitFailure(session, target, .resource_exhausted, "request too large");
        return;
    }
    if (target.timeout_invalid) {
        submitFailure(session, target, .invalid_argument, "invalid grpc-timeout");
        return;
    }
    if (target.request_metadata_invalid) {
        submitFailure(session, target, .invalid_argument, "invalid request metadata");
        return;
    }
    if (target.deadline) |value| {
        if (value.isExceeded()) {
            submitFailure(session, target, .deadline_exceeded, "deadline exceeded");
            return;
        }
    }
    if (!target.method_post) {
        submitFailure(session, target, .unimplemented, "POST required");
        return;
    }
    if (!target.content_type_grpc) {
        submitFailure(session, target, .invalid_argument, "invalid content-type");
        return;
    }
    const request_compression = target.request_compression orelse {
        submitFailure(session, target, .unimplemented, "request compression is not supported");
        return;
    };

    var context = service.ServerContext.init(target.allocator);
    var context_owned = true;
    defer if (context_owned) context.deinit();
    context.deadline = target.deadline;
    context.request_compression = request_compression;
    for (target.request_metadata.items()) |entry| context.request_metadata.append(entry.key, entry.value) catch {
        submitFailure(session, target, .internal, "metadata allocation failed");
        return;
    };
    const server = target.connection.server;
    const call_control = server.shared_allocator.create(ServerCallControl) catch {
        submitFailure(session, target, .internal, "call allocation failed");
        return;
    };
    call_control.* = .{
        .allocator = server.shared_allocator,
        .server = server,
        .target = target,
    };
    target.call_control = call_control;
    target.streaming = .{
        .handler = handler,
        .decoder = frame.Decoder.initWithCompression(target.allocator, server.stream_limits.max_message_size, request_compression),
        .context = context,
    };
    context_owned = false;
    server.lock();
    target.streaming_active = true;
    target.receive_paused = handler.receive_initially_paused;
    target.response_headers_queued = handler.initial_metadata_mode == .automatic_after_start;
    server.unlock();

    const streaming = &target.streaming.?;
    handler.on_start(handler.context, target.serverHandle(), &streaming.context) catch {
        failStreaming(target, .internal, "handler failed");
        return;
    };
    if (handler.initial_metadata_mode == .automatic_after_start) {
        server.lock();
        target.response_compression = if (target.accepts_response_gzip and
            (streaming.context.response_compression == .gzip or target.response_gzip_requested))
            .gzip
        else
            .identity;
        target.response_headers_queued = true;
        target.response_headers_submitted = true;
        server.unlock();
        submitStreamingResponse(session, target, streaming.context.initial_metadata.items()) catch target.connection.close();
    }
}

fn receiveStreamingData(target: *Stream, bytes: []const u8) bool {
    const streaming = &(target.streaming orelse return false);
    const server = target.connection.server;
    server.lock();
    const active = target.streaming_active;
    server.unlock();
    if (!active) return false;
    streaming.decoder.feedBounded(bytes, server.stream_limits.max_inbound_buffer_size) catch |err| {
        switch (err) {
            error.BufferLimitExceeded => failStreaming(target, .resource_exhausted, "request message too large"),
            else => failStreaming(target, .invalid_argument, "malformed streaming request"),
        }
        return false;
    };
    deliverStreamingMessages(target);
    return true;
}

fn deliverStreamingMessages(target: *Stream) void {
    const streaming = &(target.streaming orelse return);
    const server = target.connection.server;
    while (true) {
        server.lock();
        const active = target.streaming_active;
        const paused = target.receive_paused;
        server.unlock();
        if (!active or paused) return;

        const decoded = streaming.decoder.nextMessage() catch |err| {
            switch (err) {
                error.MessageTooLarge => failStreaming(target, .resource_exhausted, "request message too large"),
                else => failStreaming(target, .invalid_argument, "malformed streaming request"),
            }
            return;
        } orelse break;
        const compression: Compression = if (decoded.compressed) .gzip else .identity;
        streaming.context.request_compression = compression;
        server.lock();
        if (!target.streaming_active or target.receive_paused) {
            server.unlock();
            target.allocator.free(decoded.payload);
            return;
        }
        target.message_callback_active = true;
        server.unlock();
        const action_result = streaming.handler.on_message(
            streaming.handler.context,
            target.serverHandle(),
            &streaming.context,
            decoded.payload,
            compression,
        );
        const action = action_result catch {
            target.allocator.free(decoded.payload);
            server.lock();
            target.message_callback_active = false;
            target.resume_requested = false;
            server.unlock();
            failStreaming(target, .internal, "handler failed");
            return;
        };
        target.allocator.free(decoded.payload);
        server.lock();
        target.message_callback_active = false;
        const still_active = target.streaming_active;
        const resume_requested = target.resume_requested;
        target.resume_requested = false;
        if (still_active and action == .pause and !resume_requested) target.receive_paused = true;
        server.unlock();
        if (!still_active or (action == .pause and !resume_requested)) return;
    }
    if (streaming.remote_end_received) completeStreamingRemoteEnd(target);
}

fn receiveStreamingEnd(target: *Stream) void {
    const streaming = &(target.streaming orelse return);
    if (streaming.remote_end_received) return;
    streaming.remote_end_received = true;
    deliverStreamingMessages(target);
}

fn completeStreamingRemoteEnd(target: *Stream) void {
    const streaming = &(target.streaming orelse return);
    if (streaming.remote_end_called) return;
    streaming.decoder.finish() catch {
        failStreaming(target, .invalid_argument, "malformed streaming request");
        return;
    };
    streaming.remote_end_called = true;
    streaming.handler.on_remote_end(
        streaming.handler.context,
        target.serverHandle(),
        &streaming.context,
    ) catch failStreaming(target, .internal, "handler failed");
}

fn finishRequest(session: *c.nghttp2_session, stream: *Stream) void {
    stream.responded = true;
    removeStreamDeadline(stream);
    if (stream.header_too_large or stream.request_too_large) {
        submitFailure(session, stream, .resource_exhausted, "request too large");
        return;
    }
    if (stream.timeout_invalid) {
        submitFailure(session, stream, .invalid_argument, "invalid grpc-timeout");
        return;
    }
    if (stream.request_metadata_invalid) {
        submitFailure(session, stream, .invalid_argument, "invalid request metadata");
        return;
    }
    if (stream.deadline) |value| {
        if (value.isExceeded()) {
            submitFailure(session, stream, .deadline_exceeded, "deadline exceeded");
            return;
        }
    }
    if (!stream.method_post) {
        submitFailure(session, stream, .unimplemented, "POST required");
        return;
    }
    if (!stream.content_type_grpc) {
        submitFailure(session, stream, .invalid_argument, "invalid content-type");
        return;
    }
    const request_compression = stream.request_compression orelse {
        submitFailure(session, stream, .unimplemented, "request compression is not supported");
        return;
    };
    const path = stream.path orelse {
        submitFailure(session, stream, .unimplemented, "method path missing");
        return;
    };
    const handler = stream.connection.server.handlers.get(path) orelse {
        submitFailure(session, stream, .unimplemented, "method not found");
        return;
    };
    const request = frame.decodeUnaryWithCompression(
        stream.allocator,
        stream.request_body.items,
        stream.connection.server.max_request_size,
        request_compression,
    ) catch |err| {
        switch (err) {
            error.MessageTooLarge => submitFailure(session, stream, .resource_exhausted, "request message too large"),
            else => submitFailure(session, stream, .invalid_argument, "malformed unary request"),
        }
        return;
    };
    defer stream.allocator.free(request);

    var context = service.ServerContext.init(stream.allocator);
    defer context.deinit();
    context.deadline = stream.deadline;
    context.request_compression = if (stream.request_body.items[0] == 1) .gzip else .identity;
    for (stream.request_metadata.items()) |entry| context.request_metadata.append(entry.key, entry.value) catch {
        submitFailure(session, stream, .internal, "metadata allocation failed");
        return;
    };
    var response = handler.invoke(stream.allocator, &context, request) catch {
        submitFailure(session, stream, .internal, "handler failed");
        return;
    };
    defer response.deinit();
    if (context.isDeadlineExceeded()) {
        submitFailure(session, stream, .deadline_exceeded, "deadline exceeded");
        return;
    }
    stream.response_compression = if (context.response_compression == .gzip and stream.accepts_response_gzip)
        .gzip
    else
        .identity;
    for (context.trailing_metadata.items()) |entry| stream.trailing_metadata.append(entry.key, entry.value) catch {
        submitFailure(session, stream, .internal, "metadata allocation failed");
        return;
    };

    if (response.status.isOk()) {
        stream.response_body = frame.encodeWithCompression(
            stream.allocator,
            response.payload,
            stream.response_compression,
        ) catch {
            submitFailure(session, stream, .internal, "response allocation failed");
            return;
        };
    }
    stream.setStatus(response.status) catch {
        stream.connection.close();
        return;
    };
    submitResponse(session, stream, context.initial_metadata.items()) catch stream.connection.close();
}

fn submitFailure(session: *c.nghttp2_session, stream: *Stream, code: status.Code, text: []const u8) void {
    removeStreamDeadline(stream);
    stream.setStatus(status.Status.init(code, text)) catch {
        stream.connection.close();
        return;
    };
    submitResponse(session, stream, &.{}) catch stream.connection.close();
}

fn submitResponse(session: *c.nghttp2_session, stream: *Stream, initial_metadata: []const metadata.Entry) !void {
    var headers: HeaderBuilder = .{};
    defer headers.deinit(stream.allocator);
    var encoded_values: EncodedValueBuilder = .{};
    defer {
        for (encoded_values.items()) |value| value.deinit(stream.allocator);
        encoded_values.deinit(stream.allocator);
    }
    try headers.append(stream.allocator, nativeHeader(":status", "200"));
    try headers.append(stream.allocator, nativeHeader("content-type", "application/grpc"));
    try headers.append(stream.allocator, nativeHeader("grpc-encoding", stream.response_compression.name()));
    try headers.append(stream.allocator, nativeHeader("grpc-accept-encoding", "identity,gzip"));
    for (initial_metadata) |entry| {
        if (!isReservedResponseHeader(entry.key)) {
            try appendMetadataHeader(&headers, &encoded_values, stream.allocator, entry);
        }
    }
    var provider: c.nghttp2_data_provider2 = .{
        .source = .{ .ptr = stream },
        .read_callback = readResponseData,
    };
    if (c.nghttp2_submit_response2(session, stream.id, headers.items().ptr, headers.items().len, &provider) != 0) return error.NativeFailure;
}

fn submitStreamingResponse(session: *c.nghttp2_session, stream: *Stream, initial_metadata: []const metadata.Entry) !void {
    var headers: HeaderBuilder = .{};
    defer headers.deinit(stream.allocator);
    var encoded_values: EncodedValueBuilder = .{};
    defer {
        for (encoded_values.items()) |value| value.deinit(stream.allocator);
        encoded_values.deinit(stream.allocator);
    }
    try headers.append(stream.allocator, nativeHeader(":status", "200"));
    try headers.append(stream.allocator, nativeHeader("content-type", "application/grpc"));
    try headers.append(stream.allocator, nativeHeader("grpc-encoding", stream.response_compression.name()));
    try headers.append(stream.allocator, nativeHeader("grpc-accept-encoding", "identity,gzip"));
    for (initial_metadata) |entry| {
        if (!isReservedResponseHeader(entry.key)) {
            try appendMetadataHeader(&headers, &encoded_values, stream.allocator, entry);
        }
    }
    var provider: c.nghttp2_data_provider2 = .{
        .source = .{ .ptr = stream },
        .read_callback = readStreamingResponseData,
    };
    if (c.nghttp2_submit_response2(session, stream.id, headers.items().ptr, headers.items().len, &provider) != 0) return error.NativeFailure;
}

fn readResponseData(
    session: ?*c.nghttp2_session,
    _: i32,
    output: [*c]u8,
    output_length: usize,
    data_flags: ?*u32,
    source: ?*c.nghttp2_data_source,
    _: ?*anyopaque,
) callconv(.c) c.nghttp2_ssize {
    const stream: *Stream = @ptrCast(@alignCast(source.?.*.ptr.?));
    const remaining = stream.response_body[stream.response_offset..];
    const length = @min(remaining.len, output_length);
    if (length != 0) {
        @memcpy(output[0..length], remaining[0..length]);
        stream.response_offset += length;
    }
    if (stream.response_offset == stream.response_body.len) {
        data_flags.?.* |= c.NGHTTP2_DATA_FLAG_EOF | c.NGHTTP2_DATA_FLAG_NO_END_STREAM;
        if (!stream.trailer_submitted) {
            stream.trailer_submitted = true;
            submitTrailers(session.?, stream) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
        }
    }
    return @intCast(length);
}

fn readStreamingResponseData(
    session: ?*c.nghttp2_session,
    _: i32,
    output: [*c]u8,
    output_length: usize,
    data_flags: ?*u32,
    source: ?*c.nghttp2_data_source,
    _: ?*anyopaque,
) callconv(.c) c.nghttp2_ssize {
    const stream: *Stream = @ptrCast(@alignCast(source.?.*.ptr.?));
    const streaming = &(stream.streaming orelse return c.NGHTTP2_ERR_CALLBACK_FAILURE);
    if (streaming.nextOutbound()) |item| {
        const remaining = item.bytes[item.offset..];
        const length = @min(remaining.len, output_length);
        @memcpy(output[0..length], remaining[0..length]);
        item.offset += length;
        releaseOutboundReservation(stream, length, true);
        if (item.offset == item.bytes.len) {
            streaming.finishOutbound();
        }
        if (streaming.nextOutbound() == null and stream.response_finished) {
            data_flags.?.* |= c.NGHTTP2_DATA_FLAG_EOF | c.NGHTTP2_DATA_FLAG_NO_END_STREAM;
            if (!stream.trailer_submitted) {
                stream.trailer_submitted = true;
                submitStreamingTrailers(session.?, stream) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
            }
        }
        return @intCast(length);
    }
    if (!stream.response_finished) return c.NGHTTP2_ERR_DEFERRED;

    data_flags.?.* |= c.NGHTTP2_DATA_FLAG_EOF | c.NGHTTP2_DATA_FLAG_NO_END_STREAM;
    if (!stream.trailer_submitted) {
        stream.trailer_submitted = true;
        submitStreamingTrailers(session.?, stream) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    }
    return 0;
}

fn submitStreamingTrailers(session: *c.nghttp2_session, stream: *Stream) !void {
    try submitTrailers(session, stream);
}

fn submitStreamingFailureReset(stream: *Stream) !void {
    if (!stream.reset_after_trailers or stream.reset_submitted) return;
    const session = stream.connection.session orelse return;
    if (c.nghttp2_submit_rst_stream(session, c.NGHTTP2_FLAG_NONE, stream.id, c.NGHTTP2_CANCEL) != 0) {
        return error.NativeFailure;
    }
    stream.reset_submitted = true;
}

fn submitTrailers(session: *c.nghttp2_session, stream: *Stream) !void {
    var trailers: HeaderBuilder = .{};
    defer trailers.deinit(stream.allocator);
    var encoded_values: EncodedValueBuilder = .{};
    defer {
        for (encoded_values.items()) |value| value.deinit(stream.allocator);
        encoded_values.deinit(stream.allocator);
    }
    for (stream.trailing_metadata.items()) |entry| {
        if (!isReservedTrailer(entry.key)) {
            try appendMetadataHeader(&trailers, &encoded_values, stream.allocator, entry);
        }
    }
    var code_buffer: [3]u8 = undefined;
    const code = try std.fmt.bufPrint(&code_buffer, "{d}", .{@intFromEnum(stream.response_code)});
    try trailers.append(stream.allocator, nativeHeader("grpc-status", code));
    const encoded = try message.encode(stream.allocator, stream.response_message);
    defer stream.allocator.free(encoded);
    try trailers.append(stream.allocator, nativeHeader("grpc-message", encoded));
    if (c.nghttp2_submit_trailer(session, stream.id, trailers.items().ptr, trailers.items().len) != 0) return error.NativeFailure;
}

fn onFrameSent(session: ?*c.nghttp2_session, sent_frame: ?*const c.nghttp2_frame, user_data: ?*anyopaque) callconv(.c) c_int {
    const native_frame = sent_frame.?.*;
    if (native_frame.hd.type != c.NGHTTP2_HEADERS or native_frame.hd.flags & c.NGHTTP2_FLAG_END_STREAM == 0) return 0;
    const connection: *Connection = @ptrCast(@alignCast(user_data.?));
    const stream = connection.streams.get(native_frame.hd.stream_id) orelse return 0;
    if (!stream.reset_after_trailers or stream.reset_submitted) return 0;
    if (c.nghttp2_submit_rst_stream(session, c.NGHTTP2_FLAG_NONE, stream.id, c.NGHTTP2_CANCEL) != 0) {
        return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    }
    stream.reset_submitted = true;
    return 0;
}

fn onStreamClose(_: ?*c.nghttp2_session, stream_id: i32, stream_error: u32, user_data: ?*anyopaque) callconv(.c) c_int {
    const connection: *Connection = @ptrCast(@alignCast(user_data.?));
    if (connection.streams.fetchRemove(stream_id)) |entry| {
        const local_failure_reset = entry.value.reset_after_trailers and entry.value.reset_submitted;
        if (entry.value.streaming != null and (stream_error != c.NGHTTP2_NO_ERROR or !entry.value.trailer_submitted)) {
            cancelStreaming(
                entry.value,
                if (local_failure_reset) entry.value.terminal_reason else .peer_cancelled,
            );
        }
        _ = retireStream(entry.value);
    }
    if (connection.draining) {
        finishDrainIfIdle(connection.server);
    } else if (connection.streams.count() == 0) {
        armIdleDeadline(connection) catch return c.NGHTTP2_ERR_CALLBACK_FAILURE;
    }
    return 0;
}

fn ioNow(context: ?*anyopaque) u64 {
    const server: *Impl = @ptrCast(@alignCast(context.?));
    return fast_clock.now(server.io());
}

fn syncIo() std.Io {
    return std.Io.Threaded.global_single_threaded.io();
}

fn scheduleDeadlineTimer(server: *Impl) void {
    if (!server.loop_initialized or !server.deadline_timer_initialized) return;
    const earliest = (deadlineHeapPeek(server) orelse return).expires_at_ns;
    if (server.deadline_timer_armed and server.deadline_timer_target_ns == earliest) return;

    const remaining = earliest -| server.clock.now();
    const timeout_ms = @max(@as(u64, 1), std.math.divCeil(u64, remaining, std.time.ns_per_ms) catch 1);
    server.deadline_timer_target_ns = earliest;
    if (server.deadline_timer_armed) {
        server.deadline_timer.reset(
            &server.loop,
            &server.deadline_timer_completion,
            &server.deadline_timer_cancel_completion,
            timeout_ms,
            Impl,
            server,
            onDeadlineTimer,
        );
    } else {
        server.deadline_timer_armed = true;
        server.deadline_timer.run(
            &server.loop,
            &server.deadline_timer_completion,
            timeout_ms,
            Impl,
            server,
            onDeadlineTimer,
        );
    }
}

fn onDeadlineTimer(server: ?*Impl, _: *xev.Loop, _: *xev.Completion, result: xev.Timer.RunError!void) xev.CallbackAction {
    const impl = server orelse return .disarm;
    impl.deadline_timer_armed = false;
    impl.deadline_timer_target_ns = null;
    result catch |err| switch (err) {
        error.Canceled => {
            scheduleDeadlineTimer(impl);
            return .disarm;
        },
        else => return .disarm,
    };
    const now = fast_clock.validatedNow(impl.io());
    expireDeadlines(impl, now);
    drainDirtyConnections(impl);
    scheduleDeadlineTimer(impl);
    return .disarm;
}

fn expireDeadlines(server: *Impl, now: u64) void {
    while (deadlineHeapPeek(server)) |next| {
        if (next.expires_at_ns > now) return;
        const entry = deadlineHeapPop(server).?;
        switch (entry.target) {
            .connection => |connection| {
                const value = connection.connection_deadline orelse continue;
                if (value.expires_at_ns != entry.expires_at_ns) continue;
                connection.connection_deadline = null;
                if (connection.closing or connection.session == null) continue;
                server.lock();
                const shutting_down = server.state != .running or server.shutdown_request != .none;
                server.unlock();
                if (shutting_down) continue;
                switch (value.kind) {
                    .tls_handshake => {
                        if (connection.tls_handshaking) connection.closeOnLoop(&server.loop);
                    },
                    .cleartext_preface => connection.closeOnLoop(&server.loop),
                    .idle => {
                        if (connection.streams.count() != 0 or connection.draining) continue;
                        connection.draining = true;
                        connection.submitGoAway(
                            connection.highest_accepted_stream_id,
                            c.NGHTTP2_NO_ERROR,
                        ) catch {
                            connection.closeOnLoop(&server.loop);
                            continue;
                        };
                        const goaway_fully_queued = connection.flushIdleGoAway() catch |err| {
                            server.logger.write(.debug, "server idle GOAWAY flush failed error={s}", .{@errorName(err)});
                            connection.close();
                            continue;
                        };
                        server.logger.write(.debug, "server idle close last_stream_id={d} goaway_fully_queued={} queued_write_bytes={d}", .{
                            connection.highest_accepted_stream_id, goaway_fully_queued, connection.queued_write_bytes,
                        });
                        if (goaway_fully_queued)
                            connection.closeGracefully(&server.loop)
                        else
                            connection.close();
                    },
                }
            },
            .stream => |stream| {
                const value = stream.deadline orelse continue;
                if (value.expires_at_ns != entry.expires_at_ns) continue;
                const connection = stream.connection;
                if (connection.closing or connection.session == null) continue;
                if (stream.streaming) |_| {
                    if (!stream.streaming_active) continue;
                    cancelStreaming(stream, .deadline_exceeded);
                    failStreaming(stream, .deadline_exceeded, "deadline exceeded");
                } else {
                    if (stream.responded) continue;
                    stream.responded = true;
                    submitFailure(connection.session.?, stream, .deadline_exceeded, "deadline exceeded");
                }
                if (!connection.closing) _ = server.enqueueDirtyConnection(connection);
            },
        }
    }
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
    for (0..response_header_stack_capacity * 4) |_| {
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
    for (0..response_header_stack_capacity) |_| {
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

test "server header builder stack path does not allocate" {
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{
        .fail_index = 0,
    });
    var headers: HeaderBuilder = .{};
    defer headers.deinit(failing.allocator());

    for (0..response_header_stack_capacity) |_| {
        try headers.append(failing.allocator(), nativeHeader("x-test", "value"));
    }
    try std.testing.expect(!headers.overflowed);
    try std.testing.expectEqual(response_header_stack_capacity, headers.items().len);
}

test "server header builder overflow preserves order" {
    const values = [_][]const u8{ "0", "1", "2", "3", "4" };
    var headers: HeaderBuilder = .{};
    defer headers.deinit(std.testing.allocator);

    for (values) |value| try headers.append(std.testing.allocator, nativeHeader("x-test", value));
    try std.testing.expect(headers.overflowed);
    for (headers.items(), values) |header, value| {
        try std.testing.expectEqualStrings(value, header.value[0..header.valuelen]);
    }
}

test "server header builder handles every overflow allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testHeaderBuilderAllocations,
        .{},
    );
}

test "server mixed metadata header cleanup handles every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testMixedMetadataHeaderCleanup,
        .{},
    );
}

fn isValidMethodPath(path: []const u8) bool {
    if (path.len < 4 or path[0] != '/') return false;
    const separator = std.mem.indexOfScalarPos(u8, path, 1, '/') orelse return false;
    return separator > 1 and separator + 1 < path.len and std.mem.indexOfScalarPos(u8, path, separator + 1, '/') == null;
}

fn acceptsEncoding(value: []const u8, encoding: Compression) bool {
    var values = std.mem.splitScalar(u8, value, ',');
    while (values.next()) |item| {
        if (std.mem.eql(u8, std.mem.trim(u8, item, " \t"), encoding.name())) return true;
    }
    return false;
}

fn isRequestMetadata(name: []const u8) bool {
    return metadata.isApplicationKey(name) and !isReservedRequestHeader(name);
}

fn isReservedRequestHeader(name: []const u8) bool {
    const protocol_headers = [_][]const u8{ "content-type", "te", "user-agent" };
    for (protocol_headers) |header| if (std.mem.eql(u8, name, header)) return true;
    return std.mem.startsWith(u8, name, "grpc-");
}

fn isMalformedRequestMetadataName(name: []const u8) bool {
    if (name.len == 0 or name[0] == ':' or isReservedRequestHeader(name)) return false;
    return !metadata.isValidKey(name);
}

fn isReservedResponseHeader(name: []const u8) bool {
    return std.mem.eql(u8, name, "content-type") or
        std.mem.eql(u8, name, "grpc-encoding") or
        std.mem.eql(u8, name, "grpc-accept-encoding") or
        std.mem.eql(u8, name, "grpc-status") or
        std.mem.eql(u8, name, "grpc-message");
}

fn wireMessageLimit(max_message_size: usize) usize {
    const overhead = std.math.add(usize, max_message_size / 8, 1024) catch return std.math.maxInt(usize);
    const total_overhead = std.math.add(usize, overhead, frame.header_size) catch return std.math.maxInt(usize);
    return std.math.add(usize, max_message_size, total_overhead) catch std.math.maxInt(usize);
}

fn validateTransportOptions(initial_stream_window_size: u32, high: usize, low: usize) !void {
    if (initial_stream_window_size == 0 or initial_stream_window_size > std.math.maxInt(i32)) {
        return error.InvalidInitialStreamWindowSize;
    }
    if (low == 0 or low >= high) return error.InvalidWriteWatermarks;
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

fn isReservedTrailer(name: []const u8) bool {
    return std.mem.eql(u8, name, "grpc-status") or std.mem.eql(u8, name, "grpc-message");
}

fn appendTestHeader(block: *std.ArrayList(u8), name: []const u8, value: []const u8) !void {
    try block.append(std.testing.allocator, 0);
    try block.append(std.testing.allocator, @intCast(name.len));
    try block.appendSlice(std.testing.allocator, name);
    try block.append(std.testing.allocator, @intCast(value.len));
    try block.appendSlice(std.testing.allocator, value);
}

fn appendRawTestHeaders(wire: *std.ArrayList(u8), stream_id: i32, path: []const u8, end_stream: bool) !void {
    var block: std.ArrayList(u8) = .empty;
    defer block.deinit(std.testing.allocator);
    try block.appendSlice(std.testing.allocator, &.{ 0x83, 0x86, 0x04 });
    try block.append(std.testing.allocator, @intCast(path.len));
    try block.appendSlice(std.testing.allocator, path);
    try block.appendSlice(std.testing.allocator, &.{ 0x01, 0x09 });
    try block.appendSlice(std.testing.allocator, "localhost");
    try appendTestHeader(&block, "content-type", "application/grpc");

    const id: u32 = @intCast(stream_id);
    try wire.appendSlice(std.testing.allocator, &.{
        @intCast(block.items.len >> 16),
        @intCast(block.items.len >> 8),
        @intCast(block.items.len),
        c.NGHTTP2_HEADERS,
        @as(u8, @intCast(c.NGHTTP2_FLAG_END_HEADERS)) |
            if (end_stream) @as(u8, @intCast(c.NGHTTP2_FLAG_END_STREAM)) else 0,
        @intCast((id >> 24) & 0x7f),
        @intCast(id >> 16),
        @intCast(id >> 8),
        @intCast(id),
    });
    try wire.appendSlice(std.testing.allocator, block.items);
}

fn appendRawTestData(wire: *std.ArrayList(u8), stream_id: i32, payload: []const u8, end_stream: bool) !void {
    const id: u32 = @intCast(stream_id);
    try wire.appendSlice(std.testing.allocator, &.{
        @intCast(payload.len >> 16),
        @intCast(payload.len >> 8),
        @intCast(payload.len),
        c.NGHTTP2_DATA,
        if (end_stream) @as(u8, @intCast(c.NGHTTP2_FLAG_END_STREAM)) else 0,
        @intCast((id >> 24) & 0x7f),
        @intCast(id >> 16),
        @intCast(id >> 8),
        @intCast(id),
    });
    try wire.appendSlice(std.testing.allocator, payload);
}

fn testOutputHasFrame(bytes: []const u8, stream_id: i32, frame_type: u8, required_flags: u8) bool {
    var offset: usize = 0;
    while (offset + 9 <= bytes.len) {
        const payload_length = (@as(usize, bytes[offset]) << 16) |
            (@as(usize, bytes[offset + 1]) << 8) |
            bytes[offset + 2];
        const end = offset + 9 + payload_length;
        if (end > bytes.len) return false;
        const id = (@as(i32, bytes[offset + 5] & 0x7f) << 24) |
            (@as(i32, bytes[offset + 6]) << 16) |
            (@as(i32, bytes[offset + 7]) << 8) |
            bytes[offset + 8];
        if (id == stream_id and bytes[offset + 3] == frame_type and bytes[offset + 4] & required_flags == required_flags) return true;
        offset = end;
    }
    return false;
}

const TestResponseCapture = struct {
    stream1_data: std.ArrayList(u8) = .empty,
    stream3_data: std.ArrayList(u8) = .empty,
    stream1_status: ?u32 = null,
    stream3_status: ?u32 = null,
    stream1_message_matches: bool = false,
    stream1_trailing_metadata_matches: bool = false,
    stream1_ended: bool = false,
    stream3_ended: bool = false,
    stream1_reset: bool = false,

    fn deinit(self: *@This()) void {
        self.stream1_data.deinit(std.testing.allocator);
        self.stream3_data.deinit(std.testing.allocator);
    }

    fn decode(self: *@This(), bytes: []const u8) !void {
        var inflater: ?*c.nghttp2_hd_inflater = null;
        if (c.nghttp2_hd_inflate_new(&inflater) != 0) return error.OutOfMemory;
        defer c.nghttp2_hd_inflate_del(inflater);

        var offset: usize = 0;
        while (offset + 9 <= bytes.len) {
            const payload_length = (@as(usize, bytes[offset]) << 16) |
                (@as(usize, bytes[offset + 1]) << 8) |
                bytes[offset + 2];
            const end = offset + 9 + payload_length;
            if (end > bytes.len) return error.TruncatedFrame;
            const frame_type = bytes[offset + 3];
            const flags = bytes[offset + 4];
            const stream_id = (@as(i32, bytes[offset + 5] & 0x7f) << 24) |
                (@as(i32, bytes[offset + 6]) << 16) |
                (@as(i32, bytes[offset + 7]) << 8) |
                bytes[offset + 8];
            const payload = bytes[offset + 9 .. end];
            if (frame_type == c.NGHTTP2_DATA) {
                const destination = if (stream_id == 1)
                    &self.stream1_data
                else if (stream_id == 3)
                    &self.stream3_data
                else
                    null;
                if (destination) |data| try data.appendSlice(std.testing.allocator, payload);
            } else if (frame_type == c.NGHTTP2_HEADERS) {
                if (flags & c.NGHTTP2_FLAG_END_HEADERS == 0) return error.TestHeaderContinuationUnsupported;
                try self.decodeHeaderBlock(inflater.?, stream_id, payload);
            } else if (frame_type == c.NGHTTP2_RST_STREAM and stream_id == 1) {
                self.stream1_reset = true;
            }
            if (flags & c.NGHTTP2_FLAG_END_STREAM != 0) {
                if (stream_id == 1) self.stream1_ended = true;
                if (stream_id == 3) self.stream3_ended = true;
            }
            offset = end;
        }
        if (offset != bytes.len) return error.TruncatedFrame;
    }

    fn decodeHeaderBlock(self: *@This(), inflater: *c.nghttp2_hd_inflater, stream_id: i32, block: []const u8) !void {
        var offset: usize = 0;
        while (true) {
            var nv: c.nghttp2_nv = undefined;
            var inflate_flags: c_int = 0;
            const consumed = c.nghttp2_hd_inflate_hd2(
                inflater,
                &nv,
                &inflate_flags,
                block[offset..].ptr,
                block.len - offset,
                1,
            );
            if (consumed < 0) return error.HeaderDecodeFailed;
            offset += @intCast(consumed);
            if (inflate_flags & c.NGHTTP2_HD_INFLATE_EMIT != 0) {
                self.captureHeader(stream_id, nv.name[0..nv.namelen], nv.value[0..nv.valuelen]) catch return error.InvalidHeader;
            }
            if (inflate_flags & c.NGHTTP2_HD_INFLATE_FINAL != 0) {
                if (c.nghttp2_hd_inflate_end_headers(inflater) != 0) return error.HeaderDecodeFailed;
                return;
            }
            if (consumed == 0 and inflate_flags & c.NGHTTP2_HD_INFLATE_EMIT == 0) return error.HeaderDecodeFailed;
        }
    }

    fn captureHeader(self: *@This(), stream_id: i32, name: []const u8, value: []const u8) !void {
        if (std.mem.eql(u8, name, "grpc-status")) {
            const code = try std.fmt.parseInt(u32, value, 10);
            if (stream_id == 1) self.stream1_status = code;
            if (stream_id == 3) self.stream3_status = code;
        } else if (stream_id == 1 and std.mem.eql(u8, name, "grpc-message")) {
            self.stream1_message_matches = std.mem.eql(u8, value, "complete");
        } else if (stream_id == 1 and std.mem.eql(u8, name, "x-stream-trailer")) {
            self.stream1_trailing_metadata_matches = std.mem.eql(u8, value, "yes");
        }
    }
};

const TestRequestOptions = struct {
    stream_id: i32 = 1,
    include_preface: bool = true,
    end_stream: bool = true,
    body: ?[]const u8 = null,
    timeout_values: []const []const u8 = &.{},
    metadata_entries: []const metadata.Entry = &.{},
};

fn feedTestRequest(connection: *Connection, options: TestRequestOptions) !void {
    var header_block: std.ArrayList(u8) = .empty;
    defer header_block.deinit(std.testing.allocator);
    try header_block.appendSlice(std.testing.allocator, &.{ 0x83, 0x86, 0x04, 0x10 });
    try header_block.appendSlice(std.testing.allocator, "/test.Echo/Unary");
    try header_block.appendSlice(std.testing.allocator, &.{ 0x01, 0x09 });
    try header_block.appendSlice(std.testing.allocator, "localhost");
    try appendTestHeader(&header_block, "content-type", "application/grpc");
    for (options.timeout_values) |value| try appendTestHeader(&header_block, "grpc-timeout", value);
    for (options.metadata_entries) |entry| try appendTestHeader(&header_block, entry.key, entry.value);

    var wire: std.ArrayList(u8) = .empty;
    defer wire.deinit(std.testing.allocator);
    if (options.include_preface) {
        try wire.appendSlice(std.testing.allocator, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n");
        try wire.appendSlice(std.testing.allocator, &.{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 });
    }
    const stream_id: u32 = @intCast(options.stream_id);
    const headers_end_stream = options.end_stream and options.body == null;
    try wire.appendSlice(std.testing.allocator, &.{
        @intCast(header_block.items.len >> 16),
        @intCast(header_block.items.len >> 8),
        @intCast(header_block.items.len),
        c.NGHTTP2_HEADERS,
        @as(u8, @intCast(c.NGHTTP2_FLAG_END_HEADERS)) |
            if (headers_end_stream) @as(u8, @intCast(c.NGHTTP2_FLAG_END_STREAM)) else 0,
        @intCast((stream_id >> 24) & 0x7f),
        @intCast(stream_id >> 16),
        @intCast(stream_id >> 8),
        @intCast(stream_id),
    });
    try wire.appendSlice(std.testing.allocator, header_block.items);
    if (options.body) |body| {
        const encoded = try frame.encode(std.testing.allocator, body);
        defer std.testing.allocator.free(encoded);
        try wire.appendSlice(std.testing.allocator, &.{
            @intCast(encoded.len >> 16),
            @intCast(encoded.len >> 8),
            @intCast(encoded.len),
            c.NGHTTP2_DATA,
            if (options.end_stream) @as(u8, @intCast(c.NGHTTP2_FLAG_END_STREAM)) else 0,
            @intCast((stream_id >> 24) & 0x7f),
            @intCast(stream_id >> 16),
            @intCast(stream_id >> 8),
            @intCast(stream_id),
        });
        try wire.appendSlice(std.testing.allocator, encoded);
    }

    const consumed = c.nghttp2_session_mem_recv2(connection.session, wire.items.ptr, wire.items.len);
    try std.testing.expectEqual(@as(c.nghttp2_ssize, @intCast(wire.items.len)), consumed);
}

fn deinitTestConnection(connection: *Connection) void {
    if (connection.session) |session| c.nghttp2_session_del(session);
    var iterator = connection.streams.iterator();
    while (iterator.next()) |entry| {
        std.debug.assert(retireStream(entry.value_ptr.*));
    }
    connection.streams.deinit(connection.allocator());
    drainWriteRequestPool(connection);
}

fn exchangeRawHttp2(server: *Server, input: []const u8) ![]u8 {
    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const stream = try address.connect(io, .{
        .mode = .stream,
        .timeout = .none,
    });
    defer stream.close(io);

    var write_buffer: [256]u8 = undefined;
    var writer = stream.writer(io, &write_buffer);
    writer.interface.writeAll(input) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;

    var output: std.ArrayList(u8) = .empty;
    errdefer output.deinit(std.testing.allocator);
    var read_buffer: [256]u8 = undefined;
    var reader = stream.reader(io, &.{});
    while (true) {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = stream.socket.handle,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
        const length = reader.interface.readSliceShort(&read_buffer) catch return reader.err.?;
        try output.appendSlice(std.testing.allocator, read_buffer[0..length]);
        if (length == 0) break;
    }
    return output.toOwnedSlice(std.testing.allocator);
}

test "dirty connection queue deduplicates and preserves FIFO" {
    var server: Impl = undefined;
    server.dirty_connection_head = null;
    server.dirty_connection_tail = null;
    var first = Connection{ .server = &server };
    var second = Connection{ .server = &server };

    try std.testing.expect(server.enqueueDirtyConnection(&first));
    try std.testing.expect(!server.enqueueDirtyConnection(&first));
    try std.testing.expect(!server.enqueueDirtyConnection(&second));
    try std.testing.expect(server.popDirtyConnection() == &first);
    try std.testing.expect(server.popDirtyConnection() == &second);
    try std.testing.expect(server.popDirtyConnection() == null);

    try std.testing.expect(server.enqueueDirtyConnection(&first));
    try std.testing.expect(server.popDirtyConnection() == &first);
    try std.testing.expect(!first.dirty_queued);
}

test "dirty connection queue safely removes closing connections" {
    var server: Impl = undefined;
    server.dirty_connection_head = null;
    server.dirty_connection_tail = null;
    var first = Connection{ .server = &server };
    var closing = Connection{ .server = &server, .closing = true };
    var last = Connection{ .server = &server };

    _ = server.enqueueDirtyConnection(&first);
    _ = server.enqueueDirtyConnection(&closing);
    _ = server.enqueueDirtyConnection(&last);
    server.removeDirtyConnection(&closing);
    try std.testing.expect(!closing.dirty_queued);
    try std.testing.expect(closing.dirty_next == null);
    try std.testing.expect(server.popDirtyConnection() == &first);
    try std.testing.expect(server.popDirtyConnection() == &last);

    _ = server.enqueueDirtyConnection(&first);
    _ = server.enqueueDirtyConnection(&closing);
    _ = server.enqueueDirtyConnection(&last);
    server.removeDirtyConnection(&first);
    server.removeDirtyConnection(&last);
    try std.testing.expect(server.popDirtyConnection() == &closing);
    try std.testing.expect(server.popDirtyConnection() == null);
}

test "server write request pool reuses descriptors without allocation" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    defer drainWriteRequestPool(&connection);

    const first = try acquireWriteRequest(&connection, &.{});
    releaseWriteRequest(&connection, first);

    const operation_count = server.coordinator.serialized_allocator.operation_count.load(.monotonic);
    const reused = try acquireWriteRequest(&connection, &.{});
    try std.testing.expectEqual(
        operation_count,
        server.coordinator.serialized_allocator.operation_count.load(.monotonic),
    );
    try std.testing.expect(reused == first);
    try std.testing.expectEqual(@as(usize, 0), connection.write_request_pool_count);
    releaseWriteRequest(&connection, reused);
    try std.testing.expectEqual(@as(usize, 1), connection.write_request_pool_count);
}

test "server write request pool is LIFO and drains with connection" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    defer if (connection.write_request_pool_head != null) drainWriteRequestPool(&connection);

    const first = try acquireWriteRequest(&connection, &.{});
    const second = try acquireWriteRequest(&connection, &.{});
    const third = try acquireWriteRequest(&connection, &.{});
    releaseWriteRequest(&connection, first);
    releaseWriteRequest(&connection, second);
    releaseWriteRequest(&connection, third);

    const reused_third = try acquireWriteRequest(&connection, &.{});
    const reused_second = try acquireWriteRequest(&connection, &.{});
    const reused_first = try acquireWriteRequest(&connection, &.{});
    try std.testing.expect(reused_third == third);
    try std.testing.expect(reused_second == second);
    try std.testing.expect(reused_first == first);
    try std.testing.expect(reused_third != reused_second);
    try std.testing.expect(reused_second != reused_first);
    releaseWriteRequest(&connection, reused_third);
    releaseWriteRequest(&connection, reused_second);
    releaseWriteRequest(&connection, reused_first);

    drainWriteRequestPool(&connection);
    try std.testing.expect(connection.write_request_pool_head == null);
    try std.testing.expectEqual(@as(usize, 0), connection.write_request_pool_count);
}

test "server write queue bookkeeping failure returns descriptor to pool" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    defer {
        connection.pending_writes = 0;
        drainWriteRequestPool(&connection);
    }

    const pooled = try acquireWriteRequest(&connection, &.{});
    releaseWriteRequest(&connection, pooled);
    connection.pending_writes = std.math.maxInt(usize);
    const bytes = try server.impl.localAllocator().dupe(u8, "owned");
    try std.testing.expectError(error.WriteQueueSizeOverflow, connection.queueOwnedSocketWrite(bytes));
    try std.testing.expectEqual(std.math.maxInt(usize), connection.pending_writes);
    try std.testing.expectEqual(@as(usize, 1), connection.write_request_pool_count);
    try std.testing.expect(connection.write_request_pool_head == pooled);
    try std.testing.expectEqual(@as(usize, 0), connection.queued_write_bytes);
    connection.pending_writes = 0;
}

test "server write callback and discard each release once" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    server.impl.write_low_watermark_bytes = 0;
    var connection = Connection{ .server = server.impl };
    defer drainWriteRequestPool(&connection);

    const callback_bytes = try server.impl.localAllocator().dupe(u8, "callback");
    const callback_write = try acquireWriteRequest(&connection, callback_bytes);
    connection.pending_writes = 1;
    connection.queued_write_bytes = callback_bytes.len;
    var loop: xev.Loop = undefined;
    _ = onWrite(
        callback_write,
        &loop,
        &callback_write.request.completion,
        undefined,
        .{ .slice = callback_bytes },
        callback_bytes.len,
    );
    try std.testing.expectEqual(@as(usize, 1), connection.write_request_pool_count);

    const discarded_bytes = try server.impl.localAllocator().dupe(u8, "discarded");
    const discarded_write = try acquireWriteRequest(&connection, discarded_bytes);
    connection.pending_writes = 1;
    connection.queued_write_bytes = discarded_bytes.len;
    connection.write_queue.push(&discarded_write.request);
    connection.discardQueuedWrites();
    try std.testing.expectEqual(@as(usize, 1), connection.write_request_pool_count);
    try std.testing.expect(connection.write_request_pool_head == discarded_write);
    try std.testing.expectEqual(@as(usize, 0), connection.pending_writes);
    try std.testing.expectEqual(@as(usize, 0), connection.queued_write_bytes);
}

test "reactor local small allocations avoid serialized backing operations" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    const local_allocator = server.impl.localAllocator();
    var connection = Connection{ .server = server.impl };

    const warm_bytes = try local_allocator.alloc(u8, 64);
    defer local_allocator.free(warm_bytes);
    const warm_stream = try local_allocator.create(Stream);
    warm_stream.* = Stream.init(local_allocator, &connection, 1);
    defer destroyStream(warm_stream);

    const serialized = server.coordinator.serialized_allocator;
    const warmed_count = serialized.operation_count.load(.monotonic);
    for (0..1000) |_| {
        const bytes = try local_allocator.alloc(u8, 64);
        local_allocator.free(bytes);
    }
    try std.testing.expectEqual(warmed_count, serialized.operation_count.load(.monotonic));

    const reused_stream = try local_allocator.create(Stream);
    reused_stream.* = Stream.init(local_allocator, &connection, 3);
    destroyStream(reused_stream);
    try std.testing.expectEqual(warmed_count, serialized.operation_count.load(.monotonic));

    var streams: [1000]*Stream = undefined;
    var created: usize = 0;
    defer for (streams[0..created]) |stream| destroyStream(stream);
    while (created < streams.len) : (created += 1) {
        streams[created] = try local_allocator.create(Stream);
        streams[created].* = Stream.init(local_allocator, &connection, @intCast(created * 2 + 5));
    }
    for (streams[0..created]) |stream| destroyStream(stream);
    created = 0;
    const refill_operations = serialized.operation_count.load(.monotonic) - warmed_count;
    try std.testing.expect(refill_operations > 0);
    try std.testing.expect(refill_operations < streams.len / 10);
}

test "cross-thread server stream commands retain shared ownership" {
    const Handler = struct {
        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            _: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            _: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            return .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}
    };
    const Worker = struct {
        stream: raw_stream.ServerStream,
        send_error: ?anyerror = null,
        finish_error: ?anyerror = null,

        fn run(self: *@This()) void {
            self.stream.send("from-worker", .{}) catch |err| {
                self.send_error = err;
            };
            self.stream.finish(.init(.ok, "worker-finished")) catch |err| {
                self.finish_error = err;
            };
        }
    };

    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    server.impl.local_allocator_state.enterLoop();
    defer server.impl.local_allocator_state.leaveLoop();

    var connection = Connection{ .server = server.impl };
    const target = try std.testing.allocator.create(Stream);
    target.* = Stream.init(std.testing.allocator, &connection, 1);
    defer {
        target.deinit();
        std.testing.allocator.destroy(target);
    }
    target.streaming = .{
        .handler = .{
            .on_start = Handler.onStart,
            .on_message = Handler.onMessage,
            .on_remote_end = Handler.onRemoteEnd,
        },
        .decoder = frame.Decoder.init(std.testing.allocator, 1024),
        .context = service.ServerContext.init(std.testing.allocator),
    };
    target.streaming_active = true;
    target.receive_paused = true;
    target.resume_queued = true;
    try server.impl.stream_commands.append(server.impl.shared_allocator, .{
        .target = target,
        .action = .resume_receive,
    });
    target.command_refs = 1;

    var worker = Worker{ .stream = target.serverHandle() };
    const thread = try std.Thread.spawn(.{}, Worker.run, .{&worker});
    thread.join();
    try std.testing.expect(worker.send_error == null);
    try std.testing.expect(worker.finish_error == null);

    processStreamCommands(server.impl);
    try std.testing.expectEqual(@as(usize, 0), target.command_refs);
    try std.testing.expect(target.response_finished);
    try std.testing.expectEqualStrings("worker-finished", target.response_message);
    try std.testing.expect(target.response_message_allocator.?.ptr == server.impl.shared_allocator.ptr);
    const outbound = target.streaming.?.nextOutbound().?;
    try std.testing.expect(outbound.allocator.ptr == server.impl.shared_allocator.ptr);
}

test "retained server call observes cancellation after transport retirement" {
    const Capture = struct {
        cancels: usize = 0,
        terminals: usize = 0,
        terminal_id: ?raw_stream.ServerCallId = null,
        terminal_reason: ?raw_stream.ServerTerminalReason = null,
    };
    const Handler = struct {
        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            _: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            _: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            return .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onCancel(context: ?*anyopaque, stream: raw_stream.ServerStream, _: *service.ServerContext) void {
            const capture: *Capture = @ptrCast(@alignCast(context.?));
            var retained = stream.retain() catch unreachable;
            defer retained.deinit();
            std.debug.assert(retained.isCancelled());
            capture.cancels += 1;
        }

        fn onTerminal(
            context: ?*anyopaque,
            call_id: raw_stream.ServerCallId,
            reason: raw_stream.ServerTerminalReason,
        ) void {
            const capture: *Capture = @ptrCast(@alignCast(context.?));
            capture.terminals += 1;
            capture.terminal_id = call_id;
            capture.terminal_reason = reason;
        }
    };

    var capture = Capture{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    const target = try std.testing.allocator.create(Stream);
    target.* = Stream.init(std.testing.allocator, &connection, 1);
    target.streaming = .{
        .handler = .{
            .context = &capture,
            .on_start = Handler.onStart,
            .on_message = Handler.onMessage,
            .on_remote_end = Handler.onRemoteEnd,
            .on_cancel = Handler.onCancel,
            .on_terminal = Handler.onTerminal,
        },
        .decoder = frame.Decoder.init(std.testing.allocator, 1024),
        .context = service.ServerContext.init(std.testing.allocator),
    };
    target.streaming_active = true;
    const control = try server.impl.shared_allocator.create(ServerCallControl);
    control.* = .{
        .allocator = server.impl.shared_allocator,
        .server = server.impl,
        .target = target,
    };
    target.call_control = control;
    var retained = try target.serverHandle().retain();
    defer retained.deinit();
    const call_id = retained.id();

    cancelStreaming(target, .peer_cancelled);
    try std.testing.expect(retained.isCancelled());
    try std.testing.expect(!retained.isTerminal());
    try std.testing.expect(retireStream(target));

    try std.testing.expect(retained.isTerminal());
    try std.testing.expectError(error.CallClosed, retained.resumeReceive());
    try std.testing.expectEqual(@as(usize, 1), capture.cancels);
    try std.testing.expectEqual(@as(usize, 1), capture.terminals);
    try std.testing.expectEqual(call_id, capture.terminal_id.?);
    try std.testing.expectEqual(raw_stream.ServerTerminalReason.peer_cancelled, capture.terminal_reason.?);
}

test "retained server call abort is allocation free and submits a reset" {
    const Capture = struct {
        call: ?raw_stream.ServerCall = null,
        terminals: usize = 0,
        terminal_reason: ?raw_stream.ServerTerminalReason = null,
    };
    const Handler = struct {
        fn onStart(context: ?*anyopaque, stream: raw_stream.ServerStream, _: *service.ServerContext) !void {
            const capture: *Capture = @ptrCast(@alignCast(context.?));
            capture.call = try stream.retain();
        }

        fn onMessage(
            _: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            _: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            return .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onTerminal(
            context: ?*anyopaque,
            _: raw_stream.ServerCallId,
            reason: raw_stream.ServerTerminalReason,
        ) void {
            const capture: *Capture = @ptrCast(@alignCast(context.?));
            capture.terminals += 1;
            capture.terminal_reason = reason;
        }
    };

    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var capture = Capture{};
    defer if (capture.call) |*call| call.deinit();
    try server.registerStream("/test.Echo/Unary", .{
        .context = &capture,
        .on_start = Handler.onStart,
        .on_message = Handler.onMessage,
        .on_remote_end = Handler.onRemoteEnd,
        .on_terminal = Handler.onTerminal,
    });

    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer deinitTestConnection(&connection);
    try server.impl.connections.append(server.impl.localAllocator(), &connection);
    defer {
        server.impl.removeDirtyConnection(&connection);
        _ = server.impl.connections.pop();
    }
    try feedTestRequest(&connection, .{});
    const target = connection.streams.get(1).?;
    try std.testing.expect(capture.call != null);

    const original_backing = server.coordinator.serialized_allocator.backing;
    defer server.coordinator.serialized_allocator.backing = original_backing;
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{ .fail_index = 0 });
    server.coordinator.serialized_allocator.backing = failing.allocator();
    capture.call.?.abort();
    capture.call.?.abort();
    server.coordinator.serialized_allocator.backing = original_backing;
    try std.testing.expect(target.force_abort_requested);
    try std.testing.expect(target.finish_queued);

    processForcedServerCallAborts(server.impl);
    var output: std.ArrayList(u8) = .empty;
    defer output.deinit(std.testing.allocator);
    while (true) {
        var bytes: [*c]const u8 = null;
        const length = c.nghttp2_session_mem_send2(connection.session, &bytes);
        try std.testing.expect(length >= 0);
        if (length == 0) break;
        try output.appendSlice(std.testing.allocator, bytes[0..@intCast(length)]);
    }
    var reset_count: usize = 0;
    var offset: usize = 0;
    while (offset + 9 <= output.items.len) {
        const payload_length = (@as(usize, output.items[offset]) << 16) |
            (@as(usize, output.items[offset + 1]) << 8) |
            output.items[offset + 2];
        const end = offset + 9 + payload_length;
        try std.testing.expect(end <= output.items.len);
        const stream_id = (@as(u32, output.items[offset + 5] & 0x7f) << 24) |
            (@as(u32, output.items[offset + 6]) << 16) |
            (@as(u32, output.items[offset + 7]) << 8) |
            output.items[offset + 8];
        if (stream_id == 1 and output.items[offset + 3] == c.NGHTTP2_RST_STREAM) {
            try std.testing.expectEqual(@as(usize, 4), payload_length);
            const error_code = (@as(u32, output.items[offset + 9]) << 24) |
                (@as(u32, output.items[offset + 10]) << 16) |
                (@as(u32, output.items[offset + 11]) << 8) |
                output.items[offset + 12];
            try std.testing.expectEqual(@as(u32, c.NGHTTP2_INTERNAL_ERROR), error_code);
            reset_count += 1;
        }
        offset = end;
    }
    try std.testing.expectEqual(@as(usize, 1), reset_count);
    try std.testing.expectEqual(@as(usize, 1), capture.terminals);
    try std.testing.expectEqual(raw_stream.ServerTerminalReason.local_error, capture.terminal_reason.?);
    try std.testing.expect(capture.call.?.isTerminal());
}

test "shared stream command allocation failure does not use local storage" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    server.impl.local_allocator_state.enterLoop();
    defer server.impl.local_allocator_state.leaveLoop();
    var connection = Connection{ .server = server.impl };
    var target = Stream.init(std.testing.allocator, &connection, 1);
    defer target.deinit();
    target.streaming_active = true;

    const original_backing = server.coordinator.serialized_allocator.backing;
    defer server.coordinator.serialized_allocator.backing = original_backing;
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{ .fail_index = 0 });
    server.coordinator.serialized_allocator.backing = failing.allocator();
    try std.testing.expectError(error.OutOfMemory, streamSend(&target, "command", .{}));
    server.coordinator.serialized_allocator.backing = original_backing;
    try std.testing.expectEqual(@as(usize, 0), server.impl.stream_commands.items.len);
    try std.testing.expectEqual(@as(usize, 0), target.command_refs);
}

fn testServerInitAndRegistrationAllocations(allocator: std.mem.Allocator) !void {
    const Handler = struct {
        fn handle(_: *@This(), response_allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            return service.UnaryResponse.ok(response_allocator, request);
        }
    };
    var handler: Handler = .{};
    var server = try Server.init(allocator, .{ .reactor_count = 4 });
    defer server.deinit();
    try server.registerUnary(
        "/test.Allocator/Refill",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
}

test "server partial init and registration handle every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testServerInitAndRegistrationAllocations,
        .{},
    );
}

fn expectDeadlineHeapConsistent(server: *const Impl) !void {
    for (server.deadline_heap.items, 0..) |entry, index| {
        try std.testing.expectEqual(@as(?usize, index), deadlineTargetIndex(entry.target).*);
        if (index != 0) {
            try std.testing.expect(server.deadline_heap.items[(index - 1) / 2].expires_at_ns <= entry.expires_at_ns);
        }
    }
}

test "server deadline heap orders updates and mixed targets" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    var streams = [_]Stream{
        Stream.init(std.testing.allocator, &connection, 1),
        Stream.init(std.testing.allocator, &connection, 3),
        Stream.init(std.testing.allocator, &connection, 5),
    };
    defer {
        for (&streams) |*stream| stream.deinit();
        clearConnectionDeadline(&connection);
    }

    try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = &streams[0] }, 40);
    try deadlineHeapInsertOrUpdate(server.impl, .{ .connection = &connection }, 20);
    try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = &streams[1] }, 30);
    try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = &streams[2] }, 10);
    try expectDeadlineHeapConsistent(server.impl);
    try std.testing.expect(deadlineTargetsEqual(deadlineHeapPeek(server.impl).?.target, .{ .stream = &streams[2] }));

    try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = &streams[0] }, 5);
    try expectDeadlineHeapConsistent(server.impl);
    try std.testing.expect(deadlineTargetsEqual(deadlineHeapPeek(server.impl).?.target, .{ .stream = &streams[0] }));

    try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = &streams[0] }, 50);
    try expectDeadlineHeapConsistent(server.impl);
    try std.testing.expect(deadlineTargetsEqual(deadlineHeapPeek(server.impl).?.target, .{ .stream = &streams[2] }));

    const expected = [_]u64{ 10, 20, 30, 50 };
    for (expected) |expiry| {
        const entry = deadlineHeapPop(server.impl).?;
        try std.testing.expectEqual(expiry, entry.expires_at_ns);
        try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(entry.target).*);
        try expectDeadlineHeapConsistent(server.impl);
    }
    try std.testing.expect(deadlineHeapPeek(server.impl) == null);
}

test "server deadline heap removes root middle last and equal expiries" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    var streams: [7]Stream = undefined;
    for (&streams, 0..) |*stream, index| {
        stream.* = Stream.init(std.testing.allocator, &connection, @intCast(index * 2 + 1));
    }
    defer for (&streams) |*stream| stream.deinit();

    for (&streams, 0..) |*stream, index| {
        try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = stream }, @intCast((index + 1) * 10));
    }
    const root = server.impl.deadline_heap.items[0].target;
    try std.testing.expect(deadlineHeapRemove(server.impl, root));
    try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(root).*);
    try expectDeadlineHeapConsistent(server.impl);

    const middle = server.impl.deadline_heap.items[server.impl.deadline_heap.items.len / 2].target;
    try std.testing.expect(deadlineHeapRemove(server.impl, middle));
    try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(middle).*);
    try expectDeadlineHeapConsistent(server.impl);

    const last = server.impl.deadline_heap.items[server.impl.deadline_heap.items.len - 1].target;
    try std.testing.expect(deadlineHeapRemove(server.impl, last));
    try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(last).*);
    try expectDeadlineHeapConsistent(server.impl);

    while (deadlineHeapPop(server.impl)) |entry| {
        try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(entry.target).*);
    }
    for (&streams) |*stream| try deadlineHeapInsertOrUpdate(server.impl, .{ .stream = stream }, 100);
    var popped: usize = 0;
    while (deadlineHeapPop(server.impl)) |entry| {
        try std.testing.expectEqual(@as(u64, 100), entry.expires_at_ns);
        try std.testing.expectEqual(@as(?usize, null), deadlineTargetIndex(entry.target).*);
        popped += 1;
    }
    try std.testing.expectEqual(streams.len, popped);
}

test "server deadline heap removes destroyed targets" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    var stream = Stream.init(std.testing.allocator, &connection, 1);

    try setStreamDeadline(&stream, deadline.Deadline.initAfter(server.impl.clock, std.time.ns_per_s));
    stream.deinit();
    try std.testing.expectEqual(@as(usize, 0), server.impl.deadline_heap.items.len);

    try setConnectionDeadline(&connection, .tls_handshake, server.impl.clock.now() +| std.time.ns_per_s);
    connection.close();
    try std.testing.expectEqual(@as(usize, 0), server.impl.deadline_heap.items.len);
}

test "streaming finish keeps deadline until transport retirement" {
    const Handler = struct {
        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            _: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            _: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            return .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}
    };

    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    const target = try std.testing.allocator.create(Stream);
    target.* = Stream.init(std.testing.allocator, &connection, 1);
    var target_alive = true;
    defer if (target_alive) {
        discardStreamCommands(target);
        target.deinit();
        std.testing.allocator.destroy(target);
    };
    target.streaming = .{
        .handler = .{
            .on_start = Handler.onStart,
            .on_message = Handler.onMessage,
            .on_remote_end = Handler.onRemoteEnd,
        },
        .decoder = frame.Decoder.init(std.testing.allocator, 1024),
        .context = service.ServerContext.init(std.testing.allocator),
    };
    target.streaming_active = true;
    try setStreamDeadline(target, deadline.Deadline.initAfter(server.impl.clock, std.time.ns_per_s));
    const heap_index = target.deadline_heap_index.?;

    try server.impl.stream_commands.append(server.impl.shared_allocator, .{
        .target = target,
        .action = .{ .finish = .{ .code = .ok, .message = &.{}, .trailing_metadata = null } },
    });
    target.command_refs += 1;
    processStreamCommands(server.impl);

    try std.testing.expect(target.response_finished);
    try std.testing.expect(target.streaming_active);
    try std.testing.expectEqual(@as(?usize, heap_index), target.deadline_heap_index);
    try std.testing.expect(deadlineTargetsEqual(
        server.impl.deadline_heap.items[heap_index].target,
        .{ .stream = target },
    ));

    const destroyed = retireStream(target);
    target_alive = !destroyed;
    try std.testing.expect(destroyed);
    try std.testing.expectEqual(@as(usize, 0), server.impl.deadline_heap.items.len);
}

test "server streaming outbound queue preserves order and reuses drained storage" {
    var streaming: StreamingState = undefined;
    streaming.outbound = .empty;
    streaming.outbound_head = 0;
    defer {
        _ = streaming.clearOutbound();
        streaming.outbound.deinit(std.testing.allocator);
    }

    try streaming.outbound.append(std.testing.allocator, .{
        .bytes = try std.testing.allocator.dupe(u8, "first"),
        .allocator = std.testing.allocator,
    });
    try streaming.outbound.append(std.testing.allocator, .{
        .bytes = try std.testing.allocator.dupe(u8, "second"),
        .allocator = std.testing.allocator,
    });
    const capacity = streaming.outbound.capacity;

    try std.testing.expectEqualStrings("first", streaming.nextOutbound().?.bytes);
    streaming.finishOutbound();
    try std.testing.expectEqual(@as(usize, 1), streaming.outbound_head);
    try std.testing.expectEqualStrings("second", streaming.nextOutbound().?.bytes);

    streaming.finishOutbound();
    try std.testing.expectEqual(@as(usize, 0), streaming.outbound_head);
    try std.testing.expectEqual(@as(usize, 0), streaming.outbound.items.len);
    try std.testing.expectEqual(capacity, streaming.outbound.capacity);

    try streaming.outbound.append(std.testing.allocator, .{
        .bytes = try std.testing.allocator.dupe(u8, "reused"),
        .allocator = std.testing.allocator,
    });
    try std.testing.expectEqualStrings("reused", streaming.nextOutbound().?.bytes);
}

test "server streaming outbound cancellation frees only pending messages" {
    var streaming: StreamingState = undefined;
    streaming.outbound = .empty;
    streaming.outbound_head = 0;
    defer streaming.outbound.deinit(std.testing.allocator);

    try streaming.outbound.append(std.testing.allocator, .{
        .bytes = try std.testing.allocator.dupe(u8, "consumed"),
        .allocator = std.testing.allocator,
    });
    try streaming.outbound.append(std.testing.allocator, .{
        .bytes = try std.testing.allocator.dupe(u8, "partially sent"),
        .allocator = std.testing.allocator,
        .offset = 4,
    });
    try streaming.outbound.append(std.testing.allocator, .{
        .bytes = try std.testing.allocator.dupe(u8, "pending"),
        .allocator = std.testing.allocator,
    });

    streaming.finishOutbound();
    const capacity = streaming.outbound.capacity;
    try std.testing.expectEqual(@as(usize, 1), streaming.outbound_head);
    try std.testing.expectEqual(@as(usize, 10 + 7), streaming.clearOutbound());
    try std.testing.expectEqual(@as(usize, 0), streaming.outbound_head);
    try std.testing.expectEqual(@as(usize, 0), streaming.outbound.items.len);
    try std.testing.expectEqual(capacity, streaming.outbound.capacity);
}

fn expectInitialMaxConcurrentStreams(options: Options, expected: u32) !void {
    var server = try Server.init(std.testing.allocator, options);
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer deinitTestConnection(&connection);

    var data: [*c]const u8 = null;
    const length = c.nghttp2_session_mem_send2(connection.session, &data);
    try std.testing.expect(length >= 9);
    const bytes = data[0..@intCast(length)];
    try std.testing.expectEqual(@as(u8, c.NGHTTP2_SETTINGS), bytes[3]);
    const payload_length = (@as(usize, bytes[0]) << 16) |
        (@as(usize, bytes[1]) << 8) |
        bytes[2];
    try std.testing.expectEqual(@as(usize, 9) + payload_length, bytes.len);
    var offset: usize = 9;
    var found = false;
    while (offset + 6 <= bytes.len) : (offset += 6) {
        const settings_id = (@as(u16, bytes[offset]) << 8) | bytes[offset + 1];
        if (settings_id != c.NGHTTP2_SETTINGS_MAX_CONCURRENT_STREAMS) continue;
        const value = (@as(u32, bytes[offset + 2]) << 24) |
            (@as(u32, bytes[offset + 3]) << 16) |
            (@as(u32, bytes[offset + 4]) << 8) |
            bytes[offset + 5];
        try std.testing.expectEqual(expected, value);
        found = true;
    }
    try std.testing.expect(found);
}

test "server resource options are finite and reject zero" {
    const defaults: Options = .{};
    try std.testing.expectEqual(@as(usize, 1024), defaults.max_connections);
    try std.testing.expectEqual(@as(u32, 100), defaults.max_concurrent_streams_per_connection);
    try std.testing.expectEqual(@as(u64, 10 * std.time.ns_per_s), defaults.cleartext_preface_timeout_ns);
    try std.testing.expectEqual(@as(u64, 5 * 60 * std.time.ns_per_s), defaults.connection_idle_timeout_ns);

    try std.testing.expectError(error.InvalidMaxConnections, Server.init(std.testing.allocator, .{ .max_connections = 0 }));
    try std.testing.expectError(error.InvalidMaxConcurrentStreams, Server.init(std.testing.allocator, .{ .max_concurrent_streams_per_connection = 0 }));
    try std.testing.expectError(error.InvalidCleartextPrefaceTimeout, Server.init(std.testing.allocator, .{ .cleartext_preface_timeout_ns = 0 }));
    try std.testing.expectError(error.InvalidConnectionIdleTimeout, Server.init(std.testing.allocator, .{ .connection_idle_timeout_ns = 0 }));
}

test "server resource SETTINGS advertises exact concurrent stream limit" {
    try expectInitialMaxConcurrentStreams(.{}, 100);
    try expectInitialMaxConcurrentStreams(.{ .max_concurrent_streams_per_connection = 7 }, 7);
}

test "server resource stream admission remains bounded before SETTINGS acknowledgement" {
    var server = try Server.init(std.testing.allocator, .{ .max_concurrent_streams_per_connection = 2 });
    defer server.deinit();
    server.impl.state = .running;
    defer server.impl.state = .initialized;
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer {
        clearConnectionDeadline(&connection);
        deinitTestConnection(&connection);
    }
    try setConnectionDeadline(&connection, .cleartext_preface, server.impl.clock.now() +| std.time.ns_per_s);

    try feedTestRequest(&connection, .{ .stream_id = 1, .end_stream = false });
    try feedTestRequest(&connection, .{ .stream_id = 3, .include_preface = false, .end_stream = false });
    try std.testing.expectEqual(@as(usize, 2), connection.streams.count());
    try feedTestRequest(&connection, .{ .stream_id = 5, .include_preface = false, .end_stream = false });
    try std.testing.expectEqual(@as(usize, 2), connection.streams.count());
    try std.testing.expectEqual(@as(i32, 3), connection.highest_accepted_stream_id);
    var output: [*c]const u8 = null;
    var saw_refused_stream = false;
    while (true) {
        const length = c.nghttp2_session_mem_send2(connection.session, &output);
        try std.testing.expect(length >= 0);
        if (length == 0) break;
        const bytes = output[0..@intCast(length)];
        if (bytes.len >= 13 and bytes[3] == c.NGHTTP2_RST_STREAM and bytes[8] == 5) {
            const code = (@as(u32, bytes[9]) << 24) |
                (@as(u32, bytes[10]) << 16) |
                (@as(u32, bytes[11]) << 8) |
                bytes[12];
            if (code == c.NGHTTP2_REFUSED_STREAM) saw_refused_stream = true;
        }
    }
    try std.testing.expect(saw_refused_stream);

    const reset = [_]u8{ 0, 0, 4, c.NGHTTP2_RST_STREAM, 0, 0, 0, 0, 1, 0, 0, 0, 0 };
    const consumed = c.nghttp2_session_mem_recv2(connection.session, &reset, reset.len);
    try std.testing.expectEqual(@as(c.nghttp2_ssize, reset.len), consumed);
    try std.testing.expectEqual(@as(usize, 1), connection.streams.count());
    try feedTestRequest(&connection, .{ .stream_id = 7, .include_preface = false, .end_stream = false });
    try std.testing.expectEqual(@as(usize, 2), connection.streams.count());
    try std.testing.expectEqual(@as(i32, 7), connection.highest_accepted_stream_id);
}

test "server resource acknowledged stream excess emits protocol error GOAWAY" {
    var server = try Server.init(std.testing.allocator, .{ .max_concurrent_streams_per_connection = 2 });
    defer server.deinit();
    server.impl.state = .running;
    defer server.impl.state = .initialized;
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer {
        clearConnectionDeadline(&connection);
        deinitTestConnection(&connection);
    }
    var output: [*c]const u8 = null;
    try std.testing.expect(c.nghttp2_session_mem_send2(connection.session, &output) > 0);
    try setConnectionDeadline(&connection, .cleartext_preface, server.impl.clock.now() +| std.time.ns_per_s);
    const preface_and_ack = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{
        0, 0, 0, c.NGHTTP2_SETTINGS, 0,                  0, 0, 0, 0,
        0, 0, 0, c.NGHTTP2_SETTINGS, c.NGHTTP2_FLAG_ACK, 0, 0, 0, 0,
    };
    const preface_consumed = c.nghttp2_session_mem_recv2(connection.session, preface_and_ack[0..].ptr, preface_and_ack.len);
    try std.testing.expectEqual(@as(c.nghttp2_ssize, preface_and_ack.len), preface_consumed);
    try feedTestRequest(&connection, .{ .stream_id = 1, .include_preface = false, .end_stream = false });
    try feedTestRequest(&connection, .{ .stream_id = 3, .include_preface = false, .end_stream = false });

    var header_block: std.ArrayList(u8) = .empty;
    defer header_block.deinit(std.testing.allocator);
    try header_block.appendSlice(std.testing.allocator, &.{ 0x83, 0x86, 0x04, 0x10 });
    try header_block.appendSlice(std.testing.allocator, "/test.Echo/Unary");
    try header_block.appendSlice(std.testing.allocator, &.{ 0x01, 0x09 });
    try header_block.appendSlice(std.testing.allocator, "localhost");
    try appendTestHeader(&header_block, "content-type", "application/grpc");
    var excess: std.ArrayList(u8) = .empty;
    defer excess.deinit(std.testing.allocator);
    try excess.appendSlice(std.testing.allocator, &.{
        @intCast(header_block.items.len >> 16),
        @intCast(header_block.items.len >> 8),
        @intCast(header_block.items.len),
        c.NGHTTP2_HEADERS,
        c.NGHTTP2_FLAG_END_HEADERS,
        0,
        0,
        0,
        5,
    });
    try excess.appendSlice(std.testing.allocator, header_block.items);
    _ = c.nghttp2_session_mem_recv2(connection.session, excess.items.ptr, excess.items.len);
    try std.testing.expectEqual(@as(usize, 2), connection.streams.count());

    var saw_protocol_error = false;
    while (true) {
        const length = c.nghttp2_session_mem_send2(connection.session, &output);
        try std.testing.expect(length >= 0);
        if (length == 0) break;
        const bytes = output[0..@intCast(length)];
        if (bytes.len >= 17 and bytes[3] == c.NGHTTP2_GOAWAY) {
            const code = (@as(u32, bytes[13]) << 24) |
                (@as(u32, bytes[14]) << 16) |
                (@as(u32, bytes[15]) << 8) |
                bytes[16];
            if (code == c.NGHTTP2_PROTOCOL_ERROR) saw_protocol_error = true;
        }
    }
    try std.testing.expect(saw_protocol_error);
}

test "server resource cleartext SETTINGS transitions one tagged deadline to idle" {
    var server = try Server.init(std.testing.allocator, .{ .connection_idle_timeout_ns = 17 });
    defer server.deinit();
    server.impl.state = .running;
    defer server.impl.state = .initialized;
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer {
        clearConnectionDeadline(&connection);
        deinitTestConnection(&connection);
    }
    try setConnectionDeadline(&connection, .cleartext_preface, server.impl.clock.now() +| std.time.ns_per_s);
    const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 };
    for (preface[0 .. preface.len - 1]) |byte| {
        const consumed = c.nghttp2_session_mem_recv2(connection.session, &byte, 1);
        try std.testing.expectEqual(@as(c.nghttp2_ssize, 1), consumed);
        try std.testing.expectEqual(ConnectionDeadlineKind.cleartext_preface, connection.connection_deadline.?.kind);
    }
    const final = preface[preface.len - 1];
    const consumed = c.nghttp2_session_mem_recv2(connection.session, &final, 1);
    try std.testing.expectEqual(@as(c.nghttp2_ssize, 1), consumed);
    try std.testing.expectEqual(ConnectionDeadlineKind.idle, connection.connection_deadline.?.kind);
    try std.testing.expectEqual(@as(usize, 1), server.impl.deadline_heap.items.len);
}

fn waitForAdmittedConnections(coordinator: *Coordinator, expected: usize) !void {
    for (0..1000) |_| {
        if (coordinator.admitted_connections.load(.acquire) == expected) return;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(1), .awake);
    }
    return error.TestTimeout;
}

fn writeTestStream(stream: anytype, io: std.Io, bytes: []const u8) !void {
    var buffer: [256]u8 = undefined;
    var writer = stream.writer(io, &buffer);
    writer.interface.writeAll(bytes) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;
}

fn readSocketToEof(fd: std.posix.fd_t) ![]u8 {
    var output: std.ArrayList(u8) = .empty;
    errdefer output.deinit(std.testing.allocator);
    var buffer: [512]u8 = undefined;
    for (0..100) |_| {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = fd,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 10) == 0) continue;
        const length = try std.posix.read(fd, &buffer);
        if (length == 0) return output.toOwnedSlice(std.testing.allocator);
        try output.appendSlice(std.testing.allocator, buffer[0..length]);
    }
    return error.TestTimeout;
}

fn waitForSocketEof(fd: std.posix.fd_t) !void {
    const output = try readSocketToEof(fd);
    std.testing.allocator.free(output);
}

test "server resource aggregate connection admission spans reactors" {
    var server = try Server.init(std.testing.allocator, .{
        .reactor_count = 2,
        .max_connections = 1,
        .cleartext_preface_timeout_ns = std.time.ns_per_s,
    });
    defer server.deinit();
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const first = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    try waitForAdmittedConnections(server.coordinator, 1);
    try writeTestStream(first, io, "P");

    const excess_one = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    const excess_two = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    try std.Io.sleep(std.testing.io, .fromMilliseconds(20), .awake);
    try std.testing.expectEqual(@as(usize, 1), server.coordinator.admitted_connections.load(.acquire));
    excess_one.close(io);
    excess_two.close(io);

    first.close(io);
    try waitForAdmittedConnections(server.coordinator, 0);
    const replacement = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer replacement.close(io);
    try waitForAdmittedConnections(server.coordinator, 1);
}

test "server resource silent and incomplete cleartext prefaces expire" {
    var server = try Server.init(std.testing.allocator, .{
        .cleartext_preface_timeout_ns = 20 * std.time.ns_per_ms,
        .connection_idle_timeout_ns = std.time.ns_per_s,
    });
    defer server.deinit();
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const cases = [_][]const u8{
        "",
        "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n",
        "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{ 0, 0, 0, c.NGHTTP2_SETTINGS },
    };
    for (cases) |input| {
        const stream = try address.connect(io, .{ .mode = .stream, .timeout = .none });
        defer stream.close(io);
        if (input.len != 0) try writeTestStream(stream, io, input);
        try waitForSocketEof(stream.socket.handle);
    }

    const slow = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer slow.close(io);
    const magic = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
    for (magic[0 .. magic.len - 1]) |byte| {
        writeTestStream(slow, io, &.{byte}) catch break;
        try std.Io.sleep(std.testing.io, .fromMilliseconds(2), .awake);
    }
    try waitForSocketEof(slow.socket.handle);
}

test "server resource fragmented valid cleartext preface survives into idle" {
    var server = try Server.init(std.testing.allocator, .{
        .cleartext_preface_timeout_ns = 40 * std.time.ns_per_ms,
        .connection_idle_timeout_ns = std.time.ns_per_s,
    });
    defer server.deinit();
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const stream = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer stream.close(io);
    const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 };
    var offset: usize = 0;
    while (offset < preface.len) {
        const end = @min(offset + 7, preface.len);
        try writeTestStream(stream, io, preface[offset..end]);
        offset = end;
    }
    try waitForAdmittedConnections(server.coordinator, 1);
    try std.Io.sleep(std.testing.io, .fromMilliseconds(80), .awake);
    try std.testing.expectEqual(@as(usize, 1), server.coordinator.admitted_connections.load(.acquire));
}

test "server resource idle GOAWAY hard-closes a saturated cleartext output queue" {
    var server = try Server.init(std.testing.allocator, .{
        .write_high_watermark_bytes = 16,
        .write_low_watermark_bytes = 8,
    });
    defer server.deinit();
    server.impl.state = .running;
    defer server.impl.state = .initialized;
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer deinitTestConnection(&connection);

    var output: [*c]const u8 = null;
    while (c.nghttp2_session_mem_send2(connection.session, &output) > 0) {}
    connection.queued_write_bytes = server.impl.write_high_watermark_bytes;
    defer connection.queued_write_bytes = 0;
    const expires_at = server.impl.clock.now();
    try setConnectionDeadline(&connection, .idle, expires_at);

    expireDeadlines(server.impl, expires_at);

    try std.testing.expect(connection.closing);
    try std.testing.expectEqual(@as(?ConnectionDeadline, null), connection.connection_deadline);
    try std.testing.expect(c.nghttp2_session_want_write(connection.session) != 0);
}

test "server resource PING traffic does not refresh idle expiry" {
    var server = try Server.init(std.testing.allocator, .{
        .cleartext_preface_timeout_ns = std.time.ns_per_s,
        .connection_idle_timeout_ns = 30 * std.time.ns_per_ms,
        .write_high_watermark_bytes = 16,
        .write_low_watermark_bytes = 8,
    });
    defer server.deinit();
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const stream = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer stream.close(io);
    const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 };
    try writeTestStream(stream, io, preface);
    const ping = [_]u8{
        0, 0, 8, c.NGHTTP2_PING, 0, 0, 0, 0, 0,
        1, 2, 3, 4,              5, 6, 7, 8,
    };
    for (0..4) |_| {
        try std.Io.sleep(std.testing.io, .fromMilliseconds(5), .awake);
        try writeTestStream(stream, io, &ping);
    }
    const output = try readSocketToEof(stream.socket.handle);
    defer std.testing.allocator.free(output);
    var offset: usize = 0;
    var saw_no_error_goaway = false;
    while (offset + 9 <= output.len) {
        const payload_length = (@as(usize, output[offset]) << 16) |
            (@as(usize, output[offset + 1]) << 8) |
            output[offset + 2];
        const end = offset + 9 + payload_length;
        try std.testing.expect(end <= output.len);
        if (output[offset + 3] == c.NGHTTP2_GOAWAY and payload_length >= 8) {
            const code = (@as(u32, output[offset + 13]) << 24) |
                (@as(u32, output[offset + 14]) << 16) |
                (@as(u32, output[offset + 15]) << 8) |
                output[offset + 16];
            if (code == c.NGHTTP2_NO_ERROR) saw_no_error_goaway = true;
        }
        offset = end;
    }
    try std.testing.expectEqual(output.len, offset);
    try std.testing.expect(saw_no_error_goaway);
}

test "server resource active stream is idle-exempt until final close" {
    var server = try Server.init(std.testing.allocator, .{
        .cleartext_preface_timeout_ns = std.time.ns_per_s,
        .connection_idle_timeout_ns = 20 * std.time.ns_per_ms,
    });
    defer server.deinit();
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const stream = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer stream.close(io);
    const header_block = [_]u8{
        0x83, 0x86, 0x04, 0x10,
    } ++ "/test.Echo/Unary" ++ [_]u8{
        0x01, 0x09,
    } ++ "localhost" ++ [_]u8{
        0x0f, 0x10, 0x10,
    } ++ "application/grpc";
    const request = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{
        0,                                0,                               0,                          c.NGHTTP2_SETTINGS, 0,                          0, 0, 0, 0,
        @intCast(header_block.len >> 16), @intCast(header_block.len >> 8), @intCast(header_block.len), c.NGHTTP2_HEADERS,  c.NGHTTP2_FLAG_END_HEADERS, 0, 0, 0, 1,
    } ++ header_block;
    try writeTestStream(stream, io, request);
    try waitForAdmittedConnections(server.coordinator, 1);
    try std.Io.sleep(std.testing.io, .fromMilliseconds(60), .awake);
    try std.testing.expectEqual(@as(usize, 1), server.coordinator.admitted_connections.load(.acquire));

    const reset = [_]u8{ 0, 0, 4, c.NGHTTP2_RST_STREAM, 0, 0, 0, 0, 1, 0, 0, 0, 0 };
    try writeTestStream(stream, io, &reset);
    try waitForSocketEof(stream.socket.handle);
}

test "server resource shutdown releases preface admission and deadlines" {
    for ([_]bool{ false, true }) |graceful| {
        var server = try Server.init(std.testing.allocator, .{
            .max_connections = 1,
            .cleartext_preface_timeout_ns = std.time.ns_per_s,
        });
        defer server.deinit();
        try server.start();
        const local_address = try server.localAddress();
        const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
        var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
        defer io_threaded.deinit();
        const io = io_threaded.io();
        const stream = try address.connect(io, .{ .mode = .stream, .timeout = .none });
        defer stream.close(io);
        try waitForAdmittedConnections(server.coordinator, 1);
        if (graceful)
            server.shutdownGracefully(std.time.ns_per_s)
        else
            server.shutdown();
        server.wait();
        try std.testing.expectEqual(@as(usize, 0), server.coordinator.admitted_connections.load(.acquire));
        for (server.coordinator.reactors) |reactor| {
            try std.testing.expectEqual(@as(usize, 0), reactor.deadline_heap.items.len);
        }
    }
}

test "malformed HTTP/2 settings close the connection promptly" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.start();

    const preface = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n";
    const cases = [_][]const u8{
        preface ++ [_]u8{ 0, 0, 1, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0, 0 },
        preface ++ [_]u8{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 1 },
    };
    for (cases) |input| {
        const output = try exchangeRawHttp2(&server, input);
        defer std.testing.allocator.free(output);
        try std.testing.expect(output.len != 0);
    }
}

test "TLS server closes a silent handshake after the configured timeout" {
    if (!build_options.tls) return error.SkipZigTest;
    const certificate = @embedFile("testdata/localhost-cert.pem");
    const private_key = @embedFile("testdata/localhost-key.pem");
    var server = try Server.init(std.testing.allocator, .{
        .tls = .{
            .certificate_chain_pem = certificate,
            .private_key_pem = private_key,
            .handshake_timeout_ns = 10 * std.time.ns_per_ms,
        },
    });
    defer server.deinit();
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const stream = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer stream.close(io);

    var poll_fds = [_]std.posix.pollfd{.{
        .fd = stream.socket.handle,
        .events = std.posix.POLL.IN,
        .revents = 0,
    }};
    if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
    var reader = stream.reader(io, &.{});
    var byte: [1]u8 = undefined;
    try std.testing.expectEqual(@as(usize, 0), try reader.interface.readSliceShort(&byte));
}

test "malformed HTTP/2 settings close after a completed unary stream" {
    const Handler = struct {
        fn handle(_: *@This(), allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerUnary(
        "/test.Echo/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    try server.start();

    const header_block = [_]u8{
        0x83, 0x86, // :method POST, :scheme http
        0x04, 0x10,
    } ++ "/test.Echo/Unary" ++ [_]u8{
        0x01, 0x09,
    } ++ "localhost" ++ [_]u8{
        0x0f, 0x10, 0x10,
    } ++ "application/grpc";
    const headers_frame = [_]u8{
        @intCast(header_block.len >> 16),
        @intCast(header_block.len >> 8),
        @intCast(header_block.len),
        c.NGHTTP2_HEADERS,
        c.NGHTTP2_FLAG_END_HEADERS,
        0,
        0,
        0,
        1,
    } ++ header_block;
    const data_frame = [_]u8{
        0, 0, 9, c.NGHTTP2_DATA, c.NGHTTP2_FLAG_END_STREAM, 0, 0, 0, 1,
        0, 0, 0, 0,              4,
    } ++ "ping";
    const input = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{
        0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0,
    } ++ headers_frame ++ data_frame;

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const stream = try address.connect(io, .{
        .mode = .stream,
        .timeout = .none,
    });
    defer stream.close(io);

    var write_buffer: [256]u8 = undefined;
    var writer = stream.writer(io, &write_buffer);
    writer.interface.writeAll(input) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;

    var output: std.ArrayList(u8) = .empty;
    defer output.deinit(std.testing.allocator);
    var read_buffer: [256]u8 = undefined;
    var parsed: usize = 0;
    var unary_complete = false;
    while (!unary_complete) {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = stream.socket.handle,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
        const length = try std.posix.read(stream.socket.handle, &read_buffer);
        if (length == 0) return error.UnexpectedEof;
        try output.appendSlice(std.testing.allocator, read_buffer[0..length]);
        while (parsed + 9 <= output.items.len) {
            const payload_length = (@as(usize, output.items[parsed]) << 16) |
                (@as(usize, output.items[parsed + 1]) << 8) |
                output.items[parsed + 2];
            const end = parsed + 9 + payload_length;
            if (end > output.items.len) break;
            const stream_id = (@as(u32, output.items[parsed + 5] & 0x7f) << 24) |
                (@as(u32, output.items[parsed + 6]) << 16) |
                (@as(u32, output.items[parsed + 7]) << 8) |
                output.items[parsed + 8];
            if (stream_id == 1 and
                output.items[parsed + 3] == c.NGHTTP2_HEADERS and
                output.items[parsed + 4] & c.NGHTTP2_FLAG_END_STREAM != 0)
            {
                unary_complete = true;
            }
            parsed = end;
        }
    }
    const malformed_settings = [_]u8{
        0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 1,
    };
    writer.interface.writeAll(&malformed_settings) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;
    while (true) {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = stream.socket.handle,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
        const length = try std.posix.read(stream.socket.handle, &read_buffer);
        if (length == 0) break;
    }
}

test "invalid HTTP/2 max frame size emits protocol error GOAWAY" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.start();

    const input = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{
        0, 0,                                 6, c.NGHTTP2_SETTINGS, 0,    0,    0, 0, 0,
        0, c.NGHTTP2_SETTINGS_MAX_FRAME_SIZE, 0, 0,                  0x3f, 0xff,
    };
    const output = try exchangeRawHttp2(&server, input);
    defer std.testing.allocator.free(output);

    var saw_protocol_error_goaway = false;
    var offset: usize = 0;
    while (offset + 9 <= output.len) {
        const payload_length = (@as(usize, output[offset]) << 16) |
            (@as(usize, output[offset + 1]) << 8) |
            output[offset + 2];
        const end = offset + 9 + payload_length;
        try std.testing.expect(end <= output.len);
        if (output[offset + 3] == c.NGHTTP2_GOAWAY and payload_length >= 8) {
            const payload = output[offset + 9 .. end];
            const error_code = (@as(u32, payload[4]) << 24) |
                (@as(u32, payload[5]) << 16) |
                (@as(u32, payload[6]) << 8) |
                payload[7];
            try std.testing.expectEqual(@as(u32, c.NGHTTP2_PROTOCOL_ERROR), error_code);
            saw_protocol_error_goaway = true;
        }
        offset = end;
    }
    try std.testing.expectEqual(output.len, offset);
    try std.testing.expect(saw_protocol_error_goaway);
}

test "server validates registration and has deterministic lifecycle" {
    const Handler = struct {
        fn handle(_: *@This(), allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler_context = Handler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try std.testing.expectError(error.InvalidMethodPath, server.registerUnary("invalid", service.UnaryHandler.bind(Handler, &handler_context, Handler.handle)));
    try server.registerUnary("/test.Echo/Unary", service.UnaryHandler.bind(Handler, &handler_context, Handler.handle));
    try std.testing.expectError(error.MethodAlreadyRegistered, server.registerUnary("/test.Echo/Unary", service.UnaryHandler.bind(Handler, &handler_context, Handler.handle)));

    try server.start();
    const address = try server.localAddress();
    try std.testing.expectEqualStrings("127.0.0.1", address.host);
    try std.testing.expect(address.port != 0);
    try std.testing.expectEqual(address.port, try server.port());
    server.shutdown();
    server.wait();
}

test "server rejects zero reactors" {
    try std.testing.expectError(error.InvalidReactorCount, Server.init(std.testing.allocator, .{ .reactor_count = 0 }));
}

test "multi-reactor registration allocation failure rolls back every shard" {
    const Handler = struct {
        fn handle(_: *@This(), allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    var handler: Handler = .{};
    var server = try Server.init(std.testing.allocator, .{ .reactor_count = 4 });
    defer server.deinit();
    const original_backing = server.coordinator.serialized_allocator.backing;
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{ .fail_index = 2 });
    server.coordinator.serialized_allocator.backing = failing.allocator();
    defer server.coordinator.serialized_allocator.backing = original_backing;
    try std.testing.expectError(
        error.OutOfMemory,
        server.registerUnary(
            "/test.Rollback/Unary",
            service.UnaryHandler.bind(Handler, &handler, Handler.handle),
        ),
    );
    server.coordinator.serialized_allocator.backing = original_backing;
    for (server.coordinator.reactors) |reactor| {
        try std.testing.expect(!reactor.handlers.contains("/test.Rollback/Unary"));
    }
}

test "multi-reactor server binds one ephemeral and fixed port" {
    for ([_]usize{ 2, 4 }) |reactor_count| {
        var ephemeral = try Server.init(std.testing.allocator, .{ .reactor_count = reactor_count });
        defer ephemeral.deinit();
        try ephemeral.start();
        const ephemeral_address = try ephemeral.localAddress();
        try std.testing.expect(ephemeral_address.port != 0);
        for (ephemeral.coordinator.reactors) |reactor| {
            try std.testing.expectEqual(ephemeral_address.port, reactor.local_port);
            try std.testing.expectEqualStrings(ephemeral_address.host, reactor.local_host[0..reactor.local_host_len]);
        }
        ephemeral.shutdown();
        ephemeral.wait();

        const probe_address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
        var probe = try probe_address.listen(std.testing.io, .{});
        var socket_address: std.posix.sockaddr.in = undefined;
        var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
        if (std.posix.errno(std.posix.system.getsockname(
            probe.socket.handle,
            @ptrCast(&socket_address),
            &address_length,
        )) != .SUCCESS) return error.AddressQueryFailed;
        const fixed_port = std.mem.bigToNative(u16, socket_address.port);
        probe.deinit(std.testing.io);

        var fixed = try Server.init(std.testing.allocator, .{
            .port = fixed_port,
            .reactor_count = reactor_count,
        });
        defer fixed.deinit();
        try fixed.start();
        try std.testing.expectEqual(fixed_port, try fixed.port());
        for (fixed.coordinator.reactors) |reactor| try std.testing.expectEqual(fixed_port, reactor.local_port);
        fixed.shutdownGracefully(std.time.ns_per_s);
        fixed.wait();
    }
}

test "multi-reactor startup failure rolls back launched shards" {
    const Capture = struct {
        started: std.atomic.Value(bool) = .init(false),

        fn log(context: ?*anyopaque, _: u32, log_message: event_logger.BytesView) callconv(.c) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            const text = log_message.data.?[0..log_message.size];
            if (std.mem.indexOf(u8, text, "server started") != null) self.started.store(true, .release);
        }
    };

    var capture: Capture = .{};
    var server = try Server.init(std.testing.allocator, .{
        .reactor_count = 4,
        .logger = .{ .context = &capture, .callback = Capture.log },
    });
    defer server.deinit();
    server.coordinator.reactors[1].test_fail_startup = true;

    try std.testing.expectError(error.AsyncInitializationFailed, server.start());
    try std.testing.expect(!capture.started.load(.acquire));
    for (server.coordinator.reactors) |reactor| {
        try std.testing.expect(reactor.thread == null);
        try std.testing.expect(!reactor.listener_initialized or reactor.listener_closed or reactor.state == .stopped);
    }
    server.shutdown();
    server.wait();
}

test "multi-reactor shutdown modes and deinit before start" {
    var before_start = try Server.init(std.testing.allocator, .{ .reactor_count = 4 });
    before_start.deinit();

    var immediate = try Server.init(std.testing.allocator, .{ .reactor_count = 2 });
    defer immediate.deinit();
    try immediate.start();
    immediate.shutdown();
    immediate.wait();
    immediate.wait();

    var graceful = try Server.init(std.testing.allocator, .{ .reactor_count = 4 });
    defer graceful.deinit();
    try graceful.start();
    graceful.shutdownGracefully(std.time.ns_per_s);
    graceful.wait();
}

test "multi-reactor callbacks and transport allocations are concurrent-safe" {
    const Channel = @import("channel.zig").Channel;
    const Handler = struct {
        mutex: std.Io.Mutex = .init,
        thread_ids: [4]std.Thread.Id = undefined,
        thread_count: usize = 0,

        fn observeThread(self: *@This()) void {
            const thread_id = std.Thread.getCurrentId();
            self.mutex.lockUncancelable(syncIo());
            defer self.mutex.unlock(syncIo());
            for (self.thread_ids[0..self.thread_count]) |existing| {
                if (existing == thread_id) return;
            }
            if (self.thread_count < self.thread_ids.len) {
                self.thread_ids[self.thread_count] = thread_id;
                self.thread_count += 1;
            }
        }

        fn getThreadCount(self: *@This()) usize {
            self.mutex.lockUncancelable(syncIo());
            defer self.mutex.unlock(syncIo());
            return self.thread_count;
        }

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            _: *service.ServerContext,
            request: []const u8,
        ) !service.UnaryResponse {
            self.observeThread();
            return service.UnaryResponse.ok(allocator, request);
        }
    };
    const StreamHandler = struct {
        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            _: ?*anyopaque,
            stream: raw_stream.ServerStream,
            _: *service.ServerContext,
            payload: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            try stream.send(payload, .{});
            return .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, stream: raw_stream.ServerStream, _: *service.ServerContext) !void {
            try stream.finish(.ok);
        }
    };
    const Worker = struct {
        channel: *Channel,
        succeeded: bool = false,
        done: std.Io.Semaphore = .{},
        callback_mutex: std.Io.Mutex = .init,
        message_ok: bool = false,
        terminal_ok: bool = false,

        fn callbacksSucceeded(self: *@This()) bool {
            self.callback_mutex.lockUncancelable(syncIo());
            defer self.callback_mutex.unlock(syncIo());
            return self.message_ok and self.terminal_ok;
        }

        fn onMessage(
            context: ?*anyopaque,
            _: raw_stream.ClientStream,
            payload: []const u8,
            _: Compression,
        ) raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.callback_mutex.lockUncancelable(syncIo());
            defer self.callback_mutex.unlock(syncIo());
            self.message_ok = std.mem.eql(u8, payload, "stream-ping");
            return .continue_receiving;
        }

        fn onTerminal(
            context: ?*anyopaque,
            _: raw_stream.ClientStream,
            final_status: status.Status,
            _: *const metadata.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.callback_mutex.lockUncancelable(syncIo());
            self.terminal_ok = final_status.isOk();
            self.callback_mutex.unlock(syncIo());
            self.done.post(syncIo());
        }

        fn run(self: *@This()) void {
            var result_allocator: std.heap.DebugAllocator(.{ .thread_safe = false }) = .init;
            defer std.debug.assert(result_allocator.deinit() == .ok);
            var result = self.channel.callUnary(
                result_allocator.allocator(),
                "/test.Reactors/Unary",
                "ping",
                .{},
            ) catch return;
            defer result.deinit();
            if (!result.status.isOk() or !std.mem.eql(u8, result.payload, "ping")) return;

            var stream = self.channel.openStream(
                "/test.Reactors/Bidi",
                .{},
                .{
                    .context = self,
                    .on_message = onMessage,
                    .on_terminal = onTerminal,
                },
            ) catch return;
            defer stream.deinit();
            stream.send("stream-ping", .{}) catch return;
            stream.closeSend() catch return;
            self.done.waitUncancelable(syncIo());
            self.succeeded = self.callbacksSucceeded();
        }
    };

    for ([_]usize{ 2, 4 }) |reactor_count| {
        var backing: std.heap.DebugAllocator(.{ .thread_safe = false }) = .init;
        var backing_active = true;
        defer if (backing_active) std.debug.assert(backing.deinit() == .ok);
        var handler: Handler = .{};
        var server = try Server.init(backing.allocator(), .{ .reactor_count = reactor_count });
        var server_active = true;
        defer if (server_active) server.deinit();
        try server.registerUnary(
            "/test.Reactors/Unary",
            service.UnaryHandler.bind(Handler, &handler, Handler.handle),
        );
        try server.registerStream(
            "/test.Reactors/Bidi",
            .{
                .on_start = StreamHandler.onStart,
                .on_message = StreamHandler.onMessage,
                .on_remote_end = StreamHandler.onRemoteEnd,
            },
        );
        for (server.coordinator.reactors) |reactor| {
            try std.testing.expect(reactor.handlers.contains("/test.Reactors/Unary"));
            try std.testing.expect(reactor.stream_handlers.contains("/test.Reactors/Bidi"));
        }
        try server.start();

        var target_buffer: [32]u8 = undefined;
        const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
        var distributed = false;
        for (0..4) |_| {
            var channels: [16]Channel = undefined;
            var channel_count: usize = 0;
            defer for (channels[0..channel_count]) |*channel| channel.deinit();
            while (channel_count < channels.len) : (channel_count += 1) {
                channels[channel_count] = try Channel.init(std.heap.smp_allocator, target, .{});
            }
            var workers: [channels.len]Worker = undefined;
            var threads: [channels.len]std.Thread = undefined;
            for (&workers, &channels) |*worker, *channel| worker.* = .{ .channel = channel };
            for (&threads, &workers) |*thread, *worker| thread.* = try std.Thread.spawn(.{}, Worker.run, .{worker});
            for (&threads) |thread| thread.join();
            for (&workers) |worker| try std.testing.expect(worker.succeeded);

            var accepting_reactors: usize = 0;
            for (server.coordinator.reactors) |reactor| {
                if (reactor.accepted_connections.load(.acquire) != 0) accepting_reactors += 1;
            }
            if (accepting_reactors >= 2) {
                distributed = true;
                break;
            }
        }
        try std.testing.expect(distributed);
        try std.testing.expect(handler.getThreadCount() >= 2);

        server.shutdown();
        server.wait();
        server.deinit();
        server_active = false;
        try std.testing.expectEqual(std.heap.Check.ok, backing.deinit());
        backing_active = false;
    }
}

test "manual receive credit isolates a paused stream and resumes on loop" {
    const Handler = struct {
        messages: usize = 0,

        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            context: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            _: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.messages += 1;
            return if (self.messages <= 2) .pause else .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}
    };

    var handler = Handler{};
    var server = try Server.init(std.testing.allocator, .{ .stream_limits = .{
        .max_message_size = 48 * 1024,
        .max_inbound_buffer_size = 64 * 1024,
        .max_outbound_buffer_size = 64 * 1024,
    } });
    defer server.deinit();
    try server.registerStream("/test.Flow/Pause", .{
        .context = &handler,
        .on_start = Handler.onStart,
        .on_message = Handler.onMessage,
        .on_remote_end = Handler.onRemoteEnd,
    });

    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer deinitTestConnection(&connection);

    const first = try frame.encode(std.testing.allocator, "pause");
    defer std.testing.allocator.free(first);
    const payload = try std.testing.allocator.alloc(u8, 40 * 1024);
    defer std.testing.allocator.free(payload);
    @memset(payload, 'x');
    const second = try frame.encode(std.testing.allocator, payload);
    defer std.testing.allocator.free(second);

    var wire: std.ArrayList(u8) = .empty;
    defer wire.deinit(std.testing.allocator);
    try wire.appendSlice(std.testing.allocator, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n");
    try wire.appendSlice(std.testing.allocator, &.{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 });
    try appendRawTestHeaders(&wire, 1, "/test.Flow/Pause", false);
    try appendRawTestData(&wire, 1, first, false);
    var offset: usize = 0;
    while (offset < second.len) {
        const end = @min(offset + 255, second.len);
        try appendRawTestData(&wire, 1, second[offset..end], false);
        offset = end;
    }

    const consumed = c.nghttp2_session_mem_recv2(connection.session, wire.items.ptr, wire.items.len);
    try std.testing.expectEqual(@as(c.nghttp2_ssize, @intCast(wire.items.len)), consumed);
    const target = connection.streams.get(1).?;
    try std.testing.expect(target.receive_paused);
    try std.testing.expectEqual(@as(usize, 1), handler.messages);
    try std.testing.expectEqual(first.len + second.len, target.deferred_stream_credit);
    try std.testing.expect(
        c.nghttp2_session_get_local_window_size(connection.session) >
            c.nghttp2_session_get_stream_local_window_size(connection.session, 1),
    );

    processStreamCommand(server.impl, .{ .target = target, .action = .resume_receive });
    try std.testing.expect(target.receive_paused);
    try std.testing.expectEqual(first.len + second.len, target.deferred_stream_credit);
    try std.testing.expectEqual(@as(usize, 2), handler.messages);
    try std.testing.expect(
        c.nghttp2_session_get_stream_local_window_size(connection.session, 1) <
            c.NGHTTP2_INITIAL_WINDOW_SIZE,
    );

    processStreamCommand(server.impl, .{ .target = target, .action = .resume_receive });
    try std.testing.expect(!target.receive_paused);
    try std.testing.expectEqual(@as(usize, 0), target.deferred_stream_credit);
    try std.testing.expectEqual(@as(usize, 2), handler.messages);
    try std.testing.expectEqual(
        @as(i32, c.NGHTTP2_INITIAL_WINDOW_SIZE),
        c.nghttp2_session_get_stream_local_window_size(connection.session, 1),
    );
}

test "server receive resume latches during message callback" {
    const Handler = struct {
        messages: usize = 0,

        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}

        fn onMessage(
            context: ?*anyopaque,
            stream: raw_stream.ServerStream,
            _: *service.ServerContext,
            _: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.messages += 1;
            try stream.resumeReceive();
            try stream.resumeReceive();
            return .pause;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {}
    };

    var handler = Handler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    server.impl.local_allocator_state.enterLoop();
    defer server.impl.local_allocator_state.leaveLoop();

    var connection = Connection{ .server = server.impl };
    const target = try std.testing.allocator.create(Stream);
    target.* = Stream.init(std.testing.allocator, &connection, 1);
    defer {
        target.deinit();
        std.testing.allocator.destroy(target);
    }
    target.streaming = .{
        .handler = .{
            .context = &handler,
            .on_start = Handler.onStart,
            .on_message = Handler.onMessage,
            .on_remote_end = Handler.onRemoteEnd,
        },
        .decoder = frame.Decoder.init(std.testing.allocator, 1024),
        .context = service.ServerContext.init(std.testing.allocator),
    };
    target.streaming_active = true;

    const first = try frame.encode(std.testing.allocator, "first");
    defer std.testing.allocator.free(first);
    const second = try frame.encode(std.testing.allocator, "second");
    defer std.testing.allocator.free(second);
    var payload: std.ArrayList(u8) = .empty;
    defer payload.deinit(std.testing.allocator);
    try payload.appendSlice(std.testing.allocator, first);
    try payload.appendSlice(std.testing.allocator, second);

    try std.testing.expect(receiveStreamingData(target, payload.items));
    try std.testing.expectEqual(@as(usize, 2), handler.messages);
    try std.testing.expect(!target.receive_paused);
    try std.testing.expect(!target.resume_requested);
    try std.testing.expect(!target.message_callback_active);
    try std.testing.expectError(error.ReceiveNotPaused, target.serverHandle().resumeReceive());
}

test "server occupied port startup cleans up deterministically" {
    const listen_address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var listener = try listen_address.listen(std.testing.io, .{});
    defer listener.deinit(std.testing.io);

    var local_address: std.posix.sockaddr.in = undefined;
    var address_length: std.posix.socklen_t = @sizeOf(std.posix.sockaddr.in);
    if (std.posix.errno(std.posix.system.getsockname(
        listener.socket.handle,
        @ptrCast(&local_address),
        &address_length,
    )) != .SUCCESS) return error.AddressQueryFailed;

    var server = try Server.init(std.testing.allocator, .{
        .port = std.mem.bigToNative(u16, local_address.port),
    });
    defer server.deinit();
    try std.testing.expectError(error.BindFailed, server.start());
    server.shutdown();
    server.wait();
    server.shutdown();
    server.wait();
}

test "graceful shutdown exits when idle and is idempotent" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.start();

    const address = try server.localAddress();
    try std.testing.expect(address.port != 0);
    server.shutdownGracefully(std.time.ns_per_s);
    server.shutdownGracefully(0);
    server.wait();

    server.shutdownGracefully(0);
    server.shutdown();
}

test "graceful shutdown before start is idempotent" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    server.shutdownGracefully(0);
    server.shutdownGracefully(std.time.ns_per_s);
    server.shutdown();
    server.wait();
    try std.testing.expectError(error.ServerAlreadyStarted, server.start());
}

test "raw HTTP/2 request routes unary data and ends with trailers" {
    const Handler = struct {
        saw_metadata: bool = false,

        fn handle(self: *@This(), allocator: std.mem.Allocator, context: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            self.saw_metadata = std.mem.eql(u8, context.request_metadata.getFirst("x-test") orelse "", "value");
            try std.testing.expectEqualStrings("ping", request);
            context.setResponseCompression(.gzip);
            try context.addInitialMetadata("x-initial", "yes");
            try context.addTrailingMetadata("x-trailing", "yes");
            return service.UnaryResponse.ok(allocator, "pong");
        }
    };

    var handler_context = Handler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerUnary(
        "/test.Echo/Unary",
        service.UnaryHandler.bind(Handler, &handler_context, Handler.handle),
    );

    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer {
        if (connection.session) |session| c.nghttp2_session_del(session);
        var iterator = connection.streams.iterator();
        while (iterator.next()) |entry| {
            destroyStream(entry.value_ptr.*);
        }
        connection.streams.deinit(connection.allocator());
    }

    const header_block = [_]u8{
        0x83, 0x86, // :method POST, :scheme http
        0x04, 0x10,
    } ++ "/test.Echo/Unary" ++ [_]u8{
        0x01, 0x09,
    } ++ "localhost" ++ [_]u8{
        0x0f, 0x10, 0x10,
    } ++ "application/grpc" ++ [_]u8{
        0x00, 0x02,
    } ++ "te" ++ [_]u8{
        0x08,
    } ++ "trailers" ++ [_]u8{
        0x00, 0x06,
    } ++ "x-test" ++ [_]u8{
        0x05,
    } ++ "value";
    const headers_frame = [_]u8{
        @intCast(header_block.len >> 16),
        @intCast(header_block.len >> 8),
        @intCast(header_block.len),
        c.NGHTTP2_HEADERS,
        c.NGHTTP2_FLAG_END_HEADERS,
        0,
        0,
        0,
        1,
    } ++ header_block;
    const data_frame = [_]u8{
        0,
        0,
        9,
        c.NGHTTP2_DATA,
        c.NGHTTP2_FLAG_END_STREAM,
        0,
        0,
        0,
        1,
        0,
        0,
        0,
        0,
        4,
    } ++ "ping";
    const wire = "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n" ++ [_]u8{
        0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0,
    } ++ headers_frame ++ data_frame;

    const fragments = [_][]const u8{ wire[0..7], wire[7..31], wire[31..68], wire[68..] };
    for (fragments) |fragment| {
        const consumed = c.nghttp2_session_mem_recv2(connection.session, fragment.ptr, fragment.len);
        try std.testing.expectEqual(@as(c.nghttp2_ssize, @intCast(fragment.len)), consumed);
    }

    var output: std.ArrayList(u8) = .empty;
    defer output.deinit(std.testing.allocator);
    while (true) {
        var bytes: [*c]const u8 = null;
        const length = c.nghttp2_session_mem_send2(connection.session, &bytes);
        try std.testing.expect(length >= 0);
        if (length == 0) break;
        try output.appendSlice(std.testing.allocator, bytes[0..@intCast(length)]);
    }

    var response_data: std.ArrayList(u8) = .empty;
    defer response_data.deinit(std.testing.allocator);
    var saw_final_trailers = false;
    var offset: usize = 0;
    while (offset + 9 <= output.items.len) {
        const payload_length = (@as(usize, output.items[offset]) << 16) |
            (@as(usize, output.items[offset + 1]) << 8) |
            output.items[offset + 2];
        const end = offset + 9 + payload_length;
        try std.testing.expect(end <= output.items.len);
        const frame_type = output.items[offset + 3];
        const flags = output.items[offset + 4];
        const stream_id = (@as(u32, output.items[offset + 5] & 0x7f) << 24) |
            (@as(u32, output.items[offset + 6]) << 16) |
            (@as(u32, output.items[offset + 7]) << 8) |
            output.items[offset + 8];
        if (stream_id == 1 and frame_type == c.NGHTTP2_DATA) {
            try response_data.appendSlice(std.testing.allocator, output.items[offset + 9 .. end]);
        }
        if (stream_id == 1 and frame_type == c.NGHTTP2_HEADERS and flags & c.NGHTTP2_FLAG_END_STREAM != 0) {
            saw_final_trailers = true;
        }
        offset = end;
    }
    try std.testing.expectEqual(output.items.len, offset);
    const expected_response = try frame.encode(std.testing.allocator, "pong");
    defer std.testing.allocator.free(expected_response);
    try std.testing.expectEqualSlices(u8, expected_response, response_data.items);
    try std.testing.expect(saw_final_trailers);
    try std.testing.expect(handler_context.saw_metadata);
}

test "raw HTTP/2 bidi stream incrementally exchanges messages" {
    const Handler = struct {
        starts: usize = 0,
        messages: usize = 0,
        remote_ends: usize = 0,
        writable_calls: usize = 0,
        cancels: usize = 0,
        callbacks_ordered: bool = true,
        compression_matches: bool = true,
        backpressure_seen: bool = false,
        pending_response: bool = false,
        writable_send_failed: bool = false,
        remote_ended: bool = false,

        const large_response = "0123456789abcdef" ** 4;

        fn onStart(context: ?*anyopaque, _: raw_stream.ServerStream, server_context: *service.ServerContext) !void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.starts += 1;
            server_context.setResponseCompression(.gzip);
        }

        fn onMessage(
            context: ?*anyopaque,
            server_stream: raw_stream.ServerStream,
            _: *service.ServerContext,
            payload: []const u8,
            compression: Compression,
        ) !raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.callbacks_ordered = self.callbacks_ordered and self.starts == 1 and self.remote_ends == 0;
            const expected_payload = if (self.messages == 0) "one" else "two two two";
            const expected_compression: Compression = if (self.messages == 0) .identity else .gzip;
            self.compression_matches = self.compression_matches and
                std.mem.eql(u8, payload, expected_payload) and compression == expected_compression;
            self.messages += 1;
            if (self.messages == 1) {
                try server_stream.send(large_response, .{});
            } else {
                server_stream.send(payload, .{ .compression = .gzip }) catch |err| {
                    if (err != error.WouldBlock) return err;
                    self.backpressure_seen = true;
                    self.pending_response = true;
                };
            }
            return .continue_receiving;
        }

        fn onRemoteEnd(context: ?*anyopaque, server_stream: raw_stream.ServerStream, server_context: *service.ServerContext) !void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.remote_ends += 1;
            self.remote_ended = true;
            try server_context.addTrailingMetadata("x-stream-trailer", "yes");
            if (!self.pending_response) try server_stream.finish(.init(.ok, "complete"));
        }

        fn onWritable(context: ?*anyopaque, server_stream: raw_stream.ServerStream, _: *service.ServerContext) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.writable_calls += 1;
            if (!self.pending_response) return;
            server_stream.send("two two two", .{ .compression = .gzip }) catch {
                self.writable_send_failed = true;
                return;
            };
            self.pending_response = false;
            if (self.remote_ended) {
                server_stream.finish(.init(.ok, "complete")) catch {
                    self.writable_send_failed = true;
                };
            }
        }

        fn onCancel(context: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.cancels += 1;
        }
    };

    var handler = Handler{};
    var server = try Server.init(std.testing.allocator, .{ .stream_limits = .{
        .max_message_size = 64,
        .max_inbound_buffer_size = 256,
        .max_outbound_buffer_size = 69,
    }, .initial_stream_window_size = 256 });
    defer server.deinit();
    try server.registerStream("/test.Echo/Bidi", .{
        .context = &handler,
        .on_start = Handler.onStart,
        .on_message = Handler.onMessage,
        .on_remote_end = Handler.onRemoteEnd,
        .on_writable = Handler.onWritable,
        .on_cancel = Handler.onCancel,
    });
    try server.start();

    var header_block: std.ArrayList(u8) = .empty;
    defer header_block.deinit(std.testing.allocator);
    try header_block.appendSlice(std.testing.allocator, &.{ 0x83, 0x86, 0x04, 0x0f });
    try header_block.appendSlice(std.testing.allocator, "/test.Echo/Bidi");
    try header_block.appendSlice(std.testing.allocator, &.{ 0x01, 0x09 });
    try header_block.appendSlice(std.testing.allocator, "localhost");
    try appendTestHeader(&header_block, "content-type", "application/grpc");
    try appendTestHeader(&header_block, "grpc-encoding", "gzip");
    try appendTestHeader(&header_block, "grpc-accept-encoding", "identity,gzip");

    const first = try frame.encode(std.testing.allocator, "one");
    defer std.testing.allocator.free(first);
    const second = try frame.encodeWithCompression(std.testing.allocator, "two two two", .gzip);
    defer std.testing.allocator.free(second);
    var request_data: std.ArrayList(u8) = .empty;
    defer request_data.deinit(std.testing.allocator);
    try request_data.appendSlice(std.testing.allocator, first);
    try request_data.appendSlice(std.testing.allocator, second);

    var wire: std.ArrayList(u8) = .empty;
    defer wire.deinit(std.testing.allocator);
    try wire.appendSlice(std.testing.allocator, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n");
    try wire.appendSlice(std.testing.allocator, &.{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 });
    try wire.appendSlice(std.testing.allocator, &.{
        @intCast(header_block.items.len >> 16),
        @intCast(header_block.items.len >> 8),
        @intCast(header_block.items.len),
        c.NGHTTP2_HEADERS,
        c.NGHTTP2_FLAG_END_HEADERS,
        0,
        0,
        0,
        1,
    });
    try wire.appendSlice(std.testing.allocator, header_block.items);
    const split = 3;
    try wire.appendSlice(std.testing.allocator, &.{ 0, 0, split, c.NGHTTP2_DATA, 0, 0, 0, 0, 1 });
    try wire.appendSlice(std.testing.allocator, request_data.items[0..split]);
    const remaining = request_data.items.len - split;
    try wire.appendSlice(std.testing.allocator, &.{
        @intCast(remaining >> 16),
        @intCast(remaining >> 8),
        @intCast(remaining),
        c.NGHTTP2_DATA,
        c.NGHTTP2_FLAG_END_STREAM,
        0,
        0,
        0,
        1,
    });
    try wire.appendSlice(std.testing.allocator, request_data.items[split..]);

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const socket = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer socket.close(io);
    var write_buffer: [512]u8 = undefined;
    var writer = socket.writer(io, &write_buffer);
    writer.interface.writeAll(wire.items) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;

    var output: std.ArrayList(u8) = .empty;
    defer output.deinit(std.testing.allocator);
    var response_data: std.ArrayList(u8) = .empty;
    defer response_data.deinit(std.testing.allocator);
    var parsed: usize = 0;
    var saw_trailers = false;
    var read_buffer: [512]u8 = undefined;
    for (0..10) |_| {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = socket.socket.handle,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
        const length = try std.posix.read(socket.socket.handle, &read_buffer);
        if (length == 0) return error.UnexpectedEof;
        try output.appendSlice(std.testing.allocator, read_buffer[0..length]);
        while (parsed + 9 <= output.items.len) {
            const payload_length = (@as(usize, output.items[parsed]) << 16) |
                (@as(usize, output.items[parsed + 1]) << 8) |
                output.items[parsed + 2];
            const end = parsed + 9 + payload_length;
            if (end > output.items.len) break;
            const stream_id = (@as(u32, output.items[parsed + 5] & 0x7f) << 24) |
                (@as(u32, output.items[parsed + 6]) << 16) |
                (@as(u32, output.items[parsed + 7]) << 8) |
                output.items[parsed + 8];
            if (stream_id == 1 and output.items[parsed + 3] == c.NGHTTP2_DATA) {
                try response_data.appendSlice(std.testing.allocator, output.items[parsed + 9 .. end]);
            }
            if (stream_id == 1 and
                output.items[parsed + 3] == c.NGHTTP2_HEADERS and
                output.items[parsed + 4] & c.NGHTTP2_FLAG_END_STREAM != 0)
            {
                saw_trailers = true;
            }
            parsed = end;
        }
        if (saw_trailers) break;
    }
    try std.testing.expect(saw_trailers);

    var capture = TestResponseCapture{};
    defer capture.deinit();
    try capture.decode(output.items);

    var decoder = frame.Decoder.initWithCompression(std.testing.allocator, 1024, .gzip);
    defer decoder.deinit();
    try decoder.feed(response_data.items);
    const echoed_first = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(echoed_first.payload);
    const echoed_second = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(echoed_second.payload);
    try decoder.finish();
    try std.testing.expectEqualStrings(Handler.large_response, echoed_first.payload);
    try std.testing.expect(!echoed_first.compressed);
    try std.testing.expectEqualStrings("two two two", echoed_second.payload);
    try std.testing.expect(echoed_second.compressed);
    try std.testing.expectEqual(@as(usize, 1), handler.starts);
    try std.testing.expectEqual(@as(usize, 2), handler.messages);
    try std.testing.expectEqual(@as(usize, 1), handler.remote_ends);
    try std.testing.expectEqual(@as(usize, 1), handler.writable_calls);
    try std.testing.expectEqual(@as(usize, 0), handler.cancels);
    try std.testing.expect(handler.callbacks_ordered);
    try std.testing.expect(handler.compression_matches);
    try std.testing.expect(handler.backpressure_seen);
    try std.testing.expect(!handler.pending_response);
    try std.testing.expect(!handler.writable_send_failed);
    try std.testing.expectEqual(@as(?u32, 0), capture.stream1_status);
    try std.testing.expect(capture.stream1_message_matches);
    try std.testing.expect(capture.stream1_trailing_metadata_matches);
    try std.testing.expect(capture.stream1_ended);
}

test "retained server call drives an explicitly started response" {
    const Channel = @import("channel.zig").Channel;
    const Handler = struct {
        call: ?raw_stream.ServerCall = null,
        call_id: ?raw_stream.ServerCallId = null,
        starts: usize = 0,
        messages: usize = 0,
        terminals: usize = 0,
        terminal_reason: ?raw_stream.ServerTerminalReason = null,
        done: std.Io.Semaphore = .{},

        fn onStart(
            context: ?*anyopaque,
            server_stream: raw_stream.ServerStream,
            _: *service.ServerContext,
        ) !void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.starts += 1;
            self.call = try server_stream.retain();
            self.call_id = self.call.?.id();
            try self.call.?.sendInitialMetadata(&.{.{ .key = "x-explicit-initial", .value = "yes" }}, .identity);
            try std.testing.expectError(
                error.ResponseCompressionNotEnabled,
                self.call.?.send("mismatched", .{ .compression = .gzip }),
            );
            try self.call.?.resumeReceive();
        }

        fn onMessage(
            context: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            payload: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.messages += 1;
            try self.call.?.send(payload, .{});
            return .continue_receiving;
        }

        fn onRemoteEnd(
            context: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
        ) !void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            try self.call.?.finish(.ok, &.{.{ .key = "x-explicit-trailing", .value = "yes" }});
        }

        fn onTerminal(
            context: ?*anyopaque,
            call_id: raw_stream.ServerCallId,
            reason: raw_stream.ServerTerminalReason,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            std.debug.assert(call_id == self.call_id.?);
            std.debug.assert(self.call.?.isTerminal());
            std.debug.assert(!self.call.?.isCancelled());
            self.terminals += 1;
            self.terminal_reason = reason;
            self.done.post(syncIo());
        }
    };
    const ClientCapture = struct {
        initial_metadata_matches: bool = false,
        trailing_metadata_matches: bool = false,
        payload_matches: bool = false,
        status_ok: bool = false,
        done: std.Io.Semaphore = .{},

        fn onHeaders(
            context: ?*anyopaque,
            _: raw_stream.ClientStream,
            initial_metadata: *const metadata.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.initial_metadata_matches = std.mem.eql(
                u8,
                initial_metadata.getFirst("x-explicit-initial") orelse "",
                "yes",
            );
        }

        fn onMessage(
            context: ?*anyopaque,
            _: raw_stream.ClientStream,
            payload: []const u8,
            _: Compression,
        ) raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.payload_matches = std.mem.eql(u8, payload, "retained");
            return .continue_receiving;
        }

        fn onTerminal(
            context: ?*anyopaque,
            _: raw_stream.ClientStream,
            final_status: status.Status,
            trailing_metadata: *const metadata.Metadata,
        ) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.status_ok = final_status.isOk();
            self.trailing_metadata_matches = std.mem.eql(
                u8,
                trailing_metadata.getFirst("x-explicit-trailing") orelse "",
                "yes",
            );
            self.done.post(syncIo());
        }
    };

    var handler = Handler{};
    defer if (handler.call) |*call| call.deinit();
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerStream("/test.Retained/Call", .{
        .context = &handler,
        .receive_initially_paused = true,
        .initial_metadata_mode = .explicit,
        .on_start = Handler.onStart,
        .on_message = Handler.onMessage,
        .on_remote_end = Handler.onRemoteEnd,
        .on_terminal = Handler.onTerminal,
    });
    try server.start();

    var target_buffer: [32]u8 = undefined;
    const target = try std.fmt.bufPrint(&target_buffer, "127.0.0.1:{d}", .{try server.port()});
    var channel = try Channel.init(std.testing.allocator, target, .{});
    defer channel.deinit();
    var capture = ClientCapture{};
    var client_stream = try channel.openStream(
        "/test.Retained/Call",
        .{},
        .{
            .context = &capture,
            .on_headers = ClientCapture.onHeaders,
            .on_message = ClientCapture.onMessage,
            .on_terminal = ClientCapture.onTerminal,
        },
    );
    defer client_stream.deinit();
    try client_stream.send("retained", .{});
    try client_stream.closeSend();
    capture.done.waitUncancelable(syncIo());
    handler.done.waitUncancelable(syncIo());

    try std.testing.expect(capture.initial_metadata_matches);
    try std.testing.expect(capture.trailing_metadata_matches);
    try std.testing.expect(capture.payload_matches);
    try std.testing.expect(capture.status_ok);
    try std.testing.expectEqual(@as(usize, 1), handler.starts);
    try std.testing.expectEqual(@as(usize, 1), handler.messages);
    try std.testing.expectEqual(@as(usize, 1), handler.terminals);
    try std.testing.expectEqual(raw_stream.ServerTerminalReason.completed, handler.terminal_reason.?);
    try std.testing.expect(handler.call.?.isTerminal());
    try std.testing.expectError(error.CallClosed, handler.call.?.send("late", .{}));
    var cloned_call = handler.call.?.clone();
    cloned_call.deinit();
    handler.call.?.deinit();
    handler.call = null;
}

test "malformed streaming input resets only its stream and connection remains reusable" {
    const StreamingHandler = struct {
        messages: std.atomic.Value(usize) = .init(0),
        cancels: std.atomic.Value(usize) = .init(0),

        fn onStart(_: ?*anyopaque, _: raw_stream.ServerStream, context: *service.ServerContext) !void {
            try context.addTrailingMetadata("x-stream-trailer", "yes");
        }

        fn onMessage(
            context_ptr: ?*anyopaque,
            _: raw_stream.ServerStream,
            _: *service.ServerContext,
            payload: []const u8,
            _: Compression,
        ) !raw_stream.ReceiveAction {
            const self: *@This() = @ptrCast(@alignCast(context_ptr.?));
            try std.testing.expectEqualStrings("valid", payload);
            _ = self.messages.fetchAdd(1, .monotonic);
            return .continue_receiving;
        }

        fn onRemoteEnd(_: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) !void {
            return error.UnexpectedRemoteEnd;
        }

        fn onCancel(context_ptr: ?*anyopaque, _: raw_stream.ServerStream, _: *service.ServerContext) void {
            const self: *@This() = @ptrCast(@alignCast(context_ptr.?));
            _ = self.cancels.fetchAdd(1, .monotonic);
        }
    };
    const UnaryHandler = struct {
        fn handle(_: *@This(), allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            try std.testing.expectEqualStrings("reuse", request);
            return service.UnaryResponse.ok(allocator, "reused");
        }
    };

    var streaming_handler = StreamingHandler{};
    var unary_handler = UnaryHandler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerStream("/test.Echo/Bidi", .{
        .context = &streaming_handler,
        .on_start = StreamingHandler.onStart,
        .on_message = StreamingHandler.onMessage,
        .on_remote_end = StreamingHandler.onRemoteEnd,
        .on_cancel = StreamingHandler.onCancel,
    });
    try server.registerUnary(
        "/test.Echo/Unary",
        service.UnaryHandler.bind(UnaryHandler, &unary_handler, UnaryHandler.handle),
    );
    try server.start();

    const local_address = try server.localAddress();
    const address = try std.Io.net.IpAddress.parseIp4(local_address.host, local_address.port);
    var io_threaded = std.Io.Threaded.init(std.testing.allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();
    const socket = try address.connect(io, .{ .mode = .stream, .timeout = .none });
    defer socket.close(io);
    var write_buffer: [512]u8 = undefined;
    var writer = socket.writer(io, &write_buffer);

    const valid = try frame.encode(std.testing.allocator, "valid");
    defer std.testing.allocator.free(valid);
    var first_request: std.ArrayList(u8) = .empty;
    defer first_request.deinit(std.testing.allocator);
    try first_request.appendSlice(std.testing.allocator, "PRI * HTTP/2.0\r\n\r\nSM\r\n\r\n");
    try first_request.appendSlice(std.testing.allocator, &.{ 0, 0, 0, c.NGHTTP2_SETTINGS, 0, 0, 0, 0, 0 });
    try appendRawTestHeaders(&first_request, 1, "/test.Echo/Bidi", false);
    var malformed_body: std.ArrayList(u8) = .empty;
    defer malformed_body.deinit(std.testing.allocator);
    try malformed_body.appendSlice(std.testing.allocator, valid);
    try malformed_body.appendSlice(std.testing.allocator, &.{ 2, 0, 0, 0, 0 });
    try appendRawTestData(&first_request, 1, malformed_body.items, false);
    writer.interface.writeAll(first_request.items) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;

    var output: std.ArrayList(u8) = .empty;
    defer output.deinit(std.testing.allocator);
    var read_buffer: [512]u8 = undefined;
    for (0..10) |_| {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = socket.socket.handle,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
        const length = try std.posix.read(socket.socket.handle, &read_buffer);
        if (length == 0) return error.UnexpectedEof;
        try output.appendSlice(std.testing.allocator, read_buffer[0..length]);
        if (testOutputHasFrame(output.items, 1, c.NGHTTP2_RST_STREAM, 0)) break;
    }
    try std.testing.expect(testOutputHasFrame(output.items, 1, c.NGHTTP2_RST_STREAM, 0));

    const unary_body = try frame.encode(std.testing.allocator, "reuse");
    defer std.testing.allocator.free(unary_body);
    var second_request: std.ArrayList(u8) = .empty;
    defer second_request.deinit(std.testing.allocator);
    try appendRawTestHeaders(&second_request, 3, "/test.Echo/Unary", false);
    try appendRawTestData(&second_request, 3, unary_body, true);
    writer.interface.writeAll(second_request.items) catch |err| return writer.err orelse err;
    writer.interface.flush() catch |err| return writer.err orelse err;

    for (0..10) |_| {
        var poll_fds = [_]std.posix.pollfd{.{
            .fd = socket.socket.handle,
            .events = std.posix.POLL.IN,
            .revents = 0,
        }};
        if (try std.posix.poll(&poll_fds, 1000) == 0) return error.TestTimeout;
        const length = try std.posix.read(socket.socket.handle, &read_buffer);
        if (length == 0) return error.UnexpectedEof;
        try output.appendSlice(std.testing.allocator, read_buffer[0..length]);
        if (testOutputHasFrame(output.items, 3, c.NGHTTP2_HEADERS, c.NGHTTP2_FLAG_END_STREAM)) break;
    }
    try std.testing.expect(testOutputHasFrame(output.items, 3, c.NGHTTP2_HEADERS, c.NGHTTP2_FLAG_END_STREAM));

    var capture = TestResponseCapture{};
    defer capture.deinit();
    try capture.decode(output.items);
    try std.testing.expectEqual(@as(?u32, @intFromEnum(status.Code.invalid_argument)), capture.stream1_status);
    try std.testing.expect(capture.stream1_trailing_metadata_matches);
    try std.testing.expect(capture.stream1_reset);
    try std.testing.expectEqual(@as(?u32, 0), capture.stream3_status);
    const unary_response = try frame.decodeUnary(std.testing.allocator, capture.stream3_data.items, 64);
    defer std.testing.allocator.free(unary_response);
    try std.testing.expectEqualStrings("reused", unary_response);
    try std.testing.expectEqual(@as(usize, 1), streaming_handler.messages.load(.acquire));
    try std.testing.expectEqual(@as(usize, 1), streaming_handler.cancels.load(.acquire));
}

test "response encoding list parsing" {
    try std.testing.expect(acceptsEncoding("gzip", .gzip));
    try std.testing.expect(acceptsEncoding("identity, gzip", .gzip));
    try std.testing.expect(acceptsEncoding("identity,unknown,\tgzip ", .gzip));
    try std.testing.expect(!acceptsEncoding("identity", .gzip));
    try std.testing.expect(!acceptsEncoding("xgzip", .gzip));
}

test "expired and malformed grpc-timeout values fail only their stream" {
    const Handler = struct {
        calls: usize = 0,

        fn handle(self: *@This(), allocator: std.mem.Allocator, _: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            self.calls += 1;
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    const cases = [_]struct {
        values: []const []const u8,
        expected: status.Code,
    }{
        .{ .values = &.{"0n"}, .expected = .deadline_exceeded },
        .{ .values = &.{""}, .expected = .invalid_argument },
        .{ .values = &.{"1"}, .expected = .invalid_argument },
        .{ .values = &.{"123456789n"}, .expected = .invalid_argument },
        .{ .values = &.{"1x"}, .expected = .invalid_argument },
        .{ .values = &.{ "1S", "2S" }, .expected = .invalid_argument },
    };

    var handler = Handler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerUnary(
        "/test.Echo/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );

    for (cases) |case| {
        var connection = Connection{ .server = server.impl };
        try connection.initializeSession();
        defer deinitTestConnection(&connection);
        try feedTestRequest(&connection, .{ .timeout_values = case.values });
        const stream = connection.streams.get(1).?;
        try std.testing.expect(stream.timeout_seen);
        try std.testing.expect(stream.responded);
        if (case.expected == .deadline_exceeded) {
            try std.testing.expect(stream.deadline != null);
            try std.testing.expect(stream.deadline.?.isExceeded());
        }
        try std.testing.expectEqual(case.expected, stream.response_code);
        try std.testing.expect(!connection.closing);
    }
    try std.testing.expectEqual(@as(usize, 0), handler.calls);
}

test "deadline expiration pops only expired roots among many streams" {
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer deinitTestConnection(&connection);

    try feedTestRequest(&connection, .{ .end_stream = false, .timeout_values = &.{"0n"} });
    const stream = connection.streams.get(1).?;
    try std.testing.expect(!stream.responded);
    var future_streams: [256]Stream = undefined;
    for (&future_streams, 0..) |*future, index| {
        future.* = Stream.init(std.testing.allocator, &connection, @intCast(index * 2 + 3));
    }
    defer for (&future_streams) |*future| future.deinit();
    for (&future_streams) |*future| {
        try setStreamDeadline(future, deadline.Deadline.initAfter(server.impl.clock, std.time.ns_per_s));
    }

    expireDeadlines(server.impl, server.impl.clock.now());
    try std.testing.expect(stream.responded);
    try std.testing.expectEqual(status.Code.deadline_exceeded, stream.response_code);
    try std.testing.expect(!connection.closing);
    try std.testing.expectEqual(future_streams.len, server.impl.deadline_heap.items.len);
    for (&future_streams) |*future| try std.testing.expect(future.deadline_heap_index != null);
    try expectDeadlineHeapConsistent(server.impl);
    try std.testing.expect(server.impl.popDirtyConnection() == &connection);
    try std.testing.expect(server.impl.popDirtyConnection() == null);
}

test "malformed request metadata rejects one stream and preserves the connection" {
    const Handler = struct {
        calls: usize = 0,
        metadata_matches: bool = false,

        fn handle(self: *@This(), allocator: std.mem.Allocator, context: *service.ServerContext, request: []const u8) !service.UnaryResponse {
            self.calls += 1;
            const entries = context.request_metadata.items();
            self.metadata_matches = entries.len == 2 and
                std.mem.eql(u8, entries[0].key, "trace-bin") and
                std.mem.eql(u8, entries[0].value, &.{0xab}) and
                std.mem.eql(u8, entries[1].key, "trace-bin") and
                std.mem.eql(u8, entries[1].value, &.{ 0xab, 0xab, 0xab }) and
                context.request_metadata.getFirst("x-control") == null and
                context.request_metadata.getFirst("grpc-future") == null;
            return service.UnaryResponse.ok(allocator, request);
        }
    };

    var handler = Handler{};
    var server = try Server.init(std.testing.allocator, .{});
    defer server.deinit();
    try server.registerUnary(
        "/test.Echo/Unary",
        service.UnaryHandler.bind(Handler, &handler, Handler.handle),
    );
    var connection = Connection{ .server = server.impl };
    try connection.initializeSession();
    defer deinitTestConnection(&connection);

    try feedTestRequest(&connection, .{ .metadata_entries = &.{.{
        .key = "trace-bin",
        .value = "not base64!",
    }} });
    const rejected = connection.streams.get(1).?;
    try std.testing.expectEqual(status.Code.invalid_argument, rejected.response_code);
    try std.testing.expect(!connection.closing);

    try feedTestRequest(&connection, .{
        .stream_id = 3,
        .include_preface = false,
        .metadata_entries = &.{.{ .key = "x!invalid", .value = "value" }},
    });
    const invalid_key = connection.streams.get(3).?;
    try std.testing.expectEqual(status.Code.invalid_argument, invalid_key.response_code);
    try std.testing.expect(!connection.closing);

    try feedTestRequest(&connection, .{
        .stream_id = 5,
        .include_preface = false,
        .body = "ping",
        .metadata_entries = &.{
            .{ .key = "trace-bin", .value = "qw==,q6ur" },
            .{ .key = "x-control", .value = "bad\tvalue" },
            .{ .key = "grpc-future", .value = "ignored" },
        },
    });
    const accepted = connection.streams.get(5).?;
    try std.testing.expectEqual(status.Code.ok, accepted.response_code);
    try std.testing.expectEqual(@as(usize, 1), handler.calls);
    try std.testing.expect(handler.metadata_matches);
    try std.testing.expect(!connection.closing);
}
