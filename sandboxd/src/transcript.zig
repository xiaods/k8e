const std = @import("std");
const main = @import("main.zig");

/// File-backed exec transcripts (KIP-16 M4 / issue #512).
///
/// Every exec (and background run) appends one line per output stream to a
/// per-session transcript file under /workspace/.k8e_transcripts/<session>.log.
/// Consumers read bounded windows via the /transcript endpoint — the file is
/// the source of truth, memory use stays bounded, and windows are resumable by
/// absolute byte offset (mirrors ephemeral-sandbox's transcript_window model).
///
/// Format per line: `<unix_seconds> <stream> <text>\n` where stream is one of
/// stdout|stderr|cmd.

const TRANSCRIPT_DIR = "/workspace/.k8e_transcripts";
/// Hard cap for a single window read (bounds per-request memory).
pub const max_window_bytes: usize = 256 * 1024;

fn transcriptPath(buf: []u8, base_dir: []const u8, session_id: []const u8) ?[:0]u8 {
    return std.fmt.bufPrintZ(buf, "{s}/{s}.log", .{ base_dir, session_id }) catch null;
}

/// appendLine appends one transcript line for the session, creating the
/// directory and file on first use. Best-effort: failures never break exec.
pub fn appendLine(allocator: std.mem.Allocator, session_id: []const u8, stream: []const u8, text: []const u8) void {
    appendLineAt(allocator, TRANSCRIPT_DIR, session_id, stream, text);
}

/// appendLineAt is appendLine with an explicit base dir (testable).
pub fn appendLineAt(allocator: std.mem.Allocator, base_dir: []const u8, session_id: []const u8, stream: []const u8, text: []const u8) void {
    if (session_id.len == 0) return;
    var path_buf: [512]u8 = undefined;
    const path = transcriptPath(&path_buf, base_dir, session_id) orelse return;

    _ = std.os.linux.mkdir(@ptrCast(base_dir.ptr), 0o755);
    // Open append-only, creating if missing.
    // open returns usize; a failure is encoded as a huge value from -errno.
    // Cast to isize so the negativity check is reliable.
    const fd_raw = std.os.linux.open(path.ptr, std.os.linux.O{
        .CREAT = true,
        .ACCMODE = .WRONLY,
        .APPEND = true,
    }, 0o644);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return;
    defer _ = std.os.linux.close(@intCast(fd));

    const ts = getUnixSeconds();
    const line = std.fmt.allocPrint(allocator, "{d} {s} {s}\n", .{ ts, stream, text }) catch return;
    defer allocator.free(line);
    _ = std.os.linux.write(@intCast(fd), line.ptr, line.len);
}

/// getUnixSeconds returns wall-clock seconds since the epoch (same helper shape
/// as background.zig's unixSeconds).
fn getUnixSeconds() i64 {
    var ts: std.os.linux.timespec = undefined;
    _ = std.os.linux.clock_gettime(std.os.linux.CLOCK.REALTIME, &ts);
    return @intCast(ts.sec);
}

/// Window is a bounded, line-aligned slice of a session transcript.
/// The caller owns output's allocation and must free it.
pub const Window = struct {
    output: []u8, // caller-owned allocation (free with allocator)
    absolute_offset: i64, // file offset this window starts at (== requested offset when in range)
    next_offset: i64, // offset for the next window; == file size at EOF
    truncated_before: bool, // true when the window starts mid-line (bytes were skipped)
    eof: bool, // next_offset reached end of file
};

/// readWindow reads up to limit bytes starting at offset, line-aligning the end
/// so no partial line is returned. limit is clamped to max_window_bytes.
/// Returns null when the session has no transcript file yet.
pub fn readWindow(allocator: std.mem.Allocator, session_id: []const u8, offset: i64, limit: usize) ?Window {
    return readWindowAt(allocator, TRANSCRIPT_DIR, session_id, offset, limit);
}

/// readWindowAt is readWindow with an explicit base dir (testable).
pub fn readWindowAt(allocator: std.mem.Allocator, base_dir: []const u8, session_id: []const u8, offset: i64, limit: usize) ?Window {
    if (session_id.len == 0) return null;
    var path_buf: [512]u8 = undefined;
    const path = transcriptPath(&path_buf, base_dir, session_id) orelse return null;

    const fd_raw = std.os.linux.open(path.ptr, std.os.linux.O{ .ACCMODE = .RDONLY }, 0);
    const fd: isize = @bitCast(fd_raw);
    if (fd < 0) return null;
    defer _ = std.os.linux.close(@intCast(fd));

    // File size via lseek to EOF (avoids fstat portability differences).
    const fd_i32: i32 = @intCast(fd);
    const size_rc: isize = @bitCast(std.os.linux.lseek(fd_i32, 0, std.os.linux.SEEK.END));
    if (size_rc < 0) return null; // errno: lseek failed
    const file_size: i64 = @intCast(size_rc);
    _ = std.os.linux.lseek(fd_i32, 0, std.os.linux.SEEK.SET);
    if (file_size == 0) {
        return Window{ .output = "", .absolute_offset = 0, .next_offset = 0, .truncated_before = false, .eof = true };
    }

    const clamped = if (limit == 0) @min(max_window_bytes, @as(usize, @intCast(file_size))) else @min(limit, max_window_bytes);
    // Start reading at the requested offset; clamp to EOF.
    const start: i64 = if (offset < 0) 0 else @min(offset, file_size);
    const want: usize = @min(clamped, @as(usize, @intCast(@max(file_size - start, 0))));

    const buf = allocator.alloc(u8, want + 1) catch return null;

    const n_raw = std.os.linux.pread(@intCast(fd), buf.ptr, want, @intCast(start));
    const n: isize = @bitCast(n_raw);
    if (n < 0) {
        allocator.free(buf);
        return null;
    }
    var actual: usize = @intCast(n);

    // Line-align the end: trim back to the last newline so no partial line
    // leaks into the window (mirrors transcript_window line alignment).
    if (actual > 0 and buf[actual - 1] != '\n') {
        var i: usize = actual;
        while (i > 0) : (i -= 1) {
            if (buf[i - 1] == '\n') break;
        }
        actual = i;
    }

    const next_offset = start + @as(i64, @intCast(actual));
    return Window{
        .output = buf[0..actual],
        .absolute_offset = start,
        .next_offset = next_offset,
        .truncated_before = start > 0,
        .eof = next_offset >= file_size,
    };
}

/// freeWindow releases the window's output allocation (owned by the caller).
pub fn freeWindow(allocator: std.mem.Allocator, w: Window) void {
    allocator.free(w.output);
}

/// handleTranscript serves GET /transcript?session=<sid>&offset=<n>&limit=<n>.
pub fn handleTranscript(allocator: std.mem.Allocator, client_fd: i32, query: []const u8) !void {
    var session_id: []const u8 = "";
    var offset: i64 = 0;
    var limit: usize = 0;

    var params = std.mem.splitScalar(u8, query, '&');
    while (params.next()) |pair| {
        var kv = std.mem.splitScalar(u8, pair, '=');
        const key = kv.next() orelse continue;
        const val = kv.next() orelse "";
        if (std.mem.eql(u8, key, "session")) {
            session_id = val;
        } else if (std.mem.eql(u8, key, "offset")) {
            offset = std.fmt.parseInt(i64, val, 10) catch 0;
        } else if (std.mem.eql(u8, key, "limit")) {
            limit = std.fmt.parseInt(usize, val, 10) catch 0;
        }
    }

    if (session_id.len == 0) {
        try main.writeResponse(client_fd, "400 Bad Request", "application/json", "{\"error\":\"session required\"}");
        return;
    }

    const w = readWindow(allocator, session_id, offset, limit) orelse {
        try main.writeResponse(client_fd, "404 Not Found", "application/json", "{\"error\":\"no transcript\"}");
        return;
    };
    defer freeWindow(allocator, w);

    const escaped = try exec_json_escape(allocator, w.output);
    defer allocator.free(escaped);
    const resp = try std.fmt.allocPrint(allocator,
        "{{\"output\":\"{s}\",\"offset\":{d},\"next_offset\":{d},\"truncated_before\":{s},\"eof\":{s}}}",
        .{ escaped, w.absolute_offset, w.next_offset, if (w.truncated_before) "true" else "false", if (w.eof) "true" else "false" });
    defer allocator.free(resp);
    try main.writeResponse(client_fd, "200 OK", "application/json", resp);
}

/// exec_json_escape is a thin wrapper around the JSON escaping in exec.zig so
/// this module stays self-contained but reuses the proven escape path.
fn exec_json_escape(allocator: std.mem.Allocator, s: []const u8) ![]u8 {
    const exec = @import("exec.zig");
    return exec.jsonEscape(allocator, s);
}
