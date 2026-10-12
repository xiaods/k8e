const std = @import("std");

pub fn build(b: *std.Build) void {
    const targets = [_][]const u8{
        "x86_64-linux-musl",
        "aarch64-linux-musl",
        "riscv64-linux-musl",
    };

    const tandem_step = b.step("tandem", "Build Tandem etcd protocol compatibility layer");

    for (targets) |triple| {
        const query = std.Target.Query.parse(.{ .arch_os_abi = triple }) catch unreachable;
        const target = b.resolveTargetQuery(query);
        const grpc_lite = b.dependency("grpc_lite", .{
            .target = target,
            .optimize = .ReleaseSafe,
            .protobuf = false,
            .tls = true,
        });
        const mod = b.createModule(.{
            .root_source_file = b.path("src/main.zig"),
            .target = target,
            .optimize = .ReleaseSafe,
            .imports = &.{.{ .name = "grpc_lite", .module = grpc_lite.module("grpc_lite") }},
        });
        mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = target }));
        const exe = b.addExecutable(.{
            .name = b.fmt("tandem-{s}", .{triple}),
            .root_module = mod,
        });
        const install = b.addInstallArtifact(exe, .{
            .dest_dir = .{ .override = .{ .custom = "../bin" } },
        });
        tandem_step.dependOn(&install.step);
    }

    b.default_step = tandem_step;

    // Tests (native target only)
    const test_step = b.step("test", "Run unit tests");
    const native = b.standardTargetOptions(.{});
    const probe_module = b.createModule(.{ .root_source_file = b.path("src/persistence_probe.zig"), .target = native });
    probe_module.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
    const probe = b.addExecutable(.{ .name = "persistence-probe", .root_module = probe_module });
    b.step("persistence-probe", "Build real rqlite restart probe").dependOn(&b.addInstallArtifact(probe, .{}).step);
    if (native.result.os.tag == .linux) {
        const native_grpc = b.dependency("grpc_lite", .{ .target = native, .optimize = .ReleaseSafe, .protobuf = false, .tls = true });
        const native_mod = b.createModule(.{ .root_source_file = b.path("src/main.zig"), .target = native, .optimize = .ReleaseSafe });
        native_mod.addImport("grpc_lite", native_grpc.module("grpc_lite"));
        native_mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
        const native_exe = b.addExecutable(.{ .name = "tandem", .root_module = native_mod });
        const native_install = b.addInstallArtifact(native_exe, .{});
        b.step("native", "Build native Tandem for client integration tests").dependOn(&native_install.step);
    }

    // Proto wire format tests (embedded in wire.zig)
    const wire_mod = b.createModule(.{ .root_source_file = b.path("src/proto/wire.zig"), .target = native });
    const wire_tests = b.addTest(.{ .root_module = wire_mod });
    test_step.dependOn(&b.addRunArtifact(wire_tests).step);

    // Proto wire format additional tests
    const proto_test_mod = b.createModule(.{ .root_source_file = b.path("src/proto/test.zig"), .target = native });
    const proto_tests = b.addTest(.{ .root_module = proto_test_mod });
    test_step.dependOn(&b.addRunArtifact(proto_tests).step);

    // Storage layer tests
    const mvcc_mod = b.createModule(.{ .root_source_file = b.path("src/storage_test_root.zig"), .target = native });
    mvcc_mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
    const mvcc_tests = b.addTest(.{ .root_module = mvcc_mod });
    test_step.dependOn(&b.addRunArtifact(mvcc_tests).step);

    const rqlite_mod = b.createModule(.{ .root_source_file = b.path("src/storage_test_root.zig"), .target = native });
    rqlite_mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
    const rqlite_tests = b.addTest(.{ .root_module = rqlite_mod });
    test_step.dependOn(&b.addRunArtifact(rqlite_tests).step);

    // Main entry point tests
    const main_mod = b.createModule(.{ .root_source_file = b.path("src/main.zig"), .target = native });
    main_mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
    const main_tests = b.addTest(.{ .root_module = main_mod });
    test_step.dependOn(&b.addRunArtifact(main_tests).step);

    // Integration tests (storage + services)
    const integration_mod = b.createModule(.{ .root_source_file = b.path("src/storage_test_root.zig"), .target = native });
    integration_mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
    const integration_tests = b.addTest(.{ .root_module = integration_mod });
    test_step.dependOn(&b.addRunArtifact(integration_tests).step);

    // Pipeline tests (request processing e2e)
    const pipeline_mod = b.createModule(.{ .root_source_file = b.path("src/pipeline.zig"), .target = native });
    pipeline_mod.addImport("backend", b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native }));
    const pipeline_tests = b.addTest(.{ .root_module = pipeline_mod });
    test_step.dependOn(&b.addRunArtifact(pipeline_tests).step);

    // Backend transport tests
    const backend_mod = b.createModule(.{ .root_source_file = b.path("src/server/backend.zig"), .target = native });
    const backend_tests = b.addTest(.{ .root_module = backend_mod });
    test_step.dependOn(&b.addRunArtifact(backend_tests).step);
}
