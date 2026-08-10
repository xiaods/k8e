const std = @import("std");

/// isReady reports whether the workspace virtualenv is fully initialized
/// (i.e. /workspace/.venv/bin/python exists and is executable). Used by the
/// /ready application-layer handshake so consumers never claim a warm pod whose
/// venv is still lazily being created.
pub fn isReady() bool {
    const fd = std.os.linux.open("/workspace/.venv/bin/python", std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    if (@as(isize, @bitCast(fd)) >= 0) {
        _ = std.os.linux.close(@intCast(fd));
        return true;
    }
    return false;
}

/// ensureVenv creates /workspace/.venv via python3 -m venv if it does not exist.
/// Safe to call multiple times — fast no-op after the first creation.
/// Called at startup and after workspace resets which wipe /workspace.
pub fn ensureVenv() void {
    // Fast path: check if venv already exists
    const fd = std.os.linux.open("/workspace/.venv/bin/python", std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    if (@as(isize, @bitCast(fd)) >= 0) {
        _ = std.os.linux.close(@intCast(fd));
        return;
    }

    // Create venv: python3 -m venv /workspace/.venv
    const cmd = "python3 -m venv /workspace/.venv";
    const argv = [4:null]?[*:0]const u8{ "/bin/sh", "-c", cmd, null };

    const pid = std.os.linux.fork();
    if (pid == 0) {
        const envp = [2:null]?[*:0]const u8{
            "PATH=/workspace/.venv/bin:/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin",
            "VIRTUAL_ENV=/workspace/.venv",
        };
        _ = std.os.linux.execve("/bin/sh", &argv, &envp);
        std.os.linux.exit(1);
    }
    var status: u32 = 0;
    _ = std.os.linux.syscall4(.wait4, @as(usize, @bitCast(@as(isize, @intCast(pid)))), @intFromPtr(&status), 0, 0);
}
