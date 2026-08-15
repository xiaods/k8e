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
    // STATX_MTIME = 0x40 = bit 6.
    const stx_mask: u32 = @bitCast(stx.mask);
    const mtime_bit: u32 = 1 << 6;
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

// ─── Native filesystem operations (KIP-18 "ability downshift") ────────────
//
// These replace the e2b-server's shell-command workarounds (stat/mkdir/mv/rm
// via /bin/sh) with native syscalls: faster, no shell-injection surface, and
// the e2b layer can speak to them over the same JSON contract it already
// uses for write/read/list.

const PathRequest = struct {
    path: []const u8 = "",
};

const MoveRequest = struct {
    source: []const u8 = "",
    destination: []const u8 = "",
};

/// POST /files/stat {"path": "..."} → 200 {"type","size","mode","uid","gid","mtime","name"}
pub fn handleStat(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(PathRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;
    if (req.path.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    }
    const full = try path_util.resolveWorkspacePath(allocator, req.path);
    defer allocator.free(full);

    var stx: std.os.linux.Statx = undefined;
    const rc = std.os.linux.statx(std.os.linux.AT.FDCWD, full.ptr, 0, std.os.linux.STATX.BASIC_STATS, &stx);
    if (rc != 0) {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"not found\"}");
        return;
    }
    const mode: u16 = stx.mode;
    const mode_u32: u32 = mode;
    const ftype = switch (mode_u32 & std.os.linux.S.IFMT) {
        std.os.linux.S.IFREG => "file",
        std.os.linux.S.IFDIR => "dir",
        std.os.linux.S.IFLNK => "symlink",
        else => "other",
    };
    // For symlinks, resolve the target with readlink (E2B EntryInfo
    // symlink_target; KIP-18 P2).
    var symlink_target: []const u8 = "";
    var target_buf: [4096]u8 = undefined;
    if (std.mem.eql(u8, ftype, "symlink")) {
        // readlink returns a raw syscall usize (negative errno when it
        // fails); treat values that fit an isize < 0 as failure.
        const path_z: [*:0]const u8 = full.ptr;
        const raw = std.os.linux.readlink(path_z, &target_buf, target_buf.len);
        const signed: isize = @bitCast(raw);
        if (signed >= 0) {
            symlink_target = target_buf[0..@as(usize, @intCast(signed))];
        }
    }
    const target_escaped = try exec.jsonEscape(allocator, symlink_target);
    defer allocator.free(target_escaped);

    const resp = try std.fmt.allocPrint(allocator,
        "{{\"type\":\"{s}\",\"size\":{d},\"mode\":\"{o}\",\"uid\":{d},\"gid\":{d},\"mtime\":{d},\"name\":\"{s}\",\"symlink_target\":\"{s}\"}}",
        .{ ftype, stx.size, mode_u32 & 0o7777, stx.uid, stx.gid, stx.mtime.sec, req.path, target_escaped });
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

/// POST /files/mkdir {"path": "..."} → 200 {"ok":true} (or 409 already exists)
pub fn handleMkdir(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(PathRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;
    if (req.path.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    }
    const full = try path_util.resolveWorkspacePath(allocator, req.path);
    defer allocator.free(full);

    const rc = std.os.linux.mkdir(full.ptr, 0o755);
    if (rc == 0) {
        try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
        return;
    }
    const err = std.posix.errno(@as(usize, @bitCast(@as(isize, @intCast(rc)))));
    if (err == .EXIST) {
        try main.writeResponse(client_fd, "409 Conflict", "application/json", "{\"error\":\"already exists\"}");
        return;
    }
    try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"mkdir failed\"}");
}

/// POST /files/move {"source": "...", "destination": "..."} → 200 {"ok":true}
pub fn handleMove(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(MoveRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;
    if (req.source.len == 0 or req.destination.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"source and destination required\"}");
        return;
    }
    const src = try path_util.resolveWorkspacePath(allocator, req.source);
    defer allocator.free(src);
    const dst = try path_util.resolveWorkspacePath(allocator, req.destination);
    defer allocator.free(dst);

    const rc = std.os.linux.rename(src.ptr, dst.ptr);
    if (rc == 0) {
        try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
        return;
    }
    const err = std.posix.errno(@as(usize, @bitCast(@as(isize, @intCast(rc)))));
    if (err == .NOENT) {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"source not found\"}");
        return;
    }
    try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"move failed\"}");
}

/// POST /files/remove {"path": "..."} → 200 {"ok":true}. Removes files and
/// empty directories; non-empty directories are removed recursively (rm -r
/// semantics, matching the E2B SDK's files.remove).
pub fn handleRemove(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(PathRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;
    if (req.path.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    }
    const full = try path_util.resolveWorkspacePath(allocator, req.path);
    defer allocator.free(full);
    // Null-terminated copy for the recursive walker.
    const full_z = try allocator.dupeZ(u8, full);
    defer allocator.free(full_z);

    if (removeRecursive(full_z)) {
        try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
    } else {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"not found\"}");
    }
}

/// removeRecursive removes a file, empty dir, or non-empty dir tree.
/// Returns false when the path does not exist.
pub fn removeRecursive(path_z: [:0]u8) bool {
    var stx: std.os.linux.Statx = undefined;
    const st_rc = std.os.linux.statx(std.os.linux.AT.FDCWD, path_z.ptr, std.os.linux.AT.SYMLINK_NOFOLLOW, std.os.linux.STATX.BASIC_STATS, &stx);
    if (st_rc != 0) return false;
    const mode_u32: u32 = stx.mode;
    if (mode_u32 & std.os.linux.S.IFMT == std.os.linux.S.IFDIR) {
        // Walk and remove children first (deepest-first).
        const fd = std.os.linux.open(path_z.ptr, std.os.linux.O{ .ACCMODE = .RDONLY, .DIRECTORY = true }, 0);
        if (fd >= 0) {
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
                    var child_buf: [4200]u8 = undefined;
                    const child = std.fmt.bufPrintZ(&child_buf, "{s}/{s}", .{ path_z, name }) catch continue;
                    _ = removeRecursive(child);
                }
            }
            _ = std.os.linux.close(@intCast(fd));
        }
        _ = std.os.linux.rmdir(path_z.ptr);
    } else {
        _ = std.os.linux.unlink(path_z.ptr);
    }
    return true;
}
