const std = @import("std");

/// ensureVenv creates /workspace/venv via python3 -m venv if it does not exist.
/// Safe to call multiple times — fast no-op after the first creation.
pub fn ensureVenv() void {
    // Fast path: check if venv already exists
    const fd = std.os.linux.open("/workspace/venv/bin/python", std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    if (@as(isize, @bitCast(fd)) >= 0) {
        _ = std.os.linux.close(@intCast(fd));
        return;
    }

    // Create venv: python3 -m venv /workspace/venv
    const cmd = "python3 -m venv /workspace/venv";
    const argv = [4:null]?[*:0]const u8{ "/bin/sh", "-c", cmd, null };

    const pid = std.os.linux.fork();
    if (pid == 0) {
        _ = std.os.linux.execve("/bin/sh", &argv, &[1:null]?[*:0]const u8{null});
        std.os.linux.exit(1);
    }
    var status: u32 = 0;
    _ = std.os.linux.syscall4(.wait4, @as(usize, @bitCast(@as(isize, @intCast(pid)))), @intFromPtr(&status), 0, 0);
}

/// activateCommand wraps a shell command so it runs with the venv activated
/// (PATH prepended with /workspace/venv/bin). Caller owns the returned string.
pub fn activateCommand(allocator: std.mem.Allocator, command: []const u8) ![]const u8 {
    return try std.fmt.allocPrint(allocator, "export PATH=/workspace/venv/bin:$PATH && {s}", .{command});
}
