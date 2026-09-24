const std = @import("std");

pub const Clock = struct {
    context: ?*anyopaque = null,
    now_fn: *const fn (?*anyopaque) u64,

    pub fn now(self: Clock) u64 {
        return self.now_fn(self.context);
    }
};

pub const Deadline = struct {
    expires_at_ns: u64,
    clock: Clock,

    pub fn initAfter(clock: Clock, timeout_ns: u64) Deadline {
        return .{
            .expires_at_ns = clock.now() +| timeout_ns,
            .clock = clock,
        };
    }

    pub fn remainingNs(self: Deadline) u64 {
        return self.expires_at_ns -| self.clock.now();
    }

    pub fn isExceeded(self: Deadline) bool {
        return self.expires_at_ns <= self.clock.now();
    }
};

pub fn formatTimeout(buffer: []u8, timeout_ns: u64) []const u8 {
    const units = [_]struct { divisor: u64, suffix: u8 }{
        .{ .divisor = 1, .suffix = 'n' },
        .{ .divisor = std.time.ns_per_us, .suffix = 'u' },
        .{ .divisor = std.time.ns_per_ms, .suffix = 'm' },
        .{ .divisor = std.time.ns_per_s, .suffix = 'S' },
        .{ .divisor = 60 * std.time.ns_per_s, .suffix = 'M' },
        .{ .divisor = 60 * 60 * std.time.ns_per_s, .suffix = 'H' },
    };
    for (units) |unit| {
        const value = @max(@as(u64, 1), std.math.divCeil(u64, timeout_ns, unit.divisor) catch 1);
        if (value <= 99_999_999 or unit.suffix == 'H') {
            return std.fmt.bufPrint(buffer, "{d}{c}", .{ @min(value, 99_999_999), unit.suffix }) catch unreachable;
        }
    }
    unreachable;
}

pub fn parseTimeout(value: []const u8) error{InvalidTimeout}!u64 {
    if (value.len < 2 or value.len > 9) return error.InvalidTimeout;
    const digits = value[0 .. value.len - 1];
    for (digits) |digit| if (!std.ascii.isDigit(digit)) return error.InvalidTimeout;
    const amount = std.fmt.parseInt(u64, digits, 10) catch return error.InvalidTimeout;
    const multiplier: u64 = switch (value[value.len - 1]) {
        'n' => 1,
        'u' => std.time.ns_per_us,
        'm' => std.time.ns_per_ms,
        'S' => std.time.ns_per_s,
        'M' => 60 * std.time.ns_per_s,
        'H' => 60 * 60 * std.time.ns_per_s,
        else => return error.InvalidTimeout,
    };
    return std.math.mul(u64, amount, multiplier) catch std.math.maxInt(u64);
}

test "timeout formatting rounds up and selects the finest eight-digit unit" {
    var buffer: [16]u8 = undefined;
    try std.testing.expectEqualStrings("1n", formatTimeout(&buffer, 0));
    try std.testing.expectEqualStrings("99999999n", formatTimeout(&buffer, 99_999_999));
    try std.testing.expectEqualStrings("100000u", formatTimeout(&buffer, 100_000_000));
    try std.testing.expectEqualStrings("100000m", formatTimeout(&buffer, 100_000_000_000));
    try std.testing.expectEqualStrings("5124096H", formatTimeout(&buffer, std.math.maxInt(u64)));
}

test "timeout parsing accepts all units and saturates overflow" {
    try std.testing.expectEqual(@as(u64, 0), try parseTimeout("0n"));
    try std.testing.expectEqual(@as(u64, 1), try parseTimeout("1n"));
    try std.testing.expectEqual(@as(u64, 1_000), try parseTimeout("1u"));
    try std.testing.expectEqual(@as(u64, 1_000_000), try parseTimeout("1m"));
    try std.testing.expectEqual(std.time.ns_per_s, try parseTimeout("1S"));
    try std.testing.expectEqual(60 * std.time.ns_per_s, try parseTimeout("1M"));
    try std.testing.expectEqual(60 * 60 * std.time.ns_per_s, try parseTimeout("1H"));
    try std.testing.expectEqual(std.math.maxInt(u64), try parseTimeout("99999999H"));
}

test "timeout parsing rejects malformed values" {
    const malformed = [_][]const u8{
        "", "n", "1", "123456789n", "+1n", "-1n", "1x", "1 n", " 1n", "1n ",
    };
    for (malformed) |value| {
        try std.testing.expectError(error.InvalidTimeout, parseTimeout(value));
    }
}

test "deadline uses an injectable monotonic clock" {
    const FakeClock = struct {
        now_ns: u64,

        fn now(context: ?*anyopaque) u64 {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            return self.now_ns;
        }
    };
    var fake = FakeClock{ .now_ns = 100 };
    const clock = Clock{ .context = &fake, .now_fn = FakeClock.now };
    const value = Deadline.initAfter(clock, 50);
    try std.testing.expectEqual(@as(u64, 50), value.remainingNs());
    fake.now_ns = 150;
    try std.testing.expect(value.isExceeded());
    try std.testing.expectEqual(@as(u64, 0), value.remainingNs());
}
