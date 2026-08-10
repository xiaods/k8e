const std = @import("std");
const builtin = @import("builtin");
const path_util = @import("path.zig");
const files = @import("files.zig");

fn extractQueryParam(query: []const u8, key: []const u8) ?[]const u8 {
    var it = std.mem.splitScalar(u8, query, '&');
    while (it.next()) |pair| {
        var kv = std.mem.splitScalar(u8, pair, '=');
        const k = kv.next() orelse continue;
        const v = kv.next() orelse continue;
        if (std.mem.eql(u8, k, key)) return v;
    }
    return null;
}

test "extractQueryParam: single param" {
    try std.testing.expectEqualStrings("/tmp/foo.txt", extractQueryParam("path=/tmp/foo.txt", "path").?);
}

test "extractQueryParam: multiple params" {
    try std.testing.expectEqualStrings("42", extractQueryParam("since=42&limit=10", "since").?);
    try std.testing.expectEqualStrings("10", extractQueryParam("since=42&limit=10", "limit").?);
}

test "extractQueryParam: missing key returns null" {
    try std.testing.expect(extractQueryParam("since=42", "path") == null);
}

test "extractQueryParam: empty query returns null" {
    try std.testing.expect(extractQueryParam("", "path") == null);
}

test "extractQueryParam: key with empty value" {
    try std.testing.expectEqualStrings("", extractQueryParam("path=", "path").?);
}

test "resolveWorkspacePath: absolute path preserved verbatim" {
    // Regression: an earlier version clobbered the final char of absolute
    // paths, so /workspace/_k8e_run.ts was written to /workspace/_k8e_run.t
    // and `tsx /workspace/_k8e_run.ts` failed with ERR_MODULE_NOT_FOUND.
    const a = std.testing.allocator;
    const p = try path_util.resolveWorkspacePath(a, "/workspace/_k8e_run.ts");
    defer a.free(p);
    try std.testing.expectEqualStrings("/workspace/_k8e_run.ts", p);
    try std.testing.expectEqual(@as(u8, 0), p.ptr[p.len]);
}

test "resolveWorkspacePath: relative path rooted at /workspace" {
    const a = std.testing.allocator;
    const p = try path_util.resolveWorkspacePath(a, "notes.txt");
    defer a.free(p);
    try std.testing.expectEqualStrings("/workspace/notes.txt", p);
    try std.testing.expectEqual(@as(u8, 0), p.ptr[p.len]);
}

// fileMtime returns a real mtime for an existing file (Linux-only; uses the
// statx syscall which is unavailable on macOS hosts).
test "fileMtime: existing file returns non-zero mtime" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    const a = std.testing.allocator;

    // Create a temp file with a known name under /tmp.
    const path = "/tmp/k8e-files-mtime-test.txt";
    const fd = std.os.linux.open(path, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd < 0) return error.TestUnexpectedResult;
    _ = std.os.linux.write(@intCast(fd), "x", 1);
    _ = std.os.linux.close(@intCast(fd));
    defer _ = std.os.linux.unlink(path);

    const m = files.fileMtime(path) orelse return error.TestUnexpectedResult;
    // Epoch-2020 boundary sanity: any file created now is > 1_500_000_000.
    try std.testing.expect(m > 1_500_000_000);
    _ = a;
}
