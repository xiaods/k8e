const std = @import("std");
const builtin = @import("builtin");
const transcript = @import("transcript.zig");

// Transcript window semantics use raw Linux syscalls (open/pread); on macOS
// those trigger SIGSYS (same pre-existing limitation as exec_test runCommand
// fork tests). These tests are Linux-only and run in CI.


const TEST_BASE = "/tmp/k8e-transcript-test";

fn setupDir() void {
    _ = std.os.linux.mkdir(@ptrCast(TEST_BASE.ptr), 0o755);
}

fn teardownDir() void {
    _ = std.os.linux.unlink("/tmp/k8e-transcript-test/sess-1.log");
    _ = std.os.linux.unlink("/tmp/k8e-transcript-test/sess-2.log");
    _ = std.os.linux.unlink("/tmp/k8e-transcript-test/sess-3.log");
    _ = std.os.linux.rmdir(@ptrCast(TEST_BASE.ptr));
}

test "appendLine then readWindow returns full content" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var gpa = std.heap.DebugAllocator(.{}){};
    const allocator = gpa.allocator();
    defer _ = gpa.deinit();

    setupDir();
    defer teardownDir();

    transcript.appendLineAt(allocator, TEST_BASE, "sess-1", "cmd", "echo hello");
    transcript.appendLineAt(allocator, TEST_BASE, "sess-1", "stdout", "hello world");

    const w = transcript.readWindowAt(allocator, TEST_BASE, "sess-1", 0, 0) orelse {
        return error.TestUnexpectedResult;
    };
    try std.testing.expect(w.absolute_offset == 0);
    try std.testing.expect(w.eof);
    try std.testing.expect(std.mem.indexOf(u8, w.output, "echo hello") != null);
    try std.testing.expect(std.mem.indexOf(u8, w.output, "hello world") != null);
}

test "readWindow offset continuation resumes at next_offset" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var gpa = std.heap.DebugAllocator(.{}){};
    const allocator = gpa.allocator();
    defer _ = gpa.deinit();

    setupDir();
    defer teardownDir();

    transcript.appendLineAt(allocator, TEST_BASE, "sess-2", "cmd", "line one");
    transcript.appendLineAt(allocator, TEST_BASE, "sess-2", "stdout", "line two");

    const w1 = transcript.readWindowAt(allocator, TEST_BASE, "sess-2", 0, 64) orelse return error.TestUnexpectedResult;
    try std.testing.expect(w1.next_offset > 0);
    try std.testing.expect(!w1.eof);

    const w2 = transcript.readWindowAt(allocator, TEST_BASE, "sess-2", w1.next_offset, 64) orelse return error.TestUnexpectedResult;
    try std.testing.expect(w2.eof);
}

test "readWindow clamps offset to EOF and reports eof" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var gpa = std.heap.DebugAllocator(.{}){};
    const allocator = gpa.allocator();
    defer _ = gpa.deinit();

    setupDir();
    defer teardownDir();

    transcript.appendLineAt(allocator, TEST_BASE, "sess-3", "stdout", "done");

    const w = transcript.readWindowAt(allocator, TEST_BASE, "sess-3", 1 << 30, 64) orelse return error.TestUnexpectedResult;
    try std.testing.expect(w.eof);
    try std.testing.expect(w.output.len == 0);
}

test "readWindow missing session returns null" {
    if (builtin.os.tag != .linux) return error.SkipZigTest;
    var gpa = std.heap.DebugAllocator(.{}){};
    const allocator = gpa.allocator();
    defer _ = gpa.deinit();

    setupDir();
    defer teardownDir();

    try std.testing.expect(transcript.readWindowAt(allocator, TEST_BASE, "no-such-session", 0, 64) == null);
}
