const std = @import("std");
const exec = @import("exec.zig");
const files = @import("files.zig");
const workspace = @import("workspace.zig");
const background = @import("background.zig");
const venv = @import("venv.zig");
const transcript = @import("transcript.zig");
const events = @import("events.zig");

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

    venv.ensureVenv();

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
            _ = std.os.linux.close(@as(std.os.linux.fd_t, @intCast(client_fd_i32)));
            continue;
        };
        thread.detach();
    }
}

fn handleConn(allocator: std.mem.Allocator, client_fd: i32) void {
    defer _ = std.os.linux.close(@as(std.os.linux.fd_t, @intCast(client_fd)));
    handleRequest(allocator, client_fd) catch |err| {
        std.log.err("request error: {s}", .{@errorName(err)});
    };
}

/// maxRequestBodyBytes caps a single HTTP request body. Raised far above the
/// old fixed 64KiB stack buffer so WriteFile can carry large payloads; the
/// gateway gRPC layer already allows 64MiB messages (see KIP-16 M7).
const maxRequestBodyBytes: usize = 64 * 1024 * 1024;

fn handleRequest(allocator: std.mem.Allocator, client_fd: i32) !void {
    // Read the whole request. Headers are tiny; bodies (WriteFile) can be large,
    // so allocate on the heap sized by Content-Length instead of a stack array.
    var header_buf: [16384]u8 = undefined;
    var content_length: usize = 0;

    // First read headers (single read is fine for our small requests).
    const n0 = std.os.linux.read(client_fd, &header_buf, header_buf.len);
    if (n0 < 0) return error.ReadFailed;
    if (n0 == 0) return;
    const head = header_buf[0..@as(usize, @intCast(n0))];

    // Parse Content-Length so we know how much body to read.
    var head_lines = std.mem.splitScalar(u8, head, '\n');
    _ = head_lines.next(); // request line
    while (head_lines.next()) |line| {
        const trimmed = std.mem.trim(u8, line, &std.ascii.whitespace);
        if (std.ascii.startsWithIgnoreCase(trimmed, "content-length:")) {
            const val = std.mem.trim(u8, trimmed["content-length:".len..], &std.ascii.whitespace);
            content_length = std.fmt.parseInt(usize, val, 10) catch 0;
        }
    }
    if (content_length > maxRequestBodyBytes) {
        try writeResponse(client_fd, "413 Payload Too Large", "application/json", "{\"error\":\"request too large\"}");
        return;
    }

    const header_end = std.mem.indexOf(u8, head, "\r\n\r\n") orelse return;
    const body_start = header_end + 4;
    const body_in_head = if (head.len > body_start) head.len - body_start else 0;
    const remaining = if (body_in_head < content_length) content_length - body_in_head else 0;

    const buf_len = if (head.len > body_start + remaining) head.len else body_start + remaining;
    const buf = try allocator.alloc(u8, buf_len);
    defer allocator.free(buf);
    @memcpy(buf[0..head.len], head);

    var read: usize = 0;
    while (read < remaining) {
        const n = std.os.linux.read(client_fd, buf.ptr + body_start + read, remaining - read);
        if (n <= 0) break;
        read += @as(usize, @intCast(n));
    }

    // request spans headers + body: body_in_head bytes already in the first
    // read plus anything fetched by the loop.
    const body_total = if (remaining > 0) content_length else body_in_head;
    const request = buf[0 .. body_start + body_total];

    var lines = std.mem.splitScalar(u8, request, '\n');
    const request_line = lines.next() orelse return;
    var parts = std.mem.splitScalar(u8, std.mem.trim(u8, request_line, &std.ascii.whitespace), ' ');
    const method = parts.next() orelse return;
    const path_full = parts.next() orelse return;

    var path_parts = std.mem.splitScalar(u8, path_full, '?');
    const path = path_parts.next() orelse path_full;
    const query = path_parts.next() orelse "";

    const body = if (std.mem.indexOf(u8, request, "\r\n\r\n")) |i| request[i + 4 ..] else "";

    if (std.mem.eql(u8, path, "/ready") and std.mem.eql(u8, method, "POST")) {
        try handleReady(client_fd);
    } else if (std.mem.eql(u8, path, "/exec") and std.mem.eql(u8, method, "POST")) {
        try exec.handleExec(allocator, client_fd, body, false);
    } else if (std.mem.eql(u8, path, "/exec/stream") and std.mem.eql(u8, method, "POST")) {
        try exec.handleExec(allocator, client_fd, body, true);
    } else if (std.mem.eql(u8, path, "/workspace/reset") and std.mem.eql(u8, method, "POST")) {
        try workspace.handleReset(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/exec/background") and std.mem.eql(u8, method, "POST")) {
        try background.handleBgSubmit(allocator, client_fd, body);
    } else if (std.mem.startsWith(u8, path, "/exec/background/")) {
        const run_id = path["/exec/background/".len..];
        try background.handleBgPoll(allocator, client_fd, run_id);
    } else if (std.mem.eql(u8, path, "/transcript") and std.mem.eql(u8, method, "GET")) {
        try transcript.handleTranscript(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/events") and std.mem.eql(u8, method, "GET")) {
        try handleEvents(allocator, client_fd, query);
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

/// handleReady implements the application-layer readiness handshake. A bare TCP
/// connect to :2024 is not a reliable readiness signal (the socket may be open
/// before sandboxd finished initialization); consumers should call /ready and
/// require status=="ready".
fn handleReady(client_fd: i32) !void {
    const venv_ready = venv.isReady();
    const body = try std.fmt.allocPrint(std.heap.page_allocator,
        "{{\"status\":\"ready\",\"venv\":{s}}}", .{if (venv_ready) "true" else "false"});
    defer std.heap.page_allocator.free(body);
    try writeResponse(client_fd, "200 OK", "application/json", body);
}

/// handleEvents serves GET /events?limit=<n> — the last n NDJSON event lines
/// (KIP-16 M5). Returns 404 when no events have been recorded yet.
fn handleEvents(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    var limit: usize = 500;
    var params = std.mem.splitScalar(u8, query, '&');
    while (params.next()) |pair| {
        var kv = std.mem.splitScalar(u8, pair, '=');
        const key = kv.next() orelse continue;
        const val = kv.next() orelse continue;
        if (std.mem.eql(u8, key, "limit")) {
            limit = std.fmt.parseInt(usize, val, 10) catch 500;
        }
    }

    var out = std.array_list.Managed(u8).init(allocator);
    defer out.deinit();
    events.readTail(allocator, limit, &out);
    if (out.items.len == 0) {
        try writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"no events\"}");
        return;
    }

    // NDJSON lines are already valid JSON objects; wrap in a JSON array.
    var body = std.array_list.Managed(u8).init(allocator);
    defer body.deinit();
    try body.append('[');
    var first = true;
    var it = std.mem.splitScalar(u8, out.items, '\n');
    while (it.next()) |line| {
        if (line.len == 0) continue;
        if (!first) try body.append(',');
        first = false;
        try body.appendSlice(line);
    }
    try body.append(']');
    try writeResponse(client_fd, "200 OK", "application/json", body.items);
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
