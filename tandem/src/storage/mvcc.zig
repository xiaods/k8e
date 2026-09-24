// MVCC storage engine backed by rqlite
// Implements the etcd v3 KV semantics: Range, Put, DeleteRange, Txn, Compact
// All operations use rqlite's HTTP API for SQL execution with Raft consensus.

const std = @import("std");
test "watch history is owned, preserves previous metadata, and enforces compaction" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    _ = try storage.put("key", "first", 0, false);
    _ = try storage.put("key", "second", 0, false);
    const history = try storage.watchHistory(2, 2);
    defer storage.freeWatchHistory(history);
    try testing.expectEqual(@as(usize, 1), history.len);
    try testing.expectEqualStrings("first", history[0].prev_value);
    try testing.expectEqual(@as(i64, 1), history[0].prev_mod_revision);
    try testing.expectEqual(@as(i64, 1), history[0].create_revision);
    try testing.expectEqual(@as(i64, 2), history[0].version);
    try storage.compact(2);
    try testing.expectError(error.Compacted, storage.watchHistory(1, 2));
    _ = try storage.put("key", "third", 0, false);
    try testing.expectEqualStrings("second", history[0].value);
    const future = try storage.watchHistory(100, 200);
    defer storage.freeWatchHistory(future);
    try testing.expectEqual(@as(usize, 0), future.len);
}
const Allocator = std.mem.Allocator;
const rqlite = @import("rqlite.zig");

/// Storage engine configuration
pub const StorageConfig = struct {
    /// rqlite node URL (e.g., "http://127.0.0.1:4001")
    rqlite_url: []const u8 = "http://127.0.0.1:4001",
    /// Data directory for local state
    data_dir: []const u8 = "/var/lib/k8e/tandem",
};

/// MVCC key-value pair stored in rqlite
pub const MVCCKeyValue = struct {
    key: []const u8,
    value: []const u8,
    create_revision: i64,
    mod_revision: i64,
    version: i64,
    lease: i64,
};

/// Storage engine
pub const Storage = struct {
    allocator: Allocator,
    config: StorageConfig,
    client: ?rqlite.RqliteClient,
    /// Current revision counter (persisted in rqlite)
    current_revision: i64,
    /// Current raft term from rqlite
    raft_term: u64,
    memory_kvs: std.ArrayList(MVCCKeyValue),
    memory_history: std.ArrayList(WatchEvent),
    leases: LeaseManager,
    compact_revision: i64 = 0,
    txn_revision: ?i64 = null,
    /// Identifies this instance as the expiry owner for the leases it has
    /// claimed. Empty on the in-memory path, which has no peers.
    lease_owner: []const u8 = "",

    pub fn init(allocator: Allocator, config: StorageConfig) !Storage {
        var client = try rqlite.RqliteClient.init(allocator, config.rqlite_url);
        errdefer client.deinit();

        // A cold rqlite rejects requests until it has elected a leader, and the
        // schema write cannot be retried blindly, so wait for the node to be
        // serviceable before the first write.
        try client.waitForLeader();

        // Ensure schema exists
        const schema = rqlite.SqlBuilder.schemaSql();
        _ = try client.execute(schema);
        try migrateLeases(allocator, &client);

        const revision = try client.query("SELECT current_revision,raft_term FROM revision WHERE id=1");
        defer revision.deinit(allocator);
        if (revision.values.len != 1 or revision.values[0].len != 2) return error.InvalidRevisionState;
        const cur_rev = revision.values[0][0].integer;
        const raft_term: u64 = @intCast(revision.values[0][1].integer);

        const compaction = try client.query("SELECT compact_revision FROM compaction WHERE id=1");
        defer compaction.deinit(allocator);
        if (compaction.values.len != 1 or compaction.values[0].len != 1) return error.InvalidCompactionState;
        const compact_revision = compaction.values[0][0].integer;
        return .{
            .allocator = allocator,
            .config = config,
            .compact_revision = compact_revision,
            .client = client,
            .current_revision = cur_rev,
            .raft_term = raft_term,
            .memory_kvs = std.ArrayList(MVCCKeyValue).empty,
            .memory_history = std.ArrayList(WatchEvent).empty,
            .leases = LeaseManager.init(allocator),
        };
    }

    /// initMemory creates an isolated storage shell for deterministic unit
    /// and integration tests. It deliberately performs no network I/O.
    /// Production callers must use init so rqlite-backed persistence is used.
    pub fn initMemory(allocator: Allocator) Storage {
        return .{
            .allocator = allocator,
            .config = .{},
            .client = null,
            .current_revision = 0,
            .raft_term = 0,
            .memory_kvs = std.ArrayList(MVCCKeyValue).empty,
            .memory_history = std.ArrayList(WatchEvent).empty,
            .leases = LeaseManager.init(allocator),
        };
    }

    pub fn deinit(self: *Storage) void {
        for (self.memory_kvs.items) |kv| {
            self.allocator.free(kv.key);
            self.allocator.free(kv.value);
        }
        for (self.memory_history.items) |event| {
            self.allocator.free(event.key);
            self.allocator.free(event.value);
            self.allocator.free(event.prev_key);
            self.allocator.free(event.prev_value);
        }
        self.memory_kvs.deinit(self.allocator);
        self.memory_history.deinit(self.allocator);
        self.leases.deinit();
        if (self.lease_owner.len > 0) self.allocator.free(self.lease_owner);
        if (self.client) |*c| c.deinit();
    }

    /// Initialize the SQLite schema in rqlite
    pub fn initSchema(self: *Storage) !void {
        if (self.client == null) return;
        if (self.client) |*c| {
            const schema = rqlite.SqlBuilder.schemaSql();
            _ = try c.execute(schema);
        }
    }

    /// Range query: get keys in [key, range_end) with optional filters
    pub fn range(
        self: *Storage,
        key: []const u8,
        range_end: []const u8,
        limit: i64,
        revision: i64,
        keys_only: bool,
        count_only: bool,
    ) !RangeResult {
        // A historical read at or below the compaction watermark has had its
        // history deleted, so the query would return an empty result that is
        // indistinguishable from "the key did not exist at that revision".
        // etcd answers ErrCompacted; KIP-29 Appendix A requires that explicit
        // error rather than a silently wrong answer.
        if (revision > 0 and revision <= self.compact_revision) return error.Compacted;
        if (self.client == null) return self.memoryRange(key, range_end, limit, revision, keys_only, count_only);
        if (self.client) |*c| {
            const sql = try rqlite.SqlBuilder.rangeSql(
                self.allocator,
                key,
                range_end,
                limit,
                revision,
                keys_only,
                count_only,
            );
            defer self.allocator.free(sql);

            const qr = try c.query(sql);
            defer qr.deinit(self.allocator);
            if (count_only) return .{ .kvs = &.{}, .count = if (qr.values.len > 0) qr.values[0][0].integer else 0, .more = false };
            var kvs = std.ArrayList(MVCCKeyValue).empty;
            for (qr.values) |row| {
                if (row.len >= 6) {
                    const kv = MVCCKeyValue{
                        .key = try self.allocator.dupe(u8, textValue(row[0])),
                        .value = try self.allocator.dupe(u8, textValue(row[1])),
                        .create_revision = row[2].integer,
                        .mod_revision = row[3].integer,
                        .version = row[4].integer,
                        .lease = row[5].integer,
                    };
                    try kvs.append(self.allocator, kv);
                }
            }

            const count: i64 = if (qr.values.len > 0) qr.values[0][6].integer else 0;
            // toOwnedSlice resets the list, so the returned length has to be
            // captured first. Reading kvs.items.len afterwards is always zero,
            // which made `more` permanently true and left the apiserver
            // paginating a complete list forever.
            const returned: i64 = @intCast(kvs.items.len);
            return RangeResult{
                .kvs = try kvs.toOwnedSlice(self.allocator),
                .count = count,
                .more = count > returned,
            };
        }

        return RangeResult{
            .kvs = &[_]MVCCKeyValue{},
            .count = 0,
            .more = false,
        };
    }

    /// Put a key-value pair (optionally with prev_kv)
    pub fn put(
        self: *Storage,
        key: []const u8,
        value: []const u8,
        lease: i64,
        prev_kv: bool,
    ) !PutResult {
        if (self.client == null) return self.memoryPut(key, value, lease, prev_kv);
        // prev_kv is captured by reading the row before the write, the same
        // way deleteRange does. The per-key buffers are owned here: the range
        // result only owns the outer slice, so releasing that alone leaked the
        // key and value.
        var prev: ?MVCCKeyValue = null;
        if (prev_kv) {
            const found = try self.range(key, "", 1, 0, false, false);
            var keep_previous = false;
            defer if (!keep_previous) {
                for (found.kvs) |kv| {
                    self.allocator.free(kv.key);
                    self.allocator.free(kv.value);
                }
                self.allocator.free(found.kvs);
            };
            if (found.kvs.len > 0) {
                prev = found.kvs[0];
                keep_previous = true;
            }
        }
        errdefer if (prev) |kv| {
            self.allocator.free(kv.key);
            self.allocator.free(kv.value);
        };
        const sql = try rqlite.SqlBuilder.putSql(self.allocator, key, value, lease);
        defer self.allocator.free(sql);
        _ = try self.client.?.execute(sql);
        try self.refreshRevision();
        return .{ .prev_kv = prev, .revision = self.current_revision };
    }

    fn refreshRevision(self: *Storage) !void {
        const result = try self.client.?.query(rqlite.SqlBuilder.currentRevisionSql());
        defer result.deinit(self.allocator);
        if (result.values.len != 1 or result.values[0].len != 1) return error.InvalidRevisionState;
        self.current_revision = result.values[0][0].integer;
    }

    /// Delete keys in [key, range_end)
    pub fn deleteRange(
        self: *Storage,
        key: []const u8,
        range_end: []const u8,
        prev_kv: bool,
    ) !DeleteResult {
        if (self.client == null) return self.memoryDeleteRange(key, range_end, prev_kv);
        const found = try self.range(key, range_end, 0, 0, false, false);
        var keep_previous = false;
        defer if (!keep_previous) {
            for (found.kvs) |kv| {
                self.allocator.free(kv.key);
                self.allocator.free(kv.value);
            }
            self.allocator.free(found.kvs);
        };
        const sql = try rqlite.SqlBuilder.deleteRangeSql(self.allocator, key, range_end);
        defer self.allocator.free(sql);
        _ = try self.client.?.execute(sql);
        try self.refreshRevision();
        keep_previous = prev_kv;
        return .{ .deleted = found.count, .prev_kvs = if (prev_kv) found.kvs else &.{} };
    }

    /// Atomic transaction - all operations in a single rqlite execute request
    pub fn txn(self: *Storage, txn_data: TxnData) !TxnResult {
        if (self.client == null) return self.memoryTxn(txn_data);
        const sql = try rqlite.TxnBuilder.buildTxnSql(self.allocator, .{
            .compare = txn_data.compare,
            .success = txn_data.success,
            .failure = txn_data.failure,
        });
        defer self.allocator.free(sql);
        const sets = try self.client.?.transactionSets(sql);
        defer {
            for (sets) |*set| set.deinit(self.allocator);
            self.allocator.free(sets);
        }
        const qr = sets[0];

        var succeeded = true;
        var responses = std.ArrayList(TxnResponse).empty;
        errdefer {
            for (responses.items) |response| {
                if (response.prev_kv) |kv| {
                    self.allocator.free(kv.key);
                    self.allocator.free(kv.value);
                }
                self.freeTxnRange(response);
            }
            responses.deinit(self.allocator);
        }
        {
            for (qr.values) |row| {
                if (row.len < 10) continue;
                if (row[0].integer == 0) {
                    succeeded = row[1].integer != 0;
                    self.current_revision = row[2].integer;
                    continue;
                }
                const operations = if (succeeded) txn_data.success else txn_data.failure;
                const kind: TxnResponse.ResponseKind = switch (operations[responses.items.len].kind) {
                    .put => .put,
                    .delete => .delete,
                    .range => .range,
                };
                var response = TxnResponse{ .kind = kind, .data = &[_]u8{}, .ok = row[1].integer != 0, .revision = row[2].integer, .affected = row[3].integer };
                // The previous key is a BLOB column, so it arrives in the blob
                // variant; matching on .text alone silently dropped every
                // prev_kv once keys stopped being TEXT.
                const previous_key = textValue(row[4]);
                if (previous_key.len > 0) {
                    response.prev_kv = .{ .key = try self.allocator.dupe(u8, previous_key), .value = try self.allocator.dupe(u8, textValue(row[5])), .create_revision = row[6].integer, .mod_revision = row[7].integer, .version = row[8].integer, .lease = row[9].integer };
                }
                try responses.append(self.allocator, response);
            }
        }
        // Attach the range rows captured by the same request, matched to the
        // op they belong to. The capture is emitted after the branch's writes,
        // so a Range that follows a Put in the same branch sees that Put, as
        // etcd does.
        if (sets.len > 1) {
            const operations = if (succeeded) txn_data.success else txn_data.failure;
            for (sets[1].values) |row| {
                if (row.len < 10) continue;
                const op_index = row[0].integer;
                if (op_index < 1 or op_index > operations.len) continue;
                var response = &responses.items[@intCast(op_index - 1)];
                if (response.kind != .range) continue;
                const grown = try self.allocator.realloc(response.range_kvs, response.range_kvs.len + 1);
                response.range_kvs = grown;
                response.range_kvs[response.range_kvs.len - 1] = .{
                    .key = try self.allocator.dupe(u8, textValue(row[2])),
                    .value = try self.allocator.dupe(u8, textValue(row[3])),
                    .create_revision = row[4].integer,
                    .mod_revision = row[5].integer,
                    .version = row[6].integer,
                    .lease = row[7].integer,
                };
                response.range_count = row[8].integer;
            }
        }
        return TxnResult{ .succeeded = succeeded, .responses = try responses.toOwnedSlice(self.allocator) };
    }

    fn freeTxnRange(self: *Storage, response: TxnResponse) void {
        for (response.range_kvs) |kv| {
            self.allocator.free(kv.key);
            self.allocator.free(kv.value);
        }
        self.allocator.free(response.range_kvs);
    }
    fn memoryTxn(self: *Storage, txn_data: TxnData) !TxnResult {
        // Evaluate every compare before applying any operation. This gives the
        // in-memory engine the same all-or-nothing boundary as rqlite's SQL
        // transaction and prevents a failed compare from partially mutating
        // the store.
        var succeeded = true;
        for (txn_data.compare) |compare| {
            var found: ?rqlite.MvccKv = null;
            for (self.memory_kvs.items) |kv| {
                if (std.mem.eql(u8, kv.key, compare.key)) {
                    found = .{ .key = kv.key, .value = kv.value, .create_revision = kv.create_revision, .mod_revision = kv.mod_revision, .version = kv.version, .lease = kv.lease };
                    break;
                }
            }
            if (!compare.evaluate(found)) {
                succeeded = false;
                break;
            }
        }

        const operations = if (succeeded) txn_data.success else txn_data.failure;
        const base_revision = self.current_revision;
        self.txn_revision = base_revision + 1;
        defer self.txn_revision = null;
        var responses = std.ArrayList(TxnResponse).empty;
        for (operations) |operation| {
            switch (operation.kind) {
                .put => {
                    const result = try self.put(operation.key, operation.value, operation.lease, true);
                    try responses.append(self.allocator, .{ .kind = .put, .data = &[_]u8{}, .revision = result.revision, .prev_kv = result.prev_kv });
                },
                .delete => {
                    const result = try self.deleteRange(operation.key, operation.range_end, true);
                    try responses.append(self.allocator, .{ .kind = .delete, .data = &[_]u8{}, .revision = self.current_revision, .affected = result.deleted });
                    for (result.prev_kvs) |kv| {
                        self.allocator.free(kv.key);
                        self.allocator.free(kv.value);
                    }
                    self.allocator.free(result.prev_kvs);
                },
                .range => {
                    _ = try self.range(operation.key, operation.range_end, 0, 0, false, false);
                    try responses.append(self.allocator, .{ .kind = .range, .data = &[_]u8{} });
                },
            }
        }
        return .{ .succeeded = succeeded, .responses = try responses.toOwnedSlice(self.allocator) };
    }

    /// Compact all revisions below the given revision
    pub fn compact(self: *Storage, revision: i64) !void {
        if (revision <= self.compact_revision) return error.Compacted;
        if (revision > self.current_revision) return error.FutureRevision;
        if (self.client == null) {
            self.compact_revision = revision;
            var i: usize = 0;
            while (i < self.memory_history.items.len) {
                if (self.memory_history.items[i].mod_revision < revision) {
                    const old = self.memory_history.orderedRemove(i);
                    self.allocator.free(old.key);
                    self.allocator.free(old.value);
                    self.allocator.free(old.prev_key);
                    self.allocator.free(old.prev_value);
                } else i += 1;
            }
            return;
        }
        if (self.client) |*c| {
            const sql = try rqlite.SqlBuilder.compactSql(revision);
            defer std.heap.page_allocator.free(sql);
            _ = try c.execute(sql);
            self.compact_revision = revision;
        }
    }

    /// Get current revision
    pub fn currentRevision(self: *Storage) i64 {
        return self.current_revision;
    }

    /// Owned committed events, inclusive of both revision bounds. Both live
    /// delivery and historical replay use this storage boundary.
    pub fn watchHistory(self: *Storage, start: i64, end: i64) ![]WatchEvent {
        if (start <= self.compact_revision and self.compact_revision > 0) return error.Compacted;
        var events: std.ArrayList(WatchEvent) = .empty;
        errdefer {
            for (events.items) |event| self.freeWatchEvent(event);
            events.deinit(self.allocator);
        }
        if (self.client) |*client| {
            const sql = try rqlite.SqlBuilder.watchHistorySql(self.allocator, start, end);
            defer self.allocator.free(sql);
            const result = try client.query(sql);
            defer result.deinit(self.allocator);
            for (result.values) |row| {
                if (row.len != 12) return error.InvalidWatchHistory;
                const deleted = row[6].integer != 0;
                const event = WatchEvent{
                    .key = textValue(row[0]),
                    .value = if (deleted) "" else textValue(row[1]),
                    .create_revision = row[2].integer,
                    .mod_revision = row[3].integer,
                    .version = row[4].integer,
                    .lease = if (deleted) 0 else row[5].integer,
                    .prev_key = textValue(row[7]),
                    .prev_value = textValue(row[8]),
                    .prev_mod_revision = integerValue(row[9]),
                    .prev_version = integerValue(row[10]),
                    .prev_lease = integerValue(row[11]),
                    .event_type = if (deleted) .DELETE else .PUT,
                };
                const owned = try self.cloneWatchEvent(event);
                errdefer self.freeWatchEvent(owned);
                try events.append(self.allocator, owned);
            }
        } else {
            for (self.memory_history.items) |event| {
                if (event.mod_revision < start or event.mod_revision > end) continue;
                const owned = try self.cloneWatchEvent(event);
                errdefer self.freeWatchEvent(owned);
                try events.append(self.allocator, owned);
            }
        }
        return events.toOwnedSlice(self.allocator);
    }

    fn textValue(value: rqlite.Value) []const u8 {
        return switch (value) {
            .text, .blob => |bytes| bytes,
            else => "",
        };
    }
    fn integerValue(value: rqlite.Value) i64 {
        return switch (value) {
            .integer => |number| number,
            else => 0,
        };
    }

    fn cloneWatchEvent(self: *Storage, event: WatchEvent) !WatchEvent {
        var copy = event;
        copy.key = try self.allocator.dupe(u8, event.key);
        errdefer self.allocator.free(copy.key);
        copy.value = try self.allocator.dupe(u8, event.value);
        errdefer self.allocator.free(copy.value);
        copy.prev_key = try self.allocator.dupe(u8, event.prev_key);
        errdefer self.allocator.free(copy.prev_key);
        copy.prev_value = try self.allocator.dupe(u8, event.prev_value);
        return copy;
    }
    fn freeWatchEvent(self: *Storage, event: WatchEvent) void {
        self.allocator.free(event.key);
        self.allocator.free(event.value);
        self.allocator.free(event.prev_key);
        self.allocator.free(event.prev_value);
    }
    pub fn freeWatchHistory(self: *Storage, events: []WatchEvent) void {
        for (events) |event| self.freeWatchEvent(event);
        self.allocator.free(events);
    }

    /// Get current raft term
    pub fn currentRaftTerm(self: *Storage) u64 {
        return self.raft_term;
    }

    pub fn leaseManager(self: *Storage) *LeaseManager {
        return &self.leases;
    }

    pub fn grantLease(self: *Storage, lease_id: i64, ttl: i64) !void {
        if (self.client) |*client| {
            const sql = try rqlite.SqlBuilder.createLeaseForSql(self.allocator, lease_id, ttl, nowSeconds());
            defer self.allocator.free(sql);
            _ = try client.execute(sql);
            return;
        }
        try self.leases.createLease(lease_id, ttl);
    }

    pub fn keepAliveLease(self: *Storage, lease_id: i64) !void {
        if (self.client) |*client| {
            const sql = try rqlite.SqlBuilder.keepAliveLeaseForSql(self.allocator, lease_id, nowSeconds());
            defer self.allocator.free(sql);
            _ = try client.execute(sql);
            return;
        }
        try self.leases.keepAlive(lease_id);
    }

    pub fn revokeLease(self: *Storage, lease_id: i64) !void {
        if (self.client) |*client| {
            const keys_sql = try rqlite.SqlBuilder.leaseKeysForIdSql(self.allocator, lease_id);
            defer self.allocator.free(keys_sql);
            const keys = try client.query(keys_sql);
            defer keys.deinit(self.allocator);
            for (keys.values) |key_row| {
                if (key_row.len == 0) continue;
                // lease_keys.key is a BLOB column, so the decoded value is
                // .blob; reading .text yielded an empty slice and the revoke
                // deleted nothing while reporting success.
                const delete_sql = try rqlite.SqlBuilder.expireLeaseKeySql(self.allocator, textValue(key_row[0]), lease_id);
                defer self.allocator.free(delete_sql);
                _ = try client.execute(delete_sql);
            }
            try self.refreshRevision();
            const revoke_sql = try rqlite.SqlBuilder.revokeLeaseForIdSql(self.allocator, lease_id);
            defer self.allocator.free(revoke_sql);
            _ = try client.execute(revoke_sql);
            return;
        }
        const keys = try self.leases.leaseKeys(lease_id);
        defer {
            for (keys) |key| self.allocator.free(key);
            self.allocator.free(keys);
        }
        for (keys) |key| {
            _ = try self.memoryDeleteRange(key, "", false);
        }
        try self.leases.revokeLease(lease_id);
    }

    pub fn attachLeaseKey(self: *Storage, lease_id: i64, key: []const u8) !void {
        if (self.client) |*client| {
            const sql = try rqlite.SqlBuilder.attachLeaseKeyForSql(self.allocator, lease_id, key);
            defer self.allocator.free(sql);
            _ = try client.execute(sql);
            return;
        }
        try self.leases.attachKey(lease_id, key);
    }

    /// Expire leases and delete all attached keys in revision order. The
    /// generated deletes flow through memoryDeleteRange so watchers observe
    /// the same DELETE events as explicit deletes.
    /// Name this instance as the expiry owner. Set once at startup; the value
    /// is written into `leases.owner` and is what makes a claim exclusive.
    pub fn setLeaseOwner(self: *Storage, owner: []const u8) !void {
        const owned = try self.allocator.dupe(u8, owner);
        if (self.lease_owner.len > 0) self.allocator.free(self.lease_owner);
        self.lease_owner = owned;
    }

    /// Give up any leases this instance holds, so another instance can take
    /// over on failover instead of waiting for a claim that can never succeed.
    pub fn releaseLeases(self: *Storage) !void {
        if (self.client == null or self.lease_owner.len == 0) return;
        const sql = try rqlite.SqlBuilder.releaseLeasesForSql(self.allocator, self.lease_owner);
        defer self.allocator.free(sql);
        _ = try self.client.?.execute(sql);
    }

    pub fn expireLeases(self: *Storage) !i64 {
        return self.expireLeasesOwned(self.lease_owner);
    }

    /// Reap expired leases this instance owns. `owner` is empty on the
    /// in-memory path, where there is only ever one instance.
    ///
    /// KIP-29 requires expiry to be executed by exactly one owner recorded in
    /// the lease row, never by each instance's own clock. The claim is a
    /// conditional UPDATE in the same Raft transaction, so when several
    /// instances scan the same expired lease exactly one reports a row and the
    /// rest skip it. On a single node the owner never changes, which is the
    /// whole of the current behaviour; the column exists so that a
    /// three-node deployment does not have to rewrite this.
    pub fn expireLeasesOwned(self: *Storage, owner: []const u8) !i64 {
        if (self.client) |*client| {
            // The deadline was written from this same clock (see
            // createLeaseForSql), so the scan must not fall back to SQL's own
            // notion of now.
            const expired_sql = try rqlite.SqlBuilder.expiredLeasesSql(self.allocator, nowSeconds());
            defer self.allocator.free(expired_sql);
            const expired = try client.query(expired_sql);
            var deleted: i64 = 0;
            for (expired.values) |row| {
                if (row.len == 0) continue;
                const lease_id = row[0].integer;
                // Take ownership before touching a key. Without this, two
                // instances would both delete and both advance the revision.
                const claim_sql = try rqlite.SqlBuilder.claimLeaseForSql(self.allocator, lease_id, owner);
                defer self.allocator.free(claim_sql);
                const claim = try client.execute(claim_sql);
                if (claim.rows_affected == 0) continue;
                const keys_sql = try rqlite.SqlBuilder.leaseKeysForIdSql(self.allocator, lease_id);
                defer self.allocator.free(keys_sql);
                const keys = try client.query(keys_sql);
                for (keys.values) |key_row| {
                    if (key_row.len == 0) continue;
                    // lease_keys.key is a BLOB column: reading .text here
                    // produced an empty key, so expiry matched no row and a
                    // lapsed lease silently kept its key forever.
                    const delete_sql = try rqlite.SqlBuilder.expireLeaseKeySql(self.allocator, textValue(key_row[0]), lease_id);
                    defer self.allocator.free(delete_sql);
                    const result = try client.execute(delete_sql);
                    deleted += result.rows_affected;
                }
                const revoke_sql = try rqlite.SqlBuilder.revokeLeaseForIdSql(self.allocator, lease_id);
                defer self.allocator.free(revoke_sql);
                _ = try client.execute(revoke_sql);
            }
            // Each expiry advanced the stored revision, so the cached value has
            // to follow. Leaving it stale made every later response header and
            // watch range report a revision older than the data, which is
            // exactly the invariant Kubernetes uses to resume watches.
            if (deleted > 0) try self.refreshRevision();
            return deleted;
        }
        const expired = try self.leases.expiredLeases();
        defer self.allocator.free(expired);
        var deleted: i64 = 0;
        for (expired) |lease_id| {
            const keys = try self.leases.leaseKeys(lease_id);
            defer {
                for (keys) |key| self.allocator.free(key);
                self.allocator.free(keys);
            }
            for (keys) |key| {
                const result = try self.memoryDeleteRange(key, "", false);
                deleted += result.deleted;
            }
            _ = self.leases.revokeLease(lease_id) catch {};
        }
        return deleted;
    }

    fn inRange(key: []const u8, start: []const u8, end: []const u8) bool {
        if (start.len == 0 and end.len == 0) return true;
        if (end.len == 0) return std.mem.eql(u8, key, start);
        if (std.mem.eql(u8, end, "\x00")) return std.mem.order(u8, key, start) != .lt;
        return std.mem.order(u8, key, start) != .lt and std.mem.order(u8, key, end) == .lt;
    }

    fn cloneKV(self: *Storage, kv: MVCCKeyValue) !MVCCKeyValue {
        return .{
            .key = try self.allocator.dupe(u8, kv.key),
            .value = try self.allocator.dupe(u8, kv.value),
            .create_revision = kv.create_revision,
            .mod_revision = kv.mod_revision,
            .version = kv.version,
            .lease = kv.lease,
        };
    }

    fn memoryRange(self: *Storage, key: []const u8, range_end: []const u8, limit: i64, revision: i64, keys_only: bool, count_only: bool) !RangeResult {
        var result = std.ArrayList(MVCCKeyValue).empty;
        var count: i64 = 0;
        std.mem.sort(MVCCKeyValue, self.memory_kvs.items, {}, struct {
            fn lessThan(_: void, a: MVCCKeyValue, b: MVCCKeyValue) bool {
                return std.mem.lessThan(u8, a.key, b.key);
            }
        }.lessThan);
        for (self.memory_kvs.items) |kv| {
            if (!inRange(kv.key, key, range_end) or (revision > 0 and kv.mod_revision > revision)) continue;
            count += 1;
            if (!count_only and (limit <= 0 or result.items.len < @as(usize, @intCast(limit)))) {
                var copy = try self.cloneKV(kv);
                if (keys_only) {
                    self.allocator.free(copy.value);
                    copy.value = &[_]u8{};
                }
                try result.append(self.allocator, copy);
            }
        }
        const more = !count_only and count > @as(i64, @intCast(result.items.len));
        return .{ .kvs = try result.toOwnedSlice(self.allocator), .count = count, .more = more };
    }

    fn memoryPut(self: *Storage, key: []const u8, value: []const u8, lease: i64, want_prev: bool) !PutResult {
        var prev: ?MVCCKeyValue = null;
        var index: ?usize = null;
        for (self.memory_kvs.items, 0..) |kv, i| if (std.mem.eql(u8, kv.key, key)) {
            index = i;
            prev = try self.cloneKV(kv);
            break;
        };
        self.current_revision = self.txn_revision orelse (self.current_revision + 1);
        const rev = self.current_revision;
        const next = MVCCKeyValue{ .key = try self.allocator.dupe(u8, key), .value = try self.allocator.dupe(u8, value), .create_revision = if (prev) |p| p.create_revision else rev, .mod_revision = rev, .version = if (prev) |p| p.version + 1 else 1, .lease = lease };
        if (index) |i| {
            const old_lease = self.memory_kvs.items[i].lease;
            if (old_lease != 0 and old_lease != lease) self.leases.detachKey(old_lease, key);
            self.allocator.free(self.memory_kvs.items[i].key);
            self.allocator.free(self.memory_kvs.items[i].value);
            self.memory_kvs.items[i] = next;
        } else try self.memory_kvs.append(self.allocator, next);
        if (lease != 0) {
            try self.leases.attachKey(lease, key);
        }
        try self.memory_history.append(self.allocator, .{ .key = try self.allocator.dupe(u8, key), .value = try self.allocator.dupe(u8, value), .prev_key = if (prev != null) try self.allocator.dupe(u8, key) else &.{}, .prev_value = if (prev) |p| try self.allocator.dupe(u8, p.value) else &[_]u8{}, .mod_revision = rev, .create_revision = next.create_revision, .version = next.version, .lease = lease, .prev_mod_revision = if (prev) |p| p.mod_revision else 0, .prev_version = if (prev) |p| p.version else 0, .prev_lease = if (prev) |p| p.lease else 0, .event_type = .PUT });
        if (!want_prev) {
            if (prev) |p| {
                self.allocator.free(p.key);
                self.allocator.free(p.value);
            }
            prev = null;
        }
        return .{ .prev_kv = prev, .revision = rev };
    }

    fn memoryDeleteRange(self: *Storage, key: []const u8, range_end: []const u8, want_prev: bool) !DeleteResult {
        var prevs = std.ArrayList(MVCCKeyValue).empty;
        var i: usize = 0;
        var deleted: i64 = 0;
        while (i < self.memory_kvs.items.len) {
            if (!inRange(self.memory_kvs.items[i].key, key, range_end)) {
                i += 1;
                continue;
            }
            const old = self.memory_kvs.orderedRemove(i);
            if (want_prev) try prevs.append(self.allocator, try self.cloneKV(old));
            if (old.lease != 0) self.leases.detachKey(old.lease, old.key);
            if (deleted == 0) self.current_revision = self.txn_revision orelse (self.current_revision + 1);
            deleted += 1;
            try self.memory_history.append(self.allocator, .{
                .key = try self.allocator.dupe(u8, old.key),
                .value = &[_]u8{},
                .prev_key = try self.allocator.dupe(u8, old.key),
                .prev_value = try self.allocator.dupe(u8, old.value),
                .mod_revision = self.current_revision,
                .create_revision = old.create_revision,
                .version = old.version,
                .event_type = .DELETE,
                .prev_mod_revision = old.mod_revision,
                .prev_version = old.version,
                .prev_lease = old.lease,
            });
            self.allocator.free(old.key);
            self.allocator.free(old.value);
        }
        return .{ .deleted = deleted, .prev_kvs = try prevs.toOwnedSlice(self.allocator) };
    }
};

pub const RangeResult = struct {
    kvs: []MVCCKeyValue,
    count: i64,
    more: bool,
};

pub const PutResult = struct {
    prev_kv: ?MVCCKeyValue,
    revision: i64,
};

pub const DeleteResult = struct {
    deleted: i64,
    prev_kvs: []MVCCKeyValue,
};

pub const TxnData = struct {
    compare: []const rqlite.CompareOperation,
    success: []const rqlite.TxnOp,
    failure: []const rqlite.TxnOp,
};

pub const TxnResult = struct {
    succeeded: bool,
    responses: []TxnResponse,
};

pub const TxnResponse = struct {
    kind: ResponseKind,
    data: []const u8,
    ok: bool = true,
    revision: i64 = 0,
    affected: i64 = 0,
    prev_kv: ?MVCCKeyValue = null,
    /// Rows a Range op inside the transaction answered with, captured by the
    /// same rqlite request that applied the transaction. Reading them back with
    /// a separate query would let a concurrent commit change the answer after
    /// the fact, which KIP-29 §6 forbids.
    range_kvs: []MVCCKeyValue = &.{},
    range_count: i64 = 0,
    range_more: bool = false,

    pub const ResponseKind = enum {
        range,
        put,
        delete,
    };
};

// ─── Revision Management ─────────────────────────────────────

pub const Revision = struct {
    current: i64,

    pub fn init() Revision {
        return .{ .current = 0 };
    }

    pub fn next(self: *Revision) i64 {
        self.current += 1;
        return self.current;
    }

    pub fn peek(self: Revision) i64 {
        return self.current;
    }
};

// ─── Watch Event Storage ─────────────────────────────────────

pub const WatchEvent = struct {
    lease: i64 = 0,
    prev_mod_revision: i64 = 0,
    prev_version: i64 = 0,
    prev_lease: i64 = 0,
    key: []const u8,
    value: []const u8,
    prev_key: []const u8,
    prev_value: []const u8,
    mod_revision: i64,
    create_revision: i64,
    version: i64,
    event_type: EventType,

    pub const EventType = enum { PUT, DELETE };
};

pub const WatchEntry = struct {
    watch_id: i64,
    key: []const u8,
    range_end: []const u8,
    start_revision: i64,
    filters: []WatchFilter,
};

pub const WatchRegistry = struct {
    allocator: Allocator,
    watches: std.ArrayList(WatchEntry),
    next_watch_id: i64 = 1,

    pub fn init(allocator: Allocator) WatchRegistry {
        return .{
            .allocator = allocator,
            .watches = std.ArrayList(WatchEntry).empty,
        };
    }

    pub fn deinit(self: *WatchRegistry) void {
        for (self.watches.items) |watch| {
            self.allocator.free(watch.key);
            self.allocator.free(watch.range_end);
        }
        self.watches.deinit(self.allocator);
    }

    pub fn createWatch(self: *WatchRegistry, key: []const u8, range_end: []const u8, start_revision: i64) !i64 {
        const id = self.next_watch_id;
        self.next_watch_id += 1;
        try self.watches.append(self.allocator, .{
            .watch_id = id,
            .key = try self.allocator.dupe(u8, key),
            .range_end = try self.allocator.dupe(u8, range_end),
            .start_revision = start_revision,
            .filters = &[_]WatchFilter{},
        });
        return id;
    }

    pub fn cancelWatch(self: *WatchRegistry, watch_id: i64) bool {
        for (self.watches.items, 0..) |w, i| {
            if (w.watch_id == watch_id) {
                const removed = self.watches.orderedRemove(i);
                self.allocator.free(removed.key);
                self.allocator.free(removed.range_end);
                return true;
            }
        }
        return false;
    }

    pub fn matchingWatchers(self: *WatchRegistry, key: []const u8) ![]WatchEntry {
        var matches = std.ArrayList(WatchEntry).empty;
        for (self.watches.items) |watch| {
            if (watchMatches(watch, key)) try matches.append(self.allocator, watch);
        }
        return matches.toOwnedSlice(self.allocator);
    }

    pub fn replay(self: *WatchRegistry, watch_id: i64, events: []const WatchEvent, compact_revision: i64) ![]WatchEvent {
        var result = std.ArrayList(WatchEvent).empty;
        for (self.watches.items) |watch| {
            if (watch.watch_id != watch_id) continue;
            if (watch.start_revision > 0 and watch.start_revision <= compact_revision) return error.Compacted;
            for (events) |event| {
                if (event.mod_revision < watch.start_revision or !watchMatches(watch, event.key)) continue;
                if ((event.event_type == .PUT and hasFilter(watch, .NOPUT)) or (event.event_type == .DELETE and hasFilter(watch, .NODELETE))) continue;
                try result.append(self.allocator, event);
            }
            break;
        }
        return result.toOwnedSlice(self.allocator);
    }

    pub fn minRevision(self: WatchRegistry) i64 {
        var min_rev: i64 = std.math.maxInt(i64);
        for (self.watches.items) |w| {
            if (w.start_revision < min_rev) {
                min_rev = w.start_revision;
            }
        }
        return min_rev;
    }
};

fn watchMatches(watch: WatchEntry, key: []const u8) bool {
    if (watch.range_end.len == 0) return std.mem.eql(u8, watch.key, key);
    return std.mem.order(u8, key, watch.key) != .lt and std.mem.order(u8, key, watch.range_end) == .lt;
}

fn hasFilter(watch: WatchEntry, filter: WatchFilter) bool {
    for (watch.filters) |candidate| if (candidate == filter) return true;
    return false;
}

pub const WatchFilter = enum { NOPUT, NODELETE };

// ─── Lease Management ─────────────────────────────────────────

pub const LeaseManager = struct {
    allocator: Allocator,
    leases: std.ArrayList(Lease),
    key_links: std.ArrayList(LeaseKey),

    const Lease = struct { id: i64, ttl: i64, expires_at: i64 };
    const LeaseKey = struct { lease_id: i64, key: []const u8 };

    pub fn init(allocator: Allocator) LeaseManager {
        return .{ .allocator = allocator, .leases = std.ArrayList(Lease).empty, .key_links = std.ArrayList(LeaseKey).empty };
    }

    pub fn deinit(self: *LeaseManager) void {
        for (self.key_links.items) |link| self.allocator.free(link.key);
        self.key_links.deinit(self.allocator);
        self.leases.deinit(self.allocator);
    }

    pub fn createLease(self: *LeaseManager, lease_id: i64, ttl: i64) !void {
        if (ttl <= 0) return error.InvalidLeaseTTL;
        for (self.leases.items) |*lease| if (lease.id == lease_id) {
            lease.ttl = ttl;
            lease.expires_at = nowSeconds() + ttl;
            return;
        };
        try self.leases.append(self.allocator, .{ .id = lease_id, .ttl = ttl, .expires_at = nowSeconds() + ttl });
    }

    pub fn revokeLease(self: *LeaseManager, lease_id: i64) !void {
        var i: usize = 0;
        while (i < self.leases.items.len) : (i += 1) if (self.leases.items[i].id == lease_id) {
            _ = self.leases.orderedRemove(i);
            var k: usize = 0;
            while (k < self.key_links.items.len) {
                if (self.key_links.items[k].lease_id == lease_id) {
                    const link = self.key_links.orderedRemove(k);
                    self.allocator.free(link.key);
                } else k += 1;
            }
            return;
        };
        return error.LeaseNotFound;
    }

    pub fn keepAlive(self: *LeaseManager, lease_id: i64) !void {
        for (self.leases.items) |*lease| if (lease.id == lease_id) {
            lease.expires_at = nowSeconds() + lease.ttl;
            return;
        };
        return error.LeaseNotFound;
    }

    pub fn isExpired(self: *LeaseManager, lease_id: i64) !bool {
        for (self.leases.items) |lease| if (lease.id == lease_id) return lease.expires_at <= nowSeconds();
        return error.LeaseNotFound;
    }

    /// Remaining TTL in seconds, floored at zero, and the granted TTL the
    /// lease was created with. Reports LeaseNotFound for an unknown lease so
    /// LeaseTimeToLive can answer from real state instead of a constant.
    pub fn leaseTTL(self: *LeaseManager, lease_id: i64) !struct { ttl: i64, granted_ttl: i64 } {
        for (self.leases.items) |lease| {
            if (lease.id != lease_id) continue;
            const remaining = lease.expires_at - nowSeconds();
            return .{ .ttl = if (remaining > 0) remaining else 0, .granted_ttl = lease.ttl };
        }
        return error.LeaseNotFound;
    }

    /// Every live lease, for LeaseLeases.
    pub fn allLeases(self: *LeaseManager) ![]Lease {
        return self.leases.items;
    }

    pub fn expiredLeases(self: *LeaseManager) ![]i64 {
        var result = std.ArrayList(i64).empty;
        const now = nowSeconds();
        for (self.leases.items) |lease| if (lease.expires_at <= now) try result.append(self.allocator, lease.id);
        return result.toOwnedSlice(self.allocator);
    }

    pub fn leaseKeys(self: *LeaseManager, lease_id: i64) ![][]const u8 {
        var result = std.ArrayList([]const u8).empty;
        for (self.key_links.items) |link| {
            if (link.lease_id == lease_id) {
                try result.append(self.allocator, try self.allocator.dupe(u8, link.key));
            }
        }
        return result.toOwnedSlice(self.allocator);
    }

    pub fn attachKey(self: *LeaseManager, lease_id: i64, key: []const u8) !void {
        for (self.leases.items) |lease| {
            if (lease.id == lease_id) {
                for (self.key_links.items) |link| if (link.lease_id == lease_id and std.mem.eql(u8, link.key, key)) return;
                try self.key_links.append(self.allocator, .{ .lease_id = lease_id, .key = try self.allocator.dupe(u8, key) });
                return;
            }
        }
        return error.LeaseNotFound;
    }

    pub fn detachKey(self: *LeaseManager, lease_id: i64, key: []const u8) void {
        var i: usize = 0;
        while (i < self.key_links.items.len) {
            const link = self.key_links.items[i];
            if (link.lease_id == lease_id and std.mem.eql(u8, link.key, key)) {
                const removed = self.key_links.orderedRemove(i);
                self.allocator.free(removed.key);
            } else i += 1;
        }
    }
};

/// Bring a data directory written by an earlier build up to the current lease
/// schema. `CREATE TABLE IF NOT EXISTS` does not add a column to a table that
/// already exists, so the owner column has to be added explicitly or every
/// statement naming it would fail on restart against an existing data dir.
///
/// The column is only added when it is absent, because re-running the ALTER
/// against an already-migrated table fails with "duplicate column name" and
/// would abort startup.
fn migrateLeases(allocator: Allocator, client: *rqlite.RqliteClient) !void {
    const present = try client.query(rqlite.SqlBuilder.leasesOwnerColumnSql());
    defer present.deinit(allocator);
    if (present.values.len != 1 or present.values[0].len != 1) return error.InvalidLeaseSchema;
    if (present.values[0][0].integer != 0) return;
    _ = try client.execute(rqlite.SqlBuilder.migrationSql());
}

fn nowSeconds() i64 {
    var ts: std.posix.timespec = undefined;
    _ = std.posix.system.clock_gettime(.REALTIME, &ts);
    return @intCast(ts.sec);
}

// ─── Tests ───────────────────────────────────────────────────

const testing = std.testing;

test "storage init" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();

    try testing.expectEqual(@as(i64, 0), storage.current_revision);
    try testing.expectEqual(@as(u64, 0), storage.raft_term);
}

test "storage initSchema" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    try storage.initSchema();
}

test "storage range" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    const result = try storage.range("", "", 0, 0, false, false);
    try testing.expectEqual(@as(i64, 0), result.count);
}

test "storage put" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    const result = try storage.put("key", "value", 0, false);
    try testing.expectEqual(null, result.prev_kv);
}

test "storage delete" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    const result = try storage.deleteRange("key", "", false);
    try testing.expectEqual(@as(i64, 0), result.deleted);
}

test "storage txn" {
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    const data = TxnData{
        .compare = &[_]rqlite.CompareOperation{},
        .success = &[_]rqlite.TxnOp{},
        .failure = &[_]rqlite.TxnOp{},
    };
    const result = try storage.txn(data);
    try testing.expectEqual(true, result.succeeded);
}

test "revision management" {
    var rev = Revision.init();
    try testing.expectEqual(@as(i64, 0), rev.peek());
    try testing.expectEqual(@as(i64, 1), rev.next());
    try testing.expectEqual(@as(i64, 2), rev.next());
    try testing.expectEqual(@as(i64, 2), rev.peek());
}

test "watch registry create and cancel" {
    var registry = WatchRegistry.init(testing.allocator);
    defer registry.deinit();

    const id1 = try registry.createWatch("foo", "", 0);
    const id2 = try registry.createWatch("bar", "", 0);
    try testing.expectEqual(@as(i64, 1), id1);
    try testing.expectEqual(@as(i64, 2), id2);

    try testing.expect(registry.cancelWatch(id1));
    try testing.expect(!registry.cancelWatch(id1));
    try testing.expectEqual(@as(usize, 1), registry.watches.items.len);
}

test "watch registry min revision" {
    var registry = WatchRegistry.init(testing.allocator);
    defer registry.deinit();

    _ = try registry.createWatch("a", "", 5);
    _ = try registry.createWatch("b", "", 3);
    _ = try registry.createWatch("c", "", 10);

    try testing.expectEqual(@as(i64, 3), registry.minRevision());
}

test "watch registry matches ranges and replays from revision" {
    var registry = WatchRegistry.init(testing.allocator);
    defer registry.deinit();
    const id = try registry.createWatch("a", "z", 2);
    const watchers = try registry.matchingWatchers("m");
    defer testing.allocator.free(watchers);
    try testing.expectEqual(@as(usize, 1), watchers.len);
    const events = [_]WatchEvent{
        .{ .key = "m", .value = "old", .prev_key = "", .prev_value = "", .mod_revision = 1, .create_revision = 1, .version = 1, .event_type = .PUT },
        .{ .key = "m", .value = "new", .prev_key = "m", .prev_value = "old", .mod_revision = 2, .create_revision = 1, .version = 2, .event_type = .PUT },
    };
    const replayed = try registry.replay(id, &events, 0);
    defer testing.allocator.free(replayed);
    try testing.expectEqual(@as(usize, 1), replayed.len);
    try testing.expectEqual(@as(i64, 2), replayed[0].mod_revision);
}

test "lease manager init" {
    var lm = LeaseManager.init(testing.allocator);
    defer lm.deinit();
}

test "lease create/revoke/keepalive" {
    var lm = LeaseManager.init(testing.allocator);
    defer lm.deinit();

    try lm.createLease(1, 60);
    try lm.keepAlive(1);
    const expired = try lm.isExpired(1);
    try testing.expectEqual(false, expired);
    try lm.attachKey(1, "leased-key");
    const keys = try lm.leaseKeys(1);
    defer testing.allocator.free(keys);
    defer testing.allocator.free(keys[0]);
    try testing.expectEqualStrings("leased-key", keys[0]);
    // Copying the returned key makes the ownership boundary explicit before
    // revoke removes the lease association.
    try lm.revokeLease(1);
    try testing.expectError(error.LeaseNotFound, lm.isExpired(1));
}

test "lease expired leases placeholder" {
    var lm = LeaseManager.init(testing.allocator);
    defer lm.deinit();
    const expired = try lm.expiredLeases();
    try testing.expectEqual(@as(usize, 0), expired.len);
}

test "lease keys placeholder" {
    var lm = LeaseManager.init(testing.allocator);
    defer lm.deinit();
    const keys = try lm.leaseKeys(1);
    try testing.expectEqual(@as(usize, 0), keys.len);
}

test "a historical read at or below the compaction watermark reports Compacted" {
    // Without the guard the underlying history rows are simply gone, so the
    // read returned an empty success that looked like a missing key.
    var storage = Storage.initMemory(testing.allocator);
    defer storage.deinit();
    _ = try storage.put("a", "1", 0, false);
    _ = try storage.put("a", "2", 0, false);
    try storage.compact(2);

    try testing.expectError(error.Compacted, storage.range("a", "", 0, 1, false, false));
    try testing.expectError(error.Compacted, storage.range("a", "", 0, 2, false, false));
    // Reads of the current state still work; memoryRange returns owned keys.
    const current = try storage.range("a", "", 0, 0, false, false);
    for (current.kvs) |kv| {
        testing.allocator.free(kv.key);
        testing.allocator.free(kv.value);
    }
    testing.allocator.free(current.kvs);
}
