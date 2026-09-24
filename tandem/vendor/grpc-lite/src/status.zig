const std = @import("std");

pub const Code = enum(u8) {
    ok = 0,
    cancelled = 1,
    unknown = 2,
    invalid_argument = 3,
    deadline_exceeded = 4,
    not_found = 5,
    already_exists = 6,
    permission_denied = 7,
    resource_exhausted = 8,
    failed_precondition = 9,
    aborted = 10,
    out_of_range = 11,
    unimplemented = 12,
    internal = 13,
    unavailable = 14,
    data_loss = 15,
    unauthenticated = 16,

    pub fn fromInt(value: u32) Code {
        return std.enums.fromInt(Code, value) orelse .unknown;
    }
};

pub const Status = struct {
    code: Code,
    message: []const u8 = "",

    pub const ok: Status = .{ .code = .ok };

    pub fn init(code: Code, message: []const u8) Status {
        return .{ .code = code, .message = message };
    }

    pub fn isOk(self: Status) bool {
        return self.code == .ok;
    }
};

test "status code mapping covers the gRPC range" {
    var value: u32 = 0;
    while (value <= 16) : (value += 1) {
        try std.testing.expectEqual(value, @intFromEnum(Code.fromInt(value)));
    }
    try std.testing.expectEqual(Code.unknown, Code.fromInt(17));
    try std.testing.expect(Status.ok.isOk());
    try std.testing.expect(!Status.init(.internal, "failed").isOk());
}

test "status message is borrowed" {
    var message = [_]u8{ 'd', 'y', 'n', 'a', 'm', 'i', 'c' };
    const value = Status.init(.unknown, &message);
    message[0] = 'D';
    try std.testing.expectEqualStrings("Dynamic", value.message);
}
