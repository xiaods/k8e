const std = @import("std");
const main = @import("main.zig");
const exec = @import("exec.zig");

/// PTY terminal primitive (KIP-19). Allocates a pseudo-terminal, starts an
/// argv as a controlling-terminal session leader, and keeps a daemon-owned
/// pump thread draining the master so a disconnected stream never blocks the
/// foreground process. Terminal sessions are keyed by a monotonic id and are
/// pod-scoped: they die with the pod (no cross-pod migration).

// ioctl request codes (asm-generic / Linux).
const TIOCGPTN: u32 = 0x80045430; // get pty number (unsigned int)
const TIOCSPTLCK: u32 = 0x40045431; // lock/unlock pty (int)
const TIOCSCTTY: u32 = 0x540E; // make controlling terminal
const TIOCSWINSZ: u32 = 0x5414; // set window size

const Winsize = extern struct {
    ws_row: u16 = 0,
    ws_col: u16 = 0,
    ws_xpixel: u16 = 0,
    ws_ypixel: u16 = 0,
};

const BUFFER_MAX = 64 * 1024; // output ring buffer per terminal (attach/replay)
const MAX_TERMINALS = 64;
const POLL_NSEC: isize = 50 * 1_000_000; // 50ms stream poll cadence

const Terminal = struct {
    id: u32,
    master_fd: i32,
    pid: i32, // session leader
    rows: u16,
    cols: u16,
    buf: [BUFFER_MAX]u8 = [_]u8{0} ** BUFFER_MAX,
    buf_len: usize = 0,
    buf_head: usize = 0,
    total_bytes: u64 = 0,
    exit_code: i32 = 0,
    done: bool = false,
};

var terminals: [MAX_TERMINALS]?Terminal = [_]?Terminal{null} ** MAX_TERMINALS;
var count: usize = 0;
var next_id: u32 = 1;
var lock: std.atomic.Value(u32) = .init(0);

fn lockTable() void {
    while (lock.cmpxchgStrong(0, 1, .acquire, .monotonic) != null) {
        std.atomic.spinLoopHint();
    }
}

fn unlockTable() void {
    _ = lock.cmpxchgStrong(1, 0, .release, .monotonic);
}

/// True when a raw Linux syscall return value encodes a negative errno.
fn isErr(rc: usize) bool {
    return @as(isize, @bitCast(rc)) < 0;
}

fn findTerminal(id: u32) ?*Terminal {
    for (&terminals) |*slot| {
        if (slot.*) |*t| {
            if (t.id == id) return t;
        }
    }
    return null;
}

fn sleepPoll() void {
    const ts = std.os.linux.timespec{ .sec = 0, .nsec = POLL_NSEC };
    _ = std.os.linux.nanosleep(&ts, null);
}

// ── PTY allocation ──────────────────────────────────────────────────────────

const Pty = struct {
    master: i32,
    slave: i32,
};

fn openPty() !Pty {
    var ptmx_buf: [64]u8 = undefined;
    const ptmx = std.fmt.bufPrintZ(&ptmx_buf, "/dev/ptmx", .{}) catch return error.PathTooLong;
    const master_rc = std.os.linux.open(ptmx.ptr, std.os.linux.O{ .ACCMODE = .RDWR, .NOCTTY = true, .CLOEXEC = true }, 0);
    if (isErr(master_rc)) return error.OpenPtmxFailed;
    const master: i32 = @intCast(master_rc);

    var ptn: u32 = 0;
    if (std.os.linux.ioctl(master, TIOCGPTN, @intFromPtr(&ptn)) != 0) {
        _ = std.os.linux.close(master);
        return error.IoctlGptnFailed;
    }
    var unlock: i32 = 0;
    if (std.os.linux.ioctl(master, TIOCSPTLCK, @intFromPtr(&unlock)) != 0) {
        _ = std.os.linux.close(master);
        return error.IoctlSptlckFailed;
    }

    var pts_buf: [64]u8 = undefined;
    const pts = std.fmt.bufPrintZ(&pts_buf, "/dev/pts/{d}", .{ptn}) catch {
        _ = std.os.linux.close(master);
        return error.PathTooLong;
    };
    const slave_rc = std.os.linux.open(pts.ptr, std.os.linux.O{ .ACCMODE = .RDWR, .NOCTTY = true, .CLOEXEC = true }, 0);
    if (isErr(slave_rc)) {
        _ = std.os.linux.close(master);
        return error.OpenSlaveFailed;
    }
    const slave: i32 = @intCast(slave_rc);
    return .{ .master = master, .slave = slave };
}

// ── argv extraction ─────────────────────────────────────────────────────────

fn argvStrings(allocator: std.mem.Allocator, value: std.json.Value) ![][]const u8 {
    if (value != .array or value.array.items.len == 0) return error.InvalidArgv;
    const out = try allocator.alloc([]const u8, value.array.items.len);
    for (value.array.items, 0..) |item, i| {
        if (item != .string) return error.InvalidArgv;
        out[i] = item.string;
    }
    return out;
}

// ── spawn: fork a controlling-terminal session leader ───────────────────────

fn spawnTerminal(
    allocator: std.mem.Allocator,
    argv: []const []const u8,
    workdir: []const u8,
    env: std.json.Value,
    rows: u16,
    cols: u16,
) !Terminal {
    if (argv.len == 0) return error.EmptyArgv;

    const pty = try openPty();

    // Build argv/envp before fork; the child inherits the memory via COW.
    const argv_z = try allocator.alloc([:0]u8, argv.len);
    const argv_ptrs = try allocator.allocSentinel(?[*:0]const u8, argv.len, null);
    for (argv, 0..) |arg, i| {
        argv_z[i] = try allocator.dupeZ(u8, arg);
        argv_ptrs[i] = argv_z[i].ptr;
    }
    var env_build = exec.buildEnvp(allocator, env, &.{}) catch {
        for (argv_z) |s| allocator.free(s);
        allocator.free(argv_z);
        allocator.free(argv_ptrs);
        _ = std.os.linux.close(pty.master);
        _ = std.os.linux.close(pty.slave);
        return error.EnvBuildFailed;
    };

    var workdir_buf: [4096]u8 = undefined;
    const workdir_z = std.fmt.bufPrintZ(&workdir_buf, "{s}", .{workdir}) catch return error.PathTooLong;

    const child_pid = std.os.linux.fork();
    if (isErr(child_pid)) {
        env_build.deinit();
        for (argv_z) |s| allocator.free(s);
        allocator.free(argv_z);
        allocator.free(argv_ptrs);
        _ = std.os.linux.close(pty.master);
        _ = std.os.linux.close(pty.slave);
        return error.ForkFailed;
    }
    if (child_pid == 0) {
        // Child: become a session leader owning the slave as controlling tty.
        _ = std.os.linux.setsid();
        _ = std.os.linux.ioctl(pty.slave, TIOCSCTTY, 0);
        _ = std.os.linux.dup2(pty.slave, 0);
        _ = std.os.linux.dup2(pty.slave, 1);
        _ = std.os.linux.dup2(pty.slave, 2);
        if (pty.slave > 2) _ = std.os.linux.close(pty.slave);
        _ = std.os.linux.close(pty.master);
        _ = std.os.linux.chdir(workdir_z);
        _ = std.os.linux.execve(argv_ptrs[0].?, argv_ptrs.ptr, env_build.envpPtr());
        std.os.linux.exit(127);
    }

    // Parent: free fork-only state, drop slave, apply initial window size.
    env_build.deinit();
    for (argv_z) |s| allocator.free(s);
    allocator.free(argv_z);
    allocator.free(argv_ptrs);
    _ = std.os.linux.close(pty.slave);
    const ws = Winsize{ .ws_row = rows, .ws_col = cols };
    _ = std.os.linux.ioctl(pty.master, TIOCSWINSZ, @intFromPtr(&ws));

    return .{
        .id = 0,
        .master_fd = pty.master,
        .pid = @intCast(child_pid),
        .rows = rows,
        .cols = cols,
    };
}

// ── terminal table ──────────────────────────────────────────────────────────

fn register(term: Terminal) ?u32 {
    lockTable();
    defer unlockTable();
    if (count >= terminals.len) {
        _ = std.os.linux.close(term.master_fd);
        return null;
    }
    const id = next_id;
    next_id += 1;
    for (&terminals) |*slot| {
        if (slot.* == null) {
            var t = term;
            t.id = id;
            slot.* = t;
            count += 1;
            return id;
        }
    }
    _ = std.os.linux.close(term.master_fd);
    return null;
}

fn unregister(id: u32) void {
    lockTable();
    defer unlockTable();
    for (&terminals) |*slot| {
        if (slot.*) |t| {
            if (t.id == id) {
                _ = std.os.linux.close(t.master_fd);
                slot.* = null;
                count -= 1;
                return;
            }
        }
    }
}

fn appendOutput(id: u32, data: []const u8) void {
    lockTable();
    defer unlockTable();
    if (findTerminal(id)) |t| {
        for (data) |b| {
            t.buf[t.buf_head] = b;
            t.buf_head = (t.buf_head + 1) % BUFFER_MAX;
            if (t.buf_len < BUFFER_MAX) t.buf_len += 1;
        }
        t.total_bytes += data.len;
    }
}

fn markDone(id: u32, exit_code: i32) void {
    lockTable();
    defer unlockTable();
    if (findTerminal(id)) |t| {
        t.exit_code = exit_code;
        t.done = true;
    }
}

/// Copy the most recent `out.len` buffered bytes (in delivery order) into
/// `out`; returns the number of bytes written.
fn readTail(id: u32, out: []u8) usize {
    lockTable();
    defer unlockTable();
    const t = findTerminal(id) orelse return 0;
    const n = @min(t.buf_len, out.len);
    const start = (t.buf_head + BUFFER_MAX - t.buf_len) % BUFFER_MAX;
    for (0..n) |i| {
        out[i] = t.buf[(start + i) % BUFFER_MAX];
    }
    return n;
}

// ── pump thread ─────────────────────────────────────────────────────────────

fn reapExitCode(pid: i32) i32 {
    var status: u32 = 0;
    const rc = std.os.linux.syscall4(
        .wait4,
        @as(usize, @bitCast(@as(isize, @intCast(pid)))),
        @intFromPtr(&status),
        0,
        0,
    );
    if (isErr(rc)) return 0; // auto-reaped (SA_NOCLDWAIT) → no exit status
    if (std.posix.W.IFEXITED(status)) return @intCast(std.posix.W.EXITSTATUS(status));
    return 0;
}

fn pumpThread(id: u32, master_fd: i32, pid: i32) void {
    var buf: [4096]u8 = undefined;
    while (true) {
        const n = std.os.linux.read(master_fd, &buf, buf.len);
        if (n == 0) break; // EOF: all slave fds closed
        if (isErr(n)) break; // EIO/EBADF: slave side gone
        appendOutput(id, buf[0..@intCast(n)]);
    }
    markDone(id, reapExitCode(pid));
}

// ── request structs ─────────────────────────────────────────────────────────

const CreateRequest = struct {
    argv: std.json.Value = .{ .null = {} },
    workdir: []const u8 = "/workspace",
    env: std.json.Value = .{ .null = {} },
    rows: u16 = 24,
    cols: u16 = 80,
};

const InputRequest = struct {
    terminal_id: u32 = 0,
    data: []const u8 = "",
};

const ResizeRequest = struct {
    terminal_id: u32 = 0,
    rows: u16 = 0,
    cols: u16 = 0,
};

const SignalRequest = struct {
    terminal_id: u32 = 0,
    signal: []const u8 = "",
};

const DestroyRequest = struct {
    terminal_id: u32 = 0,
    grace_ms: u32 = 5000,
};

// ── POST /pty/create ────────────────────────────────────────────────────────

pub fn handleCreate(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(CreateRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    const argv = argvStrings(allocator, req.argv) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"argv must be a non-empty array of strings\"}");
        return;
    };
    defer allocator.free(argv);

    const term = spawnTerminal(allocator, argv, req.workdir, req.env, req.rows, req.cols) catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", msg);
        return;
    };

    const id = register(term) orelse {
        try main.writeResponse(client_fd, "503 Service Unavailable", "application/json", "{\"error\":\"terminal table full\"}");
        return;
    };

    const spawned = std.Thread.spawn(.{}, pumpThread, .{ id, term.master_fd, term.pid }) catch null;
    if (spawned) |t| t.detach();

    const resp = try std.fmt.allocPrint(allocator, "{{\"terminal_id\":{d},\"pid\":{d}}}", .{ id, term.pid });
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

// ── POST /pty/input ─────────────────────────────────────────────────────────

pub fn handleInput(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(InputRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    lockTable();
    const t = findTerminal(req.terminal_id);
    if (t == null) {
        unlockTable();
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"terminal not found\"}");
        return;
    }
    const master_fd = t.?.master_fd;
    unlockTable();

    var written: usize = 0;
    while (written < req.data.len) {
        const n = std.os.linux.write(master_fd, req.data.ptr + written, req.data.len - written);
        if (isErr(n) or n == 0) break;
        written += n;
    }
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

// ── POST /pty/resize ────────────────────────────────────────────────────────

pub fn handleResize(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(ResizeRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    lockTable();
    const t = findTerminal(req.terminal_id);
    if (t == null) {
        unlockTable();
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"terminal not found\"}");
        return;
    }
    const master_fd = t.?.master_fd;
    unlockTable();

    const ws = Winsize{ .ws_row = req.rows, .ws_col = req.cols };
    _ = std.os.linux.ioctl(master_fd, TIOCSWINSZ, @intFromPtr(&ws));
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

// ── GET /pty/foreground ─────────────────────────────────────────────────────

pub fn handleForeground(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    const id = parseIdQuery(query) orelse {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"terminal_id required\"}");
        return;
    };

    lockTable();
    const t = findTerminal(id);
    if (t == null) {
        unlockTable();
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"terminal not found\"}");
        return;
    }
    const master_fd = t.?.master_fd;
    unlockTable();

    const pgrp = std.posix.tcgetpgrp(master_fd) catch {
        try main.writeResponse(client_fd, "200 OK", "application/json", "{\"process_group_id\":-1,\"input_waiting\":false}");
        return;
    };
    const resp = try std.fmt.allocPrint(allocator, "{{\"process_group_id\":{d},\"input_waiting\":false}}", .{pgrp});
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

// ── POST /pty/signal ────────────────────────────────────────────────────────

pub fn handleSignal(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(SignalRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    const sig: std.os.linux.SIG = if (std.mem.eql(u8, req.signal, "SIGINT") or std.mem.eql(u8, req.signal, "INT"))
        .INT
    else if (std.mem.eql(u8, req.signal, "SIGTERM") or std.mem.eql(u8, req.signal, "TERM"))
        .TERM
    else if (std.mem.eql(u8, req.signal, "SIGKILL") or std.mem.eql(u8, req.signal, "KILL"))
        .KILL
    else if (std.mem.eql(u8, req.signal, "SIGTSTP") or std.mem.eql(u8, req.signal, "TSTP"))
        .TSTP
    else if (std.mem.eql(u8, req.signal, "SIGHUP") or std.mem.eql(u8, req.signal, "HUP"))
        .HUP
    else {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"unsupported signal\"}");
        return;
    };

    lockTable();
    const t = findTerminal(req.terminal_id);
    if (t == null) {
        unlockTable();
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"terminal not found\"}");
        return;
    }
    const master_fd = t.?.master_fd;
    unlockTable();

    const pgrp = std.posix.tcgetpgrp(master_fd) catch {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"foreground group not resolvable\"}");
        return;
    };
    _ = std.posix.kill(-pgrp, sig) catch {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"signal delivery failed\"}");
        return;
    };
    const resp = try std.fmt.allocPrint(allocator, "{{\"process_group_id\":{d}}}", .{pgrp});
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

// ── POST /pty/destroy ───────────────────────────────────────────────────────

fn isGroupAlive(pgid: i32) bool {
    const pid: std.os.linux.pid_t = -pgid;
    const probe = std.os.linux.syscall2(.kill, @as(usize, @bitCast(@as(isize, @intCast(pid)))), 0);
    return probe == 0;
}

pub fn handleDestroy(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(DestroyRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    lockTable();
    const t = findTerminal(req.terminal_id);
    if (t == null) {
        unlockTable();
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"terminal not found\"}");
        return;
    }
    const master_fd = t.?.master_fd;
    const pid = t.?.pid;
    t.?.master_fd = -1; // unregister must not re-close this fd
    unlockTable();

    // Closing the master hangs up the foreground process group (SIGHUP);
    // then TERM -> grace -> KILL the session leader's group.
    _ = std.os.linux.close(master_fd);
    _ = std.posix.kill(-pid, .TERM) catch {};

    const iters: u32 = if (req.grace_ms < 50) 1 else req.grace_ms / 50;
    var i: u32 = 0;
    while (isGroupAlive(pid) and i < iters) : (i += 1) {
        sleepPoll();
    }
    if (isGroupAlive(pid)) {
        _ = std.posix.kill(-pid, .KILL) catch {};
    }

    unregister(req.terminal_id);
    try main.writeResponse(client_fd, "200 OK", "application/json", "{\"ok\":true}");
}

// ── GET /pty/stream ─────────────────────────────────────────────────────────

fn parseIdQuery(query: []const u8) ?u32 {
    const prefix = "terminal_id=";
    if (std.mem.startsWith(u8, query, prefix)) {
        return std.fmt.parseInt(u32, query[prefix.len..], 10) catch null;
    }
    return null;
}

fn writeDataFrame(allocator: std.mem.Allocator, client_fd: i32, data: []const u8) !void {
    const encoded_len = std.base64.standard.Encoder.calcSize(data.len);
    const encoded = try allocator.alloc(u8, encoded_len);
    defer allocator.free(encoded);
    _ = std.base64.standard.Encoder.encode(encoded, data);

    _ = std.os.linux.write(client_fd, "data: ".ptr, "data: ".len);
    _ = std.os.linux.write(client_fd, encoded.ptr, encoded.len);
    _ = std.os.linux.write(client_fd, "\n\n".ptr, "\n\n".len);
}

pub fn handleStream(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    const id = parseIdQuery(query) orelse {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"terminal_id required\"}");
        return;
    };

    lockTable();
    const t0 = findTerminal(id) orelse {
        unlockTable();
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"terminal not found\"}");
        return;
    };
    const pid = t0.pid;
    unlockTable();

    const header = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: close\r\n\r\n";
    _ = std.os.linux.write(client_fd, header.ptr, header.len);

    var pid_frame_buf: [64]u8 = undefined;
    const pid_frame = std.fmt.bufPrint(&pid_frame_buf, "data: {{\"pid\":{d}}}\n\n", .{pid}) catch return;
    _ = std.os.linux.write(client_fd, pid_frame.ptr, pid_frame.len);

    var sent: u64 = 0;
    var truncated = false;
    var exit_code: i32 = 0;
    while (true) {
        lockTable();
        const tt = findTerminal(id) orelse {
            unlockTable();
            break;
        };
        const total = tt.total_bytes;
        const done = tt.done;
        exit_code = tt.exit_code;
        unlockTable();

        const new_bytes = total - sent;
        if (new_bytes > 0) {
            const n: usize = @intCast(@min(new_bytes, BUFFER_MAX));
            var out: [BUFFER_MAX]u8 = undefined;
            const got = readTail(id, out[0..n]);
            if (got > 0) try writeDataFrame(allocator, client_fd, out[0..got]);
            if (new_bytes > BUFFER_MAX) truncated = true;
            sent = total;
        }
        if (done and sent >= total) break;
        sleepPoll();
    }

    const trunc_lit: []const u8 = if (truncated) "true" else "false";
    var exit_buf: [128]u8 = undefined;
    const exit_frame = std.fmt.bufPrint(&exit_buf, "data: {{\"exit\":{d},\"signal\":\"\",\"truncated\":{s}}}\n\n", .{ exit_code, trunc_lit }) catch {
        _ = std.os.linux.write(client_fd, "data: {\"exit\":0,\"signal\":\"\"}\n\n".ptr, 28);
        return;
    };
    _ = std.os.linux.write(client_fd, exit_frame.ptr, exit_frame.len);
}
