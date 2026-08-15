const std = @import("std");
const main = @import("main.zig");

/// Process control table for streaming execs (KIP-18 "ability downshift").
///
/// The e2b-server's Process/Start maps to sandboxd's /exec/stream, but the
/// SDK's stdin / signal verbs need a handle to the running process *after*
/// the streaming connection is established. sandboxd is single-request-per-
/// connection, so a streaming exec's process must be reachable from other
/// connections: this table maps the in-guest pid to its open stdin pipe.
///
/// Lifecycle: /exec/stream registers {pid, stdin_fd} before forking; the
/// streaming handler unregisters when the child is reaped. /exec/stdin and
/// /exec/signal look the pid up here. A pid that finished between requests is
/// simply absent — the caller gets a clean not-found.
///
/// The table also answers liveness queries (E2B Process/Connect needs to
/// distinguish a still-running process from a reaped one, and the pid space
/// is the sandbox's own — node-independent, so a Connect routed to any
/// control-plane node can address the same process). config is a fixed-size
/// snapshot of the exec request for E2B Process/List; it is best-effort
/// (empty when the caller did not provide one).
const Entry = struct {
    pid: std.os.linux.pid_t,
    stdin_fd: i32,
    /// Command snapshot (best-effort, truncated to CONFIG_MAX).
    config: [CONFIG_MAX]u8 = .{0} ** CONFIG_MAX,
    config_len: usize = 0,
    /// Ring buffer of the process's recent output (KIP-18 P1 /exec/attach).
    /// The streaming exec appends as it drains stdout; an attach replays
    /// this buffer so a reconnect sees what the process produced. Fixed size
    /// bounds memory per process (BUFFER_MAX); only the tail is kept.
    buf: [BUFFER_MAX]u8 = .{0} ** BUFFER_MAX,
    buf_len: usize = 0,
    buf_head: usize = 0,
    /// Set once the process has been reaped (stdout fully drained).
    done: bool = false,
};

const CONFIG_MAX = 256;
const BUFFER_MAX = 64 * 1024;

fn setConfig(e: *Entry, command: []const u8) void {
    const n = @min(command.len, CONFIG_MAX);
    @memcpy(e.config[0..n], command[0..n]);
    e.config_len = n;
}

var entries: [64]?Entry = .{null} ** 64;
var count: usize = 0;
// Simple spinlock: sandboxd is thread-per-connection with short critical
// sections (table lookups), so a futex-free spinlock is the right primitive
// here (std.Io.Mutex in Zig 0.16 requires an Io instance we do not have).
var lock: std.atomic.Value(u32) = .init(0);

fn lockTable() void {
    while (lock.cmpxchgStrong(0, 1, .acquire, .monotonic) != null) {
        std.atomic.spinLoopHint();
    }
}

fn unlockTable() void {
    _ = lock.cmpxchgStrong(1, 0, .release, .monotonic);
}

pub fn register(pid: std.os.linux.pid_t, stdin_fd: i32) void {
    registerWithConfig(pid, stdin_fd, "");
}

pub fn registerWithConfig(pid: std.os.linux.pid_t, stdin_fd: i32, command: []const u8) void {
    lockTable();
    defer unlockTable();
    if (count >= entries.len) {
        // Table full: close the pipe rather than leak it. Streaming still
        // works; stdin control for this process is unavailable.
        _ = std.os.linux.close(@intCast(stdin_fd));
        return;
    }
    for (&entries) |*slot| {
        if (slot.* == null) {
            slot.* = .{ .pid = pid, .stdin_fd = stdin_fd };
            setConfig(&slot.*.?, command);
            count += 1;
            return;
        }
    }
}

/// isAlive reports whether the pid is still a live process in the sandbox.
/// Uses kill(pid, 0), which only probes existence — no signal is delivered.
pub fn isAlive(pid: std.os.linux.pid_t) bool {
    const probe = std.os.linux.syscall2(
        .kill,
        @as(usize, @bitCast(@as(isize, @intCast(pid)))),
        0,
    );
    return probe == 0;
}

/// configOf returns the stored command snapshot for a pid, if present.
pub fn configOf(pid: std.os.linux.pid_t) ?[]const u8 {
    lockTable();
    defer unlockTable();
    for (entries) |maybe| {
        if (maybe) |e| {
            if (e.pid == pid) {
                return e.config[0..e.config_len];
            }
        }
    }
    return null;
}

/// appendOutput stores the latest chunk into the process's ring buffer
/// (dropping the oldest when full). Called by the streaming exec as it
/// drains stdout. Safe under the table lock.
pub fn appendOutput(pid: std.os.linux.pid_t, data: []const u8) void {
    lockTable();
    defer unlockTable();
    for (&entries) |*slot| {
        if (slot.*) |*e| {
            if (e.pid == pid) {
                for (data) |b| {
                    e.buf[e.buf_head] = b;
                    e.buf_head = (e.buf_head + 1) % BUFFER_MAX;
                    if (e.buf_len < BUFFER_MAX) e.buf_len += 1;
                }
                return;
            }
        }
    }
}

/// markDone flags a process as reaped (stdout fully drained). Attach uses it
/// to know whether more output may still arrive.
pub fn markDone(pid: std.os.linux.pid_t) void {
    lockTable();
    defer unlockTable();
    for (&entries) |*slot| {
        if (slot.*) |*e| {
            if (e.pid == pid) {
                e.done = true;
                return;
            }
        }
    }
}

/// attachOutput copies the process's buffered output into a caller-owned
/// buffer. Returns the length written, or 0 if the pid is unknown.
pub fn attachOutput(pid: std.os.linux.pid_t, out: []u8) usize {
    lockTable();
    defer unlockTable();
    for (entries) |maybe| {
        if (maybe) |e| {
            if (e.pid == pid) {
                const n = @min(e.buf_len, out.len);
                // Walk the ring from oldest to newest.
                const start = (e.buf_head + BUFFER_MAX - e.buf_len) % BUFFER_MAX;
                for (0..n) |i| {
                    out[i] = e.buf[(start + i) % BUFFER_MAX];
                }
                return n;
            }
        }
    }
    return 0;
}

/// isDone reports whether the process has been reaped.
pub fn isDone(pid: std.os.linux.pid_t) bool {
    lockTable();
    defer unlockTable();
    for (entries) |maybe| {
        if (maybe) |e| {
            if (e.pid == pid) return e.done;
        }
    }
    return true; // unknown == gone
}

/// pids returns all registered pids (E2B Process/List view).
pub fn pids() [64]?std.os.linux.pid_t {
    lockTable();
    defer unlockTable();
    var out: [64]?std.os.linux.pid_t = .{null} ** 64;
    var i: usize = 0;
    for (entries) |maybe| {
        if (maybe) |e| {
            out[i] = e.pid;
            i += 1;
        }
    }
    return out;
}

pub fn unregister(pid: std.os.linux.pid_t) void {
    lockTable();
    defer unlockTable();
    for (&entries) |*slot| {
        if (slot.*) |e| {
            if (e.pid == pid) {
                _ = std.os.linux.close(@intCast(e.stdin_fd));
                slot.* = null;
                count -= 1;
                return;
            }
        }
    }
}

/// stdinFd returns the open stdin fd for a pid, or null.
fn stdinFd(pid: std.os.linux.pid_t) ?i32 {
    lockTable();
    defer unlockTable();
    for (entries) |maybe| {
        if (maybe) |e| {
            if (e.pid == pid) return e.stdin_fd;
        }
    }
    return null;
}

/// POST /exec/stdin {"pid": N, "data": "<base64>"} → 200 {"ok":true}
/// Writes decoded bytes to the process's stdin pipe.
pub fn handleStdin(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(StdinRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    const fd = stdinFd(req.pid) orelse {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"process not found\"}");
        return;
    };

    // Decode base64 payload (the SDK sends input as base64).
    const decoded_len = std.base64.standard.Decoder.calcSizeForSlice(req.data) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid base64\"}");
        return;
    };
    const decoded = try allocator.alloc(u8, decoded_len);
    defer allocator.free(decoded);
    std.base64.standard.Decoder.decode(decoded, req.data) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid base64\"}");
        return;
    };

    // Write all bytes (loop for partial writes).
    var written: usize = 0;
    while (written < decoded.len) {
        const n = std.os.linux.write(@intCast(fd), decoded.ptr + written, decoded.len - written);
        if (n < 0) break;
        if (n == 0) break;
        written += @as(usize, @intCast(n));
    }
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

/// POST /exec/stdin/close {"pid": N} → 200 {"ok":true}
/// Closes the process's stdin (EOF), like the SDK's closeStdin.
pub fn handleCloseStdin(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(PidRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    lockTable();
    defer unlockTable();
    for (&entries) |*slot| {
        if (slot.*) |*e| {
            if (e.pid == req.pid) {
                _ = std.os.linux.close(@intCast(e.stdin_fd));
                e.stdin_fd = -1; // closed: further stdin writes will fail
                try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
                return;
            }
        }
    }
    try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"process not found\"}");
}

/// POST /exec/signal {"pid": N, "signal": "SIGKILL"|"SIGTERM"} → 200 {"ok":true}
pub fn handleSignal(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(SignalRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    const sig: std.os.linux.SIG = if (std.mem.eql(u8, req.signal, "SIGKILL") or std.mem.eql(u8, req.signal, "SIGNAL_SIGKILL") or std.mem.eql(u8, req.signal, "9"))
        .KILL
    else if (std.mem.eql(u8, req.signal, "SIGTERM") or std.mem.eql(u8, req.signal, "SIGNAL_SIGTERM") or std.mem.eql(u8, req.signal, "15"))
        .TERM
    else {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"unsupported signal\"}");
        return;
    };

    const rc = std.os.linux.kill(req.pid, sig);
    if (rc != 0) {
        const err = std.posix.errno(@as(usize, @bitCast(@as(isize, @intCast(rc)))));
        if (err == .SRCH) {
            // Process already gone — the streaming handler may not have
            // reaped it yet. Report not-found so the SDK sees kill()===false.
            try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"process not found\"}");
            return;
        }
        try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"signal failed\"}");
        return;
    }
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}


const StdinRequest = struct {
    pid: std.os.linux.pid_t,
    data: []const u8 = "",
};

const PidRequest = struct {
    pid: std.os.linux.pid_t,
};

const SignalRequest = struct {
    pid: std.os.linux.pid_t,
    signal: []const u8 = "",
};

/// escapeJson escapes a string for embedding in a JSON string literal
/// (quotes, backslash, and control chars).
fn escapeJson(allocator: std.mem.Allocator, s: []const u8) ![]u8 {
    var out = std.array_list.Managed(u8).init(allocator);
    defer out.deinit();
    for (s) |c| {
        switch (c) {
            '"' => try out.appendSlice("\\\""),
            '\\' => try out.appendSlice("\\\\"),
            0...31 => {
                var buf: [8]u8 = undefined;
                const hex = try std.fmt.bufPrint(&buf, "\\u{x:0>4}", .{c});
                try out.appendSlice(hex);
            },
            else => try out.append(c),
        }
    }
    return out.toOwnedSlice();
}

/// GET /exec/processes → E2B Process/List view of the process-control table.
/// Reports every registered streaming exec: pid, whether it is still alive
/// (kill-0 probe), and the command snapshot (best-effort). This is the
/// sandbox-owned process table — pids are the sandbox's own, so the view is
/// node-independent and any control-plane node can serve it.
pub fn handleProcessList(allocator: std.mem.Allocator, client_fd: i32) !void {
    var out = std.array_list.Managed(u8).init(allocator);
    defer out.deinit();
    try out.appendSlice("{\"processes\":[");

    const all = pids();
    var wrote_any = false;
    var entry_buf: [600]u8 = undefined;
    for (all) |maybe_pid| {
        const pid = maybe_pid orelse continue;
        if (pid <= 0) continue;
        const alive = isAlive(pid);
        const cfg = configOf(pid) orelse "";
        const escaped = try escapeJson(allocator, cfg);
        defer allocator.free(escaped);

        if (wrote_any) try out.append(',');
        wrote_any = true;
        const entry = try std.fmt.bufPrint(&entry_buf,
            "{{\"pid\":{d},\"alive\":{s},\"config\":\"{s}\"}}",
            .{ pid, if (alive) "true" else "false", escaped });
        try out.appendSlice(entry);
    }

    try out.appendSlice("]}");
    try main.writeResponse(client_fd, "200 OK", "application/json", out.items);
}

/// GET /exec/attach?pid=N → E2B Process/Connect: replays the process's
/// buffered output (SSE frames) for the caller to re-read. Pids are the
/// sandbox's own, so an attach from any control-plane node addresses the
/// same process. The first frame carries {"pid":N} like /exec/stream, then
/// the buffered output, then a done marker. This slice replays the buffer;
/// live tailing a still-running process from a second consumer needs a
/// multi-reader pipe model (documented P1 follow-up).

/// parsePidQuery extracts the integer value of the "pid" query parameter.
fn parsePidQuery(query: []const u8) ?[]const u8 {
    const prefix = "pid=";
    if (std.mem.startsWith(u8, query, prefix)) {
        return query[prefix.len..];
    }
    return null;
}

/// GET /exec/attach?pid=N → E2B Process/Connect: replays the process's
/// buffered output (SSE frames) for the caller to re-read. Pids are the
/// sandbox's own, so an attach from any control-plane node addresses the
/// same process. The first frame carries {"pid":N} like /exec/stream, then
/// the buffered output, then a done marker. This slice replays the buffer;
/// live tailing a still-running process from a second consumer needs a
/// multi-reader pipe model (documented P1 follow-up).
pub fn handleAttach(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    _ = allocator;
    const raw = parsePidQuery(query) orelse {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"pid required\"}");
        return;
    };
    const pid: std.os.linux.pid_t = std.fmt.parseInt(std.os.linux.pid_t, raw, 10) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid pid\"}");
        return;
    };
    if (pid <= 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid pid\"}");
        return;
    }

    const header = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: close\r\n\r\n";
    _ = std.os.linux.write(client_fd, header.ptr, header.len);

    var pid_frame_buf: [64]u8 = undefined;
    const pid_frame = std.fmt.bufPrint(&pid_frame_buf, "data: {{\"pid\":{d}}}\n\n", .{pid}) catch return;
    _ = std.os.linux.write(client_fd, pid_frame.ptr, pid_frame.len);

    // Replay the buffered output as SSE data frames.
    var buf: [BUFFER_MAX]u8 = undefined;
    const n = attachOutput(pid, &buf);
    var line_buf: [BUFFER_MAX + 32]u8 = undefined;
    if (n > 0) {
        const line = std.fmt.bufPrint(&line_buf, "data: {s}\n\n", .{buf[0..n]}) catch {
            _ = std.os.linux.write(client_fd, "data: {\"error\":\"attach output too large\"}\n\n".ptr, 37);
            return;
        };
        _ = std.os.linux.write(client_fd, line.ptr, line.len);
    }

    // Done marker (the e2b layer reads the exit-code file for the code).
    _ = std.os.linux.write(client_fd, "data: {\"done\":true}\n\n".ptr, 24);
}
