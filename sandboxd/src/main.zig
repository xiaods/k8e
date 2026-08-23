const std = @import("std");
const httpio = @import("httpio.zig");
const exec = @import("exec.zig");
const files = @import("files.zig");
const workspace = @import("workspace.zig");
const background = @import("background.zig");
const venv = @import("venv.zig");
const transcript = @import("transcript.zig");
const events = @import("events.zig");
const processes = @import("processes.zig");
const execctl = @import("execctl.zig");
const watch = @import("watch.zig");
const pty = @import("pty.zig");

pub fn main() !void {
    // Production runs on a page allocator: the DebugAllocator here cost ~2x
    // memory and slow alloc/free on every request for the pod's whole
    // lifetime, and its abort-on-double-free (though valuable in tests)
    // crashes the daemon on any mistake. Debug builds keep the guarded
    // allocator so CI unit tests still catch ownership bugs.
    var gpa_state: std.heap.DebugAllocator(.{}) = undefined;
    const allocator: std.mem.Allocator = if (std.debug.runtime_safety) blk: {
        gpa_state = .{};
        break :blk gpa_state.allocator();
    } else std.heap.page_allocator;
    defer { if (std.debug.runtime_safety) _ = gpa_state.deinit(); }

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

fn handleRequest(allocator: std.mem.Allocator, client_fd: i32) !void {
    const maybe = httpio.readRequest(allocator, client_fd) catch |err| switch (err) {
        error.TooLarge => {
            try writeResponse(client_fd, "413 Payload Too Large", "application/json", "{\"error\":\"request too large\"}");
            return;
        },
        else => return err,
    };
    const req = maybe orelse return;
    defer allocator.free(req.alloc);

    const method = req.method;
    const path = req.path;
    const query = req.query;
    const body = req.body;

    if (std.mem.eql(u8, path, "/ready") and std.mem.eql(u8, method, "POST")) {
        try handleReady(client_fd);
    } else if (std.mem.eql(u8, path, "/exec") and std.mem.eql(u8, method, "POST")) {
        try exec.handleExec(allocator, client_fd, body, false);
    } else if (std.mem.eql(u8, path, "/exec/stream") and std.mem.eql(u8, method, "POST")) {
        try exec.handleExec(allocator, client_fd, body, true);
    } else if (std.mem.eql(u8, path, "/workspace/reset") and std.mem.eql(u8, method, "POST")) {
        try workspace.handleReset(allocator, client_fd);
    } else if (std.mem.eql(u8, path, "/exec/background") and std.mem.eql(u8, method, "POST")) {
        try background.handleBgSubmit(allocator, client_fd, body);
    } else if (std.mem.startsWith(u8, path, "/exec/background/")) {
        const run_id = path["/exec/background/".len..];
        try background.handleBgPoll(allocator, client_fd, run_id);
    } else if (std.mem.eql(u8, path, "/transcript") and std.mem.eql(u8, method, "GET")) {
        try transcript.handleTranscript(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/events") and std.mem.eql(u8, method, "GET")) {
        try handleEvents(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/processes") and std.mem.eql(u8, method, "GET")) {
        try processes.handleProcesses(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/files/write") and std.mem.eql(u8, method, "POST")) {
        try files.handleWrite(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/files/read") and std.mem.eql(u8, method, "GET")) {
        try files.handleRead(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/files/list") and std.mem.eql(u8, method, "GET")) {
        try files.handleList(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/files/stat") and std.mem.eql(u8, method, "POST")) {
        try files.handleStat(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/files/mkdir") and std.mem.eql(u8, method, "POST")) {
        try files.handleMkdir(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/files/move") and std.mem.eql(u8, method, "POST")) {
        try files.handleMove(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/files/remove") and std.mem.eql(u8, method, "POST")) {
        try files.handleRemove(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/exec/stdin") and std.mem.eql(u8, method, "POST")) {
        try execctl.handleStdin(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/exec/stdin/close") and std.mem.eql(u8, method, "POST")) {
        try execctl.handleCloseStdin(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/exec/signal") and std.mem.eql(u8, method, "POST")) {
        try execctl.handleSignal(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/exec/processes") and std.mem.eql(u8, method, "GET")) {
        // E2B Process/List view of the sandbox process-control table (KIP-18
        // P1: sandbox-owned process table, node-independent pids).
        try execctl.handleProcessList(allocator, client_fd);
    } else if (std.mem.eql(u8, path, "/exec/attach") and std.mem.eql(u8, method, "GET")) {
        // E2B Process/Connect: replay buffered output for a process (KIP-18
        // P1: sandbox-owned process table, attach from any node).
        try execctl.handleAttach(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/watch/create") and std.mem.eql(u8, method, "POST")) {
        // E2B Filesystem/CreateWatcher (KIP-18 P1 last gap).
        try watch.handleCreate(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/watch/events") and std.mem.eql(u8, method, "GET")) {
        // E2B Filesystem/GetWatcherEvents (incremental).
        try watch.handleEvents(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/watch/remove") and std.mem.eql(u8, method, "POST")) {
        // E2B Filesystem/RemoveWatcher.
        try watch.handleRemove(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/pty/create") and std.mem.eql(u8, method, "POST")) {
        // PTY terminal primitive (KIP-19): allocate + start controlling-terminal session leader.
        try pty.handleCreate(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/pty/input") and std.mem.eql(u8, method, "POST")) {
        try pty.handleInput(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/pty/resize") and std.mem.eql(u8, method, "POST")) {
        try pty.handleResize(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/pty/foreground") and std.mem.eql(u8, method, "GET")) {
        try pty.handleForeground(allocator, client_fd, query);
    } else if (std.mem.eql(u8, path, "/pty/signal") and std.mem.eql(u8, method, "POST")) {
        try pty.handleSignal(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/pty/destroy") and std.mem.eql(u8, method, "POST")) {
        try pty.handleDestroy(allocator, client_fd, body);
    } else if (std.mem.eql(u8, path, "/pty/stream") and std.mem.eql(u8, method, "GET")) {
        try pty.handleStream(allocator, client_fd, query);
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

/// writeResponse writes a complete HTTP/1.1 response with Connection: close.
/// Kept out of httpio.zig so that module stays host-portable for tests:
/// sandboxd is linux-only and writes via raw syscalls.
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
