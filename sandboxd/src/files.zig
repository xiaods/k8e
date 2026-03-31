const std = @import("std");
const main = @import("main.zig");
const exec = @import("exec.zig");

const WriteRequest = struct {
    path: []const u8 = "",
    content: []const u8 = "",
    mode: []const u8 = "w",
};

pub fn handleWrite(allocator: std.mem.Allocator, stream: std.net.Stream, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(WriteRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(stream, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.path.len == 0) {
        try main.writeResponse(stream, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    }

    const full_path = if (std.mem.startsWith(u8, req.path, "/"))
        try allocator.dupe(u8, req.path)
    else
        try std.fmt.allocPrint(allocator, "/workspace/{s}", .{req.path});
    defer allocator.free(full_path);

    // Ensure parent directory exists
    if (std.fs.path.dirname(full_path)) |dir| {
        std.fs.makeDirAbsolute(dir) catch {};
    }

    const append_mode = std.mem.eql(u8, req.mode, "a");
    const file = std.fs.createFileAbsolute(full_path, .{ .truncate = !append_mode }) catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(stream, "500 Internal Server Error", "application/json", msg);
        return;
    };
    defer file.close();

    if (append_mode) {
        try file.seekFromEnd(0);
    }
    try file.writeAll(req.content);
    try main.writeResponse(stream, "200 OK", "application/json", "{\"ok\":true}");
}

pub fn handleRead(allocator: std.mem.Allocator, stream: std.net.Stream, query: []const u8) !void {
    const path = extractQueryParam(query, "path") orelse {
        try main.writeResponse(stream, "400 Bad Request", "application/json", "{\"error\":\"path required\"}");
        return;
    };

    const full_path = if (std.mem.startsWith(u8, path, "/"))
        try allocator.dupe(u8, path)
    else
        try std.fmt.allocPrint(allocator, "/workspace/{s}", .{path});
    defer allocator.free(full_path);

    const file = std.fs.openFileAbsolute(full_path, .{}) catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(stream, "404 Not Found", "application/json", msg);
        return;
    };
    defer file.close();

    const content = file.readToEndAlloc(allocator, 10 * 1024 * 1024) catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(stream, "500 Internal Server Error", "application/json", msg);
        return;
    };
    defer allocator.free(content);

    const escaped = try exec.jsonEscape(allocator, content);
    defer allocator.free(escaped);
    const resp = try std.fmt.allocPrint(allocator, "{{\"content\":\"{s}\"}}", .{escaped});
    defer allocator.free(resp);
    try main.writeResponse(stream, "200 OK", "application/json", resp);
}

pub fn handleList(allocator: std.mem.Allocator, stream: std.net.Stream, query: []const u8) !void {
    const since_str = extractQueryParam(query, "since");
    const since: i64 = if (since_str) |s| std.fmt.parseInt(i64, s, 10) catch 0 else 0;

    var entries = std.array_list.Managed(FileEntry).init(allocator);
    defer {
        for (entries.items) |e| allocator.free(e.path);
        entries.deinit();
    }

    var dir = std.fs.openDirAbsolute("/workspace", .{ .iterate = true }) catch {
        try main.writeResponse(stream, "200 OK", "application/json", "{\"files\":[]}");
        return;
    };
    defer dir.close();

    var walker = try dir.walk(allocator);
    defer walker.deinit();

    while (try walker.next()) |entry| {
        if (entry.kind != .file) continue;
        const stat = entry.dir.statFile(entry.basename) catch continue;
        const mtime = @as(i64, @intCast(@divTrunc(stat.mtime, std.time.ns_per_s)));
        if (mtime >= since) {
            const path_copy = try std.fmt.allocPrint(allocator, "/workspace/{s}", .{entry.path});
            try entries.append(.{ .path = path_copy, .modified = mtime });
        }
    }

    // Build JSON array manually
    var json_buf = std.array_list.Managed(u8).init(allocator);
    defer json_buf.deinit();
    try json_buf.appendSlice("{\"files\":[");
    for (entries.items, 0..) |e, i| {
        if (i > 0) try json_buf.append(',');
        const escaped = try exec.jsonEscape(allocator, e.path);
        defer allocator.free(escaped);
        const item = try std.fmt.allocPrint(allocator, "{{\"path\":\"{s}\",\"modified\":{d}}}", .{ escaped, e.modified });
        defer allocator.free(item);
        try json_buf.appendSlice(item);
    }
    try json_buf.appendSlice("]}");

    const resp = try json_buf.toOwnedSlice();
    defer allocator.free(resp);
    try main.writeResponse(stream, "200 OK", "application/json", resp);
}

const FileEntry = struct {
    path: []u8,
    modified: i64,
};

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
