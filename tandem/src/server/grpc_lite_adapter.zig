const std = @import("std");
const math = std.math;
const grpc = @import("grpc_lite");
const PipelineServer = @import("../pipeline.zig").PipelineServer;
const processRequest = @import("../pipeline.zig").processRequest;
const messages = @import("../proto/messages.zig");
const watchEvent = @import("../pipeline.zig").watchEvent;
const mvcc = @import("../storage/mvcc.zig");

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
        // An unrouted method path lands here. Reporting it as `internal`
        // would tell the apiserver the endpoint is temporarily broken, so it
        // retries a feature that will never arrive; `unimplemented` is what
        // lets it fall back. This is the difference the comment above warns
        // about, reached from the routing side rather than the storage side.
        error.UnimplementedMethod => .unimplemented,
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

// initialStreamChunkLimit is etcd's first chunk size for RangeStream; later
// chunks grow or shrink towards a response-size target.
const initialStreamChunkLimit: i64 = 10;

pub fn registerRangeStream(server: *grpc.server.Server, endpoint: *UnaryEndpoint) !void {
    try server.registerStream(endpoint.path, .{
        .context = endpoint,
        .on_start = streamStart,
        .on_message = rangeStreamMessage,
        .on_remote_end = streamEnd,
    });
}

fn rangeStreamMessage(
    opaque_endpoint: ?*anyopaque,
    stream: grpc.stream.ServerStream,
    _: *grpc.service.ServerContext,
    request_data: []const u8,
    compression: grpc.compression.Compression,
) !grpc.stream.ReceiveAction {
    if (compression != .identity) return error.UnsupportedCompression;
    const endpoint: *UnaryEndpoint = @ptrCast(@alignCast(opaque_endpoint.?));
    const pipeline = endpoint.pipeline;
    const allocator = pipeline.allocator;

    const req = messages.RangeRequest.decode(request_data) catch |err| {
        try stream.send(@errorName(err), .{});
        return .continue_receiving;
    };
    defer {
        std.heap.page_allocator.free(req.key);
        std.heap.page_allocator.free(req.range_end);
    }

    // etcd's RangeStream serves the default key ordering only and rejects
    // revision filters the unary Range accepts.
    if (req.sort_order != .NONE and !(req.sort_target == .KEY and req.sort_order == .ASCEND)) {
        try stream.send(@errorName(error.InvalidRequest), .{});
        return .continue_receiving;
    }
    if (req.min_mod_revision != 0 or req.max_mod_revision != 0 or
        req.min_create_revision != 0 or req.max_create_revision != 0)
    {
        try stream.send(@errorName(error.InvalidRequest), .{});
        return .continue_receiving;
    }
    if (req.serializable) {
        try stream.send(@errorName(error.InvalidRequest), .{});
        return .continue_receiving;
    }
    if (req.keys_only) {
        try stream.send(@errorName(error.InvalidRequest), .{});
        return .continue_receiving;
    }

    // count_only: single chunk with just the count.
    if (req.count_only) {
        return streamRangeCount(pipeline, &req, stream, allocator);
    }

    return streamRangeChunks(pipeline, &req, stream, allocator);
}

fn streamRangeCount(
    pipeline: *PipelineServer,
    req: *const messages.RangeRequest,
    stream: grpc.stream.ServerStream,
    allocator: std.mem.Allocator,
) !grpc.stream.ReceiveAction {
    const result = pipeline.storage.range(req.key, req.range_end, 0, req.revision, false, true) catch |err| {
        try stream.send(@errorName(err), .{});
        return .continue_receiving;
    };
    defer freeRangeResult(allocator, result);
    try pipeline.storage.syncState();
    const header_rev = if (result.count > 0) pipeline.buildHeader().revision else req.revision;
    const response = messages.RangeStreamResponse{
        .range = .{
            .header = .{ .revision = header_rev },
            .kvs = &[_]messages.KeyValue{},
            .more = false,
            .count = result.count,
        },
    };
    const bytes = try response.encode(allocator);
    defer allocator.free(bytes);
    try stream.send(bytes, .{});
    return .continue_receiving;
}

fn streamRangeChunks(
    pipeline: *PipelineServer,
    req: *const messages.RangeRequest,
    stream: grpc.stream.ServerStream,
    allocator: std.mem.Allocator,
) !grpc.stream.ReceiveAction {
    const total_limit: i64 = if (req.limit == 0) std.math.maxInt(i64) else req.limit;
    var count: i64 = 0;
    var header_rev: i64 = 0;

    var limit: i64 = @min(initialStreamChunkLimit, total_limit);
    var key = req.key;
    const range_end = req.range_end;
    var revision = req.revision;
    var next_key: []u8 = &[_]u8{};

    while (true) {
        const result = pipeline.storage.range(key, range_end, limit, revision, false, false) catch |err| {
            allocator.free(next_key);
            try stream.send(@errorName(err), .{});
            return .continue_receiving;
        };

        if (header_rev == 0) {
            header_rev = pipeline.storage.current_revision;
            if (revision == 0) {
                revision = header_rev;
            }
        }
        count += result.count;

        const more = result.more and count != total_limit;

        // Convert MVCCKeyValue[] to KeyValue[] for the proto response.
        var kvs = try mvccToKeyValues(allocator, result.kvs);
        defer {
            for (kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(kvs);
        }
        freeRangeResult(allocator, result);

        var this_next_key: []u8 = &[_]u8{};
        if (more and kvs.len > 0) {
            // next key = last returned key + 0x00 (etcd's "continue after" sentinel)
            const last = kvs[kvs.len - 1];
            this_next_key = try allocator.alloc(u8, last.key.len + 1);
            @memcpy(this_next_key[0..last.key.len], last.key);
            this_next_key[last.key.len] = 0;
        }

        // Only the terminal chunk carries header/more/count; intermediate
        // chunks carry just the key-value pairs.
        const chunk = messages.RangeResponse{
            .header = if (more) .{} else .{ .revision = header_rev },
            .kvs = kvs,
            .more = if (more) false else false,
            .count = if (more) 0 else count,
        };
        const response = messages.RangeStreamResponse{ .range = chunk };
        const bytes = try response.encode(allocator);
        defer allocator.free(bytes);
        try stream.send(bytes, .{});

        // Free the KV slices now — encoding has copied them into bytes.
        for (kvs) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        }
        allocator.free(kvs);
        kvs = &[_]messages.KeyValue{};

        if (more and this_next_key.len > 0) {
            allocator.free(next_key);
            next_key = this_next_key;
            key = next_key;
            // Adjust next chunk limit — simple doubling up to a cap.
            limit = @min(limit * 2, total_limit - count);
            if (limit <= 0) break;
        } else {
            allocator.free(this_next_key);
            allocator.free(next_key);
            break;
        }
    }
    return .continue_receiving;
}

/// Free a RangeResult's owned allocations.
fn freeRangeResult(allocator: std.mem.Allocator, result: mvcc.RangeResult) void {
    for (result.kvs) |kv| {
        allocator.free(kv.key);
        allocator.free(kv.value);
    }
    allocator.free(result.kvs);
}

/// Convert []MVCCKeyValue to the proto-layer []KeyValue (deep-copied).
fn mvccToKeyValues(allocator: std.mem.Allocator, mvcc_kvs: []mvcc.MVCCKeyValue) ![]messages.KeyValue {
    var kvs = try allocator.alloc(messages.KeyValue, mvcc_kvs.len);
    for (mvcc_kvs, 0..) |src, i| {
        kvs[i] = messages.KeyValue{
            .key = try allocator.dupe(u8, src.key),
            .create_revision = src.create_revision,
            .mod_revision = src.mod_revision,
            .version = src.version,
            .value = try allocator.dupe(u8, src.value),
            .lease = src.lease,
        };
    }
    return kvs;
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
    try endpoint.pipeline.storage.syncState();
    try endpoint.pipeline.publishCommittedSinceCursor();
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
            if (create.start_revision > 0 and create.start_revision < compact) {
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
    // An unrouted method must report the same way. `internal` would tell the
    // apiserver the endpoint is temporarily broken and make it retry a
    // feature that is never coming.
    try std.testing.expectEqual(grpc.status.Code.unimplemented, statusForError(error.UnimplementedMethod).code);
}
