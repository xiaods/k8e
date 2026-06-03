const std = @import("std");

/// Resolve a request path to an absolute, null-terminated filesystem path.
/// Absolute paths (leading "/") are used verbatim; relative paths are rooted
/// at /workspace. The returned slice is sentinel-terminated and its .len does
/// NOT include the terminator, so it can be passed straight to syscalls via
/// .ptr. Caller owns the memory.
pub fn resolveWorkspacePath(allocator: std.mem.Allocator, path: []const u8) ![:0]u8 {
    if (std.mem.startsWith(u8, path, "/")) {
        return allocator.dupeZ(u8, path);
    }
    return std.fmt.allocPrintSentinel(allocator, "/workspace/{s}", .{path}, 0);
}
