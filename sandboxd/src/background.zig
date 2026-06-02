const std = @import("std");
const main = @import("main.zig");

const BG_DIR = "/workspace/.k8e_bg";

/// handleBgSubmit forks a child process for background execution.
/// POST /exec/background
/// Body: {"command": "...", "run_id": "...", "timeout": 300, "workdir": "/workspace"}
pub fn handleBgSubmit(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(BgSubmitRequest, allocator, body, .{ .ignore_unknown_fields = true }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.command.len == 0 or req.run_id.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"command and run_id required\"}");
        return;
    }

    // Ensure background directory exists
    var run_dir_buf: [512]u8 = undefined;
    const run_dir = try std.fmt.bufPrint(&run_dir_buf, "{s}/{s}", .{ BG_DIR, req.run_id });
    _ = std.os.linux.mkdir(@ptrCast(BG_DIR.ptr), 0o755);
    _ = std.os.linux.mkdir(@ptrCast(run_dir.ptr), 0o755);

    // Write started_at timestamp
    var started_buf: [64]u8 = undefined;
    const started_path = try std.fmt.bufPrint(&started_buf, "{s}/started_at", .{run_dir});
    const started_fd = std.os.linux.open(@as([*:0]const u8, @ptrCast(started_path.ptr)), std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (started_fd >= 0) {
        const timestamp_str = "0";
        _ = std.os.linux.write(@intCast(started_fd), timestamp_str.ptr, timestamp_str.len);
        _ = std.os.linux.close(@intCast(started_fd));
    }

    // Fork child process
    const pid = std.os.linux.fork();
    if (pid == 0) {
        // Child: redirect stdout/stderr to files, exec command
        var stdout_path: [512]u8 = undefined;
        const sp = try std.fmt.bufPrint(&stdout_path, "{s}/stdout", .{run_dir});
        var stderr_path: [512]u8 = undefined;
        const ep = try std.fmt.bufPrint(&stderr_path, "{s}/stderr", .{run_dir});

        const stdout_fd = std.os.linux.open(@as([*:0]const u8, @ptrCast(sp.ptr)), std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
        const stderr_fd = std.os.linux.open(@as([*:0]const u8, @ptrCast(ep.ptr)), std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);

        _ = std.os.linux.dup2(@intCast(stdout_fd), 1);
        _ = std.os.linux.dup2(@intCast(stderr_fd), 2);
        _ = std.os.linux.close(@intCast(stdout_fd));
        _ = std.os.linux.close(@intCast(stderr_fd));

        // Change working directory
        var cwd_buf: [4096]u8 = undefined;
        const cwd_z = std.fmt.bufPrintZ(&cwd_buf, "{s}", .{req.workdir}) catch "/workspace";
        _ = std.os.linux.chdir(cwd_z);

        // Write PID file
        var pid_buf: [512]u8 = undefined;
        const pid_path = try std.fmt.bufPrint(&pid_buf, "{s}/pid", .{run_dir});
        const pid_fd = std.os.linux.open(@as([*:0]const u8, @ptrCast(pid_path.ptr)), std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
        if (pid_fd >= 0) {
            const child_pid = std.os.linux.getpid();
            var pb: [16]u8 = undefined;
            const ps = try std.fmt.bufPrint(&pb, "{d}", .{child_pid});
            _ = std.os.linux.write(@intCast(pid_fd), ps.ptr, ps.len);
            _ = std.os.linux.close(@intCast(pid_fd));
        }

        // Null-terminate command
        var cmd_buf: [65536]u8 = undefined;
        if (req.command.len < cmd_buf.len) {
            @memcpy(cmd_buf[0..req.command.len], req.command);
            cmd_buf[req.command.len] = 0;
        }

        const argv: [3:null]?[*:0]const u8 = .{
            "/bin/sh".ptr,
            "-c".ptr,
            @as([*:0]const u8, @ptrCast(&cmd_buf)),
        };
        _ = std.os.linux.execve("/bin/sh", &argv, &[1:null]?[*:0]const u8{null});
        std.os.linux.exit(1);
    }

    // Parent: check timeout and set up killer if needed
    if (req.timeout > 0) {
        std.Thread.spawn(.{}, spawnTimeoutKiller, .{ @as(i32, @intCast(pid)), req.timeout, run_dir, allocator }) catch |_| {};
    }

    var resp_buf: [256]u8 = undefined;
    const resp = try std.fmt.bufPrint(&resp_buf, "{{\"status\":\"started\",\"run_id\":\"{s}\"}}", .{req.run_id});
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

/// handleBgPoll checks the status of a background task.
/// GET /exec/background/<run_id>
pub fn handleBgPoll(allocator: std.mem.Allocator, client_fd: i32, run_id: []const u8) !void {
    var run_dir_buf: [512]u8 = undefined;
    const run_dir = try std.fmt.bufPrint(&run_dir_buf, "{s}/{s}", .{ BG_DIR, run_id });

    // Check exit_code file
    var exit_path: [512]u8 = undefined;
    const ep = try std.fmt.bufPrint(&exit_path, "{s}/exit_code", .{run_dir});
    const exit_data = std.fs.cwd().readFileAlloc(allocator, ep, 32) catch null;
    if (exit_data) |data| {
        const exit_code = std.fmt.parseInt(i32, std.mem.trim(u8, data, &std.ascii.whitespace), 10) catch 0;
        defer allocator.free(data);

        const stdout_data = readFileOrEmpty(allocator, try std.fmt.bufPrint(&exit_path, "{s}/stdout", .{run_dir}));
        defer if (stdout_data) |d| allocator.free(d);
        const stderr_data = readFileOrEmpty(allocator, try std.fmt.bufPrint(&exit_path, "{s}/stderr", .{run_dir}));
        defer if (stderr_data) |d| allocator.free(d);

        var resp_buf: [1024]u8 = undefined;
        // Simple inline JSON (no full escaping for now — stdout/stderr are raw)
        const resp = try std.fmt.bufPrint(&resp_buf,
            "{{\"run_id\":\"{s}\",\"status\":\"completed\",\"exit_code\":{d}}}",
            .{ run_id, exit_code });
        try main.writeResponse(client_fd, "200 OK", "application/json", resp);
        return;
    }

    // Check if pid file exists → status is running
    var pid_path: [512]u8 = undefined;
    const pp = try std.fmt.bufPrint(&pid_path, "{s}/pid", .{run_dir});
    const pid_data = std.fs.cwd().readFileAlloc(allocator, pp, 16) catch null;
    if (pid_data) |data| {
        defer allocator.free(data);
        var resp_buf: [256]u8 = undefined;
        const resp = try std.fmt.bufPrint(&resp_buf, "{{\"run_id\":\"{s}\",\"status\":\"running\"}}", .{run_id});
        try main.writeResponse(client_fd, "200 OK", "application/json", resp);
        return;
    }

    // No pid, no exit_code → task not found
    var resp_buf: [256]u8 = undefined;
    const resp = try std.fmt.bufPrint(&resp_buf, "{{\"run_id\":\"{s}\",\"status\":\"not_found\"}}", .{run_id});
    try main.writeResponse(client_fd, "404 Not Found", "application/json", resp);
}

const BgSubmitRequest = struct {
    command: []const u8 = "",
    run_id: []const u8 = "",
    timeout: u32 = 0,
    workdir: []const u8 = "/workspace",
};

fn readFileOrEmpty(allocator: std.mem.Allocator, path: []const u8) ?[]u8 {
    return std.fs.cwd().readFileAlloc(allocator, path, 10 * 1024 * 1024) catch null;
}

fn spawnTimeoutKiller(pid: i32, timeout_sec: u32, run_dir: []const u8, allocator: std.mem.Allocator) void {
    _ = allocator;
    std.time.sleep(timeout_sec * std.time.ns_per_s);
    // Kill the child process
    _ = std.os.linux.kill(@as(std.os.linux.pid_t, @intCast(pid)), std.os.linux.SIG.KILL);
    // Write exit_code file so poll knows it timed out
    var exit_path_buf: [512]u8 = undefined;
    const exit_path = std.fmt.bufPrint(&exit_path_buf, "{s}/exit_code", .{run_dir}) catch return;
    const fd = std.os.linux.open(@as([*:0]const u8, @ptrCast(exit_path.ptr)), std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd >= 0) {
        _ = std.os.linux.write(@intCast(fd), "-1", 2);
        _ = std.os.linux.close(@intCast(fd));
    }
}
