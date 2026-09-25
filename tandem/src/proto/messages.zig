// etcd v3 protobuf message types
// Based on etcdserverpb/rpc.proto and mvccpb/kv.proto
// Field numbers match the official etcd proto definitions exactly.

const std = @import("std");
test "Watch decodes packed and unpacked filters and nested cancellation" {
    const packed_req = try WatchCreateRequest.decode("\x2a\x02\x00\x01");
    try std.testing.expect(packed_req.no_put and packed_req.no_delete);
    const unpacked_req = try WatchCreateRequest.decode("\x28\x00\x28\x01");
    try std.testing.expect(unpacked_req.no_put and unpacked_req.no_delete);
    const cancellation = try WatchRequest.decode("\x12\x02\x08\x2a");
    try std.testing.expectEqual(@as(i64, 42), cancellation.cancel.watch_id);
}

test "Compare value uses official etcd field seven" {
    const compare = try Compare.decode("\x10\x03\x1a\x01a\x3a\x03old");
    defer std.heap.page_allocator.free(compare.key);
    defer std.heap.page_allocator.free(compare.value);
    try std.testing.expectEqualStrings("old", compare.value);
    try std.testing.expectEqual(CompareTarget.VALUE, compare.target);
}
const wire = @import("wire.zig");
const Allocator = std.mem.Allocator;

// ─── Enum types ─────────────────────────────────────────────────────────────

/// etcd RangeRequest.SortOrder
pub const SortOrder = enum(i32) {
    NONE = 0, // default, no sorting
    ASCEND = 1, // lowest target value first
    DESCEND = 2, // highest target value first
};

/// etcd RangeRequest.SortTarget
pub const SortTarget = enum(i32) {
    KEY = 0,
    VERSION = 1,
    CREATE = 2,
    MOD = 3,
    VALUE = 4,
};

/// etcd Compare.CompareResult
pub const CompareResult = enum(i32) {
    EQUAL = 0,
    GREATER = 1,
    LESS = 2,
    NOT_EQUAL = 3,
};

/// etcd Compare.CompareTarget
pub const CompareTarget = enum(i32) {
    VERSION = 0,
    CREATE = 1,
    MOD = 2,
    VALUE = 3,
    LEASE = 4,
};

/// etcd WatchEvent.EventType
pub const EventType = enum(i32) {
    PUT = 0,
    DELETE = 1,
};

/// etcd AlarmRequest.AlarmAction
pub const AlarmAction = enum(i32) {
    GET = 0,
    ACTIVATE = 1,
    DEACTIVATE = 2,
};

/// etcd AlarmType
pub const AlarmType = enum(i32) {
    NONE = 0,
    NOSPACE = 1,
    CORRUPT = 2,
};

// ─── KV messages ─────────────────────────────────────────────────────────────

/// mvccpb.KeyValue
pub const KeyValue = struct {
    key: []const u8 = &[_]u8{},
    create_revision: i64 = 0,
    mod_revision: i64 = 0,
    version: i64 = 0,
    value: []const u8 = &[_]u8{},
    lease: i64 = 0,

    pub fn encode(self: KeyValue, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        try w.bytes(1, self.key);
        try w.v(2, @as(u64, @bitCast(self.create_revision)));
        try w.v(3, @as(u64, @bitCast(self.mod_revision)));
        try w.v(4, @as(u64, @bitCast(self.version)));
        try w.bytes(5, self.value);
        try w.v(6, @as(u64, @bitCast(self.lease)));
        return w.buf.toOwnedSlice(allocator);
    }

    pub fn decode(data: []const u8, allocator: Allocator) !KeyValue {
        var kv: KeyValue = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => kv.key = try allocator.dupe(u8, try r.readBytes()),
                2 => kv.create_revision = @as(i64, @intCast(try r.readU64())),
                3 => kv.mod_revision = @as(i64, @intCast(try r.readU64())),
                4 => kv.version = @as(i64, @intCast(try r.readU64())),
                5 => kv.value = try allocator.dupe(u8, try r.readBytes()),
                6 => kv.lease = @as(i64, @intCast(try r.readU64())),
                else => try r.skip(tag.wire_type),
            }
        }
        return kv;
    }

    pub fn deinit(self: *KeyValue, allocator: Allocator) void {
        allocator.free(self.key);
        allocator.free(self.value);
    }
};

/// mvccpb.Event
pub const Event = struct {
    type: EventType = .PUT,
    kv: KeyValue = .{},
    prev_kv: KeyValue = .{},

    pub fn encode(self: Event, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        try w.v(1, @bitCast(@as(i64, @intFromEnum(self.type))));
        const kv_bytes = try self.kv.encode(allocator);
        defer allocator.free(kv_bytes);
        try w.msg(2, kv_bytes);
        if (self.prev_kv.key.len > 0) {
            const prev_bytes = try self.prev_kv.encode(allocator);
            defer allocator.free(prev_bytes);
            try w.msg(3, prev_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }

    pub fn decode(data: []const u8, allocator: Allocator) !Event {
        var evt: Event = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => evt.type = @as(EventType, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                2 => evt.kv = try KeyValue.decode(try r.readBytes(), allocator),
                3 => evt.prev_kv = try KeyValue.decode(try r.readBytes(), allocator),
                else => try r.skip(tag.wire_type),
            }
        }
        return evt;
    }

    pub fn deinit(self: *Event, allocator: Allocator) void {
        self.kv.deinit(allocator);
        self.prev_kv.deinit(allocator);
    }
};

// ─── KV Request/Response messages ───────────────────────────────────────────

/// etcdserverpb.RangeRequest
pub const RangeRequest = struct {
    key: []const u8 = &[_]u8{},
    range_end: []const u8 = &[_]u8{},
    limit: i64 = 0,
    revision: i64 = 0,
    sort_order: SortOrder = .NONE,
    sort_target: SortTarget = .KEY,
    serializable: bool = false,
    keys_only: bool = false,
    count_only: bool = false,
    min_mod_revision: i64 = 0,
    max_mod_revision: i64 = 0,
    min_create_revision: i64 = 0,
    max_create_revision: i64 = 0,

    pub fn decode(data: []const u8) !RangeRequest {
        var req: RangeRequest = .{};

        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.key = try readBytesCopy(&r),
                2 => req.range_end = try readBytesCopy(&r),
                3 => req.limit = @as(i64, @intCast(try r.readU64())),
                4 => req.revision = @as(i64, @intCast(try r.readU64())),
                5 => req.sort_order = @as(SortOrder, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                6 => req.sort_target = @as(SortTarget, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                7 => req.serializable = try r.readBool(),
                8 => req.keys_only = try r.readBool(),
                9 => req.count_only = try r.readBool(),
                10 => req.min_mod_revision = @as(i64, @intCast(try r.readU64())),
                11 => req.max_mod_revision = @as(i64, @intCast(try r.readU64())),
                12 => req.min_create_revision = @as(i64, @intCast(try r.readU64())),
                13 => req.max_create_revision = @as(i64, @intCast(try r.readU64())),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

/// etcdserverpb.RangeResponse
pub const RangeResponse = struct {
    header: ResponseHeader = .{},
    kvs: []KeyValue = &[_]KeyValue{},
    more: bool = false,
    count: i64 = 0,

    pub fn encode(self: RangeResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        for (self.kvs) |kv| {
            const kv_bytes = try kv.encode(allocator);
            defer allocator.free(kv_bytes);
            try w.msg(2, kv_bytes);
        }
        try w.b(3, self.more);
        try w.v(4, @as(u64, @bitCast(self.count)));
        return w.buf.toOwnedSlice(allocator);
    }
};

/// etcdserverpb.PutRequest
pub const PutRequest = struct {
    key: []const u8 = &[_]u8{},
    value: []const u8 = &[_]u8{},
    lease: i64 = 0,
    prev_kv: bool = false,
    ignore_value: bool = false,
    ignore_lease: bool = false,

    pub fn decode(data: []const u8) !PutRequest {
        var req: PutRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.key = try readBytesCopy(&r),
                2 => req.value = try readBytesCopy(&r),
                3 => req.lease = @as(i64, @intCast(try r.readU64())),
                4 => req.prev_kv = try r.readBool(),
                5 => req.ignore_value = try r.readBool(),
                6 => req.ignore_lease = try r.readBool(),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

/// etcdserverpb.PutResponse
pub const PutResponse = struct {
    header: ResponseHeader = .{},
    prev_kv: KeyValue = .{},

    pub fn encode(self: PutResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        if (self.prev_kv.key.len > 0) {
            const kv_bytes = try self.prev_kv.encode(allocator);
            defer allocator.free(kv_bytes);
            try w.msg(2, kv_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

/// etcdserverpb.DeleteRangeRequest
pub const DeleteRangeRequest = struct {
    key: []const u8 = &[_]u8{},
    range_end: []const u8 = &[_]u8{},
    prev_kv: bool = false,

    pub fn decode(data: []const u8) !DeleteRangeRequest {
        var req: DeleteRangeRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.key = try readBytesCopy(&r),
                2 => req.range_end = try readBytesCopy(&r),
                3 => req.prev_kv = try r.readBool(),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

/// etcdserverpb.DeleteRangeResponse
pub const DeleteRangeResponse = struct {
    header: ResponseHeader = .{},
    deleted: i64 = 0,
    prev_kvs: []KeyValue = &[_]KeyValue{},

    pub fn encode(self: DeleteRangeResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.v(2, @as(u64, @bitCast(self.deleted)));
        for (self.prev_kvs) |kv| {
            const kv_bytes = try kv.encode(allocator);
            defer allocator.free(kv_bytes);
            try w.msg(3, kv_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

/// etcdserverpb.ResponseHeader
pub const ResponseHeader = struct {
    cluster_id: u64 = 0,
    member_id: u64 = 0,
    revision: i64 = 0,
    raft_term: u64 = 0,

    pub fn encode(self: ResponseHeader, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        try w.v(1, self.cluster_id);
        try w.v(2, self.member_id);
        try w.v(3, @as(u64, @bitCast(self.revision)));
        try w.v(4, self.raft_term);
        return w.buf.toOwnedSlice(allocator);
    }

    pub fn decode(data: []const u8) !ResponseHeader {
        var h: ResponseHeader = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => h.cluster_id = try r.readU64(),
                2 => h.member_id = try r.readU64(),
                3 => h.revision = @as(i64, @intCast(try r.readU64())),
                4 => h.raft_term = try r.readU64(),
                else => try r.skip(tag.wire_type),
            }
        }
        return h;
    }
};

// ─── Txn messages ────────────────────────────────────────────────────────────

/// etcdserverpb.Compare
pub const Compare = struct {
    result: CompareResult = .EQUAL,
    target: CompareTarget = .VERSION,
    key: []const u8 = &[_]u8{},
    range_end: []const u8 = &[_]u8{},

    // Union: exactly one of these is set
    version: i64 = 0,
    create_revision: i64 = 0,
    mod_revision: i64 = 0,
    value: []const u8 = &[_]u8{},
    lease: i64 = 0,

    pub fn decode(data: []const u8) !Compare {
        var cmp: Compare = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => cmp.result = @as(CompareResult, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                2 => cmp.target = @as(CompareTarget, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                3 => cmp.key = try readBytesCopy(&r),
                4 => cmp.version = @bitCast(try r.readU64()),
                5 => cmp.create_revision = @bitCast(try r.readU64()),
                6 => cmp.mod_revision = @bitCast(try r.readU64()),
                7 => cmp.value = try readBytesCopy(&r),
                8 => cmp.lease = @bitCast(try r.readU64()),
                64 => cmp.range_end = try readBytesCopy(&r),
                else => try r.skip(tag.wire_type),
            }
        }
        return cmp;
    }
};

/// etcdserverpb.RequestOp (oneof request)
pub const RequestOp = struct {
    pub const Request = union(enum) {
        range: RangeRequest,
        put: PutRequest,
        delete_range: DeleteRangeRequest,
        txn: TxnRequest,
    };

    request: Request = .{ .txn = .{} },

    // Decode helper: returns which variant and the decoded data
    pub fn decode(data: []const u8, allocator: Allocator) anyerror!Request {
        _ = allocator;
        var range_req: RangeRequest = .{};
        var put_req: PutRequest = .{};
        var delete_req: DeleteRangeRequest = .{};
        var txn_req: TxnRequest = .{};

        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => range_req = try RangeRequest.decode(try r.readBytes()),
                2 => put_req = try PutRequest.decode(try r.readBytes()),
                3 => delete_req = try DeleteRangeRequest.decode(try r.readBytes()),
                4 => txn_req = try TxnRequest.decode(try r.readBytes()),
                else => try r.skip(tag.wire_type),
            }
        }

        // Determine which one was set
        if (range_req.key.len > 0 or range_req.range_end.len > 0) return .{ .range = range_req };
        if (put_req.key.len > 0) return .{ .put = put_req };
        if (delete_req.key.len > 0) return .{ .delete_range = delete_req };
        return .{ .txn = txn_req };
    }
};

/// etcdserverpb.TxnRequest
pub const TxnRequest = struct {
    compare: []Compare = &[_]Compare{},
    success: []RequestOp = &[_]RequestOp{},
    failure: []RequestOp = &[_]RequestOp{},

    pub fn decode(data: []const u8) anyerror!TxnRequest {
        var compares = std.ArrayList(Compare).empty;
        var success = std.ArrayList(RequestOp).empty;
        var failure = std.ArrayList(RequestOp).empty;
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => try compares.append(std.heap.page_allocator, try Compare.decode(try r.readBytes())),
                2 => try success.append(std.heap.page_allocator, .{ .request = try RequestOp.decode(try r.readBytes(), std.heap.page_allocator) }),
                3 => try failure.append(std.heap.page_allocator, .{ .request = try RequestOp.decode(try r.readBytes(), std.heap.page_allocator) }),
                else => try r.skip(tag.wire_type),
            }
        }
        return .{
            .compare = try compares.toOwnedSlice(std.heap.page_allocator),
            .success = try success.toOwnedSlice(std.heap.page_allocator),
            .failure = try failure.toOwnedSlice(std.heap.page_allocator),
        };
    }
};

/// etcdserverpb.TxnResponse
pub const TxnResponse = struct {
    header: ResponseHeader = .{},
    succeeded: bool = false,
    responses: []ResponseOp = &[_]ResponseOp{},

    pub fn encode(self: TxnResponse, allocator: Allocator) Allocator.Error![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.b(2, self.succeeded);
        for (self.responses) |resp| {
            const resp_bytes = try resp.encode(allocator);
            defer allocator.free(resp_bytes);
            try w.msg(3, resp_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

/// etcdserverpb.ResponseOp
pub const ResponseOp = struct {
    pub const Response = union(enum) {
        range: RangeResponse,
        put: PutResponse,
        delete_range: DeleteRangeResponse,
        txn: TxnResponse,
    };

    response: Response = .{ .range = .{} },

    pub fn encode(self: ResponseOp, allocator: Allocator) Allocator.Error![]u8 {
        var w = wire.Writer.init(allocator);
        switch (self.response) {
            .range => |r| {
                const bytes = try r.encode(allocator);
                defer allocator.free(bytes);
                try w.msg(1, bytes);
            },
            .put => |p| {
                const bytes = try p.encode(allocator);
                defer allocator.free(bytes);
                try w.msg(2, bytes);
            },
            .delete_range => |d| {
                const bytes = try d.encode(allocator);
                defer allocator.free(bytes);
                try w.msg(3, bytes);
            },
            .txn => |t| {
                const bytes = try t.encode(allocator);
                defer allocator.free(bytes);
                try w.msg(4, bytes);
            },
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

/// etcdserverpb.CompactionRequest
pub const CompactionRequest = struct {
    revision: i64 = 0,
    physical: bool = false,

    pub fn decode(data: []const u8) !CompactionRequest {
        var req: CompactionRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.revision = @as(i64, @intCast(try r.readU64())),
                2 => req.physical = try r.readBool(),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

/// etcdserverpb.CompactionResponse
pub const CompactionResponse = struct {
    header: ResponseHeader = .{},

    pub fn encode(self: CompactionResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        return w.buf.toOwnedSlice(allocator);
    }
};

// ─── Watch messages ──────────────────────────────────────────────────────────

/// etcdserverpb.WatchCreateRequest
pub const WatchCreateRequest = struct {
    key: []const u8 = &[_]u8{},
    range_end: []const u8 = &[_]u8{},
    start_revision: i64 = 0,
    progress_notify: bool = false,
    filters: []WatchFilterType = &[_]WatchFilterType{},
    no_put: bool = false,
    no_delete: bool = false,
    prev_kv: bool = false,
    watch_id: i64 = 0,
    fragment: bool = false,

    pub fn decode(data: []const u8) !WatchCreateRequest {
        var req: WatchCreateRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.key = try readBytesCopy(&r),
                2 => req.range_end = try readBytesCopy(&r),
                3 => req.start_revision = @as(i64, @intCast(try r.readU64())),
                4 => req.progress_notify = try r.readBool(),
                5 => {
                    if (tag.wire_type == .length_delimited) {
                        var packed_filters = wire.Reader.init(try r.readBytes());
                        while (packed_filters.hasMore()) {
                            const filter = try packed_filters.readU64();
                            if (filter == 0) req.no_put = true;
                            if (filter == 1) req.no_delete = true;
                        }
                    } else {
                        const filter = try r.readU64();
                        if (filter == 0) req.no_put = true;
                        if (filter == 1) req.no_delete = true;
                    }
                },
                6 => req.prev_kv = try r.readBool(),
                7 => req.watch_id = @as(i64, @intCast(try r.readU64())),
                8 => req.fragment = try r.readBool(),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

pub const WatchFilterType = enum(i32) {
    NOPUT = 0,
    NODELETE = 1,
};

/// etcdserverpb.WatchRequest (oneof)
pub const WatchRequest = struct {
    pub const RequestUnion = union(enum) {
        create: WatchCreateRequest,
        cancel: WatchCancelRequest,
        progress: WatchProgressRequest,
    };

    pub fn decode(data: []const u8) !RequestUnion {
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => return .{ .create = try WatchCreateRequest.decode(try r.readBytes()) },
                2 => {
                    var nested = wire.Reader.init(try r.readBytes());
                    var id: i64 = 0;
                    while (nested.hasMore()) {
                        const inner = try nested.nextTag();
                        if (inner.field == 1) id = @bitCast(try nested.readU64()) else try nested.skip(inner.wire_type);
                    }
                    return .{ .cancel = .{ .watch_id = id } };
                },
                3 => return .{ .progress = .{} },
                else => try r.skip(tag.wire_type),
            }
        }
        return .{ .progress = .{} };
    }
};

pub const WatchCancelRequest = struct {
    watch_id: i64 = 0,
};

pub const WatchProgressRequest = struct {};

/// etcdserverpb.WatchResponse
pub const WatchResponse = struct {
    header: ResponseHeader = .{},
    watch_id: i64 = 0,
    created: bool = false,
    canceled: bool = false,
    compact_revision: i64 = 0,
    cancel_reason: []const u8 = &[_]u8{},
    fragment: bool = false,
    events: []Event = &[_]Event{},

    pub fn encode(self: WatchResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.v(2, @as(u64, @bitCast(self.watch_id)));
        try w.b(3, self.created);
        try w.b(4, self.canceled);
        try w.v(5, @as(u64, @bitCast(self.compact_revision)));
        try w.bytes(6, self.cancel_reason);
        try w.b(7, self.fragment);
        for (self.events) |evt| {
            const evt_bytes = try evt.encode(allocator);
            defer allocator.free(evt_bytes);
            try w.msg(11, evt_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

// ─── Lease messages ──────────────────────────────────────────────────────────

pub const LeaseGrantRequest = struct {
    ttl: i64 = 0,
    id: i64 = 0,

    pub fn decode(data: []const u8) !LeaseGrantRequest {
        var req: LeaseGrantRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.ttl = @as(i64, @intCast(try r.readU64())),
                2 => req.id = @as(i64, @intCast(try r.readU64())),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

pub const LeaseGrantResponse = struct {
    header: ResponseHeader = .{},
    id: i64 = 0,
    ttl: i64 = 0,

    pub fn encode(self: LeaseGrantResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.v(2, @as(u64, @bitCast(self.id)));
        try w.v(3, @as(u64, @bitCast(self.ttl)));
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const LeaseRevokeRequest = struct {
    id: i64 = 0,

    pub fn decode(data: []const u8) !LeaseRevokeRequest {
        var req: LeaseRevokeRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.id = @as(i64, @intCast(try r.readU64())),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

pub const LeaseRevokeResponse = struct {
    header: ResponseHeader = .{},

    pub fn encode(self: LeaseRevokeResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const LeaseKeepAliveRequest = struct {
    id: i64 = 0,

    pub fn decode(data: []const u8) !LeaseKeepAliveRequest {
        var req: LeaseKeepAliveRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.id = @as(i64, @intCast(try r.readU64())),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

pub const LeaseKeepAliveResponse = struct {
    header: ResponseHeader = .{},
    id: i64 = 0,
    ttl: i64 = 0,

    pub fn encode(self: LeaseKeepAliveResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.v(2, @as(u64, @bitCast(self.id)));
        try w.v(3, @as(u64, @bitCast(self.ttl)));
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const LeaseTimeToLiveRequest = struct {
    id: i64 = 0,
    keys: bool = false,

    pub fn decode(data: []const u8) !LeaseTimeToLiveRequest {
        var req: LeaseTimeToLiveRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.id = @as(i64, @intCast(try r.readU64())),
                2 => req.keys = try r.readBool(),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

pub const LeaseTimeToLiveResponse = struct {
    header: ResponseHeader = .{},
    id: i64 = 0,
    ttl: i64 = 0,
    granted_ttl: i64 = 0,
    /// etcdserverpb.LeaseTimeToLiveResponse.keys is `repeated bytes`, so each
    /// attached key is its own field. Encoding them as one blob with a
    /// separator made a lease holding two keys arrive at the client as a
    /// single key containing a NUL.
    keys: []const []const u8 = &.{},

    pub fn encode(self: LeaseTimeToLiveResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        // Field numbers follow etcdserverpb.LeaseTimeToLiveResponse:
        // header=1, ID=2, TTL=3, grantedTTL=4, keys=5.
        try w.msg(1, header_bytes);
        try w.v(2, @as(u64, @bitCast(self.id)));
        try w.v(3, @as(u64, @bitCast(self.ttl)));
        try w.v(4, @as(u64, @bitCast(self.granted_ttl)));
        for (self.keys) |key| try w.bytes(5, key);
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const LeaseLeasesRequest = struct {};

pub const LeaseLeasesResponse = struct {
    header: ResponseHeader = .{},
    leases: []LeaseStatus = &[_]LeaseStatus{},

    pub fn encode(self: LeaseLeasesResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        for (self.leases) |ls| {
            var lw = wire.Writer.init(allocator);
            try lw.v(1, @as(u64, @bitCast(ls.id)));
            const ls_bytes = try lw.buf.toOwnedSlice(allocator);
            defer allocator.free(ls_bytes);
            try w.msg(2, ls_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const LeaseStatus = struct {
    id: i64 = 0,
};

// ─── Cluster messages ────────────────────────────────────────────────────────

pub const Member = struct {
    id: u64 = 0,
    name: []const u8 = &[_]u8{},
    peer_urls: [][]const u8 = &[_][]const u8{},
    client_urls: [][]const u8 = &[_][]const u8{},
    is_learner: bool = false,

    pub fn encode(self: Member, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        try w.v(1, self.id);
        try w.bytes(2, self.name);
        for (self.peer_urls) |url| try w.bytes(3, url);
        for (self.client_urls) |url| try w.bytes(4, url);
        try w.b(5, self.is_learner);
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const MemberListRequest = struct {};

pub const MemberListResponse = struct {
    header: ResponseHeader = .{},
    members: []Member = &[_]Member{},

    pub fn encode(self: MemberListResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        for (self.members) |m| {
            const m_bytes = try m.encode(allocator);
            defer allocator.free(m_bytes);
            try w.msg(2, m_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const MemberAddRequest = struct {
    peer_urls: [][]const u8 = &[_][]const u8{},
    is_learner: bool = false,
};

pub const MemberAddResponse = struct {
    header: ResponseHeader = .{},
    member: Member = .{},
    members: []Member = &[_]Member{},
};

pub const MemberRemoveRequest = struct {
    id: u64 = 0,
};

pub const MemberRemoveResponse = struct {
    header: ResponseHeader = .{},
    members: []Member = &[_]Member{},
};

pub const MemberUpdateRequest = struct {
    id: u64 = 0,
    peer_urls: [][]const u8 = &[_][]const u8{},
};

pub const MemberUpdateResponse = struct {
    header: ResponseHeader = .{},
    members: []Member = &[_]Member{},
};

// ─── Maintenance messages ────────────────────────────────────────────────────

pub const StatusRequest = struct {};

pub const StatusResponse = struct {
    header: ResponseHeader = .{},
    version: []const u8 = &[_]u8{},
    db_size: i64 = 0,
    leader: u64 = 0,
    raft_index: u64 = 0,
    raft_term: u64 = 0,
    raft_applied_index: u64 = 0,
    errors: [][]const u8 = &[_][]const u8{},
    db_size_in_use: i64 = 0,
    is_learner: bool = false,

    pub fn encode(self: StatusResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.bytes(2, self.version);
        try w.v(3, @as(u64, @bitCast(self.db_size)));
        try w.v(4, self.leader);
        try w.v(5, self.raft_index);
        try w.v(6, self.raft_term);
        try w.v(7, self.raft_applied_index);
        for (self.errors) |e| try w.bytes(8, e);
        try w.v(9, @as(u64, @bitCast(self.db_size_in_use)));
        try w.b(10, self.is_learner);
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const AlarmRequest = struct {
    action: AlarmAction = .GET,
    member_id: u64 = 0,
    alarm: AlarmType = .NONE,

    pub fn decode(data: []const u8) !AlarmRequest {
        var req: AlarmRequest = .{};
        var r = wire.Reader.init(data);
        while (r.hasMore()) {
            const tag = try r.nextTag();
            switch (tag.field) {
                1 => req.action = @as(AlarmAction, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                2 => req.member_id = try r.readU64(),
                3 => req.alarm = @as(AlarmType, @enumFromInt(@as(i32, @intCast(try r.readU64())))),
                else => try r.skip(tag.wire_type),
            }
        }
        return req;
    }
};

pub const AlarmMember = struct {
    member_id: u64 = 0,
    alarm: AlarmType = .NONE,

    pub fn encode(self: AlarmMember, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        try w.v(1, self.member_id);
        try w.v(2, @bitCast(@as(i64, @intFromEnum(self.alarm))));
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const AlarmResponse = struct {
    header: ResponseHeader = .{},
    alarms: []AlarmMember = &[_]AlarmMember{},

    pub fn encode(self: AlarmResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        for (self.alarms) |a| {
            const a_bytes = try a.encode(allocator);
            defer allocator.free(a_bytes);
            try w.msg(2, a_bytes);
        }
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const DefragmentRequest = struct {};

pub const DefragmentResponse = struct {
    header: ResponseHeader = .{},

    pub fn encode(self: DefragmentResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const SnapshotRequest = struct {};

pub const SnapshotResponse = struct {
    header: ResponseHeader = .{},
    remaining_bytes: u64 = 0,
    blob: []const u8 = &[_]u8{},

    pub fn encode(self: SnapshotResponse, allocator: Allocator) ![]u8 {
        var w = wire.Writer.init(allocator);
        const header_bytes = try self.header.encode(allocator);
        defer allocator.free(header_bytes);
        try w.msg(1, header_bytes);
        try w.v(2, self.remaining_bytes);
        try w.bytes(3, self.blob);
        return w.buf.toOwnedSlice(allocator);
    }
};

pub const HashKVRequest = struct {
    revision: i64 = 0,
};

pub const HashKVResponse = struct {
    header: ResponseHeader = .{},
    hash: u32 = 0,
    compact_revision: i64 = 0,
};

pub const HashResponse = struct {
    header: ResponseHeader = .{},
    hash: u32 = 0,
};

pub const MoveLeaderRequest = struct {
    target_id: u64 = 0,
};

pub const MoveLeaderResponse = struct {
    header: ResponseHeader = .{},
};

// ─── Auth messages (minimal) ─────────────────────────────────────────────────

pub const AuthEnableRequest = struct {};
pub const AuthDisableRequest = struct {};
pub const AuthStatusRequest = struct {};

pub const AuthenticateRequest = struct {
    name: []const u8 = &[_]u8{},
    password: []const u8 = &[_]u8{},
};

pub const AuthEnableResponse = struct {
    header: ResponseHeader = .{},
};

pub const AuthDisableResponse = struct {
    header: ResponseHeader = .{},
};

pub const AuthStatusResponse = struct {
    header: ResponseHeader = .{},
    enabled: bool = false,
    auth_revision: u64 = 0,
};

pub const AuthenticateResponse = struct {
    header: ResponseHeader = .{},
    token: []const u8 = &[_]u8{},
};

// ─── Helper ──────────────────────────────────────────────────────────────────

fn readBytesCopy(r: *wire.Reader) ![]u8 {
    const bytes = try r.readBytes();
    return try std.heap.page_allocator.dupe(u8, bytes);
}
