const std = @import("std");
const exec = @import("exec.zig");
const files = @import("files.zig");

pub fn main() !void {
    var gpa = std.heap.GeneralPurposeAllocator(.{}){};
    defer _ = gpa.deinit();
    const allocator = gpa.allocator();

    // PID 1: reap zombies via SIGCHLD ignore
    const pid = std.os.linux.getpid();
    if (pid == 1) {
        setupSignals();
    }

    const address = try std.net.Address.parseIp("0.0.0.0", 2024);
    var server = try address.listen(.{ .reuse_address = true });
    defer server.deinit();

    std.log.info("sandboxd listening on :2024", .{});

    while (true) {
        const conn = server.accept() catch |err| {
            std.log.err("accept: {}", .{err});
            continue;
        };
        const thread = try std.Thread.spawn(.{}, handleConn, .{ allocator, conn });
        thread.detach();
    }
}

fn handleConn(allocator: std.mem.Allocator, conn: std.net.Server.Connection) void {
    defer conn.stream.close();
    handleRequest(allocator, conn.stream) catch |err| {
        std.log.err("request error: {}", .{err});
    };
}

fn handleRequest(allocator: std.mem.Allocator, stream: std.net.Stream) !void {
    var buf: [65536]u8 = undefined;
    const n = try stream.read(&buf);
    if (n == 0) return;

    const request = buf[0..n];

    var lines = std.mem.splitScalar(u8, request, '\n');
    const request_line = lines.next() orelse return;
    var parts = std.mem.splitScalar(u8, std.mem.trim(u8, request_line, &std.ascii.whitespace), ' ');
    const method = parts.next() orelse return;
    const path_full = parts.next() orelse return;

    var path_parts = std.mem.splitScalar(u8, path_full, '?');
    const path = path_parts.next() orelse path_full;
    const query = path_parts.next() orelse "";

    const body = if (std.mem.indexOf(u8, request, "\r\n\r\n")) |i| request[i + 4 ..] else "";

    if (std.mem.eql(u8, path, "/exec") and std.mem.eql(u8, method, "POST")) {
        try exec.handleExec(allocator, stream, body, false);
    } else if (std.mem.eql(u8, path, "/exec/stream") and std.mem.eql(u8, method, "GET")) {
        try exec.handleExec(allocator, stream, body, true);
    } else if (std.mem.eql(u8, path, "/files/write") and std.mem.eql(u8, method, "POST")) {
        try files.handleWrite(allocator, stream, body);
    } else if (std.mem.eql(u8, path, "/files/read") and std.mem.eql(u8, method, "GET")) {
        try files.handleRead(allocator, stream, query);
    } else if (std.mem.eql(u8, path, "/files/list") and std.mem.eql(u8, method, "GET")) {
        try files.handleList(allocator, stream, query);
    } else {
        try writeResponse(stream, "404 Not Found", "application/json", "{\"error\":\"not found\"}");
    }
}

pub fn writeResponse(stream: std.net.Stream, status: []const u8, content_type: []const u8, body: []const u8) !void {
    var buf: [4096]u8 = undefined;
    const header = try std.fmt.bufPrint(&buf,
        "HTTP/1.1 {s}\r\nContent-Type: {s}\r\nContent-Length: {d}\r\nConnection: close\r\n\r\n",
        .{ status, content_type, body.len });
    try stream.writeAll(header);
    try stream.writeAll(body);
}

fn setupSignals() void {
    // Ignore SIGCHLD to auto-reap zombies as PID 1
    const sa = std.os.linux.Sigaction{
        .handler = .{ .handler = std.os.linux.SIG.IGN },
        .mask = std.mem.zeroes(std.os.linux.sigset_t),
        .flags = std.os.linux.SA.NOCLDWAIT,
    };
    _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa, null);
}
