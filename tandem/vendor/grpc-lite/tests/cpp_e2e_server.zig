const std = @import("std");
const grpc = @import("grpc_lite");

const EchoHandler = struct {
    fn handle(
        _: *@This(),
        allocator: std.mem.Allocator,
        context: *grpc.ServerContext,
        request: []const u8,
    ) !grpc.UnaryResponse {
        try context.addInitialMetadata("x-grpc-lite-service", "demo.EchoService");
        try context.addTrailingMetadata("x-grpc-lite-method", "Echo");
        if (context.request_metadata.getFirst("x-consumer")) |value| {
            try context.addInitialMetadata("x-consumer-seen", value);
        }
        // EchoRequest and EchoReply have the same protobuf field layout.
        return grpc.UnaryResponse.ok(allocator, request);
    }
};

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const port = if (args.len > 1)
        try std.fmt.parseInt(u16, args[1], 10)
    else
        0;

    var handler: EchoHandler = .{};
    var server = try grpc.Server.init(init.gpa, .{
        .host = "127.0.0.1",
        .port = port,
    });
    defer server.deinit();
    try server.registerUnary(
        "/demo.EchoService/Echo",
        grpc.UnaryHandler.bind(EchoHandler, &handler, EchoHandler.handle),
    );
    try server.start();
    std.debug.print("grpc-lite C++ E2E server listening on 127.0.0.1:{d}\n", .{try server.port()});
    server.wait();
}
