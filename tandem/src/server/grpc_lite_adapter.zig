const std = @import("std");
const grpc = @import("grpc_lite");
const PipelineServer = @import("../pipeline.zig").PipelineServer;
const processRequest = @import("../pipeline.zig").processRequest;
const messages = @import("../proto/messages.zig");
const watchEvent = @import("../pipeline.zig").watchEvent;

pub const UnaryEndpoint = struct {
    pipeline: *PipelineServer,
    path: []const u8,
};

const WatchSubscription = struct {
    watch_id: i64,
    call: grpc.stream.ServerCall,
    key: []u8,
    range_end: []u8,
    start_revision: i64 = 0,
    prev_kv: bool = false,
    no_put: bool = false,
    no_delete: bool = false,
};

pub const WatchEndpoint = struct {
    pipeline: *PipelineServer,
    allocator: std.mem.Allocator,
    mutex: std.atomic.Mutex = .unlocked,
    subscriptions: std.ArrayList(WatchSubscription) = .empty,
    const path = "/etcdserverpb.Watch/Watch";

    pub fn init(allocator: std.mem.Allocator, pipeline: *PipelineServer) WatchEndpoint {
        return .{ .pipeline = pipeline, .allocator = allocator };
    }

    pub fn deinit(self: *WatchEndpoint) void {
        self.pipeline.watch_publish_fn = null;
        self.pipeline.watch_publish_context = null;
        for (self.subscriptions.items) |*subscription| self.freeSubscription(subscription);
        self.subscriptions.deinit(self.allocator);
    }

    fn freeSubscription(self: *WatchEndpoint, subscription: *WatchSubscription) void {
        _ = self.pipeline.watch_registry.cancelWatch(subscription.watch_id);
        subscription.call.deinit();
        self.allocator.free(subscription.key);
        self.allocator.free(subscription.range_end);
    }

    fn publish(context: *anyopaque, committed: []const messages.Event) !void {
        const self: *WatchEndpoint = @ptrCast(@alignCast(context));
        lock(&self.mutex);
        defer self.mutex.unlock();
        var index: usize = 0;
        while (index < self.subscriptions.items.len) {
            const subscription = &self.subscriptions.items[index];
            var events: std.ArrayList(messages.Event) = .empty;
            defer events.deinit(self.allocator);
            for (committed) |event| {
                if (!matches(subscription.*, event.kv.key) or event.kv.mod_revision < subscription.start_revision) continue;
                if ((subscription.no_put and event.type == .PUT) or (subscription.no_delete and event.type == .DELETE)) continue;
                var copy = event;
                if (!subscription.prev_kv) copy.prev_kv = .{};
                try events.append(self.allocator, copy);
            }
            if (events.items.len == 0) {
                index += 1;
                continue;
            }
            const response = messages.WatchResponse{
                .header = self.pipeline.buildHeader(),
                .watch_id = subscription.watch_id,
                .events = events.items,
            };
            const payload = try response.encode(self.allocator);
            defer self.allocator.free(payload);
            subscription.call.send(payload, .{}) catch {
                // Never silently drop a watch when its bounded queue fills.
                subscription.call.abort();
                var removed = self.subscriptions.orderedRemove(index);
                self.freeSubscription(&removed);
                continue;
            };
            index += 1;
        }
    }
};

fn matches(subscription: WatchSubscription, key: []const u8) bool {
    if (subscription.range_end.len == 0) return std.mem.eql(u8, subscription.key, key);
    if (std.mem.eql(u8, subscription.range_end, "\x00")) return std.mem.order(u8, key, subscription.key) != .lt;
    return std.mem.order(u8, key, subscription.key) != .lt and std.mem.order(u8, key, subscription.range_end) == .lt;
}

fn lock(mutex: *std.atomic.Mutex) void {
    while (!mutex.tryLock()) std.atomic.spinLoopHint();
}

/// Map a storage-layer error onto the gRPC status etcd would return for it.
///
/// Collapsing every error to `unimplemented` is actively harmful rather than
/// merely imprecise: `unimplemented` is the one code the apiserver reads as
/// "this endpoint does not support the feature", so a compacted read or a
/// revoked lease reported that way is indistinguishable from a missing RPC.
/// The codes below match etcd's rpctypes mapping.
pub fn statusForError(err: anyerror) grpc.status.Status {
    const code: grpc.status.Code = switch (err) {
        // etcd returns ErrCompacted and ErrFutureRev as OutOfRange.
        error.Compacted, error.FutureRevision => .out_of_range,
        error.LeaseNotFound => .not_found,
        error.InvalidLeaseTTL, error.InvalidRequest => .invalid_argument,
        error.KeyNotFound => .not_found,
        error.NestedTransactionUnsupported => .unimplemented,
        // Serving an empty body would read as a valid empty snapshot, so an
        // unimplemented maintenance call has to say so on the wire.
        error.SnapshotNotImplemented => .unimplemented,
        error.InvalidResponse, error.RqliteError => .internal,
        error.OutOfMemory => .resource_exhausted,
        else => .internal,
    };
    return grpc.status.Status.init(code, @errorName(err));
}

pub fn handleUnary(
    endpoint: *UnaryEndpoint,
    allocator: std.mem.Allocator,
    context: *grpc.service.ServerContext,
    request: []const u8,
) !grpc.service.UnaryResponse {
    _ = context;
    const payload = processRequest(endpoint.pipeline, endpoint.path, request) catch |err| {
        return grpc.service.UnaryResponse.fail(allocator, statusForError(err));
    };
    defer endpoint.pipeline.allocator.free(payload);
    return grpc.service.UnaryResponse.ok(allocator, payload);
}

pub fn registerKV(server: *grpc.server.Server, endpoint: *UnaryEndpoint) !void {
    try server.registerUnary(endpoint.path, grpc.service.UnaryHandler.bind(UnaryEndpoint, endpoint, handleUnary));
}

pub fn registerWatch(server: *grpc.server.Server, endpoint: *WatchEndpoint) !void {
    endpoint.pipeline.setWatchPublisher(endpoint, WatchEndpoint.publish);
    try server.registerStream(WatchEndpoint.path, .{
        .context = endpoint,
        .on_start = streamStart,
        .on_message = watchMessage,
        .on_remote_end = streamEnd,
        .on_terminal = watchTerminal,
    });
}

pub fn registerStream(server: *grpc.server.Server, endpoint: *UnaryEndpoint) !void {
    try server.registerStream(endpoint.path, .{
        .context = endpoint,
        .on_start = streamStart,
        .on_message = streamMessage,
        .on_remote_end = streamEnd,
    });
}

fn streamStart(_: ?*anyopaque, _: grpc.stream.ServerStream, _: *grpc.service.ServerContext) !void {}

fn streamMessage(
    opaque_endpoint: ?*anyopaque,
    stream: grpc.stream.ServerStream,
    _: *grpc.service.ServerContext,
    request: []const u8,
    compression: grpc.compression.Compression,
) !grpc.stream.ReceiveAction {
    if (compression != .identity) return error.UnsupportedCompression;
    const endpoint: *UnaryEndpoint = @ptrCast(@alignCast(opaque_endpoint.?));
    const payload = processRequest(endpoint.pipeline, endpoint.path, request) catch |err| {
        try stream.send(@errorName(err), .{});
        return .continue_receiving;
    };
    defer endpoint.pipeline.allocator.free(payload);
    try stream.send(payload, .{});
    return .continue_receiving;
}

fn sendWatch(endpoint: *WatchEndpoint, stream: grpc.stream.ServerStream, response: messages.WatchResponse) !void {
    const bytes = try response.encode(endpoint.allocator);
    defer endpoint.allocator.free(bytes);
    try stream.send(bytes, .{});
}

fn watchMessage(
    opaque_endpoint: ?*anyopaque,
    stream: grpc.stream.ServerStream,
    _: *grpc.service.ServerContext,
    request_data: []const u8,
    compression: grpc.compression.Compression,
) !grpc.stream.ReceiveAction {
    if (compression != .identity) return error.UnsupportedCompression;
    const endpoint: *WatchEndpoint = @ptrCast(@alignCast(opaque_endpoint.?));
    endpoint.pipeline.lock();
    defer endpoint.pipeline.unlock();
    lock(&endpoint.mutex);
    defer endpoint.mutex.unlock();
    const request = try messages.WatchRequest.decode(request_data);
    const header = endpoint.pipeline.buildHeader();
    switch (request) {
        .create => |create| {
            defer std.heap.page_allocator.free(create.key);
            defer std.heap.page_allocator.free(create.range_end);
            const registry = &endpoint.pipeline.watch_registry;
            const id = try registry.createWatch(create.key, create.range_end, create.start_revision);
            errdefer _ = registry.cancelWatch(id);
            const compact = endpoint.pipeline.storage.compact_revision;
            try sendWatch(endpoint, stream, .{ .header = header, .watch_id = id, .created = true });
            if (create.start_revision > 0 and create.start_revision <= compact) {
                try sendWatch(endpoint, stream, .{ .header = header, .watch_id = id, .canceled = true, .compact_revision = compact });
                _ = registry.cancelWatch(id);
                return .continue_receiving;
            }
            var call = try stream.retain();
            errdefer call.deinit();
            const key = try endpoint.allocator.dupe(u8, create.key);
            errdefer endpoint.allocator.free(key);
            const range_end = try endpoint.allocator.dupe(u8, create.range_end);
            errdefer endpoint.allocator.free(range_end);
            const subscription = WatchSubscription{
                .watch_id = id,
                .call = call,
                .key = key,
                .range_end = range_end,
                .start_revision = if (create.start_revision > 0) create.start_revision else header.revision + 1,
                .prev_kv = create.prev_kv,
                .no_put = create.no_put,
                .no_delete = create.no_delete,
            };
            // Registration and replay share the pipeline lock with committed writes.
            // No write can slip between replay and the live subscription.
            if (create.start_revision > 0) {
                var replay: std.ArrayList(messages.Event) = .empty;
                defer replay.deinit(endpoint.allocator);
                var revision: i64 = 0;
                const committed = try endpoint.pipeline.storage.watchHistory(create.start_revision, header.revision);
                defer endpoint.pipeline.storage.freeWatchHistory(committed);
                for (committed) |history| {
                    const event = watchEvent(history);
                    if (event.kv.mod_revision < create.start_revision or !matches(subscription, event.kv.key)) continue;
                    if ((create.no_put and event.type == .PUT) or (create.no_delete and event.type == .DELETE)) continue;
                    if (replay.items.len > 0 and revision != event.kv.mod_revision) {
                        try sendWatch(endpoint, stream, .{ .header = header, .watch_id = id, .events = replay.items });
                        replay.clearRetainingCapacity();
                    }
                    revision = event.kv.mod_revision;
                    var copy = event;
                    if (!create.prev_kv) copy.prev_kv = .{};
                    try replay.append(endpoint.allocator, copy);
                }
                if (replay.items.len > 0) try sendWatch(endpoint, stream, .{ .header = header, .watch_id = id, .events = replay.items });
            }
            try endpoint.subscriptions.append(endpoint.allocator, subscription);
        },
        .cancel => |cancel| {
            var found = false;
            for (endpoint.subscriptions.items, 0..) |subscription, index| {
                if (subscription.watch_id != cancel.watch_id or subscription.call.id() != stream.id()) continue;
                var removed = endpoint.subscriptions.orderedRemove(index);
                endpoint.freeSubscription(&removed);
                found = true;
                break;
            }
            try sendWatch(endpoint, stream, .{ .header = header, .watch_id = cancel.watch_id, .canceled = true, .cancel_reason = if (found) "" else "watch ID not found" });
        },
        .progress => try sendWatch(endpoint, stream, .{ .header = header, .watch_id = -1 }),
    }
    return .continue_receiving;
}

fn watchTerminal(opaque_endpoint: ?*anyopaque, call_id: grpc.stream.ServerCallId, _: grpc.stream.ServerTerminalReason) void {
    const endpoint: *WatchEndpoint = @ptrCast(@alignCast(opaque_endpoint.?));
    endpoint.pipeline.lock();
    defer endpoint.pipeline.unlock();
    lock(&endpoint.mutex);
    defer endpoint.mutex.unlock();
    var index: usize = 0;
    while (index < endpoint.subscriptions.items.len) {
        if (endpoint.subscriptions.items[index].call.id() != call_id) {
            index += 1;
            continue;
        }
        var removed = endpoint.subscriptions.orderedRemove(index);
        endpoint.freeSubscription(&removed);
    }
}

fn streamEnd(_: ?*anyopaque, stream: grpc.stream.ServerStream, _: *grpc.service.ServerContext) !void {
    try stream.finish(grpc.status.Status.ok);
}

test "watch subscription range matching follows etcd half-open ranges" {
    const exact = WatchSubscription{ .watch_id = 1, .call = undefined, .key = @constCast("key"), .range_end = @constCast("") };
    try std.testing.expect(matches(exact, "key"));
    try std.testing.expect(!matches(exact, "key2"));

    const ranged = WatchSubscription{ .watch_id = 2, .call = undefined, .key = @constCast("a"), .range_end = @constCast("d") };
    try std.testing.expect(matches(ranged, "a"));
    try std.testing.expect(matches(ranged, "c"));
    try std.testing.expect(!matches(ranged, "d"));
}

test "storage errors map onto the gRPC codes etcd returns" {
    // A compacted read must not look like an unsupported RPC: the apiserver
    // treats `unimplemented` as "this endpoint lacks the feature".
    try std.testing.expectEqual(grpc.status.Code.out_of_range, statusForError(error.Compacted).code);
    try std.testing.expectEqual(grpc.status.Code.out_of_range, statusForError(error.FutureRevision).code);
    try std.testing.expectEqual(grpc.status.Code.not_found, statusForError(error.LeaseNotFound).code);
    try std.testing.expectEqual(grpc.status.Code.invalid_argument, statusForError(error.InvalidLeaseTTL).code);
    try std.testing.expectEqual(grpc.status.Code.internal, statusForError(error.InvalidResponse).code);
    // A maintenance call the layer does not serve has to be visibly
    // unimplemented; an empty success would read as a valid empty snapshot.
    try std.testing.expectEqual(grpc.status.Code.unimplemented, statusForError(error.SnapshotNotImplemented).code);
}
