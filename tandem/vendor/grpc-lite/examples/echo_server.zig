const std = @import("std");
const demo = @import("demo_proto");
const grpc = @import("grpc_lite");
const grpc_pb = @import("grpc_lite_protobuf");

const EchoService = struct {
    allocator: std.mem.Allocator,

    fn echo(self: *@This(), request: demo.EchoRequest) error{OutOfMemory}!demo.EchoReply {
        return .{ .message = try self.allocator.dupe(u8, request.message) };
    }
};

const EchoApi = demo.EchoService(EchoService, error{OutOfMemory});

fn configureContext(_: *EchoService, context: *grpc.ServerContext) !void {
    try context.addInitialMetadata("x-grpc-lite-service", "demo.EchoService");
    try context.addTrailingMetadata("x-grpc-lite-method", "Echo");
    if (context.request_metadata.getFirst("x-consumer")) |value| {
        try context.addInitialMetadata("x-consumer-seen", value);
    }
}

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const port = if (args.len > 1)
        try std.fmt.parseInt(u16, args[1], 10)
    else
        50051;

    var service: EchoService = .{ .allocator = init.gpa };
    var registration = grpc_pb.ServiceRegistration(EchoApi).init(
        init.gpa,
        &service,
        .{ .Echo = EchoService.echo },
        .{ .context_hook = configureContext },
    );
    defer registration.deinit();
    var server = try grpc.Server.init(init.gpa, .{
        .host = "127.0.0.1",
        .port = port,
    });
    defer server.deinit();
    try registration.register(&server);
    try server.start();
    std.debug.print("grpc-lite echo server listening on 127.0.0.1:{d}\n", .{try server.port()});
    server.wait();
}
