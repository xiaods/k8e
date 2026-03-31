const std = @import("std");
const exec = @import("exec.zig");

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
