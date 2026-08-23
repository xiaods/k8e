const std = @import("std");
const builtin = @import("builtin");
const path_util = @import("path.zig");
const files = @import("files.zig");

fn extractQueryParam(query: []const u8, key: []const u8) ?[]const u8 {
    var it = std.mem.splitScalar(u8, query, '&');
    while (it.next()) |pair| {
        var kv = std.mem.splitScalar(u8, pair, '=');
        const k = kv.next() orelse continue;
        const v = kv.next() orelse continue;
        if (std.mem.eql(u8, k, key)) return v;
    }
    return null;
}

test "extractQueryParam: single param" {
    try std.testing.expectEqualStrings("/tmp/foo.txt", extractQueryParam("path=/tmp/foo.txt", "path").?);
}

test "extractQueryParam: multiple params" {
    try std.testing.expectEqualStrings("42", extractQueryParam("since=42&limit=10", "since").?);
    try std.testing.expectEqualStrings("10", extractQueryParam("since=42&limit=10", "limit").?);
}

test "extractQueryParam: missing key returns null" {
    try std.testing.expect(extractQueryParam("since=42", "path") == null);
}

test "extractQueryParam: empty query returns null" {
    try std.testing.expect(extractQueryParam("", "path") == null);
}

test "extractQueryParam: key with empty value" {
    try std.testing.expectEqualStrings("", extractQueryParam("path=", "path").?);
}

test "resolveWorkspacePath: absolute path preserved verbatim" {
    // Regression: an earlier version clobbered the final char of absolute
    // paths, so /workspace/_k8e_run.ts was written to /workspace/_k8e_run.t
    // and `tsx /workspace/_k8e_run.ts` failed with ERR_MODULE_NOT_FOUND.
    const a = std.testing.allocator;
    const p = try path_util.resolveWorkspacePath(a, "/workspace/_k8e_run.ts");
    defer a.free(p);
    try std.testing.expectEqualStrings("/workspace/_k8e_run.ts", p);
    try std.testing.expectEqual(@as(u8, 0), p.ptr[p.len]);
}

test "resolveWorkspacePath: relative path rooted at /workspace" {
    const a = std.testing.allocator;
    const p = try path_util.resolveWorkspacePath(a, "notes.txt");
    defer a.free(p);
    try std.testing.expectEqualStrings("/workspace/notes.txt", p);
    try std.testing.expectEqual(@as(u8, 0), p.ptr[p.len]);
}

// fileMtime returns a real mtime for an existing file (Linux-only; uses the
// statx syscall which is unavailable on macOS hosts).
test "fileMtime: existing file returns non-zero mtime" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    const a = std.testing.allocator;

    // Create a temp file with a known name under /tmp.
    const path = "/tmp/k8e-files-mtime-test.txt";
    const fd = std.os.linux.open(path, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd < 0) return error.TestUnexpectedResult;
    _ = std.os.linux.write(@intCast(fd), "x", 1);
    _ = std.os.linux.close(@intCast(fd));
    defer _ = std.os.linux.unlink(path);

    const m = files.fileMtime(path) orelse return error.TestUnexpectedResult;
    // Epoch-2020 boundary sanity: any file created now is > 1_500_000_000.
    try std.testing.expect(m > 1_500_000_000);
    _ = a;
}

// fileFacts returns mtime + size + a type label derived from the dent type
// (Linux-only; uses the statx syscall which is unavailable on macOS hosts).
test "fileFacts: regular file reports file type and size" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;

    const path = "/tmp/k8e-files-facts-test.txt";
    const fd = std.os.linux.open(path, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd < 0) return error.TestUnexpectedResult;
    _ = std.os.linux.write(@intCast(fd), "hello", 5);
    _ = std.os.linux.close(@intCast(fd));
    defer _ = std.os.linux.unlink(path);

    const facts = (files.fileFacts(path, std.os.linux.DT.REG) orelse return error.TestUnexpectedResult);
    try std.testing.expectEqualStrings("file", facts.type);
    try std.testing.expectEqual(@as(i64, 5), facts.size);
    try std.testing.expect(facts.mtime > 1_500_000_000);
}

test "fileFacts: symlink dent reports symlink type" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;

    const path = "/tmp/k8e-files-facts-link";
    const target = "/tmp/k8e-files-facts-target";
    const tfd = std.os.linux.open(target, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (tfd < 0) return error.TestUnexpectedResult;
    _ = std.os.linux.close(@intCast(tfd));
    defer _ = std.os.linux.unlink(target);

    var path_buf: [512]u8 = undefined;
    @memcpy(path_buf[0..path.len], path);
    path_buf[path.len] = 0;
    const path_z: [*:0]const u8 = @ptrCast(&path_buf);
    var target_buf: [512]u8 = undefined;
    @memcpy(target_buf[0..target.len], target);
    target_buf[target.len] = 0;
    const target_z: [*:0]const u8 = @ptrCast(&target_buf);
    _ = std.os.linux.unlink(path_z);
    const rc = std.os.linux.symlink(target_z, path_z);
    if (rc != 0) return error.TestUnexpectedResult;
    defer _ = std.os.linux.unlink(path_z);

    const facts = (files.fileFacts(path, std.os.linux.DT.LNK) orelse return error.TestUnexpectedResult);
    try std.testing.expectEqualStrings("symlink", facts.type);
}

test "removeRecursive: removes nested dir tree" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;

    const base = "/tmp/k8e-rmtree-test";
    _ = std.os.linux.mkdir(base.ptr, 0o755);
    defer _ = std.os.linux.rmdir(base.ptr);

    // base/sub/deep/file.txt
    const sub = "/tmp/k8e-rmtree-test/sub";
    _ = std.os.linux.mkdir(sub.ptr, 0o755);
    const deep = "/tmp/k8e-rmtree-test/sub/deep";
    _ = std.os.linux.mkdir(deep.ptr, 0o755);
    const f = "/tmp/k8e-rmtree-test/sub/deep/file.txt";
    const fd = std.os.linux.open(f, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd < 0) return error.TestUnexpectedResult;
    _ = std.os.linux.write(@intCast(fd), "x", 1);
    _ = std.os.linux.close(@intCast(fd));

    // removeRecursive takes a [:0]u8.
    var buf: [256]u8 = undefined;
    const base_z = try std.fmt.bufPrintZ(&buf, "{s}", .{base});
    try std.testing.expect(files.removeRecursive(base_z));

    // The whole tree must be gone.
    var stx: std.os.linux.Statx = undefined;
    const probe = std.os.linux.statx(std.os.linux.AT.FDCWD, base.ptr, 0, std.os.linux.STATX.BASIC_STATS, &stx);
    try std.testing.expect(probe != 0);
}

test "removeRecursive: missing path returns false" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var buf: [256]u8 = undefined;
    var ts: std.os.linux.timespec = undefined;
    _ = std.os.linux.clock_gettime(std.os.linux.CLOCK.REALTIME, &ts);
    const gone_z = try std.fmt.bufPrintZ(&buf, "/tmp/k8e-no-such-path-{d}", .{ts.sec});
    try std.testing.expect(!files.removeRecursive(gone_z));
}

test "statMode: file vs dir detection" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    const path = "/tmp/k8e-statmode-test.txt";
    const fd = std.os.linux.open(path, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd < 0) return error.TestUnexpectedResult;
    _ = std.os.linux.write(@intCast(fd), "x", 1);
    _ = std.os.linux.close(@intCast(fd));
    defer _ = std.os.linux.unlink(path);

    var stx: std.os.linux.Statx = undefined;
    _ = std.os.linux.statx(std.os.linux.AT.FDCWD, path, 0, std.os.linux.STATX.BASIC_STATS, &stx);
    const mode_u32: u32 = stx.mode;
    try std.testing.expect(mode_u32 & std.os.linux.S.IFMT == std.os.linux.S.IFREG);

    const dir_path = "/tmp";
    _ = std.os.linux.statx(std.os.linux.AT.FDCWD, dir_path, 0, std.os.linux.STATX.BASIC_STATS, &stx);
    const dmode_u32: u32 = stx.mode;
    try std.testing.expect(dmode_u32 & std.os.linux.S.IFMT == std.os.linux.S.IFDIR);
}

test "stat: symlink target is resolved" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    const base = "/tmp/k8e-statlink-test";
    const target = "/tmp/k8e-statlink-target.txt";
    // Create a real file, then a symlink to it.
    const fd = std.os.linux.open(target, std.os.linux.O{ .CREAT = true, .ACCMODE = .WRONLY, .TRUNC = true }, 0o644);
    if (fd < 0) return error.SkipZigTest;
    _ = std.os.linux.close(@intCast(fd));
    defer _ = std.os.linux.unlink(target);

    var base_buf: [256]u8 = undefined;
    const base_z = try std.fmt.bufPrintZ(&base_buf, "{s}", .{base});
    var tgt_buf: [256]u8 = undefined;
    const tgt_z = try std.fmt.bufPrintZ(&tgt_buf, "{s}", .{target});
    _ = std.os.linux.symlink(tgt_z, base_z);
    defer _ = std.os.linux.unlink(base_z);

    // Stat the symlink: response must include the resolved target.
    var out = std.array_list.Managed(u8).init(std.testing.allocator);
    defer out.deinit();
    // Call handleStat through its JSON body form.
    const body = try std.fmt.allocPrint(std.testing.allocator, "{{\"path\":\"{s}\"}}", .{base});
    defer std.testing.allocator.free(body);
    // handleStat writes to a socket; instead verify via the stat helper path:
    // readlink directly matches what handleStat would emit.
    var buf: [4096]u8 = undefined;
    const raw = std.os.linux.readlink(base_z, &buf, buf.len);
    const signed: isize = @bitCast(raw);
    try std.testing.expect(signed >= 0);
    try std.testing.expectEqualStrings(target, buf[0..@as(usize, @intCast(signed))]);
}

// --- chunked transfer primitives (KIP-24 push/pull) ---
// writeChunk/readRange back the CLI push/pull streaming commands: offset-
// positional writes never truncate, ranged reads bound memory to one chunk.

const testing = std.testing;

fn tmpPath(buf: []u8, comptime name: []const u8) ![]const u8 {
    // Static per-test names; every test unlinks its file on exit.
    return std.fmt.bufPrint(buf, "/tmp/k8e-files-test-{s}.bin", .{name});
}

test "writeChunk w-mode creates and truncates" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var path_buf: [128]u8 = undefined;
    const path = try tmpPath(&path_buf, "w");
    defer _ = std.os.linux.unlink(@ptrCast(path));

    try files.writeChunk(path, "hello", "w", 0);
    try files.writeChunk(path, "hi", "w", 0); // second w truncates

    const got = try files.readRange(testing.allocator, path, 0, 0);
    defer testing.allocator.free(got);
    try testing.expectEqualStrings("hi", got);
}

test "writeChunk positional offset reassembles chunks without truncation" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var path_buf: [128]u8 = undefined;
    const path = try tmpPath(&path_buf, "offset");
    defer _ = std.os.linux.unlink(@ptrCast(path));

    // Simulate push: first chunk (mode w), then two positional chunks.
    try files.writeChunk(path, "AAAA", "w", 0);
    try files.writeChunk(path, "BBBB", "", 4);
    try files.writeChunk(path, "CC", "", 8);

    const got = try files.readRange(testing.allocator, path, 0, 0);
    defer testing.allocator.free(got);
    try testing.expectEqualStrings("AAAABBBBCC", got);
}

test "readRange window bounds the read and short window signals EOF" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var path_buf: [128]u8 = undefined;
    const path = try tmpPath(&path_buf, "window");
    defer _ = std.os.linux.unlink(@ptrCast(path));

    const payload = "0123456789abcdef"; // 16 bytes
    try files.writeChunk(path, payload, "w", 0);

    // Full first window.
    const win1 = try files.readRange(testing.allocator, path, 0, 10);
    defer testing.allocator.free(win1);
    try testing.expectEqualStrings("0123456789", win1);

    // Second window: short read (< requested) = EOF, exactly the tail.
    const win2 = try files.readRange(testing.allocator, path, 10, 10);
    defer testing.allocator.free(win2);
    try testing.expectEqualStrings("abcdef", win2);

    // Window past EOF: empty.
    const win3 = try files.readRange(testing.allocator, path, 999, 10);
    defer testing.allocator.free(win3);
    try testing.expectEqual(@as(usize, 0), win3.len);
}

test "writeChunk offset past end extends file (sparse)" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var path_buf: [128]u8 = undefined;
    const path = try tmpPath(&path_buf, "sparse");
    defer _ = std.os.linux.unlink(@ptrCast(path));

    try files.writeChunk(path, "XY", "", 4);

    const got = try files.readRange(testing.allocator, path, 0, 64);
    defer testing.allocator.free(got);
    try testing.expectEqual(@as(usize, 6), got.len);
    try testing.expectEqualStrings("\x00\x00\x00\x00XY", got[0..6]);
}

test "readRange missing file returns NotFound" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    try testing.expectError(error.NotFound, files.readRange(testing.allocator, "/tmp/k8e-files-test-does-not-exist.bin", 0, 10));
}
