const std = @import("std");
const builtin = @import("builtin");

const PKG = "github.com/xiaods/k8e";
const PKG_K8S_CLIENT = "k8s.io/client-go/pkg";
const PKG_K8S_BASE = "k8s.io/component-base";
const PKG_CRICTL = "sigs.k8s.io/cri-tools/pkg";
const PKG_CONTAINERD = "github.com/containerd/containerd/v2";
const PKG_CNI_PLUGINS = "github.com/containernetworking/plugins";
const PKG_CRI_DOCKERD = "github.com/Mirantis/cri-dockerd";
const PKG_ETCD = "go.etcd.io/etcd";

fn buildRoot(b: *std.Build) []const u8 {
    return b.build_root.path orelse ".";
}

pub fn build(b: *std.Build) !void {
    _ = b.standardTargetOptions(.{});
    _ = b.standardOptimizeOption(.{});

    // Find bash (on Windows, check common Git for Windows paths)
    const bash = findBash();

    // Steps
    const k8e_step = b.step("k8e", "Build k8e");
    const download_step = b.step("download", "Download dependencies");
    const test_step = b.step("test", "Run unit tests");
    const deps_step = b.step("deps", "Tidy go module dependencies");
    const fmt_step = b.step("fmt", "Format source code");
    const generate_step = b.step("generate", "Run code generation");
    const package_step = b.step("package", "Package release artifacts");
    const package_cli_step = b.step("package-cli", "Package CLI release artifacts");
    const package_airgap_step = b.step("package-airgap", "Package air-gap release artifacts");
    const clean_step = b.step("clean", "Remove build artifacts");
    const all_step = b.step("all", "Build all");
    b.default_step = all_step;

    // Versioning
    const version_env = try getVersionEnv(b, bash);
    const build_date = std.mem.trim(u8, b.run(&.{ bash, "-c", "date -u '+%Y-%m-%dT%H:%M:%SZ'" }), &std.ascii.whitespace);

    // Download
    const download_cmd = b.addSystemCommand(&.{ bash, "hack/download" });
    download_step.dependOn(&download_cmd.step);

    // Tags (aligned with k3s: ctrd netcgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp)
    const tags = "ctrd netcgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp";

    // Ensure bin directory exists
    const mkdir_bin = b.addSystemCommand(&.{ "mkdir", "-p", "bin" });

    // Common version info
    const root = buildRoot(b);
    const vi = VersionInfo{
        .version = version_env.get("VERSION") orelse "v0.0.0",
        .commit = version_env.get("COMMIT") orelse "HEAD",
        .tree_state = version_env.get("TREE_STATE") orelse "clean",
        .build_date = build_date,
        .version_golang = version_env.get("VERSION_GOLANG") orelse "go",
        .version_crictl = version_env.get("VERSION_CRICTL") orelse "v0.0.0",
        .version_containerd = version_env.get("VERSION_CONTAINERD") orelse "v0.0.0",
        .pkg_containerd = version_env.get("PKG_CONTAINERD_K8E") orelse PKG_CONTAINERD,
        .version_cniplugins = version_env.get("VERSION_CNIPLUGINS") orelse "v0.0.0",
        .version_cri_dockerd = version_env.get("VERSION_CRI_DOCKERD") orelse "v0.0.0",
        .version_hcsshim = version_env.get("VERSION_HCSSHIM") orelse "v0.0.0",
    };
    const ldflags = try buildVersionFlags(b.allocator, vi);

    // Pre-build cleanup: remove stale k8e binaries (aligned with k3s scripts/build)
    const k8e_binaries = [_][]const u8{
        "k8e-agent",           "k8e-server",      "k8e-token",      "k8e-etcd-snapshot",
        "k8e-secrets-encrypt", "k8e-certificate", "k8e-completion", "kubectl",
        "containerd",          "crictl",          "ctr",
    };
    const cleanup_k8e = b.addSystemCommand(&.{
        bash, "-c",
        "for i in bin/k8e bin/k8e-agent bin/k8e-server bin/k8e-token bin/k8e-etcd-snapshot " ++
            "bin/k8e-secrets-encrypt bin/k8e-certificate bin/k8e-completion " ++
            "bin/kubectl bin/containerd bin/crictl bin/ctr" ++
            "; do [ -f \"$i\" ] && echo \"Removing $i\" && rm -f \"$i\" || true; done",
    });

    // Pre-build cleanup: remove stale containerd binaries (aligned with k3s scripts/build)
    // containerd_binaries=(
    //     "bin/containerd-shim"
    //     "bin/containerd-shim-runc-v2"
    //     "bin/runc"
    //     "bin/containerd-shim-runhcs-v1"
    //     "bin/runhcs"
    // )
    const cleanup_containerd = b.addSystemCommand(&.{
        bash, "-c",
        "for i in bin/containerd-shim bin/containerd-shim-runc-v2 bin/runc " ++
            "bin/containerd-shim-runhcs-v1 bin/runhcs" ++
            "; do [ -f \"$i\" ] && echo \"Removing $i\" && rm -f \"$i\" || true; done",
    });

    // Build k8e (aligned with k3s: system gcc, SQLite CGO flags)
    const k8e_build = b.addSystemCommand(&.{ "go", "build" });
    k8e_build.setEnvironmentVariable("CGO_ENABLED", "1");
    k8e_build.setEnvironmentVariable("CGO_CFLAGS", "-DSQLITE_ENABLE_DBSTAT_VTAB=1 -DSQLITE_USE_ALLOCA=1");
    k8e_build.addArgs(&.{ "-tags", tags, "-buildvcs=false", "-ldflags", ldflags });
    k8e_build.addArgs(&.{ "-o", "bin/k8e", "./cmd/server" });
    k8e_build.step.dependOn(&mkdir_bin.step);
    k8e_build.step.dependOn(&cleanup_k8e.step);
    k8e_build.step.dependOn(&cleanup_containerd.step);
    k8e_step.dependOn(&k8e_build.step);

    // Symlinks for k8e binaries
    for (k8e_binaries) |name| {
        const bin_path = b.fmt("bin/{s}", .{name});
        const symlink = b.addSystemCommand(&.{ "ln", "-sf", "k8e", bin_path });
        symlink.step.dependOn(&k8e_build.step);
        k8e_step.dependOn(&symlink.step);
    }

    // Test
    const go_test = b.addSystemCommand(&.{ "go", "test", "-v", "./..." });
    go_test.setEnvironmentVariable("CGO_ENABLED", "1");
    go_test.setEnvironmentVariable("GOLANG_PROTOBUF_REGISTRATION_CONFLICT", "warn");
    test_step.dependOn(&go_test.step);

    // Deps
    const go_mod_tidy = b.addSystemCommand(&.{ "go", "mod", "tidy" });
    deps_step.dependOn(&go_mod_tidy.step);

    // Format
    const go_fmt = b.addSystemCommand(&.{ "go", "fmt", "./..." });
    const zig_fmt = b.addSystemCommand(&.{ "zig", "fmt", "build.zig" });
    fmt_step.dependOn(&go_fmt.step);
    fmt_step.dependOn(&zig_fmt.step);

    // Generate
    const generate_cmd = b.addSystemCommand(&.{ bash, "hack/generate" });
    generate_step.dependOn(&generate_cmd.step);

    // Package
    const package_cmd = b.addSystemCommand(&.{ bash, "hack/package" });
    package_step.dependOn(&package_cmd.step);

    const package_cli_cmd = b.addSystemCommand(&.{ bash, "hack/package-cli" });
    package_cli_cmd.step.dependOn(all_step);
    package_cli_step.dependOn(&package_cli_cmd.step);

    const package_airgap_cmd = b.addSystemCommand(&.{ bash, "hack/package-airgap.sh" });
    package_airgap_step.dependOn(&package_airgap_cmd.step);

    // Clean
    const clean_cmd = b.addSystemCommand(&.{ "rm", "-rf", "bin", "dist", "build", ".zig-cache", "zig-out", ".cni-build" });
    clean_step.dependOn(&clean_cmd.step);

    // =========================================================================
    // Containerd binaries: containerd-shim-runc-v2, runc (Linux)
    //                      containerd-shim-runhcs-v1, runhcs (Windows)
    // Aligned with k3s scripts/build containerd_binaries handling
    // =========================================================================

    // Build containerd-shim-runc-v2 (Linux only)
    // Aligned with k3s: uses system gcc, GOPATH, tags with netgo (not netcgo)
    // Outputs to containerd source bin/ dir, then copies to project bin/ (like upstream)
    const shim_step = b.step("shim", "Build containerd-shim-runc-v2 (Linux)");
    const shim_tags = "ctrd netgo osusergo providerless urfave_cli_no_docs static_build apparmor seccomp";
    const containerd_src = "build/src/github.com/containerd/containerd";
    const shim_build = b.addSystemCommand(&.{ "go", "build" });
    shim_build.setEnvironmentVariable("CGO_ENABLED", "1");
    shim_build.setEnvironmentVariable("GOPATH", b.fmt("{s}/build", .{root}));
    shim_build.setCwd(b.path(containerd_src));
    shim_build.addArgs(&.{
        "-tags",                         shim_tags,
        "-ldflags",                      ldflags,
        "-o",                            "bin/containerd-shim-runc-v2",
        "./cmd/containerd-shim-runc-v2",
    });
    shim_build.step.dependOn(&download_cmd.step);
    shim_build.step.dependOn(&mkdir_bin.step);
    shim_build.step.dependOn(&cleanup_containerd.step);
    // Copy containerd build output to project bin/ (aligned with k3s: cp -vf ./build/src/.../containerd/bin/* ./bin/)
    const shim_cp = b.addSystemCommand(&.{
        bash,                                                                    "-c",
        b.fmt("cp -vf {s}/{s}/bin/* {s}/bin/", .{ root, containerd_src, root }),
    });
    shim_cp.step.dependOn(&shim_build.step);
    shim_step.dependOn(&shim_cp.step);

    // Build runc (Linux only)
    // Aligned with k3s: system gcc + system libseccomp-dev
    // Requires: apt install libseccomp-dev
    const runc_step = b.step("runc", "Build runc (Linux)");
    const runc_src = "build/src/github.com/opencontainers/runc";
    const runc_build = b.addSystemCommand(&.{"make"});
    runc_build.setCwd(b.path(runc_src));
    runc_build.addArgs(&.{ "EXTRA_LDFLAGS=-w -s", "BUILDTAGS=apparmor seccomp", "static" });
    runc_build.step.dependOn(&download_cmd.step);
    runc_build.step.dependOn(&cleanup_containerd.step);
    // Copy runc binary to project bin/ (aligned with k3s: cp -vf ./build/src/.../runc/runc ./bin/)
    const runc_cp = b.addSystemCommand(&.{ "cp", "-vf", b.fmt("{s}/{s}/runc", .{ root, runc_src }), b.fmt("{s}/bin/runc", .{root}) });
    runc_cp.step.dependOn(&runc_build.step);
    runc_cp.step.dependOn(&mkdir_bin.step);
    runc_step.dependOn(&runc_cp.step);

    // Build containerd-shim-runhcs-v1 and runhcs (Windows only)
    // Aligned with k3s: builds from hcsshim source, CGO_ENABLED=0, tags with netgo
    const hcsshim_step = b.step("hcsshim", "Build containerd-shim-runhcs-v1 and runhcs (Windows)");
    const hcsshim_tags = "ctrd netgo osusergo providerless urfave_cli_no_docs static_build";
    const hcsshim_src = "build/src/github.com/microsoft/hcsshim";

    // Build containerd-shim-runhcs-v1
    const runhcs_shim_build = b.addSystemCommand(&.{ "go", "build" });
    runhcs_shim_build.setEnvironmentVariable("CGO_ENABLED", "0");
    runhcs_shim_build.setEnvironmentVariable("GOPATH", b.fmt("{s}/build", .{root}));
    runhcs_shim_build.setCwd(b.path(hcsshim_src));
    runhcs_shim_build.addArgs(&.{
        "-tags",                           hcsshim_tags,
        "-ldflags",                        ldflags,
        "-o",                              "bin/containerd-shim-runhcs-v1",
        "./cmd/containerd-shim-runhcs-v1",
    });
    runhcs_shim_build.step.dependOn(&download_cmd.step);
    runhcs_shim_build.step.dependOn(&mkdir_bin.step);
    runhcs_shim_build.step.dependOn(&cleanup_containerd.step);

    // Build runhcs
    const runhcs_build = b.addSystemCommand(&.{ "go", "build" });
    runhcs_build.setEnvironmentVariable("CGO_ENABLED", "0");
    runhcs_build.setEnvironmentVariable("GOPATH", b.fmt("{s}/build", .{root}));
    runhcs_build.setCwd(b.path(hcsshim_src));
    runhcs_build.addArgs(&.{
        "-tags",        hcsshim_tags,
        "-ldflags",     ldflags,
        "-o",           "bin/runhcs",
        "./cmd/runhcs",
    });
    runhcs_build.step.dependOn(&download_cmd.step);
    runhcs_build.step.dependOn(&mkdir_bin.step);
    runhcs_build.step.dependOn(&cleanup_containerd.step);

    // Copy hcsshim build output to project bin/
    const hcsshim_cp = b.addSystemCommand(&.{
        bash,                                                                 "-c",
        b.fmt("cp -vf {s}/{s}/bin/* {s}/bin/", .{ root, hcsshim_src, root }),
    });
    hcsshim_cp.step.dependOn(&runhcs_shim_build.step);
    hcsshim_cp.step.dependOn(&runhcs_build.step);
    hcsshim_step.dependOn(&hcsshim_cp.step);

    // Build CNI plugins
    const cni_step = b.step("cni", "Build CNI plugins");
    const cni_version = vi.version_cniplugins;
    const cni_clone_abs = b.fmt("{s}/.cni-build", .{root});
    const cni_workdir_abs = b.fmt("{s}/src/github.com/containernetworking/plugins", .{cni_clone_abs});
    const cni_clone = b.addSystemCommand(&.{
        bash,                                                                                                                                                                                                    "-c",
        b.fmt("rm -rf {s} && mkdir -p {s} && git clone --single-branch --depth=1 --branch={s} https://github.com/rancher/plugins.git {s}", .{ cni_clone_abs, cni_clone_abs, cni_version, cni_workdir_abs }),
    });
    const cni_build = b.addSystemCommand(&.{ "go", "build" });
    cni_build.setEnvironmentVariable("GO111MODULE", "off");
    cni_build.setEnvironmentVariable("GOPATH", cni_clone_abs);
    cni_build.setEnvironmentVariable("CGO_ENABLED", "0");
    const cni_ldflags = b.fmt("-w -s -extldflags '-static' -X " ++ PKG_CNI_PLUGINS ++ "/pkg/utils/buildversion.BuildVersion={s}", .{vi.version_cniplugins});
    cni_build.addArgs(&.{
        "-tags",    tags,
        "-ldflags", cni_ldflags,
        "-o",       b.fmt("{s}/bin/cni", .{root}),
        ".",
    });
    cni_build.setCwd(b.path(".cni-build/src/github.com/containernetworking/plugins"));
    cni_build.step.dependOn(&cni_clone.step);
    cni_build.step.dependOn(&mkdir_bin.step);
    cni_step.dependOn(&cni_build.step);

    all_step.dependOn(k8e_step);
    all_step.dependOn(shim_step);
    all_step.dependOn(runc_step);
    all_step.dependOn(cni_step);
    // hcsshim is Windows-only; only include on Windows hosts
    if (builtin.os.tag == .windows) {
        all_step.dependOn(hcsshim_step);
    }
}

fn getVersionEnv(b: *std.Build, bash: []const u8) !std.StringHashMap([]const u8) {
    var map = std.StringHashMap([]const u8).init(b.allocator);
    const res = b.run(&.{ bash, "-c", "source hack/version.sh && env | grep -E '^(VERSION|COMMIT|TREE_STATE|VERSION_GOLANG|VERSION_CRICTL|VERSION_CONTAINERD|PKG_CONTAINERD_K8E|VERSION_CNIPLUGINS|VERSION_CRI_DOCKERD|VERSION_RUNC|VERSION_HCSSHIM)='" });
    var it = std.mem.tokenizeAny(u8, res, "\n");
    while (it.next()) |line| {
        var parts = std.mem.splitScalar(u8, line, '=');
        const key = parts.next() orelse continue;
        const value = parts.next() orelse "";
        try map.put(b.dupe(key), b.dupe(value));
    }
    return map;
}

const VersionInfo = struct {
    version: []const u8,
    commit: []const u8,
    tree_state: []const u8,
    build_date: []const u8,
    version_golang: []const u8,
    version_crictl: []const u8,
    version_containerd: []const u8,
    pkg_containerd: []const u8,
    version_cniplugins: []const u8,
    version_cri_dockerd: []const u8,
    version_hcsshim: []const u8,
};

fn xflag(allocator: std.mem.Allocator, comptime key: []const u8, value: []const u8) ![]const u8 {
    return std.fmt.allocPrint(allocator, "-X " ++ key ++ "={s}", .{value});
}

fn buildVersionFlags(allocator: std.mem.Allocator, v: VersionInfo) ![]const u8 {
    const commit_short = if (v.commit.len >= 8) v.commit[0..8] else v.commit;

    const parts = [_][]const u8{
        "-w -s -extldflags '-static -lm -ldl -lz -lpthread'",
        // k8e version
        try xflag(allocator, PKG ++ "/pkg/version.Version", v.version),
        try xflag(allocator, PKG ++ "/pkg/version.GitCommit", commit_short),
        try xflag(allocator, PKG ++ "/pkg/version.UpstreamGolang", v.version_golang),
        // k8s client-go
        try xflag(allocator, PKG_K8S_CLIENT ++ "/version.gitVersion", v.version),
        try xflag(allocator, PKG_K8S_CLIENT ++ "/version.gitCommit", v.commit),
        try xflag(allocator, PKG_K8S_CLIENT ++ "/version.gitTreeState", v.tree_state),
        try xflag(allocator, PKG_K8S_CLIENT ++ "/version.buildDate", v.build_date),
        // k8s component-base
        try xflag(allocator, PKG_K8S_BASE ++ "/version.gitVersion", v.version),
        try xflag(allocator, PKG_K8S_BASE ++ "/version.gitCommit", v.commit),
        try xflag(allocator, PKG_K8S_BASE ++ "/version.gitTreeState", v.tree_state),
        try xflag(allocator, PKG_K8S_BASE ++ "/version.buildDate", v.build_date),
        // cri-tools
        try xflag(allocator, PKG_CRICTL ++ "/version.Version", v.version_crictl),
        // containerd
        try xflag(allocator, PKG_CONTAINERD ++ "/version.Version", v.version_containerd),
        try xflag(allocator, PKG_CONTAINERD ++ "/version.Package", v.pkg_containerd),
        // cni plugins
        try xflag(allocator, PKG_CNI_PLUGINS ++ "/pkg/utils/buildversion.BuildVersion", v.version_cniplugins),
        // cri-dockerd
        try xflag(allocator, PKG_CRI_DOCKERD ++ "/cmd/version.Version", v.version_cri_dockerd),
        try xflag(allocator, PKG_CRI_DOCKERD ++ "/cmd/version.GitCommit", "HEAD"),
        try xflag(allocator, PKG_CRI_DOCKERD ++ "/cmd/version.BuildTime", v.build_date),
        // etcd
        try xflag(allocator, PKG_ETCD ++ "/api/v3/version.GitSHA", "HEAD"),
    };
    return std.mem.join(allocator, " ", &parts);
}

fn findBash() []const u8 {
    if (builtin.os.tag == .windows) {
        // On Windows, bash from Git for Windows may not be in the system PATH.
        // Check common installation locations.
        const candidates = [_][]const u8{
            "C:/Program Files/Git/bin/bash.exe",
            "C:/Program Files (x86)/Git/bin/bash.exe",
        };
        for (candidates) |path| {
            const file = std.fs.openFileAbsolute(path, .{}) catch continue;
            file.close();
            return path;
        }
    }
    return "bash";
}
