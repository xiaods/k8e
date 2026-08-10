const std = @import("std");
const main = @import("main.zig");
const venv = @import("venv.zig");

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

    var stdout_trunc = false;
    var stderr_trunc = false;
    const stdout = readAllFromFdTrunc(allocator, stdout_pipe[0], max_stdout_bytes, &stdout_trunc) catch
        allocator.dupe(u8, "") catch @panic("oom");
    const stderr = readAllFromFdTrunc(allocator, stderr_pipe[0], max_stderr_bytes, &stderr_trunc) catch
        allocator.dupe(u8, "") catch @panic("oom");

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
        if (err == .SUCCESS and std.posix.W.IFEXITED(status)) {
            break :blk @intCast(std.posix.W.EXITSTATUS(status));
        }
        break :blk 0;
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

        // Fork /bin/sh -c <command> with stdout piped
        var stdout_pipe: [2]i32 = undefined;
        if (std.os.linux.pipe2(&stdout_pipe, std.os.linux.O{ .CLOEXEC = true }) < 0) {
            try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", "{\"error\":\"pipe failed\"}");
            return;
        }

        const child_pid = std.os.linux.fork();
        if (child_pid == 0) {
            // Child: close read end, dup stdout to write end, exec
            _ = std.os.linux.close(stdout_pipe[0]);
            _ = std.os.linux.dup2(stdout_pipe[1], 1);
            _ = std.os.linux.dup2(stdout_pipe[1], 2);
            _ = std.os.linux.close(stdout_pipe[1]);

            var cwd_buf: [4096]u8 = undefined;
            const cwd_z = std.fmt.bufPrintZ(&cwd_buf, "{s}", .{req.workdir}) catch unreachable;
            _ = std.os.linux.chdir(cwd_z);

            const argv: [3:null]?[*:0]const u8 = .{ "/bin/sh".ptr, "-c".ptr, cmd_z };
            _ = std.os.linux.execve("/bin/sh", &argv, env_build.envpPtr());
            std.os.linux.exit(1);
        }

        // Parent: close write end
        _ = std.os.linux.close(stdout_pipe[1]);

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

        // Stream stdout to client
        var stdout_buf: [4096]u8 = undefined;
        while (true) {
            const n = std.os.linux.read(stdout_pipe[0], &stdout_buf, stdout_buf.len);
            if (n <= 0) break;
            const data = stdout_buf[0..@as(usize, @intCast(n))];
            var line_buf: [4200]u8 = undefined;
            const line = std.fmt.bufPrint(&line_buf, "data: {s}\n\n", .{data}) catch {
                _ = std.os.linux.write(client_fd, "data: {\"error\":\"output too large\"}\n\n".ptr, 34);
                break;
            };
            _ = std.os.linux.write(client_fd, line.ptr, line.len);
        }

        _ = std.os.linux.close(stdout_pipe[0]);

        // Reap child
        var status: u32 = 0;
        _ = std.os.linux.syscall4(
            .wait4,
            @as(usize, @bitCast(@as(isize, @intCast(child_pid)))),
            @intFromPtr(&status),
            0,
            0,
        );
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
    const resp = try std.fmt.allocPrint(allocator,
        "{{\"stdout\":\"{s}\",\"stderr\":\"{s}\",\"exit_code\":{d},\"duration_ms\":{d},\"truncated\":{s}}}",
        .{ stdout_json, stderr_json, result.exit_code, result.duration_ms, trunc_lit });
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
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
