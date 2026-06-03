const std = @import("std");
const path_util = @import("path.zig");

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
