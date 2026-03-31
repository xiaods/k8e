const std = @import("std");
const main = @import("main.zig");

const ExecRequest = struct {
    command: []const u8 = "",
    timeout: u32 = 30,
    workdir: []const u8 = "/workspace",
};

pub fn handleExec(allocator: std.mem.Allocator, stream: std.net.Stream, body: []const u8, streaming: bool) !void {
    const parsed = std.json.parseFromSlice(ExecRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(stream, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.command.len == 0) {
        try main.writeResponse(stream, "400 Bad Request", "application/json", "{\"error\":\"command required\"}");
        return;
    }

    var child = std.process.Child.init(&.{ "/bin/sh", "-c", req.command }, allocator);
    child.cwd = req.workdir;
    child.stdout_behavior = .Pipe;
    child.stderr_behavior = .Pipe;

    child.spawn() catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(stream, "500 Internal Server Error", "application/json", msg);
        return;
    };

    if (streaming) {
        const header = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: close\r\n\r\n";
        try stream.writeAll(header);

        var stdout_buf: [4096]u8 = undefined;
        while (true) {
            const n = child.stdout.?.read(&stdout_buf) catch break;
            if (n == 0) break;
            var line_buf: [4200]u8 = undefined;
            const line = try std.fmt.bufPrint(&line_buf, "data: {s}\n\n", .{stdout_buf[0..n]});
            try stream.writeAll(line);
        }
        _ = child.wait() catch {};
    } else {
        const stdout = child.stdout.?.readToEndAlloc(allocator, 10 * 1024 * 1024) catch try allocator.dupe(u8, "");
        defer allocator.free(stdout);
        const stderr = child.stderr.?.readToEndAlloc(allocator, 1 * 1024 * 1024) catch try allocator.dupe(u8, "");
        defer allocator.free(stderr);

        const term = child.wait() catch std.process.Child.Term{ .Exited = 1 };
        const exit_code: i32 = switch (term) {
            .Exited => |c| @intCast(c),
            else => 1,
        };

        const stdout_json = try jsonEscape(allocator, stdout);
        defer allocator.free(stdout_json);
        const stderr_json = try jsonEscape(allocator, stderr);
        defer allocator.free(stderr_json);

        const resp = try std.fmt.allocPrint(allocator,
            "{{\"stdout\":\"{s}\",\"stderr\":\"{s}\",\"exit_code\":{d}}}",
            .{ stdout_json, stderr_json, exit_code });
        defer allocator.free(resp);
        try main.writeResponse(stream, "200 OK", "application/json", resp);
    }
}

// jsonEscape returns the string with JSON special chars escaped (no surrounding quotes).
pub fn jsonEscape(allocator: std.mem.Allocator, s: []const u8) ![]u8 {
    var out = std.array_list.Managed(u8).init(allocator);
    for (s) |c| {
        switch (c) {
            '"' => try out.appendSlice("\\\""),
            '\\' => try out.appendSlice("\\\\"),
            '\n' => try out.appendSlice("\\n"),
            '\r' => try out.appendSlice("\\r"),
            '\t' => try out.appendSlice("\\t"),
            else => try out.append(c),
        }
    }
    return out.toOwnedSlice();
}
