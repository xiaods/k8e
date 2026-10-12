// Integration tests for Tandem
// Tests the full stack: KV service + MVCC storage + rqlite SQL builder

const std = @import("std");
const Allocator = std.mem.Allocator;

// Import from sibling directories using relative paths
const mvcc = @import("storage/mvcc.zig");
const rqlite = @import("storage/rqlite.zig");

const testing = std.testing;

// ─── Integration Test Context ────────────────────────────────────────────────

pub const TestContext = struct {
    allocator: Allocator,
    storage: mvcc.Storage,
    revision: mvcc.Revision,
    watch_registry: mvcc.WatchRegistry,
    lease_manager: mvcc.LeaseManager,

    pub fn init(allocator: Allocator) !TestContext {
        return .{
            .allocator = allocator,
            .storage = mvcc.Storage.initMemory(allocator),
            .revision = mvcc.Revision.init(),
            .watch_registry = mvcc.WatchRegistry.init(allocator),
            .lease_manager = mvcc.LeaseManager.init(allocator),
        };
    }

    pub fn deinit(self: *TestContext) void {
        self.storage.deinit();
        self.watch_registry.deinit();
        self.lease_manager.deinit();
    }
};

// ─── Tests ───────────────────────────────────────────────────────────────────

test "integration: storage + revision" {
    var ctx = try TestContext.init(testing.allocator);
    defer ctx.deinit();

    // Test that storage and revision work together
    try testing.expectEqual(@as(i64, 0), ctx.revision.peek());
    try testing.expectEqual(@as(i64, 1), ctx.revision.next());
    try testing.expectEqual(@as(i64, 2), ctx.revision.next());
}

test "integration: watch registry + storage" {
    var ctx = try TestContext.init(testing.allocator);
    defer ctx.deinit();

    // Create a watcher
    const watch_id = try ctx.watch_registry.createWatch("test_key", "", 0);
    try testing.expectEqual(@as(i64, 1), watch_id);

    // Verify watcher exists
    const watchers = try ctx.watch_registry.matchingWatchers("test_key");
    defer testing.allocator.free(watchers);
    try testing.expectEqual(@as(usize, 1), watchers.len);

    // Cancel watcher
    try testing.expect(ctx.watch_registry.cancelWatch(watch_id));
}

test "integration: lease manager + storage" {
    var ctx = try TestContext.init(testing.allocator);
    defer ctx.deinit();

    // Create a lease
    try ctx.lease_manager.createLease(1, 60);
    try ctx.lease_manager.keepAlive(1);

    // Check not expired
    const expired = try ctx.lease_manager.isExpired(1);
    try testing.expectEqual(false, expired);

    // Revoke lease
    try ctx.lease_manager.revokeLease(1);
}

test "integration: sql builder + mvcc" {
    const allocator = testing.allocator;

    // Build a range SQL
    const range_sql = try rqlite.SqlBuilder.rangeSql(allocator, "a", "z", 100, 0, false, false);
    defer allocator.free(range_sql);
    try testing.expect(std.mem.indexOf(u8, range_sql, "SELECT") != null);

    // Build a put SQL
    const put_sql = try rqlite.SqlBuilder.putSql(allocator, "key", "value", 0);
    defer allocator.free(put_sql);
    try testing.expect(std.mem.indexOf(u8, put_sql, "ON CONFLICT(key) DO UPDATE") != null);

    // Build a delete SQL
    const delete_sql = try rqlite.SqlBuilder.deleteRangeSql(allocator, "a", "z");
    defer allocator.free(delete_sql);
    try testing.expect(std.mem.indexOf(u8, delete_sql, "DELETE FROM kv") != null);
}

test "integration: compare operation in txn context" {
    // Test that compare operations work correctly for txn
    const kv = mvcc.MVCCKeyValue{
        .key = "test",
        .value = "value",
        .create_revision = 1,
        .mod_revision = 2,
        .version = 2,
        .lease = 0,
    };

    // Create a compare for version == 2
    const cmp = rqlite.CompareOperation{
        .key = "test",
        .target = .version,
        .result = .equal,
        .value = &[_]u8{},
    };

    // Should evaluate to false (version is 2, target is 0)
    try testing.expect(cmp.evaluate(rqlite.MvccKv{
        .key = kv.key,
        .value = kv.value,
        .create_revision = kv.create_revision,
        .mod_revision = kv.mod_revision,
        .version = kv.version,
        .lease = kv.lease,
    }) == false);
}

test "integration: full mvcc lifecycle" {
    var ctx = try TestContext.init(testing.allocator);
    defer ctx.deinit();

    // 1. Schema init
    try ctx.storage.initSchema();

    // 2. Range (empty)
    const range_result = try ctx.storage.range("", "", 0, 0, false, false);
    try testing.expectEqual(@as(i64, 0), range_result.count);

    // 3. Put
    const put_result = try ctx.storage.put("key1", "value1", 0, false);
    try testing.expectEqual(null, put_result.prev_kv);

    // 4. Range again
    const range_result2 = try ctx.storage.range("", "", 0, 0, false, false);
    defer {
        for (range_result2.kvs) |kv| {
            testing.allocator.free(kv.key);
            testing.allocator.free(kv.value);
        }
        testing.allocator.free(range_result2.kvs);
    }
    try testing.expectEqual(@as(i64, 1), range_result2.count);

    // 5. Delete
    const delete_result = try ctx.storage.deleteRange("key1", "", false);
    try testing.expectEqual(@as(i64, 1), delete_result.deleted);

    // 6. Compact
    try testing.expectError(error.FutureRevision, ctx.storage.compact(100));
    try ctx.storage.compact(ctx.storage.currentRevision());
}
