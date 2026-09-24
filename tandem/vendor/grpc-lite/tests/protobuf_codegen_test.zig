const std = @import("std");
const demo = @import("demo_proto");

test "generated echo message matches canonical protobuf wire bytes" {
    const request: demo.EchoRequest = .{ .message = "hello grpc-lite" };
    var writer: std.Io.Writer.Allocating = .init(std.testing.allocator);
    defer writer.deinit();
    try request.encode(&writer.writer, std.testing.allocator);

    try std.testing.expectEqualSlices(u8, &.{
        0x0a, 0x0f, 'h', 'e', 'l', 'l', 'o', ' ',
        'g',  'r',  'p', 'c', '-', 'l', 'i', 't',
        'e',
    }, writer.written());

    var reader: std.Io.Reader = .fixed(writer.written());
    var decoded = try demo.EchoRequest.decode(&reader, std.testing.allocator);
    defer decoded.deinit(std.testing.allocator);
    try std.testing.expectEqualStrings("hello grpc-lite", decoded.message);
}

test "generated service exposes package and method types" {
    const EchoApi = demo.EchoService(void, error{});
    try std.testing.expectEqualStrings("demo", EchoApi.package);
    try std.testing.expectEqualStrings("EchoService", EchoApi.service_name);

    const echo_field = @typeInfo(EchoApi).@"struct".fields[0];
    try std.testing.expectEqualStrings("Echo", echo_field.name);
}
