const std = @import("std");

pub fn encode(allocator: std.mem.Allocator, message: []const u8) ![]u8 {
    var output: std.ArrayList(u8) = .empty;
    errdefer output.deinit(allocator);

    for (message) |byte| {
        if (isSafe(byte)) {
            try output.append(allocator, byte);
        } else {
            try output.appendSlice(allocator, &.{
                '%',
                hex(byte >> 4),
                hex(byte & 0x0f),
            });
        }
    }
    return output.toOwnedSlice(allocator);
}

pub fn decode(allocator: std.mem.Allocator, message: []const u8) ![]u8 {
    var output: std.ArrayList(u8) = .empty;
    errdefer output.deinit(allocator);

    var index: usize = 0;
    while (index < message.len) {
        if (message[index] != '%') {
            try output.append(allocator, message[index]);
            index += 1;
            continue;
        }
        if (index + 2 >= message.len) return error.InvalidGrpcMessage;
        const high = try unhex(message[index + 1]);
        const low = try unhex(message[index + 2]);
        try output.append(allocator, high << 4 | low);
        index += 3;
    }
    return output.toOwnedSlice(allocator);
}

fn isSafe(byte: u8) bool {
    return byte >= 0x20 and byte < 0x7f and byte != '%';
}

fn hex(value: u8) u8 {
    return if (value < 10) '0' + value else 'A' + value - 10;
}

fn unhex(value: u8) !u8 {
    return switch (value) {
        '0'...'9' => value - '0',
        'a'...'f' => value - 'a' + 10,
        'A'...'F' => value - 'A' + 10,
        else => error.InvalidGrpcMessage,
    };
}

test "grpc-message percent encoding round trips" {
    const encoded = try encode(std.testing.allocator, "bad % value\n");
    defer std.testing.allocator.free(encoded);
    try std.testing.expectEqualStrings("bad %25 value%0A", encoded);

    const decoded = try decode(std.testing.allocator, encoded);
    defer std.testing.allocator.free(decoded);
    try std.testing.expectEqualStrings("bad % value\n", decoded);
}
