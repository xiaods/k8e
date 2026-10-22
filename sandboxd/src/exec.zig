const std = @import("std");
const main = @import("main.zig");
const venv = @import("venv.zig");
const execctl = @import("execctl.zig");

const default_path_value = "/workspace/.venv/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin";
const default_venv_value = "/workspace/.venv";

const ExecRequest = struct {
    command: []const u8 = "",
    timeout: u32 = 30,
    workdir: []const u8 = "/workspace",
    // Optional object of string->string; absent/null means defaults only.
    env: std.json.Value = .{ .null = {} },
    // Optional session id for transcript recording (KIP-16 M4).
    session_id: []const u8 = "",
};

// max_stdout_bytes / max_stderr_bytes: capture caps (~1 MiB agent-facing policy).
const max_stdout_bytes: usize = 1 * 1024 * 1024;
const max_stderr_bytes: usize = 1 * 1024 * 1024;

pub const ExecResult = struct {
    stdout: []u8,
    stderr: []u8,
    exit_code: i32,
    duration_ms: i64 = 0,
    truncated: bool = false,

    pub fn deinit(self: ExecResult, allocator: std.mem.Allocator) void {
        allocator.free(self.stdout);
        allocator.free(self.stderr);
    }
};

/// Owned envp for execve: null-terminated pointer list + owned KEY=VAL strings.
pub const EnvBuild = struct {
    allocator: std.mem.Allocator,
    /// Pointers for execve; last entry is null. Length is entry_count+1.
    ptrs: []?[*:0]const u8,
    entries: [][:0]u8,

    pub fn deinit(self: *EnvBuild) void {
        for (self.entries) |e| self.allocator.free(e);
        self.allocator.free(self.entries);
        self.allocator.free(self.ptrs);
        self.* = undefined;
    }

    pub fn envpPtr(self: *const EnvBuild) [*:null]?[*:0]const u8 {
        return @ptrCast(self.ptrs.ptr);
    }
};

/// buildEnvp merges sandbox defaults (PATH, VIRTUAL_ENV) with optional user env.
/// User keys override defaults. extra_pairs are forced last (e.g. K8E_BG_* for background).
pub fn buildEnvp(
    allocator: std.mem.Allocator,
    user_env: std.json.Value,
    extra_pairs: []const struct { []const u8, []const u8 },
) !EnvBuild {
    var map = std.StringHashMap([]const u8).init(allocator);
    defer map.deinit();

    try map.put("PATH", default_path_value);
    try map.put("VIRTUAL_ENV", default_venv_value);

    if (user_env == .object) {
        var it = user_env.object.iterator();
        while (it.next()) |entry| {
            if (entry.key_ptr.*.len == 0) continue;
            if (entry.value_ptr.* != .string) continue;
            try map.put(entry.key_ptr.*, entry.value_ptr.*.string);
        }
    }

    for (extra_pairs) |pair| {
        try map.put(pair[0], pair[1]);
    }

    const n = map.count();
    var entries = try allocator.alloc([:0]u8, n);
    var filled: usize = 0;
    errdefer {
        for (entries[0..filled]) |e| allocator.free(e);
        allocator.free(entries);
    }
    var ptrs = try allocator.alloc(?[*:0]const u8, n + 1);
    errdefer allocator.free(ptrs);

    var i: usize = 0;
    var mit = map.iterator();
    while (mit.next()) |e| : (i += 1) {
        const kv = try std.fmt.allocPrintSentinel(allocator, "{s}={s}", .{ e.key_ptr.*, e.value_ptr.* }, 0);
        entries[i] = kv;
        ptrs[i] = kv.ptr;
        filled = i + 1;
    }
    ptrs[n] = null;

    return EnvBuild{
        .allocator = allocator,
        .ptrs = ptrs,
        .entries = entries,
    };
}

/// runCommand spawns /bin/sh -c <command> in workdir and returns stdout/stderr.
/// Uses raw fork/exec with pipe-based I/O. user_env is a JSON value (object or null).
pub fn runCommand(allocator: std.mem.Allocator, command: []const u8, workdir: []const u8) !ExecResult {
    return runCommandWithEnv(allocator, command, workdir, .{ .null = {} });
}

pub fn runCommandWithEnv(allocator: std.mem.Allocator, command: []const u8, workdir: []const u8, user_env: std.json.Value) !ExecResult {
    // Null-terminate command for execve (child has no allocator)
    var cmd_buf: [65536]u8 = undefined;
    if (command.len >= cmd_buf.len) return error.CommandTooLong;
    @memcpy(cmd_buf[0..command.len], command);
    cmd_buf[command.len] = 0;
    const cmd_z: [*:0]const u8 = @ptrCast(&cmd_buf);

    var env_build = try buildEnvp(allocator, user_env, &.{});
    defer env_build.deinit();

    var stdout_pipe: [2]i32 = undefined;
    var stderr_pipe: [2]i32 = undefined;
    if (std.os.linux.pipe2(&stdout_pipe, std.os.linux.O{ .CLOEXEC = true }) < 0) {
        return error.PipeFailed;
    }
    if (std.os.linux.pipe2(&stderr_pipe, std.os.linux.O{ .CLOEXEC = true }) < 0) {
        _ = std.os.linux.close(stdout_pipe[0]);
        _ = std.os.linux.close(stdout_pipe[1]);
        return error.PipeFailed;
    }

    var ts_start: std.os.linux.timespec = undefined;
    _ = std.os.linux.clock_gettime(std.os.linux.CLOCK.MONOTONIC, &ts_start);

    const pid = std.os.linux.fork();
    if (pid == 0) {
        // Child process
        _ = std.os.linux.close(stdout_pipe[0]); // close read end
        _ = std.os.linux.close(stderr_pipe[0]); // close read end

        // Redirect stdout/stderr to pipes
        _ = std.os.linux.dup2(stdout_pipe[1], 1);
        _ = std.os.linux.dup2(stderr_pipe[1], 2);
        _ = std.os.linux.close(stdout_pipe[1]);
        _ = std.os.linux.close(stderr_pipe[1]);

        // Change working directory
        var cwd_buf: [4096]u8 = undefined;
        const cwd_z = std.fmt.bufPrintZ(&cwd_buf, "{s}", .{workdir}) catch return error.PathTooLong;
        _ = std.os.linux.chdir(cwd_z);

        // Exec /bin/sh with merged env (defaults + session env)
        const argv: [3:null]?[*:0]const u8 = .{
            "/bin/sh".ptr,
            "-c".ptr,
            cmd_z,
        };
        _ = std.os.linux.execve("/bin/sh", &argv, env_build.envpPtr());
        // If execve returns, it's an error
        std.os.linux.exit(1);
    }

    // Parent process
    _ = std.os.linux.close(stdout_pipe[1]); // close write end
    _ = std.os.linux.close(stderr_pipe[1]); // close write end

    // Drain stdout and stderr CONCURRENTLY: a child that fills its stderr
    // pipe (>64KiB) while the parent blocks reading stdout to EOF would
    // deadlock forever (no timeout killer on this path). Read stderr on a
    // helper thread so both pipes drain together.
    const StdErrResult = struct { data: []u8, truncated: bool };
    var stderr_res: StdErrResult = undefined;
    var stderr_trunc = false;
    var stderr_thread: ?std.Thread = null;
    if (std.Thread.spawn(.{}, struct {
        fn read(a: std.mem.Allocator, fd: i32, max: usize, out: *StdErrResult) void {
            var truncated = false;
            out.data = readAllFromFdTrunc(a, fd, max, &truncated) catch
                a.dupe(u8, "") catch @panic("oom");
            out.truncated = truncated;
        }
    }.read, .{ allocator, stderr_pipe[0], max_stderr_bytes, &stderr_res })) |t| {
        stderr_thread = t;
    } else |err| {
        // Thread spawn failure is vanishingly rare; fall back to sequential
        // reads (small outputs unaffected).
        std.log.warn("exec: stderr drain thread spawn failed: {s}", .{@errorName(err)});
    }

    var stdout_trunc = false;
    const stdout = readAllFromFdTrunc(allocator, stdout_pipe[0], max_stdout_bytes, &stdout_trunc) catch
        allocator.dupe(u8, "") catch @panic("oom");

    const stderr: []u8 = blk: {
        if (stderr_thread) |t| {
            t.join();
            stderr_trunc = stderr_res.truncated;
            break :blk stderr_res.data;
        }
        // Fallback: no thread available, drain sequentially (small outputs).
        break :blk readAllFromFdTrunc(allocator, stderr_pipe[0], max_stderr_bytes, &stderr_trunc) catch
            allocator.dupe(u8, "") catch @panic("oom");
    };

    _ = std.os.linux.close(stdout_pipe[0]);
    _ = std.os.linux.close(stderr_pipe[0]);

    const exit_code: i32 = blk: {
        var status: u32 = 0;
        const rc = std.os.linux.syscall4(
            .wait4,
            @as(usize, @bitCast(@as(isize, @intCast(pid)))),
            @intFromPtr(&status),
            0,
            0,
        );
        const err = std.posix.errno(@as(usize, @bitCast(rc)));
        // A real SIGCHLD handler (main.setupSignals) leaves the status waitable,
        // so this recovers it. The old fallback of 0 reported success for a
        // killed command, which made `cmd; echo $?` lie to every caller.
        if (err != .SUCCESS) break :blk -1;
        if (std.posix.W.IFEXITED(status)) break :blk @intCast(std.posix.W.EXITSTATUS(status));
        if (std.posix.W.IFSIGNALED(status)) {
            break :blk 128 + @as(i32, @intCast(@as(u8, @truncate(status))));
        }
        break :blk -1;
    };

    var ts_end: std.os.linux.timespec = undefined;
    _ = std.os.linux.clock_gettime(std.os.linux.CLOCK.MONOTONIC, &ts_end);
    const duration_ms: i64 = blk: {
        const sec = ts_end.sec - ts_start.sec;
        const nsec = ts_end.nsec - ts_start.nsec;
        break :blk @as(i64, @intCast(sec)) * 1000 + @divTrunc(@as(i64, @intCast(nsec)), 1_000_000);
    };

    return ExecResult{
        .stdout = stdout,
        .stderr = stderr,
        .exit_code = exit_code,
        .duration_ms = if (duration_ms < 0) 0 else duration_ms,
        .truncated = stdout_trunc or stderr_trunc,
    };
}

pub fn readAllFromFd(allocator: std.mem.Allocator, fd: i32, max_bytes: usize) ![]u8 {
    var trunc = false;
    return readAllFromFdTrunc(allocator, fd, max_bytes, &trunc);
}

/// readAllFromFdTrunc reads up to max_bytes; sets *truncated when more data was available.
pub fn readAllFromFdTrunc(allocator: std.mem.Allocator, fd: i32, max_bytes: usize, truncated: *bool) ![]u8 {
    truncated.* = false;
    var buf = try allocator.alloc(u8, max_bytes);
    errdefer allocator.free(buf);
    var total: usize = 0;
    var tmp: [4096]u8 = undefined;
    while (total < max_bytes) {
        const n = std.os.linux.read(fd, &tmp, @min(tmp.len, max_bytes - total));
        if (n < 0) {
            allocator.free(buf);
            return error.ReadFailed;
        }
        if (n == 0) break;
        @memcpy(buf[total..][0..@as(usize, @intCast(n))], tmp[0..@as(usize, @intCast(n))]);
        total += @as(usize, @intCast(n));
    }
    if (total >= max_bytes) {
        // Drain remainder so the writer does not block; mark truncated.
        var drain: [4096]u8 = undefined;
        while (true) {
            const n = std.os.linux.read(fd, &drain, drain.len);
            if (n <= 0) break;
            truncated.* = true;
        }
    }
    // Shrink to exact size so DebugAllocator canary check passes
    if (allocator.resize(buf, total)) {
        return buf.ptr[0..total];
    }
    // Resize not supported — copy to exact-fit buffer
    const exact = try allocator.alloc(u8, total);
    @memcpy(exact, buf[0..total]);
    allocator.free(buf);
    return exact;
}

/// spawnTimeoutKiller kills the child process after timeout_sec seconds.
/// Checks process liveness first to avoid killing a reused PID.
fn spawnTimeoutKiller(pid: std.os.linux.pid_t, timeout_sec: u32) void {
    const ts = std.os.linux.timespec{ .sec = @as(isize, @intCast(timeout_sec)), .nsec = 0 };
    _ = std.os.linux.nanosleep(&ts, null);
    // Signal 0 probes whether the process still exists (no signal delivered).
    const probe = std.os.linux.syscall2(
        .kill,
        @as(usize, @bitCast(@as(isize, @intCast(pid)))),
        0,
    );
    if (probe != 0) return;
    _ = std.os.linux.kill(pid, std.os.linux.SIG.KILL);
}

pub fn handleExec(allocator: std.mem.Allocator, client_fd: i32, body: []const u8, streaming: bool) !void {
    const parsed = std.json.parseFromSlice(ExecRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.command.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"command required\"}");
        return;
    }

    // Ensure venv exists (survives workspace resets)
    venv.ensureVenv();

    if (streaming) {
        // Null-terminate command for execve (child has no allocator)
        var cmd_buf: [65536]u8 = undefined;
        if (req.command.len >= cmd_buf.len) {
            try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"command too long\"}");
            return;
        }
        @memcpy(cmd_buf[0..req.command.len], req.command);
        cmd_buf[req.command.len] = 0;
        const cmd_z: [*:0]const u8 = @ptrCast(&cmd_buf);

        var env_build = buildEnvp(allocator, req.env, &.{}) catch {
            try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"env build failed\"}");
            return;
        };
        defer env_build.deinit();

        // Fork /bin/sh -c <command> with stdout and stderr piped SEPARATELY.
        // They used to share one pipe (both fds dup2'd onto stdout_pipe), so
        // the stream carried no channel information and every byte reached
        // the client as stdout — the E2B SDK's stderr was always empty and
        // its on_stderr callback never fired. Separate pipes let each frame
        // name its channel.
        var stdout_pipe: [2]i32 = undefined;
        if (std.os.linux.pipe2(&stdout_pipe, std.os.linux.O{ .CLOEXEC = true }) < 0) {
            try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"pipe failed\"}");
            return;
        }
        var stderr_pipe: [2]i32 = undefined;
        if (std.os.linux.pipe2(&stderr_pipe, std.os.linux.O{ .CLOEXEC = true }) < 0) {
            _ = std.os.linux.close(stdout_pipe[0]);
            _ = std.os.linux.close(stdout_pipe[1]);
            try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"pipe failed\"}");
            return;
        }
        var stdin_pipe: [2]i32 = undefined;
        if (std.os.linux.pipe2(&stdin_pipe, std.os.linux.O{ .CLOEXEC = true }) < 0) {
            _ = std.os.linux.close(stdout_pipe[0]);
            _ = std.os.linux.close(stdout_pipe[1]);
            _ = std.os.linux.close(stderr_pipe[0]);
            _ = std.os.linux.close(stderr_pipe[1]);
            try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"pipe failed\"}");
            return;
        }

        const child_pid = std.os.linux.fork();
        if (child_pid == 0) {
            // Child: close read ends, dup stdout/stderr to their own write
            // ends, dup stdin from the pipe read end, exec.
            _ = std.os.linux.close(stdout_pipe[0]);
            _ = std.os.linux.close(stderr_pipe[0]);
            _ = std.os.linux.close(stdin_pipe[1]); // child keeps read end only
            _ = std.os.linux.dup2(stdout_pipe[1], 1);
            _ = std.os.linux.dup2(stderr_pipe[1], 2);
            _ = std.os.linux.dup2(stdin_pipe[0], 0);
            _ = std.os.linux.close(stdout_pipe[1]);
            _ = std.os.linux.close(stderr_pipe[1]);
            _ = std.os.linux.close(stdin_pipe[0]);

            var cwd_buf: [4096]u8 = undefined;
            const cwd_z = std.fmt.bufPrintZ(&cwd_buf, "{s}", .{req.workdir}) catch unreachable;
            _ = std.os.linux.chdir(cwd_z);

            const argv: [3:null]?[*:0]const u8 = .{ "/bin/sh".ptr, "-c".ptr, cmd_z };
            _ = std.os.linux.execve("/bin/sh", &argv, env_build.envpPtr());
            std.os.linux.exit(1);
        }

        // Parent: close write ends (stdout, stderr) and the child's stdin
        // read end; keep the stdin write fd for /exec/stdin via the process
        // table. Also snapshot the command so E2B Process/List can report it
        // and a Connect can describe the process.
        _ = std.os.linux.close(stdout_pipe[1]);
        _ = std.os.linux.close(stderr_pipe[1]);
        _ = std.os.linux.close(stdin_pipe[0]);
        execctl.registerWithConfig(@intCast(child_pid), stdin_pipe[1], req.command);

        // spawn timeout killer thread
        if (req.timeout > 0) {
            const child_pid_i32: std.os.linux.pid_t = @intCast(child_pid);
            const maybe_watcher = std.Thread.spawn(.{}, spawnTimeoutKiller, .{ child_pid_i32, req.timeout }) catch null;
            if (maybe_watcher) |w| {
                w.detach();
            }
        }

        // Write SSE header
        const header = "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nCache-Control: no-cache\r\nConnection: close\r\n\r\n";
        _ = std.os.linux.write(client_fd, header.ptr, header.len);

        // First SSE frame carries the in-guest pid: the e2b layer addresses
        // /exec/stdin and /exec/signal by it (its synthetic process-table pid
        // is not the sandbox's real pid). Frame: data: {"pid":N}\n\n
        var pid_frame_buf: [64]u8 = undefined;
        const pid_frame = std.fmt.bufPrint(&pid_frame_buf, "data: {{\"pid\":{d}}}\n\n", .{child_pid}) catch {
            _ = std.os.linux.write(client_fd, "data: {\"error\":\"pid frame\"}\n\n".ptr, "data: {\"error\":\"pid frame\"}\n\n".len);
            return;
        };
        _ = std.os.linux.write(client_fd, pid_frame.ptr, pid_frame.len);

        // Stream stdout and stderr to the client, buffering for attach
        // (E2B Connect). Each frame names its channel so the client can put
        // the bytes back on the right stream:
        //
        //   data: {"stdout":"<base64>"}\n\n
        //   data: {"stderr":"<base64>"}\n\n
        //
        // The payload is base64 for two reasons: it is JSON-safe (raw bytes
        // may contain quotes, newlines or invalid UTF-8), and it keeps the
        // SSE frame boundary intact — a raw payload containing a blank line
        // used to split the frame and leak the "data: " prefix into the
        // client's stdout.
        //
        // Both pipes are drained together: reading one to EOF before
        // touching the other deadlocks as soon as a child fills the
        // unread pipe (>64KiB) while the other is still open.
        try streamBothChannels(client_fd, stdout_pipe[0], stderr_pipe[0], child_pid);

        _ = std.os.linux.close(stdout_pipe[0]);
        _ = std.os.linux.close(stderr_pipe[0]);
        execctl.markDone(@intCast(child_pid));

        // Reap the child and report its exit code as the final SSE frame so
        // the e2b layer gets the honest code in-stream.
        //
        // wait4 succeeds here because setupSignals installs a real SIGCHLD
        // handler (not SIG_IGN/SA_NOCLDWAIT), so the kernel leaves the status
        // word for us instead of auto-reaping it.
        var status: u32 = 0;
        const wrc = std.os.linux.syscall4(
            .wait4,
            @as(usize, @bitCast(@as(isize, @intCast(child_pid)))),
            @intFromPtr(&status),
            0,
            0,
        );
        const werr = std.posix.errno(@as(usize, @bitCast(wrc)));
        const exit_code: i32 = if (werr != .SUCCESS)
            // Genuinely unreapable (should not happen with a real handler).
            -1
        else if (std.posix.W.IFEXITED(status))
            @intCast(std.posix.W.EXITSTATUS(status))
        else if (std.posix.W.IFSIGNALED(status))
            // Report the terminating signal the way a shell does: 128+signo,
            // so `kill -9` surfaces as 137 rather than a bare -1.
            128 + @as(i32, @intCast(@as(u8, @truncate(status))))
        else
            -1;
        var exit_frame_buf: [64]u8 = undefined;
        const exit_frame = std.fmt.bufPrint(&exit_frame_buf, "data: {{\"exit\":{d}}}\n\n", .{exit_code}) catch {
            _ = std.os.linux.write(client_fd, "data: {\"exit\":-1}\n\n".ptr, "data: {\"exit\":-1}\n\n".len);
            return;
        };
        _ = std.os.linux.write(client_fd, exit_frame.ptr, exit_frame.len);

        execctl.unregister(@intCast(child_pid));
        return;
    }

    const result = runCommandWithEnv(allocator, req.command, req.workdir, req.env) catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", msg);
        return;
    };
    defer result.deinit(allocator);

    // Disk-only event stream (KIP-16 L5): record exec completion.
    const events = @import("events.zig");
    var ev_buf: [128]u8 = undefined;
    const ev_extra = std.fmt.bufPrint(&ev_buf, ",\"exit\":{d},\"dur_ms\":{d}", .{ result.exit_code, result.duration_ms }) catch "";
    events.append(req.session_id, "exec_end", ev_extra);

    const stdout_json = try jsonEscape(allocator, result.stdout);
    defer allocator.free(stdout_json);
    const stderr_json = try jsonEscape(allocator, result.stderr);
    defer allocator.free(stderr_json);

    const trunc_lit: []const u8 = if (result.truncated) "true" else "false";
    const resp = try std.fmt.allocPrint(allocator, "{{\"stdout\":\"{s}\",\"stderr\":\"{s}\",\"exit_code\":{d},\"duration_ms\":{d},\"truncated\":{s}}}", .{ stdout_json, stderr_json, result.exit_code, result.duration_ms, trunc_lit });
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

/// streamBothChannels drains a command's stdout and stderr pipes and writes
/// one SSE frame per chunk, each naming the channel it came from:
///
///     data: {"stdout":"<base64>"}\n\n
///     data: {"stderr":"<base64>"}\n\n
///
/// The two pipes are polled together, never one after the other: a child
/// that fills the stderr pipe (>64KiB) while the parent is still reading
/// stdout would deadlock forever, because the child cannot exit until its
/// stderr is drained and the parent is waiting for stdout to reach EOF.
///
/// Chunks are base64-encoded so the payload is JSON-safe (raw bytes may
/// contain quotes, newlines or invalid UTF-8) and so the SSE frame boundary
/// survives: a raw payload containing a blank line used to split the frame
/// and leak the "data: " prefix into the client's stdout.
fn streamBothChannels(client_fd: i32, stdout_fd: i32, stderr_fd: i32, child_pid: usize) !void {
    const fds = [_]i32{ stdout_fd, stderr_fd };
    const channels = [_][]const u8{ "stdout", "stderr" };
    var open = [_]bool{ true, true };
    var buf: [4096]u8 = undefined;

    while (open[0] or open[1]) {
        var poll_fds: [2]std.os.linux.pollfd = undefined;
        var slot_of = [_]usize{ 0, 0 };
        var n_poll: usize = 0;
        for (fds, 0..) |fd, i| {
            if (!open[i]) continue;
            poll_fds[n_poll] = .{ .fd = fd, .events = std.os.linux.POLL.IN, .revents = 0 };
            slot_of[n_poll] = i;
            n_poll += 1;
        }
        if (n_poll == 0) break;

        // std.posix.poll retries on EINTR internally, so a signal (the
        // daemon installs a SIGCHLD handler) never surfaces as an error.
        const ready = std.posix.poll(poll_fds[0..n_poll], -1) catch |err| {
            if (err == error.SystemResources) return err;
            return err;
        };
        if (ready == 0) continue;

        for (poll_fds[0..n_poll], 0..) |pfd, i| {
            const ch = slot_of[i];
            if (pfd.revents & std.os.linux.POLL.IN == 0) {
                // No data and no readable end: this channel is finished.
                if (pfd.revents & (std.os.linux.POLL.HUP | std.os.linux.POLL.ERR | std.os.linux.POLL.NVAL) != 0) {
                    open[ch] = false;
                }
                continue;
            }
            const n = std.os.linux.read(pfd.fd, &buf, buf.len);
            if (n <= 0) {
                open[ch] = false;
                continue;
            }
            const data = buf[0..@as(usize, @intCast(n))];
            execctl.appendOutput(@intCast(child_pid), data);
            try writeChannelFrame(client_fd, channels[ch], data);
        }
    }
}

/// writeChannelFrame writes one channel-tagged SSE frame carrying the
/// base64-encoded payload.
fn writeChannelFrame(client_fd: i32, channel: []const u8, data: []const u8) !void {
    const enc = std.base64.standard.Encoder;
    const encoded = try std.heap.page_allocator.alloc(u8, enc.calcSize(data.len));
    defer std.heap.page_allocator.free(encoded);
    _ = enc.encode(encoded, data);

    var line_buf: [16384]u8 = undefined;
    const line = std.fmt.bufPrint(&line_buf, "data: {{\"{s}\":\"{s}\"}}\n\n", .{ channel, encoded }) catch {
        _ = std.os.linux.write(client_fd, "data: {\"error\":\"output too large\"}\n\n".ptr, "data: {\"error\":\"output too large\"}\n\n".len);
        return;
    };
    _ = std.os.linux.write(client_fd, line.ptr, line.len);
}

// jsonEscape returns the string with JSON special chars escaped (no surrounding quotes).
pub fn jsonEscape(allocator: std.mem.Allocator, s: []const u8) ![]u8 {
    var out = try std.ArrayList(u8).initCapacity(allocator, 0);
    for (s) |c| {
        switch (c) {
            '"' => try out.appendSlice(allocator, "\\\""),
            '\\' => try out.appendSlice(allocator, "\\\\"),
            '\n' => try out.appendSlice(allocator, "\\n"),
            '\r' => try out.appendSlice(allocator, "\\r"),
            '\t' => try out.appendSlice(allocator, "\\t"),
            else => try out.append(allocator, c),
        }
    }
    return out.toOwnedSlice(allocator);
}
