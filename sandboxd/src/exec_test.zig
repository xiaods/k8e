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
