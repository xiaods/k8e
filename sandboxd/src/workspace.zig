const syscall = @import("syscall.zig");
const std = @import("std");
const main = @import("main.zig");
const background = @import("background.zig");

/// handleReset removes all files/dirs under /workspace using raw syscalls,
/// then recreates the directory. POST /workspace/reset
///
/// Processes are reset too: the pod goes back to the warm pool after this, so a
/// leftover background run — e.g. a server still holding its port — would leak
/// into the next tenant's session. Kill them before deleting their state files.
pub fn handleReset(allocator: std.mem.Allocator, client_fd: i32) !void {
    const workspace_path = "/workspace";

    background.killAllRuns(allocator);

    // Collect all paths to delete by walking the directory
    var paths = std.array_list.Managed([]const u8).init(allocator);
    defer {
        for (paths.items) |p| allocator.free(p);
        paths.deinit();
    }
    collectPaths(allocator, &paths, workspace_path, "") catch |err| {
        if (err == error.FileNotFound or err == error.NotDir) {
            // /workspace doesn't exist or is gone — recreate and return
            _ = std.os.linux.mkdir(@ptrCast(workspace_path.ptr), 0o755);
            try sendOK(client_fd);
            return;
        }
        return sendError(client_fd, "reset: walk workspace failed");
    };

    // Delete deepest first
    var i: usize = paths.items.len;
    while (i > 0) {
        i -= 1;
        const entry = paths.items[i];
        const full_path = try std.fmt.allocPrint(allocator, "{s}/{s}", .{ workspace_path, entry });
        defer allocator.free(full_path);
        const full_z = try allocator.dupeZ(u8, full_path);
        defer allocator.free(full_z);
        // Try rmdir (for directories), then unlink (for files)
        _ = std.os.linux.rmdir(full_z.ptr);
        _ = std.os.linux.unlink(full_z.ptr);
    }

    // Recreate /workspace
    _ = std.os.linux.mkdir(@ptrCast(workspace_path.ptr), 0o755);
    try sendOK(client_fd);
}

fn collectPaths(allocator: std.mem.Allocator, paths: *std.array_list.Managed([]const u8), base: []const u8, sub: []const u8) !void {
    const full_raw = if (sub.len > 0)
        try std.fmt.allocPrint(allocator, "{s}/{s}", .{ base, sub })
    else
        try allocator.dupe(u8, base);
    defer allocator.free(full_raw);
    const full_z = try allocator.dupeZ(u8, full_raw);
    defer allocator.free(full_z);

    const fd_raw = std.os.linux.open(full_z.ptr, std.os.linux.O{ .ACCMODE = .RDONLY, .DIRECTORY = true }, 0);
    // open() returns the NEGATED errno as a usize on failure; `-fd` on the
    // raw usize is meaningless. Reinterpret the word to recover the errno.
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) {
        const err = std.os.linux.errno(fd_raw);
        return switch (err) {
            .NOENT, .NOTDIR => error.FileNotFound,
            else => error.OpenFailed,
        };
    }
    defer _ = std.os.linux.close(@intCast(fd));

    var buf: [4096]u8 align(@alignOf(std.os.linux.dirent64)) = undefined;
    while (true) {
        const n = std.os.linux.getdents64(@intCast(fd), @ptrCast(&buf), buf.len);
        const count = syscall.dirReadCount(n) orelse break;
        var pos: usize = 0;
        while (pos < count) {
            const dent = @as(*align(1) std.os.linux.dirent64, @ptrCast(&buf[pos]));
            pos += dent.reclen;
            const name = std.mem.sliceTo(@as([*:0]u8, @ptrCast(&dent.name)), 0);
            if (std.mem.eql(u8, name, ".") or std.mem.eql(u8, name, "..")) continue;

            const entry_path = if (sub.len > 0)
                try std.fmt.allocPrint(allocator, "{s}/{s}", .{ sub, name })
            else
                try allocator.dupe(u8, name);
            try paths.append(entry_path);

            if (dent.type == std.os.linux.DT.DIR) {
                const next_sub = if (sub.len > 0)
                    try std.fmt.allocPrint(allocator, "{s}/{s}", .{ sub, name })
                else
                    try allocator.dupe(u8, name);
                defer allocator.free(next_sub);
                try collectPaths(allocator, paths, base, next_sub);
            }
        }
    }
}

fn sendOK(client_fd: i32) !void {
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

fn sendError(client_fd: i32, msg: []const u8) !void {
    var body_buf: [512]u8 = undefined;
    const body = std.fmt.bufPrint(&body_buf, "{{\"error\":\"{s}\"}}", .{msg}) catch "{\"error\":\"unknown\"}";
    try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", body);
}
