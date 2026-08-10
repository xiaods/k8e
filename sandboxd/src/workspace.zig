const std = @import("std");
const main = @import("main.zig");

/// handleReset removes all files/dirs under /workspace (or a scoped subdir
/// when ?path=<sub> is given, KIP-16 M1 slice 2 — per-session isolation),
/// then recreates the directory. POST /workspace/reset
pub fn handleReset(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    var scope: []const u8 = "";
    var params = std.mem.splitScalar(u8, query, '&');
    while (params.next()) |pair| {
        var kv = std.mem.splitScalar(u8, pair, '=');
        const key = kv.next() orelse continue;
        const val = kv.next() orelse continue;
        if (std.mem.eql(u8, key, "path")) {
            scope = val;
        }
    }

    // Scope root: /workspace or /workspace/<scope>.
    const workspace_path = "/workspace";
    var scope_buf: [512]u8 = undefined;
    const reset_root = if (scope.len > 0)
        std.fmt.bufPrint(&scope_buf, "{s}/{s}", .{ workspace_path, scope }) catch workspace_path
    else
        workspace_path;

    // Collect all paths to delete by walking the directory
    var paths = std.array_list.Managed([]const u8).init(allocator);
    defer {
        for (paths.items) |p| allocator.free(p);
        paths.deinit();
    }
    const sub_prefix: []const u8 = if (scope.len > 0) scope else "";
    collectPaths(allocator, &paths, reset_root, sub_prefix) catch |err| {
        if (err == error.FileNotFound or err == error.NotDir) {
            // The reset root doesn't exist — recreate (or leave scoped absent)
            if (scope.len == 0) {
                _ = std.os.linux.mkdir(@ptrCast(workspace_path.ptr), 0o755);
            }
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
        const full_path = try std.fmt.allocPrint(allocator, "{s}/{s}", .{ reset_root, entry });
        defer allocator.free(full_path);
        const full_z = try allocator.dupeZ(u8, full_path);
        defer allocator.free(full_z);
        // Try rmdir (for directories), then unlink (for files)
        _ = std.os.linux.rmdir(full_z.ptr);
        _ = std.os.linux.unlink(full_z.ptr);
    }

    // Recreate the reset root (only for the unscoped /workspace; scoped roots
    // stay absent so the session appears fresh).
    if (scope.len == 0) {
        _ = std.os.linux.mkdir(@ptrCast(workspace_path.ptr), 0o755);
    }
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

    const fd = std.os.linux.open(full_z.ptr, std.os.linux.O{ .ACCMODE = .RDONLY, .DIRECTORY = true }, 0);
    if (fd < 0) {
        const err = @as(std.posix.E, @enumFromInt(-fd));
        return switch (err) {
            .NOENT, .NOTDIR => error.FileNotFound,
            else => error.OpenFailed,
        };
    }
    defer _ = std.os.linux.close(@intCast(fd));

    var buf: [4096]u8 align(@alignOf(std.os.linux.dirent64)) = undefined;
    while (true) {
        const n = std.os.linux.getdents64(@intCast(fd), @ptrCast(&buf), buf.len);
        if (n <= 0) break;
        var pos: usize = 0;
        while (pos < @as(usize, @intCast(n))) {
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
