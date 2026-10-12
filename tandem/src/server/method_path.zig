pub const MethodPath = struct {
    service: []const u8,
    method: []const u8,

    pub fn parse(path: []const u8) !MethodPath {
        if (path.len < 3 or path[0] != '/') return error.InvalidPath;
        const separator = std.mem.indexOfScalarPos(u8, path, 1, '/') orelse return error.InvalidPath;
        const service = path[1..separator];
        const method = path[separator + 1 ..];
        if (service.len == 0 or method.len == 0) return error.InvalidPath;
        return .{ .service = service, .method = method };
    }
};

const std = @import("std");

test "method path parses service and method" {
    const parsed = try MethodPath.parse("/etcdserverpb.KV/Range");
    try std.testing.expectEqualStrings("etcdserverpb.KV", parsed.service);
    try std.testing.expectEqualStrings("Range", parsed.method);
}
