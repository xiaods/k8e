const std = @import("std");

pub fn build(b: *std.Build) void {
    const target = b.standardTargetOptions(.{});
    const optimize = b.standardOptimizeOption(.{});
    const sanitize_thread = b.option(bool, "sanitize-thread", "Enable ThreadSanitizer") orelse false;
    const sanitize_c = b.option(bool, "sanitize-c", "Enable C undefined behavior detection") orelse false;
    const enable_gperftools = b.option(bool, "gperftools", "Enable gperftools support") orelse false;
    const enable_tls = b.option(bool, "tls", "Enable TLS support") orelse false;
    const grpc_lite = b.dependency("grpc_lite", .{
        .target = target,
        .optimize = optimize,
        .@"sanitize-thread" = sanitize_thread,
        .@"sanitize-c" = sanitize_c,
        .gperftools = enable_gperftools,
        .protobuf = false,
        .tls = enable_tls,
    });
    const root_module = b.createModule(.{
        .root_source_file = b.path(if (enable_gperftools) "src/gperftools.zig" else "src/main.zig"),
        .target = target,
        .optimize = optimize,
        .sanitize_thread = sanitize_thread,
        .sanitize_c = if (sanitize_c) .full else .off,
        .omit_frame_pointer = if (sanitize_thread or sanitize_c or enable_gperftools) false else null,
        .imports = &.{.{ .name = "grpc_lite", .module = grpc_lite.module("grpc_lite") }},
    });
    const consumer_options = b.addOptions();
    consumer_options.addOption(bool, "tls", enable_tls);
    root_module.addOptions("consumer_options", consumer_options);
    if (enable_gperftools) {
        root_module.addImport("grpc_lite_gperftools", grpc_lite.module("grpc_lite_gperftools"));
    }
    const executable = b.addExecutable(.{
        .name = "grpc-lite-raw-consumer",
        .root_module = root_module,
    });
    b.installArtifact(executable);
}
