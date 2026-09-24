const std = @import("std");
const Compression = @import("compression.zig").Compression;
const metadata = @import("metadata.zig");
const status = @import("status.zig");

pub const default_max_message_size = 4 * 1024 * 1024;

pub const Options = struct {
    metadata: []const metadata.Entry = &.{},
    timeout_ns: ?u64 = null,
    max_response_size: usize = default_max_message_size,
    request_compression: Compression = .identity,
};

/// Borrowed unary result passed to `Callbacks.on_complete`.
///
/// Every slice, including `status.message` and metadata entries, is valid only
/// until the callback returns.
pub const AsyncResult = struct {
    status: status.Status,
    payload: []const u8,
    response_compression: Compression,
    initial_metadata: *const metadata.Metadata,
    trailing_metadata: *const metadata.Metadata,
};

/// Completion callback for an event-driven raw unary call.
///
/// The callback runs on the Channel transport loop thread. It must not block,
/// retain borrowed result data, or call `Channel.deinit`.
pub const Callbacks = struct {
    context: ?*anyopaque = null,
    on_complete: *const fn (?*anyopaque, AsyncResult) void,
};

pub const Result = struct {
    allocator: std.mem.Allocator,
    status: status.Status,
    payload: []u8,
    response_compression: Compression,
    initial_metadata: metadata.Metadata,
    trailing_metadata: metadata.Metadata,

    pub fn init(
        allocator: std.mem.Allocator,
        result_status: status.Status,
        payload: []const u8,
    ) !Result {
        return initWithCompression(allocator, result_status, payload, .identity);
    }

    pub fn initWithCompression(
        allocator: std.mem.Allocator,
        result_status: status.Status,
        payload: []const u8,
        response_compression: Compression,
    ) !Result {
        const owned_message = try allocator.dupe(u8, result_status.message);
        errdefer allocator.free(owned_message);
        const owned_payload = try allocator.dupe(u8, payload);
        errdefer allocator.free(owned_payload);

        return .{
            .allocator = allocator,
            .status = .{ .code = result_status.code, .message = owned_message },
            .payload = owned_payload,
            .response_compression = response_compression,
            .initial_metadata = metadata.Metadata.init(allocator),
            .trailing_metadata = metadata.Metadata.init(allocator),
        };
    }

    pub fn deinit(self: *Result) void {
        self.allocator.free(self.status.message);
        self.allocator.free(self.payload);
        self.initial_metadata.deinit();
        self.trailing_metadata.deinit();
        self.* = undefined;
    }
};

fn testResultAllocations(allocator: std.mem.Allocator) !void {
    var result = try Result.initWithCompression(
        allocator,
        status.Status.init(.invalid_argument, "bad request"),
        "response",
        .gzip,
    );
    defer result.deinit();
    try result.initial_metadata.append("x-initial", "value");
    _ = try result.trailing_metadata.appendDecoded("trace-bin", "qw==");
}

test "call result owns payload status and metadata" {
    var result = try Result.init(
        std.testing.allocator,
        status.Status.init(.invalid_argument, "bad request"),
        "response",
    );
    defer result.deinit();

    try result.initial_metadata.append("x-test", "value");
    try std.testing.expectEqual(status.Code.invalid_argument, result.status.code);
    try std.testing.expectEqualStrings("bad request", result.status.message);
    try std.testing.expectEqualStrings("response", result.payload);
}

test "call result copies dynamic status and payload" {
    const source_message = try std.testing.allocator.dupe(u8, "dynamic status");
    defer std.testing.allocator.free(source_message);
    const source_payload = try std.testing.allocator.dupe(u8, "dynamic payload");
    defer std.testing.allocator.free(source_payload);

    var result = try Result.init(
        std.testing.allocator,
        status.Status.init(.unknown, source_message),
        source_payload,
    );
    defer result.deinit();
    @memset(source_message, 'x');
    @memset(source_payload, 'y');

    try std.testing.expectEqualStrings("dynamic status", result.status.message);
    try std.testing.expectEqualStrings("dynamic payload", result.payload);
}

test "call result handles every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testResultAllocations,
        .{},
    );
}

test "async call result exposes borrowed response views" {
    var initial = metadata.Metadata.init(std.testing.allocator);
    defer initial.deinit();
    var trailing = metadata.Metadata.init(std.testing.allocator);
    defer trailing.deinit();
    try initial.append("x-initial", "value");
    try trailing.append("x-trailing", "value");

    const result: AsyncResult = .{
        .status = .init(.ok, "borrowed"),
        .payload = "payload",
        .response_compression = .gzip,
        .initial_metadata = &initial,
        .trailing_metadata = &trailing,
    };
    try std.testing.expect(result.status.isOk());
    try std.testing.expectEqualStrings("borrowed", result.status.message);
    try std.testing.expectEqualStrings("payload", result.payload);
    try std.testing.expectEqualStrings("value", result.initial_metadata.getFirst("x-initial").?);
    try std.testing.expectEqualStrings("value", result.trailing_metadata.getFirst("x-trailing").?);
}
