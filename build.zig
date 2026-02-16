const std = @import("std");

pub fn build(b: *std.Build) !void {
    const target = b.standardTargetOptions(.{});
    _ = b.standardOptimizeOption(.{});

    const use_seccomp = b.option(bool, "seccomp", "Enable seccomp support") orelse false;
    const use_selinux = b.option(bool, "selinux", "Enable selinux support") orelse false;
    const use_apparmor = b.option(bool, "apparmor", "Enable apparmor support") orelse false;

    // Steps
    const k8e_step = b.step("k8e", "Build k8e");
    const download_step = b.step("download", "Download dependencies");
    const test_step = b.step("test", "Run unit tests");
    const all_step = b.step("all", "Build all");
    b.default_step = all_step;

    // Versioning
    const version_env = try getVersionEnv(b);

    // Download
    const download_cmd = b.addSystemCommand(&.{ "bash", "hack/download" });
    download_step.dependOn(&download_cmd.step);

    // Tags
    var tags: []const u8 = "ctrd netcgo osusergo providerless urfave_cli_no_docs static_build";
    if (use_seccomp) tags = b.fmt("{s} seccomp", .{tags});
    if (use_selinux) tags = b.fmt("{s} selinux", .{tags});
    if (use_apparmor) tags = b.fmt("{s} apparmor", .{tags});

    // Build k8e
    const k8e_bin = try buildGoBinary(b, target, .{
        .name = "k8e",
        .package = "./cmd/server",
        .version_env = version_env,
        .tags = tags,
    });
    k8e_step.dependOn(&k8e_bin.step);

    // Symlinks
    const k8e_binaries = [_][]const u8{
        "k8e-agent", "k8e-server", "k8e-token", "k8e-etcd-snapshot",
        "k8e-secrets-encrypt", "k8e-certificate", "k8e-completion",
        "kubectl", "containerd", "crictl", "ctr",
    };
    for (k8e_binaries) |name| {
        const bin_path = b.fmt("bin/{s}", .{name});
        const symlink = b.addSystemCommand(&.{ "ln", "-sf", "k8e", bin_path });
        symlink.step.dependOn(&k8e_bin.step);
        k8e_step.dependOn(&symlink.step);
    }

    // Test
    const go_test = b.addSystemCommand(&.{ "go", "test", "-v", "./..." });
    test_step.dependOn(&go_test.step);

    all_step.dependOn(k8e_step);
}

fn getVersionEnv(b: *std.Build) !std.StringHashMap([]const u8) {
    var map = std.StringHashMap([]const u8).init(b.allocator);
    const res = b.run(&.{ "bash", "-c", "source hack/version.sh && env | grep -E '^(VERSION|COMMIT|TREE_STATE|VERSION_GOLANG|VERSION_CRICTL|VERSION_CONTAINERD|PKG_CONTAINERD_K8E|VERSION_CNIPLUGINS|VERSION_CRI_DOCKERD|VERSION_RUNC)='" });
    var it = std.mem.tokenizeAny(u8, res, "\n");
    while (it.next()) |line| {
        var parts = std.mem.splitScalar(u8, line, '=');
        const key = parts.next() orelse continue;
        const value = parts.next() orelse "";
        try map.put(b.dupe(key), b.dupe(value));
    }
    return map;
}

fn buildGoBinary(b: *std.Build, target: std.Build.ResolvedTarget, options: struct {
    name: []const u8,
    package: []const u8,
    version_env: std.StringHashMap([]const u8),
    tags: []const u8,
}) !*std.Build.Step.Run {
    const go_build = b.addSystemCommand(&.{ "go", "build" });
    const goos = switch (target.result.os.tag) {
        .linux => "linux",
        .windows => "windows",
        .macos => "darwin",
        else => "linux",
    };
    const goarch = switch (target.result.cpu.arch) {
        .x86_64 => "amd64",
        .aarch64 => "arm64",
        .arm => "arm",
        else => "amd64",
    };
    go_build.setEnvironmentVariable("GOOS", goos);
    go_build.setEnvironmentVariable("GOARCH", goarch);
    go_build.setEnvironmentVariable("CGO_ENABLED", "1");
    const zig_target = b.fmt("{s}-{s}-{s}", .{ @tagName(target.result.cpu.arch), @tagName(target.result.os.tag), @tagName(target.result.abi) });
    go_build.setEnvironmentVariable("CC", b.fmt("zig cc -target {s}", .{zig_target}));
    go_build.setEnvironmentVariable("CXX", b.fmt("zig c++ -target {s}", .{zig_target}));
    go_build.addArgs(&.{ "-tags", options.tags });
    const v = options.version_env;
    const PKG = "github.com/xiaods/k8e";
    const commit = v.get("COMMIT") orelse "HEAD";
    const commit_short = if (commit.len >= 8) commit[0..8] else commit;
    const ldflags = b.fmt("-w -s -extldflags '-static' -X {s}/pkg/version.Version={s} -X {s}/pkg/version.GitCommit={s} -X {s}/pkg/version.UpstreamGolang={s}", .{
        PKG, v.get("VERSION") orelse "v0.0.0",
        PKG, commit_short,
        PKG, v.get("VERSION_GOLANG") orelse "go",
    });
    go_build.addArgs(&.{ "-ldflags", ldflags });
    go_build.addArgs(&.{ "-o", b.fmt("bin/{s}", .{options.name}), options.package });
    return go_build;
}
