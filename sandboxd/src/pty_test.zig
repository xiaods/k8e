const std = @import("std");
const pty = @import("pty.zig");

// PTY primitive tests (KIP-19 M2). These exercise Linux-only syscalls
// (/dev/ptmx, setsid, TIOCSCTTY), so they must run on a Linux kernel
// (CI linux/amd64 or an OrbStack Linux container), not macOS.

fn isErr(rc: usize) bool {
    return @as(isize, @bitCast(rc)) < 0;
}

test "openPty: allocates a master/slave pair" {
    const p = try pty.openPty();
    defer _ = std.os.linux.close(p.master);
    defer _ = std.os.linux.close(p.slave);
    try std.testing.expect(p.master > 2);
    try std.testing.expect(p.slave > 2);
}

test "spawnTerminal: runs argv as a session leader on the PTY" {
    const allocator = std.testing.allocator;
    const argv = [_][]const u8{ "/bin/sh", "-c", "printf pty-ok" };
    const term = try pty.spawnTerminal(allocator, &argv, "/tmp", .{ .null = {} }, 24, 80);
    defer _ = std.os.linux.close(term.master_fd);
    try std.testing.expect(term.pid > 1);

    // Drain the master until EOF and require the child's output (ONLCR maps
    // '\n' to '\r\n', so only assert on the stable prefix).
    var buf: [256]u8 = undefined;
    var total: usize = 0;
    while (total < buf.len) {
        const n = std.os.linux.read(term.master_fd, buf[total..].ptr, buf.len - total);
        if (n == 0) break; // EOF: slave side closed
        if (isErr(n)) break;
        total += n;
    }
    try std.testing.expect(std.mem.indexOf(u8, buf[0..total], "pty-ok") != null);
}

test "table: register, appendOutput, readTail, markDone, unregister" {
    const p = try pty.openPty();
    defer _ = std.os.linux.close(p.slave);

    const term = pty.Terminal{
        .id = 0,
        .master_fd = p.master,
        .pid = 4242,
        .rows = 24,
        .cols = 80,
    };
    const id = pty.register(term) orelse return error.TableFull;
    defer pty.unregister(id);

    pty.appendOutput(id, "hello");
    pty.appendOutput(id, "world");

    var out: [16]u8 = undefined;
    const n = pty.readTail(id, out[0..]);
    try std.testing.expectEqualStrings("helloworld", out[0..n]);

    // done is a separate flag from the buffered output.
    pty.markDone(id, 7);
    const n2 = pty.readTail(id, out[0..]);
    try std.testing.expectEqualStrings("helloworld", out[0..n2]);
}

test "table: readTail returns nothing for an unknown id" {
    var out: [8]u8 = undefined;
    const n = pty.readTail(9999, out[0..]);
    try std.testing.expectEqual(@as(usize, 0), n);
}
