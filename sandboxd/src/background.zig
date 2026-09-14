const std = @import("std");
const main = @import("main.zig");
const exec = @import("exec.zig");

const BG_DIR = "/workspace/.k8e_bg";

// Fixed wrapper run by /bin/sh. An EXIT trap records the real exit code of the
// user command on any normal shell exit (including `exit N` and errors), so a
// completed background task is detectable without waitpid — sandboxd reaps all
// children via SIGCHLD=IGN (see main.zig). The command and run dir are passed
// through the environment (K8E_BG_CMD / K8E_BG_DIR) to avoid shell-quoting the
// user command into this string. A SIGKILL from the timeout killer is not
// trappable, so the killer writes exit_code=-1 itself.
const BG_WRAPPER = "trap 'rc=$?; printf %s \"$rc\" > \"$K8E_BG_DIR/exit_code\"' EXIT; eval \"$K8E_BG_CMD\"";

/// handleBgSubmit forks a child that runs the command in the background.
/// POST /exec/background
/// Body: {"command": "...", "run_id": "...", "timeout": 300, "workdir": "/workspace"}
///
/// `timeout` is the run's lifetime cap in seconds; 0 means "no cap", which is
/// what a long-running service (the `run --background` + `expose` flow) needs.
/// The submitter must not send a short default blindly: sandboxd SIGKILLs the
/// run once the cap expires, so a 30s default killed every exposed server
/// thirty seconds after it started.
pub fn handleBgSubmit(allocator: std.mem.Allocator, client_fd: i32, body: []const u8) !void {
    const parsed = std.json.parseFromSlice(BgSubmitRequest, allocator, body, .{ .ignore_unknown_fields = true, .allocate = .alloc_always }) catch {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"invalid json\"}");
        return;
    };
    defer parsed.deinit();
    const req = parsed.value;

    if (req.command.len == 0 or req.run_id.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"command and run_id required\"}");
        return;
    }

    // Ensure background directories exist (paths must be null-terminated for syscalls).
    var run_dir_buf: [512]u8 = undefined;
    const run_dir = try std.fmt.bufPrintZ(&run_dir_buf, "{s}/{s}", .{ BG_DIR, req.run_id });
    _ = std.os.linux.mkdir(@ptrCast(BG_DIR.ptr), 0o755);
    _ = std.os.linux.mkdir(run_dir.ptr, 0o755);

    // Record the real start time (unix seconds).
    {
        var started_path: [600]u8 = undefined;
        const sp = std.fmt.bufPrintZ(&started_path, "{s}/started_at", .{run_dir}) catch unreachable;
        const fd = std.os.linux.open(sp.ptr, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
        if (fd >= 0) {
            var ts_buf: [24]u8 = undefined;
            const ts_str = std.fmt.bufPrint(&ts_buf, "{d}", .{unixSeconds()}) catch "0";
            _ = std.os.linux.write(@intCast(fd), ts_str.ptr, ts_str.len);
            _ = std.os.linux.close(@intCast(fd));
        }
    }

    // Build envp in the parent so the post-fork child can execve without allocating.
    // Defaults (PATH/VIRTUAL_ENV) + session env + K8E_BG_* for the wrapper.
    var env_build = try exec.buildEnvp(allocator, req.env, &.{
        .{ "K8E_BG_DIR", run_dir },
        .{ "K8E_BG_CMD", req.command },
    });
    defer env_build.deinit();

    const wrapper_z: [*:0]const u8 = BG_WRAPPER;
    const argv = [3:null]?[*:0]const u8{ "/bin/sh".ptr, "-c".ptr, wrapper_z };

    const pid = std.os.linux.fork();
    if (pid == 0) {
        // Child: lead a new process group (pgid == pid) so the timeout killer
        // can signal the whole tree. The wrapper `eval`s the user command, so
        // the command — and anything it forks, e.g. an exposed HTTP server — is
        // a grandchild of sandboxd; killing only the wrapper pid left it
        // running as an orphan while poll already reported "timed_out".
        _ = std.os.linux.syscall2(.setpgid, 0, 0);

        // Child: redirect stdout/stderr to files, then exec the wrapper.
        var stdout_path: [600]u8 = undefined;
        const sp = std.fmt.bufPrintZ(&stdout_path, "{s}/stdout", .{run_dir}) catch std.os.linux.exit(1);
        var stderr_path: [600]u8 = undefined;
        const ep = std.fmt.bufPrintZ(&stderr_path, "{s}/stderr", .{run_dir}) catch std.os.linux.exit(1);

        const stdout_fd = std.os.linux.open(sp.ptr, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
        const stderr_fd = std.os.linux.open(ep.ptr, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
        _ = std.os.linux.dup2(@intCast(stdout_fd), 1);
        _ = std.os.linux.dup2(@intCast(stderr_fd), 2);
        _ = std.os.linux.close(@intCast(stdout_fd));
        _ = std.os.linux.close(@intCast(stderr_fd));

        var cwd_buf: [4096]u8 = undefined;
        const cwd_z = std.fmt.bufPrintZ(&cwd_buf, "{s}", .{req.workdir}) catch "/workspace";
        _ = std.os.linux.chdir(cwd_z);

        // Record the pid so poll can report "running" before completion.
        var pid_path: [600]u8 = undefined;
        const pp = std.fmt.bufPrintZ(&pid_path, "{s}/pid", .{run_dir}) catch std.os.linux.exit(1);
        const pid_fd = std.os.linux.open(pp.ptr, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
        if (pid_fd >= 0) {
            var pb: [16]u8 = undefined;
            const ps = std.fmt.bufPrint(&pb, "{d}", .{std.os.linux.getpid()}) catch "";
            _ = std.os.linux.write(@intCast(pid_fd), ps.ptr, ps.len);
            _ = std.os.linux.close(@intCast(pid_fd));
        }

        _ = std.os.linux.execve("/bin/sh", &argv, env_build.envpPtr());
        std.os.linux.exit(1);
    }

    // Parent: arm a timeout killer if requested.
    if (req.timeout > 0) {
        const run_dir_owned = allocator.dupeZ(u8, run_dir) catch null;
        if (run_dir_owned) |owned| {
            const watcher = std.Thread.spawn(.{}, spawnTimeoutKiller, .{ @as(i32, @intCast(pid)), req.timeout, owned, allocator }) catch blk: {
                allocator.free(owned);
                break :blk null;
            };
            if (watcher) |w| w.detach();
        }
    }

    var resp_buf: [256]u8 = undefined;
    const resp = try std.fmt.bufPrint(&resp_buf, "{{\"status\":\"started\",\"run_id\":\"{s}\"}}", .{req.run_id});
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);

    // Disk-only event stream (KIP-16 L5): record background submission.
    const events = @import("events.zig");
    var ev_buf: [256]u8 = undefined;
    const ev_extra = std.fmt.bufPrint(&ev_buf, ",\"run_id\":\"{s}\"", .{req.run_id}) catch "";
    events.append("", "bg_submit", ev_extra);
}

/// handleBgPoll reports the status of a background task and returns its output
/// once an exit_code has been recorded.
/// GET /exec/background/<run_id>
pub fn handleBgPoll(allocator: std.mem.Allocator, client_fd: i32, run_id: []const u8) !void {
    var path_buf: [600]u8 = undefined;

    // Terminal state: exit_code present.
    const exit_path = try std.fmt.bufPrintZ(&path_buf, "{s}/{s}/exit_code", .{ BG_DIR, run_id });
    if (readFileZ(allocator, exit_path.ptr, 32)) |exit_data| {
        defer allocator.free(exit_data);
        const exit_code = std.fmt.parseInt(i32, std.mem.trim(u8, exit_data, &std.ascii.whitespace), 10) catch 0;

        var sp_buf: [600]u8 = undefined;
        const stdout_path = try std.fmt.bufPrintZ(&sp_buf, "{s}/{s}/stdout", .{ BG_DIR, run_id });
        const stdout_raw = readFileZ(allocator, stdout_path.ptr, 10 * 1024 * 1024) orelse try allocator.dupe(u8, "");
        defer allocator.free(stdout_raw);

        var ep_buf: [600]u8 = undefined;
        const stderr_path = try std.fmt.bufPrintZ(&ep_buf, "{s}/{s}/stderr", .{ BG_DIR, run_id });
        const stderr_raw = readFileZ(allocator, stderr_path.ptr, 10 * 1024 * 1024) orelse try allocator.dupe(u8, "");
        defer allocator.free(stderr_raw);

        const stdout_esc = try exec.jsonEscape(allocator, stdout_raw);
        defer allocator.free(stdout_esc);
        const stderr_esc = try exec.jsonEscape(allocator, stderr_raw);
        defer allocator.free(stderr_esc);

        const status = if (exit_code == -1) "timed_out" else "completed";
        const resp = try std.fmt.allocPrint(allocator, "{{\"run_id\":\"{s}\",\"status\":\"{s}\",\"exit_code\":{d},\"stdout\":\"{s}\",\"stderr\":\"{s}\"}}", .{ run_id, status, exit_code, stdout_esc, stderr_esc });
        defer allocator.free(resp);
        try main.writeResponse(client_fd, "200 OK", "application/json", resp);
        return;
    }

    // Running: pid recorded but no exit_code yet.
    const pid_path = try std.fmt.bufPrintZ(&path_buf, "{s}/{s}/pid", .{ BG_DIR, run_id });
    if (readFileZ(allocator, pid_path.ptr, 16)) |pid_data| {
        allocator.free(pid_data);
        var resp_buf: [256]u8 = undefined;
        const resp = try std.fmt.bufPrint(&resp_buf, "{{\"run_id\":\"{s}\",\"status\":\"running\"}}", .{run_id});
        try main.writeResponse(client_fd, "200 OK", "application/json", resp);
        return;
    }

    var resp_buf: [256]u8 = undefined;
    const resp = try std.fmt.bufPrint(&resp_buf, "{{\"run_id\":\"{s}\",\"status\":\"not_found\"}}", .{run_id});
    try main.writeResponse(client_fd, "404 Not Found", "application/json", resp);
}

const BgSubmitRequest = struct {
    command: []const u8 = "",
    run_id: []const u8 = "",
    timeout: u32 = 0,
    workdir: []const u8 = "/workspace",
    // Optional object of string->string; applied at exec time with K8E_BG_* extras.
    env: std.json.Value = .{ .null = {} },
};

/// killAllRuns SIGKILLs every still-running background run recorded under
/// BG_DIR, then leaves the run directories for /workspace/reset to delete.
///
/// Called from /workspace/reset because a pod handed back to the warm pool must
/// not carry the previous tenant's processes: an orphaned server keeps its port
/// bound, so the next tenant's service — and any `expose` of that port — fails
/// to start. Runs that already recorded an exit code are skipped, since their
/// pid may have been recycled by an unrelated process.
pub fn killAllRuns(allocator: std.mem.Allocator) void {
    const dir_z: [*:0]const u8 = BG_DIR;
    const fd = std.os.linux.open(dir_z, std.os.linux.O{ .ACCMODE = .RDONLY, .DIRECTORY = true }, 0);
    if (@as(isize, @bitCast(fd)) < 0) return;
    defer _ = std.os.linux.close(@intCast(fd));

    var buf: [4096]u8 align(@alignOf(std.os.linux.dirent64)) = undefined;
    while (true) {
        const n = std.os.linux.getdents64(@intCast(fd), @ptrCast(&buf), buf.len);
        if (n <= 0) break;
        const total: usize = @intCast(n);
        var pos: usize = 0;
        while (pos < total) {
            const dent = @as(*align(1) std.os.linux.dirent64, @ptrCast(&buf[pos]));
            pos += dent.reclen;
            const name = std.mem.sliceTo(@as([*:0]u8, @ptrCast(&dent.name)), 0);
            if (std.mem.eql(u8, name, ".") or std.mem.eql(u8, name, "..")) continue;

            var exit_path_buf: [600]u8 = undefined;
            const exit_path = std.fmt.bufPrintZ(&exit_path_buf, "{s}/{s}/exit_code", .{ BG_DIR, name }) catch continue;
            if (readFileZ(allocator, exit_path.ptr, 32)) |recorded| {
                allocator.free(recorded);
                continue; // already finished on its own
            }

            var pid_path_buf: [600]u8 = undefined;
            const pid_path = std.fmt.bufPrintZ(&pid_path_buf, "{s}/{s}/pid", .{ BG_DIR, name }) catch continue;
            const raw = readFileZ(allocator, pid_path.ptr, 32) orelse continue;
            defer allocator.free(raw);
            const pid = std.fmt.parseInt(i32, std.mem.trim(u8, raw, " \n\r\t"), 10) catch continue;
            if (pid <= 1) continue;

            // Group first (the run's child called setpgid), pid as a fallback.
            _ = std.os.linux.kill(-pid, std.os.linux.SIG.KILL);
            _ = std.os.linux.kill(pid, std.os.linux.SIG.KILL);
        }
    }
}

// readFileZ reads up to max bytes from a null-terminated path via raw syscalls,
// returning null if the file cannot be opened.
fn readFileZ(allocator: std.mem.Allocator, path: [*:0]const u8, max: usize) ?[]u8 {
    const fd = std.os.linux.open(path, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    if (@as(isize, @bitCast(fd)) < 0) return null;
    defer _ = std.os.linux.close(@intCast(fd));
    return exec.readAllFromFd(allocator, @intCast(fd), max) catch null;
}

// unixSeconds returns the current wall-clock time in seconds since the epoch.
fn unixSeconds() i64 {
    var ts: std.os.linux.timespec = undefined;
    _ = std.os.linux.clock_gettime(std.os.linux.CLOCK.REALTIME, &ts);
    return @intCast(ts.sec);
}

// spawnTimeoutKiller kills the run after timeout_sec if it has not already
// completed, recording exit_code=-1 so poll reports "timed_out". It signals the
// run's entire process group (the child called setpgid, so pgid == pid), so a
// service started by the wrapper dies with it instead of surviving the kill as
// an orphan holding its port.
fn spawnTimeoutKiller(pid: i32, timeout_sec: u32, run_dir: [:0]u8, allocator: std.mem.Allocator) void {
    defer allocator.free(run_dir);
    const ts = std.os.linux.timespec{ .sec = @as(isize, @intCast(timeout_sec)), .nsec = 0 };
    _ = std.os.linux.nanosleep(&ts, null);

    var exit_path_buf: [600]u8 = undefined;
    const exit_path = std.fmt.bufPrintZ(&exit_path_buf, "{s}/exit_code", .{run_dir}) catch return;

    // If the command already finished, leave its recorded exit code untouched.
    const probe = std.os.linux.open(exit_path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    if (@as(isize, @bitCast(probe)) >= 0) {
        _ = std.os.linux.close(@intCast(probe));
        return;
    }

    _ = std.os.linux.kill(-pid, std.os.linux.SIG.KILL);
    // Fallback for the case where the group signal could not be delivered
    // (setpgid not applied): a no-op once the group kill reaped this pid.
    _ = std.os.linux.kill(@as(std.os.linux.pid_t, @intCast(pid)), std.os.linux.SIG.KILL);
    const fd = std.os.linux.open(exit_path.ptr, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd >= 0) {
        _ = std.os.linux.write(@intCast(fd), "-1", 2);
        _ = std.os.linux.close(@intCast(fd));
    }
}
