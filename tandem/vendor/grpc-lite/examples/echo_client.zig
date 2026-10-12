const std = @import("std");
const demo = @import("demo_proto");
const grpc = @import("grpc_lite");
const grpc_pb = @import("grpc_lite_protobuf");

const EchoApi = demo.EchoService(void, error{});

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const target = if (args.len > 1) args[1] else "127.0.0.1:50051";
    const request = if (args.len > 2) args[2] else "hello grpc-lite";

    var channel = try grpc.Channel.init(init.gpa, target, .{});
    defer channel.deinit();
    var client = grpc_pb.ServiceClient(EchoApi).init(&channel);
    var result = try client.callUnary(
        init.gpa,
        "Echo",
        demo.EchoRequest{ .message = request },
        .{ .timeout_ns = 5 * std.time.ns_per_s },
    );
    defer result.deinit();

    if (!result.raw.status.isOk()) {
        std.debug.print("RPC failed: {t}: {s}\n", .{ result.raw.status.code, result.raw.status.message });
        return error.RpcFailed;
    }
    std.debug.print("response: {s}\n", .{result.response.?.message});
}
