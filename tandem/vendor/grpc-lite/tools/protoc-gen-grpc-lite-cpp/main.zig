const std = @import("std");
const plugin = @import("protobuf_plugin");
const generator = @import("generator.zig");

pub fn main() !void {
    const allocator = std.heap.smp_allocator;
    var threaded: std.Io.Threaded = .init(allocator, .{ .environ = .empty });
    const io = threaded.io();

    var stdin_buffer: [4096]u8 = undefined;
    var stdin = std.Io.File.stdin().reader(io, &stdin_buffer);
    var request = try plugin.CodeGeneratorRequest.decode(&stdin.interface, allocator);
    defer request.deinit(allocator);

    var response: plugin.CodeGeneratorResponse = .{
        .supported_features = @intFromEnum(plugin.CodeGeneratorResponse.Feature.FEATURE_PROTO3_OPTIONAL),
    };
    defer response.deinit(allocator);
    generator.generate(allocator, request, &response) catch |err| {
        response.@"error" = try std.fmt.allocPrint(
            allocator,
            "protoc-gen-grpc_lite_cpp failed: {s}",
            .{@errorName(err)},
        );
    };

    var stdout_buffer: [4096]u8 = undefined;
    var stdout = std.Io.File.stdout().writer(io, &stdout_buffer);
    try response.encode(&stdout.interface, allocator);
    try stdout.interface.flush();
}
