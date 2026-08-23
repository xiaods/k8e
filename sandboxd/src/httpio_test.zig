const std = @import("std");
const testing = std.testing;
const httpio = @import("httpio.zig");

// --- regression: split-body reassembly (E2E panic "index out of bounds") ---
//
// The old framing math in main.zig handleRequest sized the request buffer as
// max(head.len, header_end + remaining) while the final slice needed
// header_end + content_length. Whenever the FIRST read returned headers plus
// SOME body bytes (0 < body_in_head < content_length) the slice indexed past
// the allocation and sandboxd's connection thread panicked:
//
//   thread 300 panic: index out of bounds: index 20626, len 16686
//
// These tests drive readRequest through a temp-file fd: reads on a regular
// file return up to the 16KiB header_buf cap per call, so a >16KiB request
// deterministically produces the "first read carries headers + PARTIAL body"
// shape that triggered the panic.

const ReqFile = struct {
    tmp: std.testing.TmpDir,
    file: std.Io.File,

    fn init(bytes: []const u8) !ReqFile {
        var tmp = std.testing.tmpDir(.{});
        errdefer tmp.cleanup();
        const f = try tmp.dir.createFile(testing.io, "request.bin", .{ .read = true });
        try f.writePositionalAll(testing.io, bytes, 0);
        return .{ .tmp = tmp, .file = f };
    }

    fn deinit(rf: *ReqFile) void {
        rf.file.close(testing.io);
        rf.tmp.cleanup();
    }
};

fn buildRequest(content_length: usize) ![]u8 {
    const head_fmt = "POST /files/write HTTP/1.1\r\nHost: sandboxd\r\nContent-Length: {d}\r\n\r\n";
    var header_buf: [256]u8 = undefined;
    const head = try std.fmt.bufPrint(&header_buf, head_fmt, .{content_length});
    const buf = try testing.allocator.alloc(u8, head.len + content_length);
    // Body bytes distinct per position so corruption would be detectable.
    for (buf[head.len..], 0..) |*b, i| b.* = @intCast(i % 251 + 1);
    @memcpy(buf[0..head.len], head);
    return buf;
}

test "readRequest reassembles body split across reads (regression)" {
    const allocator = testing.allocator;

    // 20000-byte body + ~70B headers exceeds the 16KiB first-read cap, so the
    // first read lands mid-body — the exact shape behind the E2E panic
    // ("index 20626, len 16686": first read 16686B vs required 20626B).
    const req_bytes = try buildRequest(20000);
    defer allocator.free(req_bytes);

    var rf = try ReqFile.init(req_bytes);
    defer rf.deinit();

    const req = (try httpio.readRequest(allocator, rf.file.handle)) orelse return error.TestUnexpectedResult;
    defer allocator.free(req.alloc);

    try testing.expectEqualStrings("POST", req.method);
    try testing.expectEqualStrings("/files/write", req.path);
    // Body must be complete AND uncorrupted: the old append offset overwrote
    // the partial body copied from head.
    try testing.expectEqual(@as(usize, 20000), req.body.len);
    try testing.expectEqualSlices(u8, req_bytes[req_bytes.len - 20000 ..], req.body);
}

test "readRequest parses single-read GET" {
    const allocator = testing.allocator;
    const raw_req = "GET /events?limit=5 HTTP/1.1\r\nHost: sandboxd\r\nContent-Length: 0\r\n\r\n";

    var rf = try ReqFile.init(raw_req);
    defer rf.deinit();

    const req = (try httpio.readRequest(allocator, rf.file.handle)) orelse return error.TestUnexpectedResult;
    defer allocator.free(req.alloc);

    try testing.expectEqualStrings("GET", req.method);
    try testing.expectEqualStrings("/events", req.path);
    try testing.expectEqualStrings("limit=5", req.query);
    try testing.expectEqual(@as(usize, 0), req.body.len);
}

test "readRequest clamps short body to what arrived" {
    const allocator = testing.allocator;
    // Peer promises 100 bytes but sends 10 (EOF ends the read loop).
    const raw_req = "POST /exec/stdin HTTP/1.1\r\nContent-Length: 100\r\n\r\n0123456789";

    var rf = try ReqFile.init(raw_req);
    defer rf.deinit();

    const req = (try httpio.readRequest(allocator, rf.file.handle)) orelse return error.TestUnexpectedResult;
    defer allocator.free(req.alloc);

    try testing.expectEqualStrings("0123456789", req.body);
}

test "readRequest rejects oversized declared body" {
    const allocator = testing.allocator;
    const raw_req = "POST /files/write HTTP/1.1\r\nContent-Length: 999999999\r\n\r\nx";

    var rf = try ReqFile.init(raw_req);
    defer rf.deinit();

    const result = httpio.readRequest(allocator, rf.file.handle);
    try testing.expectError(error.TooLarge, result);
}

test "readRequest returns null for empty connection" {
    const allocator = testing.allocator;

    var rf = try ReqFile.init("");
    defer rf.deinit();

    const req = try httpio.readRequest(allocator, rf.file.handle);
    try testing.expect(req == null);
}
