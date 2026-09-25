// Request processing pipeline for Tandem
// Bridges grpc-lite transport handling with protobuf messages (messages.zig)
// and implements all service handlers.
//
// This module is the main entry point for processing etcd v3 API requests.
// It connects:
//   - Protobuf deserialization (messages.zig decode)
//   - Service logic (handlers)
//   - Protobuf serialization (messages.zig encode)

const std = @import("std");
const Allocator = std.mem.Allocator;
const messages = @import("proto/messages.zig");
const MethodPath = @import("server/method_path.zig").MethodPath;
const wire = @import("proto/wire.zig");
const mvcc = @import("storage/mvcc.zig");
const rqlite = @import("storage/rqlite.zig");

/// The etcd version this layer claims in Maintenance.Status. Kubernetes parses
/// it to decide whether the endpoint supports watch progress notifications
/// (>= 3.4.31 / 3.5.13), so it has to stay a real semantic version and stay
/// above those thresholds.
pub const etcdVersion = "3.6.0";
test "transaction publishes committed put and delete history in a single revision" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    _ = try server.storage.put("a", "old", 0, false);
    _ = try server.storage.put("b", "old", 0, false);
    const Capture = struct {
        batches: usize = 0,
        fn publish(context: *anyopaque, events: []const messages.Event) !void {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.batches += 1;
            try testing.expectEqual(@as(usize, 2), events.len);
            try testing.expectEqual(messages.EventType.PUT, events[0].type);
            try testing.expectEqual(messages.EventType.DELETE, events[1].type);
            try testing.expectEqual(events[0].kv.mod_revision, events[1].kv.mod_revision);
            try testing.expectEqualStrings("old", events[0].prev_kv.value);
            try testing.expectEqualStrings("old", events[1].prev_kv.value);
        }
    };
    var capture = Capture{};
    server.setWatchPublisher(&capture, Capture.publish);
    const request = "\x12\x0a\x12\x08\x0a\x01a\x12\x03new" ++ "\x12\x05\x1a\x03\x0a\x01b";
    const response = try processRequest(&server, "/etcdserverpb.KV/Txn", request);
    defer testing.allocator.free(response);
    try testing.expectEqual(@as(usize, 1), capture.batches);
    try testing.expectEqual(@as(i64, 3), server.storage.current_revision);
    try testing.expectEqual(@as(usize, 4), server.storage.memory_history.items.len);
    const put = watchEvent(server.storage.memory_history.items[2]);
    try testing.expectEqual(@as(i64, 1), put.kv.create_revision);
    try testing.expectEqual(@as(i64, 2), put.kv.version);
    try testing.expectEqual(@as(i64, 1), put.prev_kv.mod_revision);
}

test "a watch resuming from a compacted revision is refused, not silently empty" {
    // KIP-29 requires an explicit Compacted error here, "never an empty
    // stream". A watch that silently accepts a compacted start_revision never
    // fires, so the controller waiting on it stops converging with no error
    // anywhere.
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    _ = try server.storage.put("a", "1", 0, false);
    _ = try server.storage.put("a", "2", 0, false);
    const compacted = try processRequest(&server, "/etcdserverpb.KV/Compact", "\x08\x02");
    defer testing.allocator.free(compacted);
    try testing.expectEqual(@as(i64, 2), server.storage.compact_revision);

    // WatchRequest.create_request is field 1, a nested message; inside it
    // key is field 1 and start_revision is field 3.
    const create = "\x0a\x05" ++ "\x0a\x01a" ++ "\x18\x01";
    try testing.expectError(error.Compacted, processRequest(&server, "/etcdserverpb.Watch/Watch", create));
    // The watermark itself is compacted too, matching Range, Compact and
    // watchHistory, which all reject at or below it: everything at or below
    // the watermark has been removed, so a watch starting there can never
    // deliver the events it was asked to resume from.
    const at_watermark = "\x0a\x05" ++ "\x0a\x01a" ++ "\x18\x02";
    try testing.expectError(error.Compacted, processRequest(&server, "/etcdserverpb.Watch/Watch", at_watermark));
    // Above the watermark is still available.
    const after_watermark = try processRequest(&server, "/etcdserverpb.Watch/Watch", "\x0a\x05" ++ "\x0a\x01a" ++ "\x18\x03");
    defer testing.allocator.free(after_watermark);
    // A watch with no start_revision means "from now", and is never compacted.
    const from_now = try processRequest(&server, "/etcdserverpb.Watch/Watch", "\x0a\x03" ++ "\x0a\x01a");
    defer testing.allocator.free(from_now);
}

test "compaction records a boundary and rejects a future revision" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    _ = try server.storage.put("a", "1", 0, false);
    _ = try server.storage.put("a", "2", 0, false);
    const response = try processRequest(&server, "/etcdserverpb.KV/Compact", "\x08\x02");
    defer testing.allocator.free(response);
    try testing.expectEqual(@as(i64, 2), server.storage.compact_revision);
    try testing.expectError(error.Compacted, server.storage.compact(1));
    try testing.expectError(error.FutureRevision, server.storage.compact(3));
}

/// Pipeline server state - holds shared state for all requests
pub const PipelineServer = struct {
    allocator: Allocator,
    revision: i64 = 0,
    raft_term: u64 = 1,
    storage: mvcc.Storage,
    watch_registry: mvcc.WatchRegistry,
    watch_publish_context: ?*anyopaque = null,
    watch_publish_fn: ?*const fn (*anyopaque, []const messages.Event) anyerror!void = null,
    watch_cursor: i64 = 0,
    mutex: std.atomic.Mutex = .unlocked,

    pub fn lock(self: *PipelineServer) void {
        while (!self.mutex.tryLock()) std.atomic.spinLoopHint();
    }

    pub fn unlock(self: *PipelineServer) void {
        self.mutex.unlock();
    }

    pub fn initMemory(allocator: Allocator) PipelineServer {
        return .{
            .allocator = allocator,
            .revision = 0,
            .raft_term = 1,
            .storage = mvcc.Storage.initMemory(allocator),
            .watch_registry = mvcc.WatchRegistry.init(allocator),
        };
    }

    /// Production initialization never falls back to an ephemeral store.
    pub fn initPersistent(allocator: Allocator, config: mvcc.StorageConfig) !PipelineServer {
        const storage = try mvcc.Storage.init(allocator, config);
        return .{
            .allocator = allocator,
            .revision = storage.current_revision,
            .watch_cursor = storage.current_revision,
            .raft_term = storage.raft_term,
            .storage = storage,
            .watch_registry = mvcc.WatchRegistry.init(allocator),
        };
    }

    pub fn deinit(self: *PipelineServer) void {
        self.storage.deinit();
        self.watch_registry.deinit();
    }

    /// Build a response header with current state
    pub fn buildHeader(self: *PipelineServer) messages.ResponseHeader {
        return .{
            .cluster_id = 0,
            .member_id = 0,
            .revision = self.storage.current_revision,
            .raft_term = self.raft_term,
        };
    }

    /// Increment and return the next revision
    pub fn nextRevision(self: *PipelineServer) i64 {
        self.revision += 1;
        return self.revision;
    }

    pub fn setWatchPublisher(
        self: *PipelineServer,
        context: *anyopaque,
        publish: *const fn (*anyopaque, []const messages.Event) anyerror!void,
    ) void {
        self.watch_publish_context = context;
        self.watch_publish_fn = publish;
        self.watch_cursor = self.storage.current_revision;
    }

    fn publishWatchEvents(self: *PipelineServer, events: []const messages.Event) void {
        if (self.watch_publish_fn) |publish| publish(self.watch_publish_context.?, events) catch |err| {
            std.log.err("failed to publish committed watch event: {s}", .{@errorName(err)});
        };
    }

    /// Poll the shared history under the pipeline lock. Local writes and
    /// commits from other Tandem instances use the same revision cursor.
    pub fn publishCommittedSinceCursor(self: *PipelineServer) !void {
        const end = self.storage.current_revision;
        if (end <= self.watch_cursor) return;
        if (self.watch_publish_fn == null) {
            self.watch_cursor = end;
            return;
        }
        const history = try self.storage.watchHistory(self.watch_cursor + 1, end);
        defer self.storage.freeWatchHistory(history);
        var events: std.ArrayList(messages.Event) = .empty;
        defer events.deinit(self.allocator);
        for (history) |event| try events.append(self.allocator, watchEvent(event));
        if (events.items.len != 0) self.publishWatchEvents(events.items);
        self.watch_cursor = end;
    }
};

// ─── Request Processor ───────────────────────────────────────────────────────

/// Process a full gRPC request through the pipeline
pub fn processRequest(server: *PipelineServer, method_path: []const u8, request_data: []const u8) ![]u8 {
    server.lock();
    defer server.unlock();
    try server.storage.syncState();
    const path = try MethodPath.parse(method_path);
    const response = try routeRequest(server, path, request_data);
    errdefer server.allocator.free(response);
    try server.publishCommittedSinceCursor();
    return response;
}

pub fn watchEvent(event: mvcc.WatchEvent) messages.Event {
    return .{
        .type = if (event.event_type == .PUT) .PUT else .DELETE,
        .kv = .{ .key = event.key, .value = event.value, .mod_revision = event.mod_revision, .create_revision = if (event.event_type == .PUT) event.create_revision else 0, .version = if (event.event_type == .PUT) event.version else 0, .lease = event.lease },
        .prev_kv = if (event.prev_key.len > 0) .{ .key = event.prev_key, .value = event.prev_value, .create_revision = event.create_revision, .mod_revision = event.prev_mod_revision, .version = event.prev_version, .lease = event.prev_lease } else .{},
    };
}

/// Adapter used by the protocol server without introducing a module cycle.
/// Route and handle a request based on parsed method path
pub fn routeRequest(server: *PipelineServer, path: MethodPath, request_data: []const u8) ![]u8 {
    if (std.mem.eql(u8, path.service, "etcdserverpb.KV")) {
        if (std.mem.eql(u8, path.method, "Range")) return try handleKVRange(server, request_data);
        if (std.mem.eql(u8, path.method, "Put")) return try handleKVPut(server, request_data);
        if (std.mem.eql(u8, path.method, "DeleteRange")) return try handleKVDeleteRange(server, request_data);
        if (std.mem.eql(u8, path.method, "Txn")) return try handleKVTxn(server, request_data);
        if (std.mem.eql(u8, path.method, "Compact")) return try handleKVCompact(server, request_data);
    } else if (std.mem.eql(u8, path.service, "etcdserverpb.Watch")) {
        if (std.mem.eql(u8, path.method, "Watch")) return try handleWatch(server, request_data);
    } else if (std.mem.eql(u8, path.service, "etcdserverpb.Lease")) {
        if (std.mem.eql(u8, path.method, "LeaseGrant")) return try handleLeaseGrant(server, request_data);
        if (std.mem.eql(u8, path.method, "LeaseRevoke")) return try handleLeaseRevoke(server, request_data);
        if (std.mem.eql(u8, path.method, "LeaseKeepAlive")) return try handleLeaseKeepAlive(server, request_data);
        if (std.mem.eql(u8, path.method, "LeaseTimeToLive")) return try handleLeaseTimeToLive(server, request_data);
        if (std.mem.eql(u8, path.method, "LeaseLeases")) return try handleLeaseLeases(server, request_data);
    } else if (std.mem.eql(u8, path.service, "etcdserverpb.Cluster")) {
        if (std.mem.eql(u8, path.method, "MemberList")) return try handleMemberList(server, request_data);
        if (std.mem.eql(u8, path.method, "MemberAdd")) return try handleMemberAdd(server, request_data);
        if (std.mem.eql(u8, path.method, "MemberRemove")) return try handleMemberRemove(server, request_data);
        if (std.mem.eql(u8, path.method, "MemberUpdate")) return try handleMemberUpdate(server, request_data);
        if (std.mem.eql(u8, path.method, "MemberPromote")) return try handleMemberPromote(server, request_data);
    } else if (std.mem.eql(u8, path.service, "etcdserverpb.Maintenance")) {
        if (std.mem.eql(u8, path.method, "Status")) return try handleStatus(server, request_data);
        if (std.mem.eql(u8, path.method, "Defragment")) return try handleDefragment(server, request_data);
        if (std.mem.eql(u8, path.method, "Hash")) return try handleHash(server, request_data);
        if (std.mem.eql(u8, path.method, "HashKV")) return try handleHashKV(server, request_data);
        if (std.mem.eql(u8, path.method, "Snapshot")) return try handleSnapshot(server, request_data);
        if (std.mem.eql(u8, path.method, "MoveLeader")) return try handleMoveLeader(server, request_data);
        if (std.mem.eql(u8, path.method, "Alarm")) return try handleAlarm(server, request_data);
    } else if (std.mem.eql(u8, path.service, "etcdserverpb.Auth")) {
        if (std.mem.eql(u8, path.method, "AuthEnable")) return try handleAuthEnable(server, request_data);
        if (std.mem.eql(u8, path.method, "AuthDisable")) return try handleAuthDisable(server, request_data);
        if (std.mem.eql(u8, path.method, "AuthStatus")) return try handleAuthStatus(server, request_data);
        if (std.mem.eql(u8, path.method, "Authenticate")) return try handleAuthenticate(server, request_data);
        if (std.mem.eql(u8, path.method, "UserAdd")) return try handleUserAdd(server, request_data);
        if (std.mem.eql(u8, path.method, "UserGet")) return try handleUserGet(server, request_data);
        if (std.mem.eql(u8, path.method, "UserList")) return try handleUserList(server, request_data);
        if (std.mem.eql(u8, path.method, "UserDelete")) return try handleUserDelete(server, request_data);
        if (std.mem.eql(u8, path.method, "UserChangePassword")) return try handleUserChangePassword(server, request_data);
        if (std.mem.eql(u8, path.method, "UserGrantRole")) return try handleUserGrantRole(server, request_data);
        if (std.mem.eql(u8, path.method, "UserRevokeRole")) return try handleUserRevokeRole(server, request_data);
        if (std.mem.eql(u8, path.method, "RoleAdd")) return try handleRoleAdd(server, request_data);
        if (std.mem.eql(u8, path.method, "RoleGet")) return try handleRoleGet(server, request_data);
        if (std.mem.eql(u8, path.method, "RoleList")) return try handleRoleList(server, request_data);
        if (std.mem.eql(u8, path.method, "RoleDelete")) return try handleRoleDelete(server, request_data);
        if (std.mem.eql(u8, path.method, "RoleGrantPermission")) return try handleRoleGrantPermission(server, request_data);
        if (std.mem.eql(u8, path.method, "RoleRevokePermission")) return try handleRoleRevokePermission(server, request_data);
    }

    return error.UnimplementedMethod;
}

// ─── KV Service Handlers ────────────────────────────────────────────────────

fn handleKVRange(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.RangeRequest.decode(request_data);

    const result = try server.storage.range(request.key, request.range_end, request.limit, request.revision, request.keys_only, request.count_only);
    defer {
        for (result.kvs) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        }
        allocator.free(result.kvs);
    }
    // A peer may commit between the entry refresh and this linearizable read.
    // The header must not predate data returned by the read itself.
    try server.storage.syncState();
    if (request.revision > 0 and request.revision <= server.storage.compact_revision) return error.Compacted;
    var kvs = try allocator.alloc(messages.KeyValue, result.kvs.len);
    for (result.kvs, 0..) |kv, i| kvs[i] = .{
        .key = kv.key,
        .create_revision = kv.create_revision,
        .mod_revision = kv.mod_revision,
        .version = kv.version,
        .value = if (request.keys_only) &[_]u8{} else kv.value,
        .lease = kv.lease,
    };
    const response = messages.RangeResponse{
        .header = server.buildHeader(),
        .kvs = kvs,
        .more = result.more,
        .count = result.count,
    };
    const encoded = try response.encode(allocator);
    allocator.free(kvs);

    std.heap.page_allocator.free(request.key);
    std.heap.page_allocator.free(request.range_end);
    return encoded;
}

fn handleKVPut(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.PutRequest.decode(request_data);
    const result = try server.storage.put(request.key, request.value, request.lease, request.prev_kv);
    defer if (result.prev_kv) |previous| {
        allocator.free(previous.key);
        allocator.free(previous.value);
    };
    var header = server.buildHeader();
    header.revision = result.revision;
    var previous: messages.KeyValue = .{};
    if (result.prev_kv) |kv| previous = .{ .key = kv.key, .create_revision = kv.create_revision, .mod_revision = kv.mod_revision, .version = kv.version, .value = kv.value, .lease = kv.lease };
    const response = messages.PutResponse{ .header = header, .prev_kv = if (result.prev_kv != null) previous else .{} };
    const encoded = try response.encode(allocator);

    std.heap.page_allocator.free(request.key);
    std.heap.page_allocator.free(request.value);
    return encoded;
}

fn handleKVDeleteRange(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.DeleteRangeRequest.decode(request_data);

    const result = try server.storage.deleteRange(request.key, request.range_end, true);
    defer {
        for (result.prev_kvs) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        }
        allocator.free(result.prev_kvs);
    }
    const previous_len = if (request.prev_kv) result.prev_kvs.len else 0;
    var previous = try allocator.alloc(messages.KeyValue, previous_len);
    if (request.prev_kv) {
        for (result.prev_kvs, 0..) |kv, i| previous[i] = .{ .key = kv.key, .create_revision = kv.create_revision, .mod_revision = kv.mod_revision, .version = kv.version, .value = kv.value, .lease = kv.lease };
    }
    const response = messages.DeleteRangeResponse{
        .header = server.buildHeader(),
        .deleted = result.deleted,
        .prev_kvs = previous,
    };
    const encoded = try response.encode(allocator);

    std.heap.page_allocator.free(request.key);
    std.heap.page_allocator.free(request.range_end);
    allocator.free(previous);
    return encoded;
}

fn handleKVTxn(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.TxnRequest.decode(request_data);
    var compares = try allocator.alloc(rqlite.CompareOperation, request.compare.len);
    defer allocator.free(compares);
    for (request.compare, 0..) |compare, i| {
        compares[i] = .{
            .key = compare.key,
            .target = switch (compare.target) {
                .VERSION => .version,
                .CREATE => .create,
                .MOD => .mod,
                .VALUE => .value,
                .LEASE => .lease,
            },
            .result = switch (compare.result) {
                .EQUAL => .equal,
                .GREATER => .greater,
                .LESS => .less,
                .NOT_EQUAL => .not_equal,
            },
            .value = compare.value,
            .target_value = switch (compare.target) {
                .VERSION => compare.version,
                .CREATE => compare.create_revision,
                .MOD => compare.mod_revision,
                .LEASE => compare.lease,
                .VALUE => 0,
            },
        };
    }
    const success = try txnOps(allocator, request.success);
    defer allocator.free(success);
    const failure = try txnOps(allocator, request.failure);
    defer allocator.free(failure);
    const result = try server.storage.txn(.{ .compare = compares, .success = success, .failure = failure });
    const selected = if (result.succeeded) request.success else request.failure;
    var responses = try allocator.alloc(messages.ResponseOp, selected.len);
    defer allocator.free(responses);
    for (selected, 0..) |operation, i| {
        const stored = if (i < result.responses.len) result.responses[i] else null;
        responses[i] = try txnResponse(server, operation, stored);
    }
    defer freeStorageTxnResponses(allocator, result.responses);

    const response = messages.TxnResponse{
        .header = server.buildHeader(),
        .succeeded = result.succeeded,
        .responses = responses,
    };
    const encoded = try response.encode(allocator);
    freeTxnResponses(allocator, responses);
    return encoded;
}

fn freeTxnResponses(allocator: Allocator, responses: []messages.ResponseOp) void {
    for (responses) |response| switch (response.response) {
        .range => |range| {
            // The bytes belong to the storage response, freed separately.
            if (range.kvs.len != 0) allocator.free(range.kvs);
        },
        else => {},
    };
}

fn freeStorageTxnResponses(allocator: Allocator, responses: []mvcc.TxnResponse) void {
    for (responses) |response| {
        if (response.prev_kv) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        }
        for (response.range_kvs) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        }
        allocator.free(response.range_kvs);
    }
    allocator.free(responses);
}

fn txnResponse(server: *PipelineServer, operation: messages.RequestOp, stored: ?mvcc.TxnResponse) !messages.ResponseOp {
    return switch (operation.request) {
        .range => |request| blk: {
            // The rows were captured by the same rqlite request that applied
            // the transaction. Re-reading them here would let a concurrent
            // commit change the answer after the fact, which KIP-29 §6 forbids,
            // and would also ignore this op's keys_only/limit/revision.
            if (stored) |result| {
                const visible_len: usize = if (request.count_only) 0 else if (request.limit > 0) @min(result.range_kvs.len, @as(usize, @intCast(request.limit))) else result.range_kvs.len;
                var kvs = try server.allocator.alloc(messages.KeyValue, visible_len);
                for (result.range_kvs[0..visible_len], 0..) |kv, i| kvs[i] = .{
                    .key = kv.key,
                    .create_revision = kv.create_revision,
                    .mod_revision = kv.mod_revision,
                    .version = kv.version,
                    .value = if (request.keys_only) &[_]u8{} else kv.value,
                    .lease = kv.lease,
                };
                break :blk .{ .response = .{ .range = .{
                    .header = server.buildHeader(),
                    .kvs = kvs,
                    .more = !request.count_only and (result.range_more or result.range_kvs.len > visible_len),
                    .count = result.range_count,
                } } };
            }
            // No captured rows means the branch selected no Range op, so the
            // answer is an empty successful read.
            break :blk .{ .response = .{ .range = .{ .header = server.buildHeader(), .kvs = &.{}, .more = false, .count = 0 } } };
        },
        .put => .{ .response = .{ .put = .{ .header = server.buildHeader(), .prev_kv = if (stored) |result| if (result.prev_kv) |kv| .{ .key = kv.key, .value = kv.value, .create_revision = kv.create_revision, .mod_revision = kv.mod_revision, .version = kv.version, .lease = kv.lease } else .{} else .{} } } },
        .delete_range => .{ .response = .{ .delete_range = .{ .header = server.buildHeader(), .deleted = if (stored) |result| result.affected else 0 } } },
        .txn => return error.NestedTransactionUnsupported,
    };
}

fn txnOps(allocator: Allocator, requests: []const messages.RequestOp) ![]rqlite.TxnOp {
    var result = try allocator.alloc(rqlite.TxnOp, requests.len);
    for (requests, 0..) |request, i| result[i] = switch (request.request) {
        .range => |op| .{ .kind = .range, .key = op.key, .value = &[_]u8{}, .range_end = op.range_end, .lease = 0 },
        .put => |op| .{ .kind = .put, .key = op.key, .value = op.value, .range_end = &[_]u8{}, .lease = op.lease },
        .delete_range => |op| .{ .kind = .delete, .key = op.key, .value = &[_]u8{}, .range_end = op.range_end, .lease = 0 },
        .txn => return error.NestedTransactionUnsupported,
    };
    return result;
}

fn handleKVCompact(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.CompactionRequest.decode(request_data);
    try server.storage.compact(request.revision);

    const response = messages.CompactionResponse{
        .header = server.buildHeader(),
    };

    return response.encode(allocator);
}

// ─── Watch Service Handlers ──────────────────────────────────────────────────

fn handleWatch(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.WatchRequest.decode(request_data);

    var created = false;
    var watch_id: i64 = 0;
    var canceled = false;

    switch (request) {
        .create => |create| {
            // Refuse a start_revision that has already been compacted, before
            // registering anything. The history is gone at that point, so the
            // watch would be created, would never fire, and the caller would
            // see a successful create. KIP-29 requires an explicit Compacted
            // error here rather than an empty stream. A start_revision of 0
            // means "from now" and is never compacted.
            if (create.start_revision > 0 and create.start_revision <= server.storage.compact_revision) {
                std.heap.page_allocator.free(create.key);
                std.heap.page_allocator.free(create.range_end);
                return error.Compacted;
            }
            watch_id = try server.watch_registry.createWatch(create.key, create.range_end, create.start_revision);
            created = true;
            std.heap.page_allocator.free(create.key);
            std.heap.page_allocator.free(create.range_end);
        },
        .cancel => |cancel| {
            watch_id = cancel.watch_id;
            canceled = server.watch_registry.cancelWatch(cancel.watch_id);
        },
        .progress => {},
    }

    const response = messages.WatchResponse{
        .header = server.buildHeader(),
        .watch_id = watch_id,
        .created = created,
        .canceled = canceled,
        .compact_revision = 0,
        .cancel_reason = &[_]u8{},
        .fragment = false,
        .events = &[_]messages.Event{},
    };

    return response.encode(allocator);
}

// ─── Lease Service Handlers ──────────────────────────────────────────────────

fn handleLeaseGrant(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.LeaseGrantRequest.decode(request_data);
    const id = if (request.id != 0) blk: {
        try server.storage.grantLease(request.id, request.ttl);
        break :blk request.id;
    } else try server.storage.allocateLease(request.ttl);

    const response = messages.LeaseGrantResponse{
        .header = server.buildHeader(),
        .id = id,
        .ttl = request.ttl,
    };

    return response.encode(allocator);
}

fn handleLeaseRevoke(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.LeaseRevokeRequest.decode(request_data);
    try server.storage.revokeLease(request.id);

    const response = messages.LeaseRevokeResponse{
        .header = server.buildHeader(),
    };

    return response.encode(allocator);
}

fn handleLeaseKeepAlive(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.LeaseKeepAliveRequest.decode(request_data);
    try server.storage.keepAliveLease(request.id);

    const response = messages.LeaseKeepAliveResponse{
        .header = server.buildHeader(),
        .id = request.id,
        .ttl = 60,
    };

    return response.encode(allocator);
}

fn handleLeaseTimeToLive(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    const request = try messages.LeaseTimeToLiveRequest.decode(request_data);

    const manager = server.storage.leaseManager();
    const ttl = manager.leaseTTL(request.id);

    // etcd returns the attached keys only when the request asks for them.
    var keys = std.ArrayList(u8).empty;
    if (request.keys) {
        const attached = try manager.leaseKeys(request.id);
        defer allocator.free(attached);
        for (attached) |key| {
            if (keys.items.len != 0) try keys.appendSlice(allocator, "\x00");
            try keys.appendSlice(allocator, key);
            allocator.free(key);
        }
    }
    defer keys.deinit(allocator);

    const response = messages.LeaseTimeToLiveResponse{
        .header = server.buildHeader(),
        .id = request.id,
        .ttl = ttl.ttl,
        .granted_ttl = ttl.granted_ttl,
        .keys = try keys.toOwnedSlice(allocator),
    };
    return response.encode(allocator);
}

fn handleLeaseLeases(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const live = try server.storage.leaseManager().allLeases();
    const statuses = try allocator.alloc(messages.LeaseStatus, live.len);
    for (live, 0..) |lease, i| statuses[i] = .{ .id = lease.id };

    const response = messages.LeaseLeasesResponse{
        .header = server.buildHeader(),
        .leases = statuses,
    };
    const encoded = try response.encode(allocator);
    allocator.free(statuses);
    return encoded;
}

// ─── Cluster Service Handlers ────────────────────────────────────────────────

fn handleMemberList(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.MemberListResponse{
        .header = server.buildHeader(),
        .members = &[_]messages.Member{},
    };

    return response.encode(allocator);
}

fn handleMemberAdd(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.MemberAddResponse{
        .header = server.buildHeader(),
        .member = .{},
        .members = &[_]messages.Member{},
    };

    return encodeMemberAddResponse(response, allocator);
}

fn handleMemberRemove(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.MemberRemoveResponse{
        .header = server.buildHeader(),
        .members = &[_]messages.Member{},
    };

    return encodeMemberRemoveResponse(response, allocator);
}

fn handleMemberUpdate(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.MemberUpdateResponse{
        .header = server.buildHeader(),
        .members = &[_]messages.Member{},
    };

    return encodeMemberUpdateResponse(response, allocator);
}

fn handleMemberPromote(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.MemberUpdateResponse{
        .header = server.buildHeader(),
        .members = &[_]messages.Member{},
    };

    return encodeMemberUpdateResponse(response, allocator);
}

fn encodeMemberAddResponse(response: messages.MemberAddResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    const member_bytes = try response.member.encode(allocator);
    defer allocator.free(member_bytes);
    try w.msg(2, member_bytes);
    for (response.members) |m| {
        const m_bytes = try m.encode(allocator);
        defer allocator.free(m_bytes);
        try w.msg(3, m_bytes);
    }
    return w.buf.toOwnedSlice(allocator);
}

fn encodeMemberRemoveResponse(response: messages.MemberRemoveResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    for (response.members) |m| {
        const m_bytes = try m.encode(allocator);
        defer allocator.free(m_bytes);
        try w.msg(2, m_bytes);
    }
    return w.buf.toOwnedSlice(allocator);
}

fn encodeMemberUpdateResponse(response: messages.MemberUpdateResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    for (response.members) |m| {
        const m_bytes = try m.encode(allocator);
        defer allocator.free(m_bytes);
        try w.msg(2, m_bytes);
    }
    return w.buf.toOwnedSlice(allocator);
}

// ─── Maintenance Service Handlers ─────────────────────────────────────────────

fn handleStatus(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    // DbSize feeds the apiserver's `etcd_object_counts`/monitor metric, so a
    // constant zero understates the datastore forever. rqlite reports the
    // on-disk size; without a live client (unit tests) the value stays zero
    // rather than pretending to know a size.
    var db_size: i64 = 0;
    var db_size_in_use: i64 = 0;
    if (server.storage.client) |*client| {
        // A size query that rqlite rejects is reported as an unknown size, not
        // as a failure of the whole Status call, which the apiserver uses for
        // its feature probe.
        if (client.query("SELECT page_count*page_size AS bytes FROM pragma_page_count(), pragma_page_size()")) |result| {
            defer result.deinit(allocator);
            if (result.values.len == 1 and result.values[0].len == 1) {
                db_size = result.values[0][0].integer;
                db_size_in_use = db_size;
            }
        } else |_| {}
    }

    const response = messages.StatusResponse{
        .header = server.buildHeader(),
        .version = etcdVersion,
        .db_size = db_size,
        .leader = 1,
        .raft_index = 1,
        .raft_term = @intCast(server.raft_term),
        .raft_applied_index = 1,
        .errors = &[_][]const u8{},
        .db_size_in_use = db_size_in_use,
        .is_learner = false,
    };

    return response.encode(allocator);
}

fn handleDefragment(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.DefragmentResponse{
        .header = server.buildHeader(),
    };

    return response.encode(allocator);
}

fn handleHash(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.HashResponse{
        .header = server.buildHeader(),
        .hash = 0,
    };

    return encodeHashResponse(response, allocator);
}

fn handleHashKV(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.HashKVResponse{
        .header = server.buildHeader(),
        .hash = 0,
        .compact_revision = 0,
    };

    return encodeHashKVResponse(response, allocator);
}

fn handleSnapshot(_: *PipelineServer, _: []const u8) ![]u8 {
    // An empty body here would be a silent success: the apiserver would treat
    // the response as a valid zero-length snapshot and record one it can never
    // restore from. KIP-29 requires an unsupported call to say so.
    return error.SnapshotNotImplemented;
}

fn handleMoveLeader(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;

    const response = messages.MoveLeaderResponse{
        .header = server.buildHeader(),
    };

    return encodeMoveLeaderResponse(response, allocator);
}

fn handleAlarm(server: *PipelineServer, request_data: []const u8) ![]u8 {
    const allocator = server.allocator;
    _ = try messages.AlarmRequest.decode(request_data);

    const response = messages.AlarmResponse{
        .header = server.buildHeader(),
        .alarms = &[_]messages.AlarmMember{},
    };

    return response.encode(allocator);
}

fn encodeHashResponse(response: messages.HashResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    try w.fixed32(2, response.hash);
    return w.buf.toOwnedSlice(allocator);
}

fn encodeHashKVResponse(response: messages.HashKVResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    try w.fixed32(2, response.hash);
    try w.v(3, @as(u64, @bitCast(response.compact_revision)));
    return w.buf.toOwnedSlice(allocator);
}

fn encodeMoveLeaderResponse(response: messages.MoveLeaderResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    return w.buf.toOwnedSlice(allocator);
}

// ─── Auth Service Handlers ───────────────────────────────────────────────────

fn handleAuthEnable(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;
    const response = messages.AuthEnableResponse{ .header = server.buildHeader() };
    return encodeAuthResponse(response.header, allocator);
}

fn handleAuthDisable(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;
    const response = messages.AuthDisableResponse{ .header = server.buildHeader() };
    return encodeAuthResponse(response.header, allocator);
}

fn handleAuthStatus(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;
    const response = messages.AuthStatusResponse{
        .header = server.buildHeader(),
        .enabled = false,
        .auth_revision = 0,
    };
    return encodeAuthStatusResponse(response, allocator);
}

fn handleAuthenticate(server: *PipelineServer, _: []const u8) ![]u8 {
    const allocator = server.allocator;
    const response = messages.AuthenticateResponse{
        .header = server.buildHeader(),
        .token = "",
    };
    return encodeAuthenticateResponse(response, allocator);
}

fn handleUserAdd(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleUserGet(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleUserList(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleUserDelete(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleUserChangePassword(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleUserGrantRole(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleUserRevokeRole(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleRoleAdd(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleRoleGet(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleRoleList(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleRoleDelete(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleRoleGrantPermission(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}
fn handleRoleRevokePermission(server: *PipelineServer, _: []const u8) ![]u8 {
    return handleAuthEnable(server, &[_]u8{});
}

fn encodeAuthResponse(header: messages.ResponseHeader, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    return w.buf.toOwnedSlice(allocator);
}

fn encodeAuthStatusResponse(response: messages.AuthStatusResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    try w.b(2, response.enabled);
    try w.v(3, response.auth_revision);
    return w.buf.toOwnedSlice(allocator);
}

fn encodeAuthenticateResponse(response: messages.AuthenticateResponse, allocator: Allocator) ![]u8 {
    var w = wire.Writer.init(allocator);
    const header_bytes = try response.header.encode(allocator);
    defer allocator.free(header_bytes);
    try w.msg(1, header_bytes);
    try w.bytes(2, response.token);
    return w.buf.toOwnedSlice(allocator);
}

// ─── Tests ───────────────────────────────────────────────────────────────────

const testing = std.testing;

test "pipeline server init" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    const header = server.buildHeader();
    try testing.expectEqual(@as(i64, 0), header.revision);
    try testing.expectEqual(@as(u64, 1), header.raft_term);
}

test "Range pagination exposes more and total count on the wire" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    _ = try server.storage.put("page/b", "second", 0, false);
    _ = try server.storage.put("page/a", "first", 0, false);
    var w = wire.Writer.init(testing.allocator);
    defer w.buf.deinit(testing.allocator);
    try w.bytes(1, "page/");
    try w.bytes(2, "page0");
    try w.v(3, 1);
    const data = try processRequest(&server, "/etcdserverpb.KV/Range", w.buf.items);
    defer testing.allocator.free(data);
    var r = wire.Reader.init(data);
    var more = false;
    var count: u64 = 0;
    var returned: usize = 0;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            2 => {
                var kv = try messages.KeyValue.decode(try r.readBytes(), testing.allocator);
                defer kv.deinit(testing.allocator);
                try testing.expectEqualStrings("page/a", kv.key);
                returned += 1;
            },
            3 => more = (try r.readU64()) != 0,
            4 => count = try r.readU64(),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(more);
    try testing.expectEqual(@as(u64, 2), count);
    try testing.expectEqual(@as(usize, 1), returned);
    const counted = try server.storage.range("page/", "page0", 1, 0, false, true);
    defer testing.allocator.free(counted.kvs);
    try testing.expectEqual(@as(i64, 2), counted.count);
    try testing.expectEqual(@as(usize, 0), counted.kvs.len);
    try testing.expect(!counted.more);
}

test "transaction range applies limit and keeps the full count" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    const rows = [_]mvcc.MVCCKeyValue{
        .{ .key = "a", .value = "one", .create_revision = 1, .mod_revision = 1, .version = 1, .lease = 0 },
        .{ .key = "b", .value = "two", .create_revision = 1, .mod_revision = 1, .version = 1, .lease = 0 },
    };
    const response = try txnResponse(&server, .{ .request = .{ .range = .{ .key = "a", .range_end = "c", .limit = 1 } } }, .{ .kind = .range, .data = "", .range_kvs = @constCast(&rows), .range_count = 2 });
    defer testing.allocator.free(response.response.range.kvs);
    const range = response.response.range;
    try testing.expectEqual(@as(usize, 1), range.kvs.len);
    try testing.expectEqualStrings("a", range.kvs[0].key);
    try testing.expectEqual(@as(i64, 2), range.count);
    try testing.expect(range.more);
}

test "watch cursor publishes commits made outside the request path once" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    const Capture = struct {
        count: usize = 0,
        fn publish(context: *anyopaque, events: []const messages.Event) !void {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.count += events.len;
        }
    };
    var capture = Capture{};
    server.setWatchPublisher(&capture, Capture.publish);
    _ = try server.storage.put("remote", "value", 0, false);
    try server.publishCommittedSinceCursor();
    try server.publishCommittedSinceCursor();
    try testing.expectEqual(@as(usize, 1), capture.count);
}

test "pipeline server revision increment" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    const rev1 = server.nextRevision();
    try testing.expectEqual(@as(i64, 1), rev1);
    const rev2 = server.nextRevision();
    try testing.expectEqual(@as(i64, 2), rev2);
}

test "pipeline routes unknown service to error" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    const result = processRequest(&server, "/unknown.Service/Method", &[_]u8{});
    try testing.expectError(error.UnimplementedMethod, result);
}

test "an unsupported maintenance call reports an error instead of an empty body" {
    // KIP-29 forbids an empty success for a call the layer does not implement:
    // an empty Snapshot body decodes as a valid zero-length snapshot, so the
    // apiserver would record a snapshot it can never restore.
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    try testing.expectError(error.SnapshotNotImplemented, processRequest(&server, "/etcdserverpb.Maintenance/Snapshot", &[_]u8{}));
}

test "status reports a parseable version for the watch-progress feature gate" {
    // Kubernetes reads the version from Maintenance.Status to decide whether the
    // endpoint supports watch progress notifications (>= 3.4.31 / 3.5.13), so
    // it has to be a real, parseable semantic version rather than a placeholder.
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();
    const response = try processRequest(&server, "/etcdserverpb.Maintenance/Status", &[_]u8{});
    defer server.allocator.free(response);
    var iter = std.mem.splitScalar(u8, etcdVersion, '.');
    const major = iter.next().?;
    const minor = iter.next().?;
    const patch = iter.next().?;
    try testing.expectEqual(@as(i64, 3), try std.fmt.parseInt(i64, major, 10));
    const minor_value = try std.fmt.parseInt(i64, minor, 10);
    const patch_value = try std.fmt.parseInt(i64, patch, 10);
    // Above the 3.4.31 / 3.5.13 thresholds the apiserver compares against.
    try testing.expect(minor_value > 5 or (minor_value == 5 and patch_value > 0) or minor_value == 4 and patch_value > 31);
    // The encoded response actually carries the version.
    try testing.expect(std.mem.indexOf(u8, response, etcdVersion) != null);
}

// ─── End-to-End Pipeline Tests ──────────────────────────────────────────────

test "e2e: KV Range request through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    var w = wire.Writer.init(testing.allocator);
    try w.bytes(1, "test");
    try w.v(3, 100);
    const request_data = try w.buf.toOwnedSlice(testing.allocator);
    defer testing.allocator.free(request_data);

    const response_data = try processRequest(&server, "/etcdserverpb.KV/Range", request_data);
    defer testing.allocator.free(response_data);

    // Decode the RangeResponse
    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                const header_bytes = try r.readBytes();
                const header = try messages.ResponseHeader.decode(header_bytes);
                try testing.expectEqual(@as(i64, 0), header.revision);
                try testing.expectEqual(@as(u64, 1), header.raft_term);
                header_decoded = true;
            },
            2, 3, 4 => try r.skip(tag.wire_type),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
}

test "e2e: KV Put request through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    const Capture = struct {
        count: usize = 0,
        event_type: messages.EventType = .DELETE,
        key: [16]u8 = undefined,
        key_len: usize = 0,

        fn publish(context: *anyopaque, events: []const messages.Event) !void {
            const self: *@This() = @ptrCast(@alignCast(context));
            self.count += events.len;
            const event = events[0];
            self.event_type = event.type;
            self.key_len = @min(event.kv.key.len, self.key.len);
            @memcpy(self.key[0..self.key_len], event.kv.key[0..self.key_len]);
        }
    };
    var capture = Capture{};
    server.setWatchPublisher(&capture, Capture.publish);

    var w = wire.Writer.init(testing.allocator);
    try w.bytes(1, "mykey");
    try w.bytes(2, "myvalue");
    try w.v(3, 0);
    try w.b(4, false);
    const request_data = try w.buf.toOwnedSlice(testing.allocator);
    defer testing.allocator.free(request_data);

    const response_data = try processRequest(&server, "/etcdserverpb.KV/Put", request_data);
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                const header_bytes = try r.readBytes();
                const header = try messages.ResponseHeader.decode(header_bytes);
                try testing.expectEqual(@as(i64, 1), header.revision);
                try testing.expectEqual(@as(u64, 1), header.raft_term);
                header_decoded = true;
            },
            2 => try r.skip(tag.wire_type),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
    try testing.expectEqual(@as(usize, 1), capture.count);
    try testing.expectEqual(messages.EventType.PUT, capture.event_type);
    try testing.expectEqualStrings("mykey", capture.key[0..capture.key_len]);
}

test "txn uses shared mvcc state for put and delete" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    _ = try server.storage.put("txn-key", "before", 0, false);
    const result = try server.storage.txn(.{
        .compare = &[_]rqlite.CompareOperation{},
        .success = &[_]rqlite.TxnOp{.{ .kind = .delete, .key = "txn-key", .value = "", .range_end = "txn-key~", .lease = 0 }},
        .failure = &[_]rqlite.TxnOp{},
    });
    defer freeStorageTxnResponses(testing.allocator, result.responses);
    try testing.expect(result.succeeded);
    const after = try server.storage.range("txn-key", "txn-key~", 0, 0, false, false);
    defer {
        for (after.kvs) |kv| {
            testing.allocator.free(kv.key);
            testing.allocator.free(kv.value);
        }
        testing.allocator.free(after.kvs);
    }
    try testing.expectEqual(@as(usize, 0), after.kvs.len);
}

test "e2e: KV DeleteRange request through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    var w = wire.Writer.init(testing.allocator);
    try w.bytes(1, "mykey");
    const request_data = try w.buf.toOwnedSlice(testing.allocator);
    defer testing.allocator.free(request_data);

    const response_data = try processRequest(&server, "/etcdserverpb.KV/DeleteRange", request_data);
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                _ = try r.readBytes();
                header_decoded = true;
            },
            2 => try r.skip(tag.wire_type),
            3 => try r.skip(tag.wire_type),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
}

test "e2e: KV Txn request through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    var w = wire.Writer.init(testing.allocator);
    const request_data = try w.buf.toOwnedSlice(testing.allocator);
    defer testing.allocator.free(request_data);

    const response_data = try processRequest(&server, "/etcdserverpb.KV/Txn", request_data);
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    var succeeded_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                _ = try r.readBytes();
                header_decoded = true;
            },
            2 => {
                const succeeded = try r.readBool();
                try testing.expectEqual(true, succeeded);
                succeeded_decoded = true;
            },
            3 => try r.skip(tag.wire_type),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
    try testing.expect(succeeded_decoded);
}

test "e2e: Lease Grant request through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    var w = wire.Writer.init(testing.allocator);
    try w.v(1, 60);
    const request_data = try w.buf.toOwnedSlice(testing.allocator);
    defer testing.allocator.free(request_data);

    const response_data = try processRequest(&server, "/etcdserverpb.Lease/LeaseGrant", request_data);
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    var id_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                _ = try r.readBytes();
                header_decoded = true;
            },
            2 => {
                const id = try r.readU64();
                try testing.expect(id > 0);
                id_decoded = true;
            },
            3 => try r.skip(tag.wire_type),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
    try testing.expect(id_decoded);
}

test "e2e: Cluster MemberList through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    const response_data = try processRequest(&server, "/etcdserverpb.Cluster/MemberList", &[_]u8{});
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                _ = try r.readBytes();
                header_decoded = true;
            },
            2 => try r.skip(tag.wire_type),
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
}

test "e2e: Maintenance Status through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    const response_data = try processRequest(&server, "/etcdserverpb.Maintenance/Status", &[_]u8{});
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    var version_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                _ = try r.readBytes();
                header_decoded = true;
            },
            2 => {
                const version = try r.readBytes();
                try testing.expectEqualStrings("3.6.0", version);
                version_decoded = true;
            },
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
    try testing.expect(version_decoded);
}

test "e2e: Auth AuthStatus through pipeline" {
    var server = PipelineServer.initMemory(testing.allocator);
    defer server.deinit();

    const response_data = try processRequest(&server, "/etcdserverpb.Auth/AuthStatus", &[_]u8{});
    defer testing.allocator.free(response_data);

    var r = wire.Reader.init(response_data);
    var header_decoded = false;
    while (r.hasMore()) {
        const tag = try r.nextTag();
        switch (tag.field) {
            1 => {
                _ = try r.readBytes();
                header_decoded = true;
            },
            else => try r.skip(tag.wire_type),
        }
    }
    try testing.expect(header_decoded);
}
