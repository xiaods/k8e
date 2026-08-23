const std = @import("std");
const main = @import("main.zig");
const exec = @import("exec.zig");

/// Process topology endpoint (KIP-16 M5 follow-up / issue #513).
///
/// Mirrors ephemeral-sandbox's namespace-identity process tracking: read-only
/// /proc enumeration reporting pid, command, and state for every process
/// visible in the pod's pid namespace. Reading never triggers collection and
/// an idle daemon does zero extra work (the /proc scan happens only when a
/// consumer requests it).

/// handleProcesses serves GET /processes — JSON array of running processes.
pub fn handleProcesses(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    _ = query;

    var out = std.array_list.Managed(u8).init(allocator);
    defer out.deinit();
    try out.appendSlice("{\"processes\":[");

    const proc_dir = "/proc";

    // Enumerate /proc/<pid> numeric dirs.
    const fd_raw = std.os.linux.open(proc_dir.ptr, std.os.linux.O{ .ACCMODE = .RDONLY, .DIRECTORY = true }, 0);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) {
        try main.writeResponse(client_fd, "200 OK", "application/json", "{\"processes\":[]}");
        return;
    }
    defer _ = std.os.linux.close(@intCast(fd));

    var buf: [4096]u8 align(@alignOf(std.os.linux.dirent64)) = undefined;
    var wrote_any = false;
    while (true) {
        const n = std.os.linux.getdents64(@intCast(fd), @ptrCast(&buf), buf.len);
        if (n <= 0) break;
        var pos: usize = 0;
        while (pos < @as(usize, @intCast(n))) {
            const dent = @as(*align(1) std.os.linux.dirent64, @ptrCast(&buf[pos]));
            pos += dent.reclen;
            const name = std.mem.sliceTo(@as([*:0]u8, @ptrCast(&dent.name)), 0);
            // Only numeric pid dirs.
            const pid = std.fmt.parseInt(i32, name, 10) catch continue;
            if (pid <= 0) continue;

            const comm = readComm(allocator, name) orelse continue;
            defer allocator.free(comm);
            const escaped = try exec.jsonEscape(allocator, comm);
            defer allocator.free(escaped);
            const state = readState(name) orelse '?';

            if (wrote_any) try out.append(',');
            wrote_any = true;
            var entry_buf: [512]u8 = undefined;
            const entry = try std.fmt.bufPrint(&entry_buf, "{{\"pid\":{d},\"comm\":\"{s}\",\"state\":\"{c}\"}}",
                .{ pid, escaped, state });
            try out.appendSlice(entry);
        }
    }

    try out.appendSlice("]}");
    try main.writeResponse(client_fd, "200 OK", "application/json", out.items);
}

/// readComm returns the process command name from /proc/<pid>/comm.
/// Allocates with the caller's allocator so the caller can free it — the
/// previous page_allocator-here / request-allocator-there mismatch was UB
/// (DebugAllocator aborts on cross-allocator frees; /processes crashed).
fn readComm(allocator: std.mem.Allocator, pid: []const u8) ?[]u8 {
    var path_buf: [64]u8 = undefined;
    const path = std.fmt.bufPrintZ(&path_buf, "/proc/{s}/comm", .{pid}) catch return null;

    const fd_raw = std.os.linux.open(path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return null;
    defer _ = std.os.linux.close(@intCast(fd));

    var buf: [256]u8 = undefined;
    const n = std.os.linux.read(@intCast(fd), &buf, buf.len);
    if (n <= 0) return null;
    var s = buf[0..@as(usize, @intCast(n))];
    while (s.len > 0 and (s[s.len - 1] == '\n' or s[s.len - 1] == '\r')) {
        s = s[0 .. s.len - 1];
    }
    return allocator.dupe(u8, s) catch null;
}

/// readState returns the first char of /proc/<pid>/stat state field ("R"/"S"/...).
fn readState(pid: []const u8) ?u8 {
    var path_buf: [64]u8 = undefined;
    const path = std.fmt.bufPrintZ(&path_buf, "/proc/{s}/stat", .{pid}) catch return null;

    const fd_raw = std.os.linux.open(path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return null;
    defer _ = std.os.linux.close(@intCast(fd));

    var buf: [512]u8 = undefined;
    const n = std.os.linux.read(@intCast(fd), &buf, buf.len);
    if (n <= 0) return null;
    const s = buf[0..@as(usize, @intCast(n))];
    // Format: pid (comm) state ... — find the last ')' then skip space.
    const close = std.mem.lastIndexOfScalar(u8, s, ')') orelse return null;
    const rest = s[close + 1 ..];
    if (rest.len < 2) return null;
    return rest[1]; // after the separating space
}
