const std = @import("std");

pub const Level = enum(u32) {
    debug = 0,
    info = 1,
    warn = 2,
    err = 3,
};

pub const BytesView = extern struct {
    data: ?[*]const u8 = null,
    size: usize = 0,
};

pub const Callback = *const fn (?*anyopaque, u32, BytesView) callconv(.c) void;

pub const Logger = struct {
    context: ?*anyopaque = null,
    callback: ?Callback = null,

    pub fn write(self: Logger, level: Level, comptime format: []const u8, args: anytype) void {
        const callback = self.callback orelse return;
        var buffer: [1024]u8 = undefined;
        const message = std.fmt.bufPrint(&buffer, format, args) catch "grpc-lite log message truncated";
        callback(self.context, @intFromEnum(level), .{ .data = message.ptr, .size = message.len });
    }
};

test "logger formats and forwards a borrowed message" {
    const Capture = struct {
        level: u32 = 0,
        message: [32]u8 = undefined,
        message_len: usize = 0,

        fn log(context: ?*anyopaque, level: u32, message: BytesView) callconv(.c) void {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            self.level = level;
            self.message_len = @min(message.size, self.message.len);
            @memcpy(self.message[0..self.message_len], message.data.?[0..self.message_len]);
        }
    };

    var capture: Capture = .{};
    const logger: Logger = .{ .context = &capture, .callback = Capture.log };
    logger.write(.warn, "connection {s}", .{"closed"});
    try std.testing.expectEqual(@as(u32, @intFromEnum(Level.warn)), capture.level);
    try std.testing.expectEqualStrings("connection closed", capture.message[0..capture.message_len]);
}
