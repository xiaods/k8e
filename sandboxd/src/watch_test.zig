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
