const std = @import("std");
const PipelineServer = @import("pipeline.zig").PipelineServer;
const rqlite = @import("storage/rqlite.zig");

pub fn main(init: std.process.Init) !void {
    const url = init.environ_map.get("TANDEM_RQLITE_ADDR") orelse return error.MissingRqliteURL;
    const phase = init.environ_map.get("TANDEM_PROBE_PHASE") orelse return error.MissingPhase;
    const allocator = init.gpa;
    if (std.mem.eql(u8, phase, "uncertain-write")) {
        var client = try @import("storage/rqlite.zig").RqliteClient.init(allocator, url);
        defer client.deinit();
        if (client.execute("UPDATE revision SET current_revision=current_revision+1 WHERE id=1")) |_| {
            return error.ExpectedTransportFailure;
        } else |_| {}
        return;
    }
    var pipeline = try PipelineServer.initPersistent(allocator, .{ .rqlite_url = url });
    defer pipeline.deinit();
    if (std.mem.eql(u8, phase, "range")) {
        _ = try pipeline.storage.put("page/b", "second", 0, false);
        _ = try pipeline.storage.put("page/a", "first", 0, false);
        const page = try pipeline.storage.range("page/", "page0", 1, 0, false, false);
        defer {
            for (page.kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(page.kvs);
        }
        if (page.kvs.len != 1 or page.count != 2 or !page.more) return error.InvalidPage;
        if (!std.mem.eql(u8, page.kvs[0].key, "page/a")) return error.InvalidPageOrder;
        const count = try pipeline.storage.range("page/", "page0", 1, 0, false, true);
        defer allocator.free(count.kvs);
        if (count.count != 2 or count.kvs.len != 0 or count.more) return error.InvalidCount;
        return;
    }
    const key = "restart'key;\x00";
    const value = "persisted\x00\xffvalue";
    if (std.mem.eql(u8, phase, "txn-compares")) {
        // Every Compare target, end to end through a real rqlite transaction
        // rather than the predicate alone. A missing key compares as
        // version/create/mod/lease = 0 and value = empty, which is what etcd
        // does and what the create-if-absent path depends on.
        _ = try pipeline.storage.put("cmp/present", "v1", 77, false);
        // The revision is not assumed: put advances it first, and the store
        // starts at 1, so the first put lands at 2. Read the real values so
        // these cases assert the compare semantics rather than a hard-coded
        // revision.
        const stored = try pipeline.storage.range("cmp/present", "", 0, 0, false, false);
        defer {
            for (stored.kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(stored.kvs);
        }
        if (stored.kvs.len != 1) return error.CompareSetupFailed;
        const create_rev = stored.kvs[0].create_revision;
        const mod_rev = stored.kvs[0].mod_revision;
        const Case = struct {
            name: []const u8,
            compare: rqlite.CompareOperation,
            want: bool,
        };
        const cases = [_]Case{
            .{ .name = "version equal", .compare = .{ .key = "cmp/present", .target = .version, .result = .equal, .target_value = 1, .value = "" }, .want = true },
            .{ .name = "version not equal", .compare = .{ .key = "cmp/present", .target = .version, .result = .not_equal, .target_value = 99, .value = "" }, .want = true },
            .{ .name = "create equal", .compare = .{ .key = "cmp/present", .target = .create, .result = .equal, .target_value = create_rev, .value = "" }, .want = true },
            .{ .name = "mod equal", .compare = .{ .key = "cmp/present", .target = .mod, .result = .equal, .target_value = mod_rev, .value = "" }, .want = true },
            .{ .name = "lease equal", .compare = .{ .key = "cmp/present", .target = .lease, .result = .equal, .target_value = 77, .value = "" }, .want = true },
            .{ .name = "value equal", .compare = .{ .key = "cmp/present", .target = .value, .result = .equal, .target_value = 0, .value = "v1" }, .want = true },
            // A lease compare that targets the version must not be satisfied by
            // it, which is the mistake a shared accessor invites.
            .{ .name = "lease not equal version", .compare = .{ .key = "cmp/present", .target = .lease, .result = .equal, .target_value = 1, .value = "" }, .want = false },
            .{ .name = "missing create", .compare = .{ .key = "cmp/absent", .target = .create, .result = .equal, .target_value = 0, .value = "" }, .want = true },
            .{ .name = "missing value empty", .compare = .{ .key = "cmp/absent", .target = .value, .result = .equal, .target_value = 0, .value = "" }, .want = true },
            .{ .name = "missing version nonzero", .compare = .{ .key = "cmp/absent", .target = .version, .result = .equal, .target_value = 1, .value = "" }, .want = false },
        };
        for (cases) |case| {
            const result = try pipeline.storage.txn(.{
                .compare = &.{case.compare},
                .success = &.{.{ .kind = .put, .key = "cmp/out", .value = case.name, .range_end = "", .lease = 0 }},
                .failure = &.{.{ .kind = .put, .key = "cmp/out", .value = "no", .range_end = "", .lease = 0 }},
            });
            defer {
                for (result.responses) |response| {
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
                allocator.free(result.responses);
            }
            if (result.succeeded != case.want) {
                std.debug.print("compare case failed: {s}\n", .{case.name});
                return error.CompareTargetMismatch;
            }
        }
        std.debug.print("txn-compares ok {d}\n", .{cases.len});
        return;
    }
    if (std.mem.eql(u8, phase, "txn-cas")) {
        const result = try pipeline.storage.txn(.{
            .compare = &.{.{ .key = "cas", .target = .version, .result = .equal, .target_value = 0, .value = "" }},
            .success = &.{.{ .kind = .put, .key = "cas", .value = value, .range_end = "", .lease = 0 }},
            .failure = &.{},
        });
        defer allocator.free(result.responses);
        std.debug.print("CAS winner={any}\n", .{result.succeeded});
        return;
    }
    if (std.mem.eql(u8, phase, "rollback")) {
        const before = pipeline.storage.current_revision;
        const client = &pipeline.storage.client.?;
        if (client.execute("UPDATE revision SET current_revision=current_revision+1 WHERE id=1;INSERT INTO deliberately_missing_table VALUES(1);")) |_| {
            return error.ExpectedSQLFailure;
        } else |err| {
            if (err != error.RqliteError) return err;
        }
        const revision = try client.query("SELECT current_revision FROM revision WHERE id=1");
        defer revision.deinit(allocator);
        if (revision.values[0][0].integer != before) return error.TransactionDidNotRollback;
        return;
    }
    if (std.mem.eql(u8, phase, "txn-range")) {
        // A Range inside a Txn has to observe the writes of the very branch it
        // belongs to, from the same rqlite request. Reading it back afterwards
        // is what KIP-29 §6 forbids, and it also drifts: the answer can belong
        // to a later commit than the one that produced it.
        _ = try pipeline.storage.put("txn/a", "before", 0, false);
        const read = try pipeline.storage.txn(.{
            // The key already exists at version 1, so create-if-absent would
            // take the failure branch; this asserts the version instead.
            .compare = &.{.{ .key = "txn/a", .target = .version, .result = .equal, .target_value = 1, .value = "" }},
            .success = &.{
                .{ .kind = .put, .key = "txn/a", .value = "after", .range_end = "", .lease = 0 },
                .{ .kind = .range, .key = "txn/a", .value = "", .range_end = "", .lease = 0 },
                .{ .kind = .put, .key = "txn/b", .value = "second", .range_end = "", .lease = 0 },
                .{ .kind = .range, .key = "txn/", .value = "", .range_end = "txn0", .lease = 0 },
            },
            .failure = &.{.{ .kind = .range, .key = "txn/", .value = "", .range_end = "txn0", .lease = 0 }},
        });
        defer {
            for (read.responses) |response| {
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
            allocator.free(read.responses);
        }
        if (!read.succeeded) return error.TxnCompareDidNotSucceed;
        if (read.responses.len != 4) return error.TxnResponseCountMismatch;
        // The first Range follows its own branch's Put, so it must see "after".
        const single = read.responses[1].range_kvs;
        if (single.len != 1 or !std.mem.eql(u8, single[0].value, "after")) return error.TxnRangeMissedOwnWrite;
        // The prefix Range follows the Put of a second key, so it must see both
        // and report a count matching the same snapshot.
        const both = read.responses[3].range_kvs;
        if (both.len != 2) return error.TxnRangeMissingSecondKey;
        if (!std.mem.eql(u8, both[0].key, "txn/a") or !std.mem.eql(u8, both[1].key, "txn/b")) return error.TxnRangeOrder;
        if (read.responses[3].range_count != 2) return error.TxnRangeCount;
        // Both writes and both reads belong to one revision.
        if (read.responses[0].revision != read.responses[3].revision) return error.TxnRangeSplitRevision;
        const failed = try pipeline.storage.txn(.{
            .compare = &.{.{ .key = "txn/a", .target = .version, .result = .equal, .target_value = 0, .value = "" }},
            .success = &.{
                .{ .kind = .put, .key = "txn/unused", .value = "bad", .range_end = "", .lease = 0 },
                .{ .kind = .range, .key = "txn/unused", .value = "", .range_end = "", .lease = 0 },
            },
            .failure = &.{.{ .kind = .range, .key = "txn/", .value = "", .range_end = "txn0", .lease = 0 }},
        });
        defer {
            for (failed.responses) |response| {
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
            allocator.free(failed.responses);
        }
        if (failed.succeeded or failed.responses.len != 1) return error.TxnFailureBranchNotSelected;
        if (failed.responses[0].range_kvs.len != 2 or failed.responses[0].range_count != 2) return error.TxnFailureRangeMisindexed;
        std.debug.print("txn-range ok rev={d} single={d} both={d}\n", .{ read.responses[0].revision, single.len, both.len });
        return;
    }
    if (std.mem.eql(u8, phase, "lease-owner")) {
        // KIP-29: expiry runs under exactly one owner recorded in the lease
        // row, not on every instance's clock. Two owners scan the same expired
        // lease; the claim must let exactly one through, or both would delete
        // and both would advance the revision.
        try pipeline.storage.setLeaseOwner("instance-a");
        const client = &pipeline.storage.client.?;
        _ = try client.execute(
            \\INSERT OR REPLACE INTO leases(id,ttl,created_at,expires_at,owner)
            \\VALUES(5555,60,0,1,'')
        );
        const a = try pipeline.storage.expireLeasesOwned("instance-a");
        const b = try pipeline.storage.expireLeasesOwned("instance-b");
        std.debug.print("lease-owner a={d} b={d}\n", .{ a, b });
        if (b != 0) return error.SecondInstanceReapedToo;
        // The lease is gone once instance-a acted, so a later scan by either
        // owner has nothing left to do.
        const after = try pipeline.storage.expireLeasesOwned("instance-b");
        if (after != 0) return error.ExpiredLeaseReapedTwice;
        return;
    }
    if (std.mem.eql(u8, phase, "lease-release")) {
        // A lease left owned by an instance that is going away must become
        // claimable again, or a failover waits out a claim that can never
        // succeed. Releasing hands it back; a second release is a no-op.
        const client = &pipeline.storage.client.?;
        try pipeline.storage.setLeaseOwner("instance-a");
        _ = try client.execute(
            \\INSERT OR REPLACE INTO leases(id,ttl,created_at,expires_at,owner,owner_expires_at)
            \\VALUES(6666,60,0,1,'instance-a',4000000000)
        );
        // While instance-a holds it, nobody else may claim it.
        if (try pipeline.storage.expireLeasesOwned("instance-b") != 0) return error.ClaimStolenFromLiveOwner;
        try pipeline.storage.releaseLeases();
        const released = try client.query("SELECT COUNT(*) FROM leases WHERE id=6666 AND owner=''");
        defer released.deinit(allocator);
        if (released.values.len != 1 or released.values[0][0].integer != 1) return error.ReleaseDidNotFreeOwner;
        // Releasing twice must not error or change anything.
        try pipeline.storage.releaseLeases();
        std.debug.print("lease-release ok\n", .{});
        return;
    }
    if (std.mem.eql(u8, phase, "lease-takeover")) {
        try pipeline.storage.grantLease(7777, 60);
        _ = try pipeline.storage.put("takeover/key", "old", 7777, false);
        _ = try pipeline.storage.client.?.execute("UPDATE leases SET expires_at=1,owner='dead-node',owner_expires_at=1 WHERE id=7777");
        _ = try pipeline.storage.expireLeasesOwned("live-node");
        const remaining = try pipeline.storage.range("takeover/key", "", 0, 0, false, false);
        defer {
            for (remaining.kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(remaining.kvs);
        }
        if (remaining.kvs.len != 0) return error.CrashedOwnerKeptKey;
        std.debug.print("lease-takeover ok\n", .{});
        return;
    }
    if (std.mem.eql(u8, phase, "lease-ids")) {
        var peer = try PipelineServer.initPersistent(allocator, .{ .rqlite_url = url });
        defer peer.deinit();
        const first = try pipeline.storage.allocateLease(60);
        const second = try peer.storage.allocateLease(60);
        if (second != first + 1) return error.LeaseIdsCollided;
        try pipeline.storage.grantLease(9000, 60);
        const after_explicit = try peer.storage.allocateLease(60);
        if (after_explicit <= 9000) return error.LeaseSequenceDidNotAdvance;
        std.debug.print("lease-ids ok {d} {d} {d}\n", .{ first, second, after_explicit });
        return;
    }
    if (std.mem.eql(u8, phase, "standalone-prev")) {
        _ = try pipeline.storage.put("prev/a", "first", 0, false);
        _ = try pipeline.storage.put("prev/b", "other", 0, false);
        const updated = try pipeline.storage.put("prev/a", "second", 0, true);
        defer if (updated.prev_kv) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        };
        if (updated.prev_kv == null or !std.mem.eql(u8, updated.prev_kv.?.value, "first")) return error.PutPreviousValueWrong;
        const deleted = try pipeline.storage.deleteRange("prev/", "prev0", true);
        defer {
            for (deleted.prev_kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(deleted.prev_kvs);
        }
        if (deleted.deleted != 2 or deleted.prev_kvs.len != 2) return error.DeletePreviousCountWrong;
        if (!std.mem.eql(u8, deleted.prev_kvs[0].value, "second")) return error.DeletePreviousValueWrong;
        std.debug.print("standalone-prev ok\n", .{});
        return;
    }
    if (std.mem.eql(u8, phase, "write")) {
        _ = try pipeline.storage.put(key, value, 0, false);
        return;
    }
    if (std.mem.eql(u8, phase, "lease-expiry")) {
        // Grant a short lease, attach a key and let it lapse, then run the
        // reaper. The in-memory lease manager is bypassed entirely so this
        // only exercises the SQL that production runs against rqlite.
        try pipeline.storage.setLeaseOwner("instance-a");
        const leased = "leased/key\x00\xff";
        try pipeline.storage.grantLease(4242, 1);
        _ = try pipeline.storage.put(leased, value, 4242, false);
        try pipeline.storage.attachLeaseKey(4242, leased);
        const attached = try pipeline.storage.range(leased, "", 0, 0, false, false);
        defer {
            for (attached.kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(attached.kvs);
        }
        if (attached.kvs.len != 1 or attached.kvs[0].lease != 4242) return error.LeasedKeyMissing;
        // Backdate the deadline rather than sleeping for a real TTL. The value
        // must come from the same wall clock the storage layer uses, not from
        // SQL, whose `now` in a write statement is a Raft-assigned instant.
        var ts: std.posix.timespec = undefined;
        _ = std.posix.system.clock_gettime(.REALTIME, &ts);
        const past = @as(i64, @intCast(ts.sec)) - 1;
        const backdate = try std.fmt.allocPrint(allocator, "UPDATE leases SET expires_at={d} WHERE id=4242", .{past});
        defer allocator.free(backdate);
        _ = try pipeline.storage.client.?.execute(backdate);
        const deleted = try pipeline.storage.expireLeases();
        const after = try pipeline.storage.range(leased, "", 0, 0, false, false);
        defer {
            for (after.kvs) |kv| {
                allocator.free(kv.key);
                allocator.free(kv.value);
            }
            allocator.free(after.kvs);
        }
        const events = try pipeline.storage.watchHistory(attached.kvs[0].mod_revision, pipeline.storage.current_revision);
        defer pipeline.storage.freeWatchHistory(events);
        std.debug.print("lease-expiry deleted={d} remaining={d} events={d}\n", .{ deleted, after.kvs.len, events.len });
        if (after.kvs.len != 0) return error.ExpiredLeaseStillHasKey;
        // The Put recorded a live version and the expiry a tombstone, so the
        // history for this key holds both. The tombstone must be last and must
        // carry the revision the store now reports, otherwise a watch resuming
        // from the expiry would be told the store is older than its data.
        if (events.len < 2) return error.ExpiryProducedNoDeleteEvent;
        const tombstone = events[events.len - 1];
        if (tombstone.event_type != .DELETE) return error.ExpiryProducedNoDeleteEvent;
        if (tombstone.mod_revision != pipeline.storage.current_revision) return error.ExpiryRevisionNotCurrent;
        if (events[0].mod_revision != attached.kvs[0].mod_revision) return error.ExpiryRewrotePutRevision;
        return;
    }
    if (!std.mem.eql(u8, phase, "read")) return error.UnknownPhase;
    const result = try pipeline.storage.range(key, "", 0, 0, false, false);
    defer {
        for (result.kvs) |kv| {
            allocator.free(kv.key);
            allocator.free(kv.value);
        }
        allocator.free(result.kvs);
    }
    if (result.kvs.len != 1 or !std.mem.eql(u8, result.kvs[0].value, value)) return error.PersistenceLost;
    if (pipeline.storage.current_revision != result.kvs[0].mod_revision) return error.RevisionLost;
    const events = try pipeline.storage.watchHistory(result.kvs[0].mod_revision, pipeline.storage.current_revision);
    defer pipeline.storage.freeWatchHistory(events);
    if (events.len != 1 or !std.mem.eql(u8, events[0].value, value)) return error.HistoryLost;
    std.debug.print("persistent KV, revision and Watch history recovered\n", .{});
}
