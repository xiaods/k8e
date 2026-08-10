const std = @import("std");

/// Disk-only, activity-gated event stream (KIP-16 L5 / issue #513).
///
/// Mirrors ephemeral-sandbox's observability principle: events are appended as
/// NDJSON lines to a capped rotating file on disk; reading never triggers
/// collection and an idle daemon does zero work (a write only happens when an
/// operation occurs). This keeps the daemon's memory bounded and adds no wakeup
/// cost when nothing is happening.
///
/// Format per line: {"t":<unix_sec>,"ev":"<event>","sid":"<session>",...}
/// Events: exec_start, exec_end, file_write, file_read, bg_submit, bg_poll.

const EVENT_DIR = "/workspace/.k8e_events";
/// Hard cap for the event file; when exceeded the file is rotated (old -> .1).
pub const max_event_bytes: usize = 4 * 1024 * 1024;
const MAX_LINE: usize = 4096;

/// append writes one NDJSON event line. Best-effort: failures never break the
/// operation being observed. When the file exceeds max_event_bytes it is
/// rotated to <file>.1 (overwriting the previous generation), keeping disk
/// usage bounded to ~2x the cap.
pub fn append(session_id: []const u8, event: []const u8, extra: []const u8) void {
    appendAt(EVENT_DIR, session_id, event, extra);
}

/// appendAt is append with an explicit event dir (testable).
pub fn appendAt(dir: []const u8, session_id: []const u8, event: []const u8, extra: []const u8) void {
    var path_buf: [512]u8 = undefined;
    const path = std.fmt.bufPrintZ(&path_buf, "{s}/events.ndjson", .{dir}) catch return;

    _ = std.os.linux.mkdir(@ptrCast(dir.ptr), 0o755);

    // Rotate if the current file exceeds the cap.
    rotateIfNeeded(path) catch {};

    const fd_raw = std.os.linux.open(path.ptr, std.os.linux.O{
        .CREAT = true,
        .ACCMODE = .WRONLY,
        .APPEND = true,
    }, 0o644);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return;
    defer _ = std.os.linux.close(@intCast(fd));

    const ts = getUnixSeconds();
    var line_buf: [MAX_LINE]u8 = undefined;
    const line = std.fmt.bufPrint(&line_buf, "{{\"t\":{d},\"ev\":\"{s}\",\"sid\":\"{s}\"{s}}}\n",
        .{ ts, event, session_id, extra }) catch return;
    _ = std.os.linux.write(@intCast(fd), line.ptr, line.len);
}

/// rotateIfNeeded renames events.ndjson -> events.ndjson.1 when it exceeds the
/// cap, so the active file starts fresh and total disk usage stays bounded.
fn rotateIfNeeded(path: [:0]const u8) !void {
    const fd_raw = std.os.linux.open(path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return; // no file yet
    defer _ = std.os.linux.close(@intCast(fd));

    const size_rc: isize = @bitCast(std.os.linux.lseek(@intCast(fd), 0, std.os.linux.SEEK.END));
    if (size_rc < 0) return;
    if (@as(usize, @intCast(size_rc)) < max_event_bytes) return;

    // events.ndjson -> events.ndjson.1 (overwrite old generation)
    var old_buf: [512]u8 = undefined;
    const old_path = std.fmt.bufPrintZ(&old_buf, "{s}.1", .{path}) catch return;
    _ = std.os.linux.rename(path.ptr, old_path.ptr);
}

/// getUnixSeconds returns wall-clock seconds since the epoch.
fn getUnixSeconds() i64 {
    var ts: std.os.linux.timespec = undefined;
    _ = std.os.linux.clock_gettime(std.os.linux.CLOCK.REALTIME, &ts);
    return @intCast(ts.sec);
}
