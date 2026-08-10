const std = @import("std");
const main = @import("main.zig");
const exec = @import("exec.zig");
const path_util = @import("path.zig");

const WriteRequest = struct {
    path: []const u8 = "",
    content: []const u8 = "",
    mode: []const u8 = "w",
};

pub fn handleWrite(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(WriteRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.path.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    }

    const full_path = try path_util.resolveWorkspacePath(allocator, req.path);
    defer allocator.free(full_path);

    // Ensure parent directory exists using mkdir recursion
    if (std.fs.path.dirname(full_path)) |dir| {
        mkdirRecursive(dir) catch {};
    }

    const append_mode = std.mem.eql(u8, req.mode, "a");
    const open_flags = if (append_mode)
        std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .APPEND = true }
    else
        std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true };
    const mode: u32 = 0o644;

    const fd = std.os.linux.open(full_path.ptr, open_flags, mode);
    if (fd < 0) {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"open failed\"}}", .{});
        defer allocator.free(msg);
        try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", msg);
        return;
    }
    defer _ = std.os.linux.close(@intCast(fd));

    const write_rc = std.os.linux.write(@intCast(fd), req.content.ptr, req.content.len);
    if (write_rc < 0) {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"write failed\"}}", .{});
        defer allocator.free(msg);
        try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", msg);
        return;
    }
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

fn mkdirRecursive(dir: []const u8) !void {
    // Use raw mkdir syscall on each path component
    var path_buf: [4096]u8 = undefined;
    @memcpy(path_buf[0..dir.len], dir);
    path_buf[dir.len] = 0;

    var i: usize = 0;
    while (i < dir.len) : (i += 1) {
        if (dir[i] == '/' and i > 0) {
            path_buf[i] = 0;
            _ = std.os.linux.mkdir(@ptrCast(&path_buf), 0o755);
            path_buf[i] = '/';
        }
    }
    _ = std.os.linux.mkdir(@ptrCast(&path_buf), 0o755);
}

pub fn handleRead(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    const path = extractQueryParam(query, "path") orelse {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    };

    const full_path = try path_util.resolveWorkspacePath(allocator, path);
    defer allocator.free(full_path);

    const fd = std.os.linux.open(full_path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    if (fd < 0) {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"not found\"}}", .{});
        defer allocator.free(msg);
        try main.writeResponse(client_fd, "404 Not Found", "application/json", msg);
        return;
    }
    defer _ = std.os.linux.close(@intCast(fd));

    // Read file content (up to 64MB; raised from 10MB so snapshot restore and
    // large-file reads work — see KIP-16 M7)
    const max_size: usize = 64 * 1024 * 1024;

    const content = try allocator.alloc(u8, max_size);
    defer allocator.free(content);
    const n = std.os.linux.read(@intCast(fd), content.ptr, max_size);
    const actual = if (n > 0) content[0..@as(usize, @intCast(n))] else &[_]u8{};

    const escaped = try exec.jsonEscape(allocator, actual);
    defer allocator.free(escaped);
    const resp = try std.fmt.allocPrint(allocator, "{{\"content\":\"{s}\"}}", .{escaped});
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

pub fn handleList(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    // Optional since=<unix_seconds>: only include files modified after it.
    var since: i64 = 0;
    if (extractQueryParam(query, "since")) |v| {
        since = std.fmt.parseInt(i64, v, 10) catch 0;
    }
    var entries = std.array_list.Managed(FileEntry).init(allocator);
    defer {
        for (entries.items) |e| allocator.free(e.path);
        entries.deinit();
    }

    listDirRecursive(allocator, &entries, "/workspace", "", since) catch {};

    // Build JSON array manually
    var json_buf = std.array_list.Managed(u8).init(allocator);
    defer json_buf.deinit();
    try json_buf.appendSlice("{\"files\":[");
    var first = true;
    for (entries.items) |e| {
        if (!first) try json_buf.append(',');
        first = false;
        const escaped = try exec.jsonEscape(allocator, e.path);
        defer allocator.free(escaped);
        const item = try std.fmt.allocPrint(allocator, "{{\"path\":\"{s}\",\"modified\":{d}}}", .{ escaped, e.modified });
        defer allocator.free(item);
        try json_buf.appendSlice(item);
    }
    try json_buf.appendSlice("]}");

    const resp = try json_buf.toOwnedSlice();
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

fn listDirRecursive(allocator: std.mem.Allocator, entries: *std.array_list.Managed(FileEntry), base: []const u8, sub: []const u8, since: i64) !void {
    const full_path_raw = if (sub.len > 0)
        try std.fmt.allocPrint(allocator, "{s}/{s}", .{ base, sub })
    else
        try allocator.dupe(u8, base);
    defer allocator.free(full_path_raw);
    const full_path = try allocator.dupeZ(u8, full_path_raw);
    defer allocator.free(full_path);

    const fd = std.os.linux.open(full_path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY, .DIRECTORY = true }, 0);
    if (fd < 0) return;
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
                try std.fmt.allocPrint(allocator, "/workspace/{s}/{s}", .{ sub, name })
            else
                try std.fmt.allocPrint(allocator, "/workspace/{s}", .{name});
            errdefer allocator.free(entry_path);

            if (dent.type == std.os.linux.DT.DIR) {
                const next_sub_raw = if (sub.len > 0)
                    try std.fmt.allocPrint(allocator, "{s}/{s}", .{ sub, name })
                else
                    try allocator.dupe(u8, name);
                defer allocator.free(next_sub_raw);
                try listDirRecursive(allocator, entries, base, next_sub_raw, since);
                allocator.free(entry_path);
                continue;
            }

            // Modification time via statx (real mtime, not 0 — KIP-16 M2
            // enables diff/since-based incremental snapshots).
            const mtime = fileMtime(entry_path) orelse 0;

            if (since > 0 and mtime < since) {
                allocator.free(entry_path);
                continue;
            }

            try entries.append(.{ .path = entry_path, .modified = mtime });
        }
    }
}

const FileEntry = struct {
    path: []u8,
    modified: i64,
};

/// fileMtime returns the file's last-modification time (unix seconds) via the
/// statx syscall, or null on any error. statx works on Linux targets; the
/// caller treats null as mtime 0.
pub fn fileMtime(path: []const u8) ?i64 {
    var path_z_buf: [4096]u8 = undefined;
    if (path.len >= path_z_buf.len) return null;
    @memcpy(path_z_buf[0..path.len], path);
    path_z_buf[path.len] = 0;
    const path_z: [*:0]const u8 = @ptrCast(&path_z_buf);

    var stx: std.os.linux.Statx = undefined;
    const rc = std.os.linux.statx(
        std.os.linux.AT.FDCWD,
        path_z,
        0, // follow symlinks like read does
        std.os.linux.STATX.BASIC_STATS,
        &stx,
    );
    if (rc != 0) return null;
    // Check the mask actually reports mtime (varies by kernel/filesystem).
    const stx_mask: u32 = @bitCast(stx.mask);
    const mtime_bit: u32 = 1 << 14; // STATX_MTIME
    if (stx_mask & mtime_bit == 0) return null;
    return stx.mtime.sec;
}

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
