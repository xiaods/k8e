const std = @import("std");
const exec = @import("exec.zig");

// --- jsonEscape tests ---

test "jsonEscape: plain string unchanged" {
    const allocator = std.testing.allocator;
    const out = try exec.jsonEscape(allocator, "hello world");
    defer allocator.free(out);
    try std.testing.expectEqualStrings("hello world", out);
}

test "jsonEscape: escapes double quote" {
    const allocator = std.testing.allocator;
    const out = try exec.jsonEscape(allocator, "say \"hi\"");
    defer allocator.free(out);
    try std.testing.expectEqualStrings("say \\\"hi\\\"", out);
}

test "jsonEscape: escapes backslash" {
    const allocator = std.testing.allocator;
    const out = try exec.jsonEscape(allocator, "a\\b");
    defer allocator.free(out);
    try std.testing.expectEqualStrings("a\\\\b", out);
}

test "jsonEscape: escapes newline, carriage return, tab" {
    const allocator = std.testing.allocator;
    const out = try exec.jsonEscape(allocator, "\n\r\t");
    defer allocator.free(out);
    try std.testing.expectEqualStrings("\\n\\r\\t", out);
}

test "jsonEscape: empty string" {
    const allocator = std.testing.allocator;
    const out = try exec.jsonEscape(allocator, "");
    defer allocator.free(out);
    try std.testing.expectEqualStrings("", out);
}

// --- runCommand tests (exercises posix.waitpid path) ---

test "runCommand: echo stdout" {
    const allocator = std.testing.allocator;
    const result = try exec.runCommand(allocator, "echo hello", "/tmp");
    defer result.deinit(allocator);
    try std.testing.expectEqualStrings("hello\n", result.stdout);
    try std.testing.expectEqualStrings("", result.stderr);
    try std.testing.expectEqual(@as(i32, 0), result.exit_code);
}

test "runCommand: exit code non-zero" {
    const allocator = std.testing.allocator;
    const result = try exec.runCommand(allocator, "exit 42", "/tmp");
    defer result.deinit(allocator);
    try std.testing.expectEqual(@as(i32, 42), result.exit_code);
}

test "runCommand: stderr captured" {
    const allocator = std.testing.allocator;
    const result = try exec.runCommand(allocator, "echo err >&2", "/tmp");
    defer result.deinit(allocator);
    try std.testing.expectEqualStrings("", result.stdout);
    try std.testing.expectEqualStrings("err\n", result.stderr);
    try std.testing.expectEqual(@as(i32, 0), result.exit_code);
}

test "runCommand: multiline output" {
    const allocator = std.testing.allocator;
    const result = try exec.runCommand(allocator, "printf 'a\\nb\\nc\\n'", "/tmp");
    defer result.deinit(allocator);
    try std.testing.expectEqualStrings("a\nb\nc\n", result.stdout);
    try std.testing.expectEqual(@as(i32, 0), result.exit_code);
}

test "runCommand: python3 arithmetic" {
    const allocator = std.testing.allocator;
    const result = try exec.runCommand(allocator, "python3 -c \"print(6*7)\"", "/tmp");
    defer result.deinit(allocator);
    try std.testing.expectEqualStrings("42\n", result.stdout);
    try std.testing.expectEqual(@as(i32, 0), result.exit_code);
}

// --- PID 1 / SIGCHLD=IGN regression tests ---
// These tests simulate the SA_NOCLDWAIT environment where waitpid returns
// ECHILD because the kernel auto-reaps children. runCommand must not panic.

test "runCommand: survives SIGCHLD=IGN (ECHILD race)" {
    const allocator = std.testing.allocator;

    // Set SIGCHLD to SIG_IGN + SA_NOCLDWAIT, same as sandboxd PID 1 setup
    const sa = std.os.linux.Sigaction{
        .handler = .{ .handler = std.os.linux.SIG.IGN },
        .mask = std.mem.zeroes(std.os.linux.sigset_t),
        .flags = std.os.linux.SA.NOCLDWAIT,
    };
    _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa, null);
    defer {
        // Restore default SIGCHLD after test
        const sa_default = std.os.linux.Sigaction{
            .handler = .{ .handler = std.os.linux.SIG.DFL },
            .mask = std.mem.zeroes(std.os.linux.sigset_t),
            .flags = 0,
        };
        _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa_default, null);
    }

    const result = try exec.runCommand(allocator, "echo pid1-safe", "/tmp");
    defer result.deinit(allocator);
    // Must not panic; stdout should still be captured before wait
    try std.testing.expectEqualStrings("pid1-safe\n", result.stdout);
}

test "runCommand: exit code under SIGCHLD=IGN" {
    const allocator = std.testing.allocator;

    const sa = std.os.linux.Sigaction{
        .handler = .{ .handler = std.os.linux.SIG.IGN },
        .mask = std.mem.zeroes(std.os.linux.sigset_t),
        .flags = std.os.linux.SA.NOCLDWAIT,
    };
    _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa, null);
    defer {
        const sa_default = std.os.linux.Sigaction{
            .handler = .{ .handler = std.os.linux.SIG.DFL },
            .mask = std.mem.zeroes(std.os.linux.sigset_t),
            .flags = 0,
        };
        _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa_default, null);
    }

    // exit code may be 0 (ECHILD path) but must not panic
    const result = try exec.runCommand(allocator, "exit 7", "/tmp");
    defer result.deinit(allocator);
    // No panic is the primary assertion; exit_code is best-effort under NOCLDWAIT
    _ = result.exit_code;
}

test "runCommand: concurrent execs under SIGCHLD=IGN" {
    const allocator = std.testing.allocator;

    const sa = std.os.linux.Sigaction{
        .handler = .{ .handler = std.os.linux.SIG.IGN },
        .mask = std.mem.zeroes(std.os.linux.sigset_t),
        .flags = std.os.linux.SA.NOCLDWAIT,
    };
    _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa, null);
    defer {
        const sa_default = std.os.linux.Sigaction{
            .handler = .{ .handler = std.os.linux.SIG.DFL },
            .mask = std.mem.zeroes(std.os.linux.sigset_t),
            .flags = 0,
        };
        _ = std.os.linux.sigaction(std.os.linux.SIG.CHLD, &sa_default, null);
    }

    // Run 3 commands back-to-back to stress the ECHILD race
    for (0..3) |i| {
        const cmd = try std.fmt.allocPrint(allocator, "echo run{d}", .{i});
        defer allocator.free(cmd);
        const result = try exec.runCommand(allocator, cmd, "/tmp");
        defer result.deinit(allocator);
        const expected = try std.fmt.allocPrint(allocator, "run{d}\n", .{i});
        defer allocator.free(expected);
        try std.testing.expectEqualStrings(expected, result.stdout);
    }
}
