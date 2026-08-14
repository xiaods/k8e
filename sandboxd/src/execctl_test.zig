const std = @import("std");
const execctl = @import("execctl.zig");

// Ring buffer append + attach round-trip: what we append is what attach
// returns, in order.
test "appendOutput then attachOutput returns the data" {
    const pid: std.os.linux.pid_t = 12345;
    execctl.registerWithConfig(pid, -1, "echo hi");

    defer execctl.unregister(pid);
    execctl.appendOutput(pid, "hello ");
    execctl.appendOutput(pid, "world");

    var buf: [1024]u8 = undefined;
    const n = execctl.attachOutput(pid, &buf);
    try std.testing.expectEqualStrings("hello world", buf[0..n]);
}

// Ring buffer keeps only the tail when more than BUFFER_MAX is appended.
test "ring buffer drops the oldest when full" {
    const pid: std.os.linux.pid_t = 12346;
    execctl.registerWithConfig(pid, -1, "cat");
    defer execctl.unregister(pid);

    // Append more than the buffer size.
    const chunk = [_]u8{'x'} ** 1024;
    var total: usize = 0;
    while (total < 70 * 1024) : (total += 1024) {
        execctl.appendOutput(pid, &chunk);
    }

    var buf: [1024]u8 = undefined;
    const n = execctl.attachOutput(pid, &buf);
    // Must be capped at BUFFER_MAX (64 KiB).
    try std.testing.expect(n <= 64 * 1024);
    // All surviving bytes are 'x'.
    for (buf[0..n]) |b| {
        try std.testing.expectEqual('x', b);
    }
}

// attachOutput on an unknown pid returns 0.
test "attachOutput unknown pid returns 0" {
    var buf: [16]u8 = undefined;
    try std.testing.expectEqual(@as(usize, 0), execctl.attachOutput(999999, &buf));
}

// isDone flips after markDone.
test "markDone flips isDone" {
    const pid: std.os.linux.pid_t = 12347;
    execctl.registerWithConfig(pid, -1, "sleep");
    defer execctl.unregister(pid);

    try std.testing.expect(!execctl.isDone(pid));
    execctl.markDone(pid);
    try std.testing.expect(execctl.isDone(pid));
    // Unknown pid is treated as done (gone).
    try std.testing.expect(execctl.isDone(999999));
}
