//! Minimal HTTP/1.x request framing for sandboxd.
//!
//! Extracted from main.zig so the reassembly logic is unit-testable on any
//! host (uses std.posix, not std.os.linux). Owns the full read side: first
//! read grabs headers (and possibly part of the body), then the remaining
//! Content-Length bytes are appended after them — see readRequest for the
//! layout invariants that previously produced an "index out of bounds" panic
//! when a TCP segment split the body across reads (E2E: sandboxd thread
//! panic at main.zig handleRequest).

const std = @import("std");

/// maxRequestBodyBytes caps a single HTTP request body. Raised far above the
/// old fixed 64KiB stack buffer so WriteFile can carry large payloads; the
/// gateway gRPC layer already allows 64MiB messages (see KIP-16 M7).
pub const maxRequestBodyBytes: usize = 64 * 1024 * 1024;

pub const Error = error{
    ReadFailed,
    TooLarge,
    Malformed,
};

/// A parsed HTTP request. Every slice points into `alloc`; free it with the
/// same allocator passed to readRequest (free the FULL `alloc` slice — the
/// parsed views may be shorter when a short body was clamped).
pub const Request = struct {
    method: []const u8,
    path: []const u8,
    query: []const u8,
    body: []const u8,
    /// Full backing allocation (may extend past the clamped request bytes
    /// when the peer sent fewer body bytes than Content-Length promised).
    alloc: []u8,
    /// The clamped request bytes: headers + the body that actually arrived.
    raw: []const u8,
};

const header_buf_len: usize = 16384;

/// readRequest reads one HTTP/1.x request from fd and parses its request
/// line, path, query string, and body.
///
/// Buffer layout (the invariant the old code got wrong): `head` — the first
/// read — may already carry `body_in_head` body bytes right after the
/// headers, and the follow-up reads append the rest AFTER those bytes, so the
/// allocation must hold `header_end + content_length` bytes total:
///
///   [ headers | body_in_head | remaining ]
///   ^buf                       ^append_at ^request end
///
/// Sizing the buffer without the `body_in_head` overlap both truncated the
/// allocation (panic: index past len) and made the append loop overwrite the
/// body bytes already copied from head. Short bodies (peer closed early) are
/// clamped to what actually arrived instead of exposing uninitialized bytes.
///
/// Returns null when the connection closes before a header terminator is
/// seen (keep-alive probes, empty connections).
pub fn readRequest(allocator: std.mem.Allocator, fd: i32) !?Request {
    var header_buf: [header_buf_len]u8 = undefined;
    var content_length: usize = 0;

    // First read: headers plus whatever body bytes arrived in the segment.
    const n0 = std.posix.read(fd, &header_buf) catch return error.ReadFailed;
    if (n0 == 0) return null;
    const head = header_buf[0..n0];

    // Parse Content-Length so we know how much body to expect.
    var head_lines = std.mem.splitScalar(u8, head, '\n');
    _ = head_lines.next(); // request line
    while (head_lines.next()) |line| {
        const trimmed = std.mem.trim(u8, line, &std.ascii.whitespace);
        if (std.ascii.startsWithIgnoreCase(trimmed, "content-length:")) {
            const val = std.mem.trim(u8, trimmed["content-length:".len..], &std.ascii.whitespace);
            content_length = std.fmt.parseInt(usize, val, 10) catch 0;
        }
    }
    if (content_length > maxRequestBodyBytes) return error.TooLarge;

    const header_end = std.mem.indexOf(u8, head, "\r\n\r\n") orelse return null;
    const body_start = header_end + 4;
    const body_in_head = if (head.len > body_start) head.len - body_start else 0;
    const remaining = if (body_in_head < content_length) content_length - body_in_head else 0;

    // Headers + FULL body: head.len covers headers+partial body, and the
    // loop appends the remainder after it (never on top of it).
    const buf_len = @max(head.len, body_start + content_length);
    const buf = try allocator.alloc(u8, buf_len);
    errdefer allocator.free(buf);
    @memcpy(buf[0..head.len], head);

    var read: usize = 0;
    while (read < remaining) {
        // Append after the partial body already copied from head.
        const n = std.posix.read(fd, buf[body_start + body_in_head + read ..]) catch break;
        if (n == 0) break;
        read += n;
    }

    // Clamp to what actually arrived: a short body must not leak
    // uninitialized buffer bytes into handlers.
    const received = body_in_head + read;
    const body_total = @min(received, content_length);

    const raw = buf[0 .. body_start + body_total];

    var lines = std.mem.splitScalar(u8, raw, '\n');
    const request_line = lines.next() orelse return error.Malformed;
    var parts = std.mem.splitScalar(u8, std.mem.trim(u8, request_line, &std.ascii.whitespace), ' ');
    const method = parts.next() orelse return error.Malformed;
    const path_full = parts.next() orelse return error.Malformed;

    var path_parts = std.mem.splitScalar(u8, path_full, '?');
    const path = path_parts.next() orelse path_full;
    const query = path_parts.next() orelse "";

    const body = if (std.mem.indexOf(u8, raw, "\r\n\r\n")) |i| raw[i + 4 ..] else "";

    return .{ .method = method, .path = path, .query = query, .body = body, .alloc = buf, .raw = raw };
}

test {}
