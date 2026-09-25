const std = @import("std");
const grpc = @import("grpc_lite");
const perf = @import("grpc_lite_gperftools");

pub fn main() !void {
    _ = grpc.version;

    const memory = try perf.allocator.alloc(u8, 64);
    defer perf.allocator.free(memory);
    if (!perf.owns(memory.ptr)) return error.TcmallocNotLinked;

    std.mem.doNotOptimizeAway(memory);
}
