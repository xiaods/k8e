const std = @import("std");

pub fn enableTcpNoDelay(fd: std.posix.fd_t) !void {
    const enabled: c_int = 1;
    try std.posix.setsockopt(
        fd,
        std.posix.IPPROTO.TCP,
        std.posix.TCP.NODELAY,
        std.mem.asBytes(&enabled),
    );
}

pub fn enableReusePort(fd: std.posix.fd_t) !void {
    const enabled: c_int = 1;
    try std.posix.setsockopt(
        fd,
        std.posix.SOL.SOCKET,
        std.posix.SO.REUSEPORT,
        std.mem.asBytes(&enabled),
    );
}

test "enable TCP_NODELAY" {
    const address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var listener = try address.listen(std.testing.io, .{});
    defer listener.deinit(std.testing.io);

    try enableTcpNoDelay(listener.socket.handle);
    var enabled: c_int = 0;
    var length: std.posix.socklen_t = @sizeOf(c_int);
    const rc = std.posix.system.getsockopt(
        listener.socket.handle,
        std.posix.IPPROTO.TCP,
        std.posix.TCP.NODELAY,
        @ptrCast(&enabled),
        &length,
    );
    try std.testing.expectEqual(std.posix.E.SUCCESS, std.posix.errno(rc));
    try std.testing.expectEqual(@as(c_int, 1), enabled);
}

test "enable SO_REUSEPORT" {
    const address = try std.Io.net.IpAddress.parseIp4("127.0.0.1", 0);
    var listener = try address.listen(std.testing.io, .{});
    defer listener.deinit(std.testing.io);

    try enableReusePort(listener.socket.handle);
    var enabled: c_int = 0;
    var length: std.posix.socklen_t = @sizeOf(c_int);
    const rc = std.posix.system.getsockopt(
        listener.socket.handle,
        std.posix.SOL.SOCKET,
        std.posix.SO.REUSEPORT,
        @ptrCast(&enabled),
        &length,
    );
    try std.testing.expectEqual(std.posix.E.SUCCESS, std.posix.errno(rc));
    try std.testing.expectEqual(@as(c_int, 1), enabled);
}
