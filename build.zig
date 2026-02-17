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
    const build_date = std.mem.trim(u8, b.run(&.{ "date", "-u", "+%Y-%m-%dT%H:%M:%SZ" }), &std.ascii.whitespace);

    // Download
    const download_cmd = b.addSystemCommand(&.{ "bash", "hack/download" });
    download_step.dependOn(&download_cmd.step);

    // Tags
    var tags: []const u8 = "ctrd netcgo osusergo providerless urfave_cli_no_docs static_build";
    if (use_seccomp) tags = b.fmt("{s} seccomp", .{tags});
    if (use_selinux) tags = b.fmt("{s} selinux", .{tags});
    if (use_apparmor) tags = b.fmt("{s} apparmor", .{tags});

    // Ensure bin directory exists
    const mkdir_bin = b.addSystemCommand(&.{ "mkdir", "-p", "bin" });

    // Build k8e
    const k8e_bin = try buildGoBinary(b, target, .{
        .name = "k8e",
        .package = "./cmd/server",
        .version_env = version_env,
        .tags = tags,
        .build_date = build_date,
    });
    k8e_bin.step.dependOn(&mkdir_bin.step);
    k8e_step.dependOn(&k8e_bin.step);

    // Symlinks
    const k8e_binaries = [_][]const u8{
        "k8e-agent",           "k8e-server",      "k8e-token",      "k8e-etcd-snapshot",
        "k8e-secrets-encrypt", "k8e-certificate", "k8e-completion", "kubectl",
        "containerd",          "crictl",          "ctr",
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

    // Build containerd-shim-runc-v2 (Linux only)
    const shim_step = b.step("shim", "Build containerd-shim-runc-v2");
    const shim_tags = "netgo osusergo static_build";
    const containerd_src = "build/src/github.com/containerd/containerd";
    const shim_build = b.addSystemCommand(&.{ "go", "build" });
    shim_build.setEnvironmentVariable("CGO_ENABLED", "1");
    shim_build.setEnvironmentVariable("GOPATH", b.fmt("{s}/build", .{b.build_root}));
    const shim_zig_target = b.fmt("{s}-linux-musl", .{@tagName(target.result.cpu.arch)});
    shim_build.setEnvironmentVariable("CC", b.fmt("zig cc -target {s}", .{shim_zig_target}));
    shim_build.setEnvironmentVariable("CXX", b.fmt("zig c++ -target {s}", .{shim_zig_target}));
    shim_build.setEnvironmentVariable("GOOS", "linux");
    shim_build.setEnvironmentVariable("GOARCH", goarch_str(target));
    shim_build.setCwd(b.path(containerd_src));
    const version_containerd = version_env.get("VERSION_CONTAINERD") orelse "v0.0.0";
    const pkg_containerd = version_env.get("PKG_CONTAINERD_K8E") orelse "github.com/containerd/containerd";
    const version_crictl = version_env.get("VERSION_CRICTL") orelse "v0.0.0";
    const version_cniplugins = version_env.get("VERSION_CNIPLUGINS") orelse "v0.0.0";
    const version_cri_dockerd = version_env.get("VERSION_CRI_DOCKERD") orelse "v0.0.0";
    const version_str = version_env.get("VERSION") orelse "v0.0.0";
    const commit_str = version_env.get("COMMIT") orelse "HEAD";
    const tree_state_str = version_env.get("TREE_STATE") orelse "clean";
    const shim_ldflags = b.fmt(
        "-w -s -extldflags '-static -lm -ldl -lz -lpthread'" ++
            " -X {s}/pkg/version.Version={s}" ++
            " -X {s}/pkg/version.GitCommit={s}" ++
            " -X k8s.io/client-go/pkg/version.gitVersion={s}" ++
            " -X k8s.io/client-go/pkg/version.gitCommit={s}" ++
            " -X k8s.io/client-go/pkg/version.gitTreeState={s}" ++
            " -X k8s.io/client-go/pkg/version.buildDate={s}" ++
            " -X k8s.io/component-base/version.gitVersion={s}" ++
            " -X k8s.io/component-base/version.gitCommit={s}" ++
            " -X k8s.io/component-base/version.gitTreeState={s}" ++
            " -X k8s.io/component-base/version.buildDate={s}" ++
            " -X sigs.k8s.io/cri-tools/pkg/version.Version={s}" ++
            " -X github.com/containerd/containerd/version.Version={s}" ++
            " -X github.com/containerd/containerd/version.Package={s}" ++
            " -X github.com/containernetworking/plugins/pkg/utils/buildversion.BuildVersion={s}" ++
            " -X github.com/Mirantis/cri-dockerd/cmd/version.Version={s}" ++
            " -X github.com/Mirantis/cri-dockerd/cmd/version.GitCommit=HEAD" ++
            " -X github.com/Mirantis/cri-dockerd/cmd/version.BuildTime={s}" ++
            " -X go.etcd.io/etcd/api/v3/version.GitSHA=HEAD",
        .{
            "github.com/xiaods/k8e", version_str,
            "github.com/xiaods/k8e", if (commit_str.len >= 8) commit_str[0..8] else commit_str,
            version_str,             commit_str,
            tree_state_str,          build_date,
            version_str,             commit_str,
            tree_state_str,          build_date,
            version_crictl,          version_containerd,
            pkg_containerd,          version_cniplugins,
            version_cri_dockerd,     build_date,
        },
    );
    shim_build.addArgs(&.{ "-tags", shim_tags, "-ldflags", shim_ldflags, "-o", b.fmt("{s}/bin/containerd-shim-runc-v2", .{b.build_root}), "./cmd/containerd-shim-runc-v2" });
    shim_build.step.dependOn(&download_cmd.step);
    shim_build.step.dependOn(&mkdir_bin.step);
    shim_step.dependOn(&shim_build.step);

    // Build runc (Linux only)
    const runc_step = b.step("runc", "Build runc");
    const runc_src = "build/src/github.com/opencontainers/runc";
    const runc_build = b.addSystemCommand(&.{"make"});
    runc_build.setEnvironmentVariable("CC", b.fmt("zig cc -target {s}", .{shim_zig_target}));
    runc_build.setEnvironmentVariable("EXTRA_LDFLAGS", "-w -s");
    runc_build.setEnvironmentVariable("BUILDTAGS", "apparmor seccomp");
    runc_build.setCwd(b.path(runc_src));
    runc_build.addArgs(&.{"static"});
    runc_build.step.dependOn(&download_cmd.step);
    const runc_cp = b.addSystemCommand(&.{ "cp", "-vf", b.fmt("{s}/{s}/runc", .{ b.build_root, runc_src }), b.fmt("{s}/bin/runc", .{b.build_root}) });
    runc_cp.step.dependOn(&runc_build.step);
    runc_cp.step.dependOn(&mkdir_bin.step);
    runc_step.dependOn(&runc_cp.step);

    // Build CNI plugins
    const cni_step = b.step("cni", "Build CNI plugins");
    const cni_version = version_env.get("VERSION_CNIPLUGINS") orelse "v1.6.0-k3s1";
    const cni_clone_rel = ".cni-build";
    const cni_workdir_rel = cni_clone_rel ++ "/src/github.com/containernetworking/plugins";
    const cni_clone_abs = b.fmt("{s}/{s}", .{ b.build_root, cni_clone_rel });
    const cni_workdir_abs = b.fmt("{s}/{s}", .{ b.build_root, cni_workdir_rel });
    const cni_clone = b.addSystemCommand(&.{
        "bash",                                                                                                                                                                                              "-c",
        b.fmt("rm -rf {s} && mkdir -p {s} && git clone --single-branch --depth=1 --branch={s} https://github.com/rancher/plugins.git {s}", .{ cni_clone_abs, cni_clone_abs, cni_version, cni_workdir_abs }),
    });
    const cni_build = b.addSystemCommand(&.{ "go", "build" });
    cni_build.setEnvironmentVariable("GO111MODULE", "off");
    cni_build.setEnvironmentVariable("GOPATH", cni_clone_abs);
    cni_build.setEnvironmentVariable("CGO_ENABLED", "0");
    cni_build.setEnvironmentVariable("GOOS", "linux");
    cni_build.setEnvironmentVariable("GOARCH", goarch_str(target));
    cni_build.addArgs(&.{ "-tags", tags, "-ldflags", b.fmt("-w -s -extldflags '-static' -X {s}/pkg/utils/buildversion.BuildVersion={s}", .{ "github.com/containernetworking/plugins", version_cniplugins }) });
    cni_build.addArgs(&.{ "-o", b.fmt("{s}/bin/cni", .{b.build_root}), "." });
    cni_build.setCwd(b.path(cni_workdir_rel));
    cni_build.step.dependOn(&cni_clone.step);
    cni_build.step.dependOn(&mkdir_bin.step);
    cni_step.dependOn(&cni_build.step);

    all_step.dependOn(k8e_step);
    all_step.dependOn(shim_step);
    all_step.dependOn(runc_step);
    all_step.dependOn(cni_step);
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

fn goarch_str(target: std.Build.ResolvedTarget) []const u8 {
    return switch (target.result.cpu.arch) {
        .x86_64 => "amd64",
        .aarch64 => "arm64",
        .arm => "arm",
        else => "amd64",
    };
}

fn buildGoBinary(b: *std.Build, target: std.Build.ResolvedTarget, options: struct {
    name: []const u8,
    package: []const u8,
    version_env: std.StringHashMap([]const u8),
    tags: []const u8,
    build_date: []const u8,
}) !*std.Build.Step.Run {
    const go_build = b.addSystemCommand(&.{ "go", "build" });
    const goos = switch (target.result.os.tag) {
        .linux => "linux",
        .windows => "windows",
        .macos => "darwin",
        else => "linux",
    };
    const goarch = goarch_str(target);
    go_build.setEnvironmentVariable("GOOS", goos);
    go_build.setEnvironmentVariable("GOARCH", goarch);
    go_build.setEnvironmentVariable("CGO_ENABLED", "1");
    const zig_target = b.fmt("{s}-{s}-{s}", .{ @tagName(target.result.cpu.arch), @tagName(target.result.os.tag), if (target.result.os.tag == .linux) "musl" else @tagName(target.result.abi) });
    go_build.setEnvironmentVariable("CC", b.fmt("zig cc -target {s}", .{zig_target}));
    go_build.setEnvironmentVariable("CXX", b.fmt("zig c++ -target {s}", .{zig_target}));
    go_build.addArgs(&.{ "-tags", options.tags, "-buildvcs=false" });
    const v = options.version_env;
    const PKG = "github.com/xiaods/k8e";
    const PKG_K8S_CLIENT = "k8s.io/client-go/pkg";
    const PKG_K8S_BASE = "k8s.io/component-base";
    const PKG_CRICTL = "sigs.k8s.io/cri-tools/pkg";
    const PKG_CONTAINERD = "github.com/containerd/containerd";
    const PKG_CNI_PLUGINS = "github.com/containernetworking/plugins";
    const PKG_CRI_DOCKERD = "github.com/Mirantis/cri-dockerd";
    const PKG_ETCD = "go.etcd.io/etcd";
    const commit = v.get("COMMIT") orelse "HEAD";
    const commit_short = if (commit.len >= 8) commit[0..8] else commit;
    const version = v.get("VERSION") orelse "v0.0.0";
    const tree_state = v.get("TREE_STATE") orelse "clean";
    const ldflags = b.fmt(
        "-w -s -extldflags '-static -lm -ldl -lz -lpthread'" ++
            " -X {s}/pkg/version.Version={s}" ++
            " -X {s}/pkg/version.GitCommit={s}" ++
            " -X {s}/pkg/version.UpstreamGolang={s}" ++
            " -X {s}/version.gitVersion={s}" ++
            " -X {s}/version.gitCommit={s}" ++
            " -X {s}/version.gitTreeState={s}" ++
            " -X {s}/version.buildDate={s}" ++
            " -X {s}/version.gitVersion={s}" ++
            " -X {s}/version.gitCommit={s}" ++
            " -X {s}/version.gitTreeState={s}" ++
            " -X {s}/version.buildDate={s}" ++
            " -X {s}/version.Version={s}" ++
            " -X {s}/version.Version={s}" ++
            " -X {s}/version.Package={s}" ++
            " -X {s}/pkg/utils/buildversion.BuildVersion={s}" ++
            " -X {s}/cmd/version.Version={s}" ++
            " -X {s}/cmd/version.GitCommit=HEAD" ++
            " -X {s}/cmd/version.BuildTime={s}" ++
            " -X {s}/api/v3/version.GitSHA=HEAD",
        .{
            PKG,                version,
            PKG,                commit_short,
            PKG,                v.get("VERSION_GOLANG") orelse "go",
            PKG_K8S_CLIENT,     version,
            PKG_K8S_CLIENT,     commit,
            PKG_K8S_CLIENT,     tree_state,
            PKG_K8S_CLIENT,     options.build_date,
            PKG_K8S_BASE,       version,
            PKG_K8S_BASE,       commit,
            PKG_K8S_BASE,       tree_state,
            PKG_K8S_BASE,       options.build_date,
            PKG_CRICTL,         v.get("VERSION_CRICTL") orelse "v0.0.0",
            PKG_CONTAINERD,     v.get("VERSION_CONTAINERD") orelse "v0.0.0",
            PKG_CONTAINERD,     v.get("PKG_CONTAINERD_K8E") orelse PKG_CONTAINERD,
            PKG_CNI_PLUGINS,    v.get("VERSION_CNIPLUGINS") orelse "v0.0.0",
            PKG_CRI_DOCKERD,    v.get("VERSION_CRI_DOCKERD") orelse "v0.0.0",
            PKG_CRI_DOCKERD,    PKG_CRI_DOCKERD,
            options.build_date, PKG_ETCD,
        },
    );
    go_build.addArgs(&.{ "-ldflags", ldflags });
    go_build.addArgs(&.{ "-o", b.fmt("bin/{s}", .{options.name}), options.package });
    return go_build;
}
