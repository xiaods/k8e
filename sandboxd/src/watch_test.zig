const std = @import("std");
const builtin = @import("builtin");
const watch = @import("watch.zig");

// Watch event-type mapping (pure logic, no inotify needed — runs anywhere).
test "watch: event type mapping" {
    try std.testing.expectEqual(@as(u8, 1), watch.eventTypeOf(std.os.linux.IN.CREATE)); // CREATE
    try std.testing.expectEqual(@as(u8, 2), watch.eventTypeOf(std.os.linux.IN.MODIFY)); // WRITE
    try std.testing.expectEqual(@as(u8, 3), watch.eventTypeOf(std.os.linux.IN.DELETE)); // REMOVE
    try std.testing.expectEqual(@as(u8, 4), watch.eventTypeOf(std.os.linux.IN.MOVED_FROM)); // RENAME
    try std.testing.expectEqual(@as(u8, 4), watch.eventTypeOf(std.os.linux.IN.MOVED_TO)); // RENAME
    try std.testing.expectEqual(@as(u8, 5), watch.eventTypeOf(std.os.linux.IN.ATTRIB)); // CHMOD
    try std.testing.expectEqual(@as(u8, 0), watch.eventTypeOf(std.os.linux.IN.ACCESS)); // unmapped
}

// Watcher-id query parsing (pure logic).
test "watch: watcher_id query parse" {
    const id = watch.parseWatcherIdQuery("watcher_id=7");
    try std.testing.expect(id != null);
    try std.testing.expectEqualStrings("7", id.?);
    try std.testing.expect(watch.parseWatcherIdQuery("foo=1") == null);
}

// --- inotify record parsing ---------------------------------------------
//
// inotify_event.len is the NAME length (including the NUL), not the record
// length. Reading it as the record length made every event report an
// empty name and desynchronised the event stream, so the SDK saw "the
// watched directory changed" instead of "a.txt was created".

/// Build one raw inotify record the way the kernel writes it: a fixed
/// header followed by the NUL-terminated name, padded so the record
/// length is a multiple of 4.
fn inotifyRecord(wd: i32, mask: u32, name: []const u8) []u8 {
    const header_len = @sizeOf(std.os.linux.inotify_event);
    const name_len = name.len + 1; // the NUL counts
    const padded = (name_len + 3) & ~@as(usize, 3);
    const rec_len = header_len + padded;

    var buf = std.testing.allocator.alloc(u8, rec_len) catch unreachable;
    defer std.testing.allocator.free(buf);
    @memset(buf, 0);

    const ev = @as(*align(1) std.os.linux.inotify_event, @ptrCast(&buf[0]));
    ev.wd = wd;
    ev.mask = mask;
    ev.cookie = 0;
    ev.len = @intCast(padded);
    @memcpy(buf[header_len..][0..name.len], name);

    // Copy into a fresh slice the caller owns.
    const out = std.testing.allocator.alloc(u8, rec_len) catch unreachable;
    @memcpy(out, buf);
    return out;
}

const CollectedEvent = struct {
    wd: i32,
    mask: u32,
    name: []u8,
};

fn collect(out: *std.array_list.Managed(CollectedEvent), rec: watch.InotifyRecord) void {
    out.append(.{ .wd = rec.wd, .mask = rec.mask, .name = @constCast(rec.name) }) catch unreachable;
}

test "watch: parses a short file name" {
    // "a.txt" is 5 bytes - well under the 16-byte header. The old parser
    // reported an empty name for every name this short.
    const rec = inotifyRecord(3, std.os.linux.IN.CREATE, "a.txt");
    defer std.testing.allocator.free(rec);

    var out = std.array_list.Managed(CollectedEvent).init(std.testing.allocator);
    defer out.deinit();

    watch.parseInotifyRecords(rec, &out, collect);

    try std.testing.expectEqual(@as(usize, 1), out.items.len);
    try std.testing.expectEqualStrings("a.txt", out.items[0].name);
    try std.testing.expectEqual(@as(i32, 3), out.items[0].wd);
    try std.testing.expectEqual(std.os.linux.IN.CREATE, out.items[0].mask);
}

test "watch: parses a name longer than the header" {
    const long = "a-fairly-long-file-name-indeed.bin";
    const rec = inotifyRecord(1, std.os.linux.IN.MODIFY, long);
    defer std.testing.allocator.free(rec);

    var out = std.array_list.Managed(CollectedEvent).init(std.testing.allocator);
    defer out.deinit();

    watch.parseInotifyRecords(rec, &out, collect);

    try std.testing.expectEqual(@as(usize, 1), out.items.len);
    try std.testing.expectEqualStrings(long, out.items[0].name);
}

test "watch: parses a stream of records without losing sync" {
    // The old parser advanced the cursor by the name length instead of the
    // record length, so the second record was read from the wrong offset.
    const names = [_][]const u8{ "a.txt", "b.txt", "c.txt" };
    var total: usize = 0;
    var parts: [3][]u8 = undefined;
    for (names, 0..) |n, i| {
        parts[i] = inotifyRecord(@intCast(i + 1), std.os.linux.IN.CREATE, n);
        total += parts[i].len;
    }
    defer for (parts) |p| std.testing.allocator.free(p);

    const stream = std.testing.allocator.alloc(u8, total) catch unreachable;
    defer std.testing.allocator.free(stream);
    var off: usize = 0;
    for (parts) |p| {
        @memcpy(stream[off..][0..p.len], p);
        off += p.len;
    }

    var out = std.array_list.Managed(CollectedEvent).init(std.testing.allocator);
    defer out.deinit();

    watch.parseInotifyRecords(stream, &out, collect);

    try std.testing.expectEqual(@as(usize, 3), out.items.len);
    for (names, 0..) |n, i| {
        try std.testing.expectEqualStrings(n, out.items[i].name);
        try std.testing.expectEqual(@as(i32, @intCast(i + 1)), out.items[i].wd);
    }
}

test "watch: an event about the watched object itself has no name" {
    // len == 0: the event is about the watch target, not an entry in it.
    const rec = inotifyRecord(5, std.os.linux.IN.ATTRIB, "");
    defer std.testing.allocator.free(rec);

    var out = std.array_list.Managed(CollectedEvent).init(std.testing.allocator);
    defer out.deinit();

    watch.parseInotifyRecords(rec, &out, collect);

    try std.testing.expectEqual(@as(usize, 1), out.items.len);
    try std.testing.expectEqualStrings("", out.items[0].name);
}
