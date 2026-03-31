const std = @import("std");

pub fn build(b: *std.Build) void {
    const targets = [_][]const u8{
        "x86_64-linux-musl",
        "aarch64-linux-musl",
        "riscv64-linux-musl",
    };

    const sandboxd_step = b.step("sandboxd", "Build sandboxd init process");

    for (targets) |triple| {
        const query = std.Target.Query.parse(.{ .arch_os_abi = triple }) catch unreachable;
        const target = b.resolveTargetQuery(query);
        const mod = b.createModule(.{
            .root_source_file = b.path("src/main.zig"),
            .target = target,
            .optimize = .ReleaseSafe,
        });
        const exe = b.addExecutable(.{
            .name = b.fmt("sandboxd-{s}", .{triple}),
            .root_module = mod,
        });
        const install = b.addInstallArtifact(exe, .{
            .dest_dir = .{ .override = .{ .custom = "../../bin" } },
        });
        sandboxd_step.dependOn(&install.step);
    }

    b.default_step = sandboxd_step;

    // Tests (native target only)
    const test_step = b.step("test", "Run unit tests");
    const native = b.standardTargetOptions(.{});

    const exec_mod = b.createModule(.{ .root_source_file = b.path("src/exec_test.zig"), .target = native });
    const exec_tests = b.addTest(.{ .root_module = exec_mod });
    test_step.dependOn(&b.addRunArtifact(exec_tests).step);

    const files_mod = b.createModule(.{ .root_source_file = b.path("src/files_test.zig"), .target = native });
    const files_tests = b.addTest(.{ .root_module = files_mod });
    test_step.dependOn(&b.addRunArtifact(files_tests).step);
}
