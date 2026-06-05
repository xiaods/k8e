const std = @import("std");
const main = @import("main.zig");
const venv = @import("venv.zig");

const ExecRequest = struct {
    command: []const u8 = "",
    timeout: u32 = 30,
    workdir: []const u8 = "/workspace",
};

pub const ExecResult = struct {
    stdout: []u8,
    stderr: []u8,
    exit_code: i32,

    pub fn deinit(self: ExecResult, allocator: std.mem.Allocator) void {
        allocator.free(self.stdout);
        allocator.free(self.stderr);
    }
};

/// runCommand spawns /bin/sh -c <command> in workdir and returns stdout/stderr.
/// Uses raw fork/exec with pipe-based I/O.
pub fn runCommand(allocator: std.mem.Allocator, command: []const u8, workdir: []const u8) !ExecResult {
    // Null-terminate command for execve (child has no allocator)
    var cmd_buf: [65536]u8 = undefined;
    if (command.len >= cmd_buf.len) return error.CommandTooLong;
    @memcpy(cmd_buf[0..command.len], command);
    cmd_buf[command.len] = 0;
    const cmd_z: [*:0]const u8 = @ptrCast(&cmd_buf);
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

        // Exec /bin/sh
        const argv: [3:null]?[*:0]const u8 = .{
            "/bin/sh".ptr,
            "-c".ptr,
            cmd_z,
        };
        _ = std.os.linux.execve("/bin/sh", &argv, &[1:null]?[*:0]const u8{null});
        // If execve returns, it's an error
        std.os.linux.exit(1);
    }

    // Parent process
    _ = std.os.linux.close(stdout_pipe[1]); // close write end
    _ = std.os.linux.close(stderr_pipe[1]); // close write end

    const stdout = readAllFromFd(allocator, stdout_pipe[0], 10 * 1024 * 1024) catch
        allocator.dupe(u8, "") catch @panic("oom");
    const stderr = readAllFromFd(allocator, stderr_pipe[0], 1 * 1024 * 1024) catch
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

    return ExecResult{ .stdout = stdout, .stderr = stderr, .exit_code = exit_code };
}

pub fn readAllFromFd(allocator: std.mem.Allocator, fd: i32, max_bytes: usize) ![]u8 {
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
fn spawnTimeoutKiller(pid: std.os.linux.pid_t, timeout_sec: u32) void {
    const ts = std.os.linux.timespec{ .sec = @as(isize, @intCast(timeout_sec)), .nsec = 0 };
    _ = std.os.linux.nanosleep(&ts, null);
    _ = std.os.linux.kill(pid, std.os.linux.SIG.KILL);
}

pub fn handleExec(allocator: std.mem.Allocator, client_fd: i32, body: []const u8, streaming: bool) !void {
    const parsed = std.json.parseFromSlice(ExecRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.command.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"command required\"}");
        return;
    }

    venv.ensureVenv();
    const activated = try venv.activateCommand(allocator, req.command);
    defer allocator.free(activated);

    if (streaming) {
        // Null-terminate command for execve (child has no allocator)
        var cmd_buf: [65536]u8 = undefined;
        if (activated.len >= cmd_buf.len) {
            try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"command too long\"}");
            return;
        }
        @memcpy(cmd_buf[0..activated.len], activated);
        cmd_buf[activated.len] = 0;
        const cmd_z: [*:0]const u8 = @ptrCast(&cmd_buf);

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
            _ = std.os.linux.execve("/bin/sh", &argv, &[1:null]?[*:0]const u8{null});
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

    const result = runCommand(allocator, activated, req.workdir) catch |err| {
        const msg = try std.fmt.allocPrint(allocator, "{{\"error\":\"{s}\"}}", .{@errorName(err)});
        defer allocator.free(msg);
        try main.writeResponse(client_fd, "500 Internal Server Error", "application/json", msg);
        return;
    };
    defer result.deinit(allocator);

    const stdout_json = try jsonEscape(allocator, result.stdout);
    defer allocator.free(stdout_json);
    const stderr_json = try jsonEscape(allocator, result.stderr);
    defer allocator.free(stderr_json);

    const resp = try std.fmt.allocPrint(allocator,
        "{{\"stdout\":\"{s}\",\"stderr\":\"{s}\",\"exit_code\":{d}}}",
        .{ stdout_json, stderr_json, result.exit_code });
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
