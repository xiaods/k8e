const std = @import("std");
const nanozlog = @import("nanozlog");

pub const Level = nanozlog.Level;
pub const Config = nanozlog.Config;

pub const RuntimeLevel = enum(u8) {
    info,
    debug,
};

var stderr_buf: [8192]u8 = undefined;
var stderr_writer: std.Io.File.Writer = undefined;

const PrintLevel = enum { ERR, WARN, INFO, DEBUG, TRACE };

var verbose_flag: std.atomic.Value(bool) = .init(false);
var runtime_level: std.atomic.Value(u8) = .init(@intFromEnum(RuntimeLevel.info));

fn printMeta(writer: *std.Io.Writer, meta: nanozlog.Meta) std.Io.Writer.Error!void {
    try writer.print(
        "{d}/{d:02}/{d:02} {d:02}:{d:02}:{d:02}.{d:03}{d:03} {s:<5} [{d}] ",
        .{
            meta.year,
            meta.month,
            meta.day,
            meta.hour,
            meta.minute,
            meta.second,
            meta.millisecond,
            meta.microsecond,
            @tagName(@as(PrintLevel, @enumFromInt(@intFromEnum(meta.level)))),
            meta.thread_id,
        },
    );
    if (verbose_flag.load(.monotonic)) {
        try writer.print("{s}:{d} ", .{ meta.src.file, meta.src.line });
    }
}

pub fn initGlobal(allocator: std.mem.Allocator, io: std.Io, verbose: bool) !void {
    verbose_flag.store(verbose, .release);
    runtime_level.store(@intFromEnum(if (verbose) RuntimeLevel.debug else RuntimeLevel.info), .release);
    stderr_writer = std.Io.File.stderr().writer(io, &stderr_buf);
    try nanozlog.initNanoZlog(allocator, io, &stderr_writer.interface, .{
        .is_localtime = true,
        .polling_interval = 100_000_000,
        .flush_delay = 200_000_000,
        .min_level = .debug,
        .print_meta_cb = &printMeta,
    });
}

pub fn setRuntimeLevel(level: RuntimeLevel) void {
    runtime_level.store(@intFromEnum(level), .release);
    verbose_flag.store(level == .debug, .release);
}

pub fn getRuntimeLevel() RuntimeLevel {
    return @enumFromInt(runtime_level.load(.acquire));
}

pub fn deinitGlobal(allocator: std.mem.Allocator) void {
    nanozlog.deinitNanoZlog(allocator);
}

pub fn err(comptime src: std.builtin.SourceLocation, comptime format: []const u8, args: anytype) void {
    nanozlog.err(src, format, args);
}

pub fn warn(comptime src: std.builtin.SourceLocation, comptime format: []const u8, args: anytype) void {
    nanozlog.warn(src, format, args);
}

pub fn info(comptime src: std.builtin.SourceLocation, comptime format: []const u8, args: anytype) void {
    nanozlog.info(src, format, args);
}

pub fn debug(comptime src: std.builtin.SourceLocation, comptime format: []const u8, args: anytype) void {
    if (getRuntimeLevel() == .debug) nanozlog.debug(src, format, args);
}

pub fn trace(comptime src: std.builtin.SourceLocation, comptime format: []const u8, args: anytype) void {
    if (getRuntimeLevel() == .debug) nanozlog.trace(src, format, args);
}

test "runtime log level changes atomically" {
    setRuntimeLevel(.info);
    try std.testing.expectEqual(RuntimeLevel.info, getRuntimeLevel());
    setRuntimeLevel(.debug);
    try std.testing.expectEqual(RuntimeLevel.debug, getRuntimeLevel());
    setRuntimeLevel(.info);
}
