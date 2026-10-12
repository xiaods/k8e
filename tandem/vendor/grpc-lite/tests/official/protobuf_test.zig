const std = @import("std");
const testing = @import("grpc_testing");

test "official grpc.testing messages encode and decode" {
    var request: testing.SimpleRequest = .{
        .response_size = 314159,
        .payload = .{
            .body = try std.testing.allocator.alloc(u8, 271828),
        },
    };
    defer request.deinit(std.testing.allocator);
    @memset(@constCast(request.payload.?.body), 0);

    var writer: std.Io.Writer.Allocating = .init(std.testing.allocator);
    defer writer.deinit();
    try request.encode(&writer.writer, std.testing.allocator);

    var reader: std.Io.Reader = .fixed(writer.written());
    var decoded = try testing.SimpleRequest.decode(&reader, std.testing.allocator);
    defer decoded.deinit(std.testing.allocator);
    try std.testing.expectEqual(@as(i32, 314159), decoded.response_size);
    try std.testing.expectEqual(@as(usize, 271828), decoded.payload.?.body.len);
}

test "official service retains standard package and method names" {
    const Api = testing.TestService(void, error{});
    try std.testing.expectEqualStrings("grpc.testing", Api.package);
    try std.testing.expectEqualStrings("TestService", Api.service_name);
    try std.testing.expect(@hasField(Api, "EmptyCall"));
    try std.testing.expect(@hasField(Api, "UnaryCall"));
}
