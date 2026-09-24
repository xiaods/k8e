const std = @import("std");
const Compression = @import("compression.zig").Compression;
const deadline = @import("deadline.zig");
const metadata = @import("metadata.zig");
const status = @import("status.zig");

pub const ServerContext = struct {
    allocator: std.mem.Allocator,
    request_compression: Compression = .identity,
    response_compression: Compression = .identity,
    deadline: ?deadline.Deadline = null,
    request_metadata: metadata.Metadata,
    initial_metadata: metadata.Metadata,
    trailing_metadata: metadata.Metadata,

    pub fn init(allocator: std.mem.Allocator) ServerContext {
        return .{
            .allocator = allocator,
            .request_metadata = metadata.Metadata.init(allocator),
            .initial_metadata = metadata.Metadata.init(allocator),
            .trailing_metadata = metadata.Metadata.init(allocator),
        };
    }

    pub fn deinit(self: *ServerContext) void {
        self.request_metadata.deinit();
        self.initial_metadata.deinit();
        self.trailing_metadata.deinit();
        self.* = undefined;
    }

    pub fn addInitialMetadata(self: *ServerContext, key: []const u8, value: []const u8) !void {
        try self.initial_metadata.append(key, value);
    }

    pub fn addTrailingMetadata(self: *ServerContext, key: []const u8, value: []const u8) !void {
        try self.trailing_metadata.append(key, value);
    }

    pub fn setResponseCompression(self: *ServerContext, response_compression: Compression) void {
        self.response_compression = response_compression;
    }

    /// Returns whether the client supplied a deadline for this call.
    pub fn hasDeadline(self: *const ServerContext) bool {
        return self.deadline != null;
    }

    /// Returns the remaining monotonic duration, saturated at zero.
    pub fn remainingTimeNs(self: *const ServerContext) ?u64 {
        return if (self.deadline) |value| value.remainingNs() else null;
    }

    /// Returns whether the call deadline has elapsed.
    pub fn isDeadlineExceeded(self: *const ServerContext) bool {
        return if (self.deadline) |value| value.isExceeded() else false;
    }
};

pub const UnaryResponse = struct {
    allocator: std.mem.Allocator,
    status: status.Status,
    payload: []u8,

    pub fn init(
        allocator: std.mem.Allocator,
        response_status: status.Status,
        payload: []const u8,
    ) !UnaryResponse {
        const owned_message = try allocator.dupe(u8, response_status.message);
        errdefer allocator.free(owned_message);
        return .{
            .allocator = allocator,
            .status = .{ .code = response_status.code, .message = owned_message },
            .payload = try allocator.dupe(u8, payload),
        };
    }

    pub fn ok(allocator: std.mem.Allocator, payload: []const u8) !UnaryResponse {
        return init(allocator, status.Status.ok, payload);
    }

    pub fn fail(allocator: std.mem.Allocator, response_status: status.Status) !UnaryResponse {
        return init(allocator, response_status, "");
    }

    pub fn deinit(self: *UnaryResponse) void {
        self.allocator.free(self.status.message);
        self.allocator.free(self.payload);
        self.* = undefined;
    }
};

pub const UnaryHandler = struct {
    context: ?*anyopaque,
    invoke_fn: *const fn (
        context: ?*anyopaque,
        allocator: std.mem.Allocator,
        server_context: *ServerContext,
        request: []const u8,
    ) anyerror!UnaryResponse,

    pub fn bind(
        comptime T: type,
        context: *T,
        comptime handler: fn (*T, std.mem.Allocator, *ServerContext, []const u8) anyerror!UnaryResponse,
    ) UnaryHandler {
        return .{
            .context = context,
            .invoke_fn = struct {
                fn invoke(
                    opaque_context: ?*anyopaque,
                    allocator: std.mem.Allocator,
                    server_context: *ServerContext,
                    request: []const u8,
                ) anyerror!UnaryResponse {
                    const typed_context: *T = @ptrCast(@alignCast(opaque_context.?));
                    return handler(typed_context, allocator, server_context, request);
                }
            }.invoke,
        };
    }

    pub fn invoke(
        self: UnaryHandler,
        allocator: std.mem.Allocator,
        server_context: *ServerContext,
        request: []const u8,
    ) !UnaryResponse {
        return self.invoke_fn(self.context, allocator, server_context, request);
    }
};

fn testUnaryResponseAllocations(allocator: std.mem.Allocator) !void {
    var response = try UnaryResponse.init(
        allocator,
        status.Status.init(.internal, "failure"),
        "payload",
    );
    defer response.deinit();
}

test "typed unary handler owns its response" {
    const Echo = struct {
        prefix: []const u8,

        fn handle(
            self: *@This(),
            allocator: std.mem.Allocator,
            context: *ServerContext,
            request: []const u8,
        ) !UnaryResponse {
            try context.addTrailingMetadata("x-handler", "echo");
            const payload = try std.mem.concat(allocator, u8, &.{ self.prefix, request });
            defer allocator.free(payload);
            return UnaryResponse.ok(allocator, payload);
        }
    };

    var echo = Echo{ .prefix = "echo:" };
    const handler = UnaryHandler.bind(Echo, &echo, Echo.handle);
    var context = ServerContext.init(std.testing.allocator);
    defer context.deinit();
    var response = try handler.invoke(std.testing.allocator, &context, "hello");
    defer response.deinit();

    try std.testing.expectEqualStrings("echo:hello", response.payload);
    try std.testing.expectEqualStrings("echo", context.trailing_metadata.getFirst("x-handler").?);
}

test "server context exposes an optional live deadline" {
    const FakeClock = struct {
        now_ns: u64,

        fn now(context: ?*anyopaque) u64 {
            const self: *@This() = @ptrCast(@alignCast(context.?));
            return self.now_ns;
        }
    };

    var context = ServerContext.init(std.testing.allocator);
    defer context.deinit();
    try std.testing.expect(!context.hasDeadline());
    try std.testing.expectEqual(@as(?u64, null), context.remainingTimeNs());
    try std.testing.expect(!context.isDeadlineExceeded());

    var fake = FakeClock{ .now_ns = 100 };
    const clock = deadline.Clock{ .context = &fake, .now_fn = FakeClock.now };
    context.deadline = deadline.Deadline.initAfter(clock, 50);
    try std.testing.expect(context.hasDeadline());
    try std.testing.expectEqual(@as(?u64, 50), context.remainingTimeNs());
    fake.now_ns = 150;
    try std.testing.expect(context.isDeadlineExceeded());
}

test "unary response owns its status message and payload" {
    const message = try std.testing.allocator.dupe(u8, "dynamic status");
    defer std.testing.allocator.free(message);
    const payload = try std.testing.allocator.dupe(u8, "dynamic payload");
    defer std.testing.allocator.free(payload);
    var response = try UnaryResponse.init(
        std.testing.allocator,
        status.Status.init(.unknown, message),
        payload,
    );
    defer response.deinit();
    @memset(message, 'x');
    @memset(payload, 'y');

    try std.testing.expectEqualStrings("dynamic status", response.status.message);
    try std.testing.expectEqualStrings("dynamic payload", response.payload);
}

test "unary response handles every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testUnaryResponseAllocations,
        .{},
    );
}
