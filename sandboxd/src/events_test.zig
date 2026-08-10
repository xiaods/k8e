const std = @import("std");
const builtin = @import("builtin");
const events = @import("events.zig");

// Event stream tests use raw Linux syscalls (open/write); Linux-only like
// transcript tests (SkipZigTest on macOS hosts).
const TEST_DIR = "/tmp/k8e-events-test";

fn setup() void {
    _ = std.os.linux.mkdir(@ptrCast(TEST_DIR.ptr), 0o755);
}

fn teardown() void {
    _ = std.os.linux.unlink("/tmp/k8e-events-test/events.ndjson");
    _ = std.os.linux.unlink("/tmp/k8e-events-test/events.ndjson.1");
    _ = std.os.linux.rmdir(@ptrCast(TEST_DIR.ptr));
}

fn readFile(path: []const u8, buf: []u8) []const u8 {
    var path_z_buf: [512]u8 = undefined;
    if (path.len >= path_z_buf.len) return buf[0..0];
    @memcpy(path_z_buf[0..path.len], path);
    path_z_buf[path.len] = 0;
    const path_z: [*:0]const u8 = @ptrCast(&path_z_buf);
    const fd_raw = std.os.linux.open(path_z, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return buf[0..0];
    defer _ = std.os.linux.close(@intCast(fd));
    const n = std.os.linux.read(@intCast(fd), buf.ptr, buf.len);
    if (n < 0) return buf[0..0];
    return buf[0..@as(usize, @intCast(n))];
}

test "appendAt writes NDJSON line" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var gpa = std.heap.DebugAllocator(.{}){};
    const allocator = gpa.allocator();
    defer _ = gpa.deinit();

    setup();
    defer teardown();

    events.appendAt(TEST_DIR, "sess-1", "exec_end", ",\"exit\":0");
    var buf: [1024]u8 = undefined;
    const content = readFile("/tmp/k8e-events-test/events.ndjson", &buf);
    try std.testing.expect(content.len > 0);
    try std.testing.expect(std.mem.indexOf(u8, content, "exec_end") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "sess-1") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "\"exit\":0") != null);
    _ = allocator;
}

test "appendAt two events both present" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var gpa = std.heap.DebugAllocator(.{}){};
    const allocator = gpa.allocator();
    defer _ = gpa.deinit();

    setup();
    defer teardown();

    events.appendAt(TEST_DIR, "sess-1", "exec_end", "");
    events.appendAt(TEST_DIR, "sess-1", "bg_submit", ",\"run_id\":\"r1\"");
    var buf: [2048]u8 = undefined;
    const content = readFile("/tmp/k8e-events-test/events.ndjson", &buf);
    try std.testing.expect(std.mem.indexOf(u8, content, "exec_end") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "bg_submit") != null);
    try std.testing.expect(std.mem.indexOf(u8, content, "r1") != null);
    _ = allocator;
}
