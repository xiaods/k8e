const std = @import("std");
const exec = @import("exec.zig");
const files = @import("files.zig");
const workspace = @import("workspace.zig");

pub fn main() !void {
    var gpa = std.heap.DebugAllocator(.{}){};
    defer _ = gpa.deinit();
    const allocator = gpa.allocator();

    // PID 1: reap zombies via SIGCHLD ignore
    const pid = std.os.linux.getpid();
    if (pid == 1) {
        setupSignals();
    }

    // Raw socket: create, bind, listen
    const sockfd = @as(i32, @intCast(std.os.linux.socket(
        std.os.linux.AF.INET,
        std.os.linux.SOCK.STREAM | std.os.linux.SOCK.CLOEXEC,
        0,
    )));
    if (sockfd < 0) {
        std.log.err("socket failed: {s}", .{@tagName(@as(std.posix.E, @enumFromInt(-sockfd)))});
        return error.SocketFailed;
    }
    defer _ = std.os.linux.close(sockfd);

    // SO_REUSEADDR
    const one: i32 = 1;
    _ = std.os.linux.setsockopt(sockfd, std.os.linux.SOL.SOCKET, std.os.linux.SO.REUSEADDR, @ptrCast(&one), @sizeOf(i32));

    var addr = std.os.linux.sockaddr.in{
        .family = std.os.linux.AF.INET,
        .port = std.mem.nativeToBig(u16, 2024),
        .addr = 0, // INADDR_ANY
    };
    const bind_rc = std.os.linux.bind(sockfd, @ptrCast(&addr), @sizeOf(std.os.linux.sockaddr.in));
    if (bind_rc < 0) {
        std.log.err("bind failed: {s}", .{@tagName(@as(std.posix.E, @enumFromInt(-@as(isize, @bitCast(bind_rc)))))});
        return error.BindFailed;
    }

    const listen_rc = std.os.linux.listen(sockfd, 128);
    if (listen_rc < 0) {
        std.log.err("listen failed: {s}", .{@tagName(@as(std.posix.E, @enumFromInt(-@as(isize, @bitCast(listen_rc)))))});
        return error.ListenFailed;
    }

    std.log.info("sandboxd listening on :2024", .{});

    while (true) {
        var client_addr: std.os.linux.sockaddr = undefined;
        var addr_len: std.os.linux.socklen_t = @sizeOf(std.os.linux.sockaddr);
        const client_fd = std.os.linux.accept4(
            sockfd,
            &client_addr,
            &addr_len,
            std.os.linux.SOCK.CLOEXEC,
        );
        if (client_fd < 0) {
            const err = @as(std.posix.E, @enumFromInt(-client_fd));
            if (err == std.posix.E.INTR) continue;
            std.log.err("accept: {d}", .{client_fd});
            continue;
        }
        const client_fd_i32: i32 = @intCast(client_fd);
        const thread = std.Thread.spawn(.{}, handleConn, .{ allocator, client_fd_i32 }) catch {
            _ = std.os.linux.close(client_fd_i32);
            continue;
        };
        thread.detach();
    }
}

fn handleConn(allocator: std.mem.Allocator, client_fd: i32) void {
    defer _ = std.os.linux.close(client_fd);
    handleRequest(allocator, client_fd) catch |err| {
        std.log.err("request error: {s}", .{@errorName(err)});
    };
}

fn handleRequest(allocator: std.mem.Allocator, client_fd: i32) !void {
    var buf: [65536]u8 = undefined;
    const n = std.os.linux.read(client_fd, &buf, buf.len);
    if (n < 0) return error.ReadFailed;
    if (n == 0) return;

    const request = buf[0..@as(usize, @intCast(n))];

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
        try exec.handleExec(allocator, client_fd, body, false);
    } else if (std.mem.eql(u8, path, "/exec/stream") and std.mem.eql(u8, method, "POST")) {
        try exec.handleExec(allocator, client_fd, body, true);
    } else if (std.mem.eql(u8, path, "/workspace/reset") and std.mem.eql(u8, method, "POST")) {
        try workspace.handleReset(allocator, client_fd);
    } else if (std.mem.eql(u8, path, "/files/write") and std.mem.eql(u8, method, "POST")) {
        try files.handleWrite(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/files/read") and std.mem.eql(u8, method, "GET")) {
        try files.handleRead(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/files/list") and std.mem.eql(u8, method, "GET")) {
        try files.handleList(allocator, client_fd, query);
    } else {
        try writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"not found\"}");
    }
}

pub fn writeResponse(client_fd: i32, status: []const u8, content_type: []const u8, body: []const u8) !void {
    var header_buf: [4096]u8 = undefined;
    const header = try std.fmt.bufPrint(&header_buf,
        "HTTP/1.1 {s}\r\nContent-Type: {s}\r\nContent-Length: {d}\r\nConnection: close\r\n\r\n",
        .{ status, content_type, body.len });
    _ = std.os.linux.write(client_fd, header.ptr, header.len);
    _ = std.os.linux.write(client_fd, body.ptr, body.len);
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
