const std = @import("std");
const grpc = @import("grpc_lite");

pub fn main() !void {
    _ = grpc.version;

    const memory = try std.heap.c_allocator.alloc(u8, 4096);
    defer std.heap.c_allocator.free(memory);
    std.mem.doNotOptimizeAway(memory);
}
