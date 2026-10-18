const std = @import("std");
const builtin = @import("builtin");
const manifest = @import("build.zig.zon");

pub const protobuf_codegen = @import("tools/protobuf_codegen.zig");

pub fn createProtocStep(
    dependency: *std.Build.Dependency,
    target: std.Build.ResolvedTarget,
    optimize: std.builtin.OptimizeMode,
    options: protobuf_codegen.RunProtocStep.Options,
) ?*protobuf_codegen.RunProtocStep {
    const protobuf_dependency = dependency.builder.lazyDependency("protobuf", .{
        .target = target,
        .optimize = optimize,
    }) orelse return null;
    return protobuf_codegen.RunProtocStep.create(
        protobuf_dependency.builder,
        target,
        options,
    );
}

pub fn build(b: *std.Build) void {
    const requested_target = b.standardTargetOptions(.{});
    const transpile_c = b.option(bool, "transpile-c", "Emit the grpc-lite implementation as C source") orelse false;
    const target = if (transpile_c) blk: {
        var query = std.Target.Query.fromTarget(&requested_target.result);
        query.ofmt = .c;
        break :blk b.resolveTargetQuery(query);
    } else requested_target;
    // Published C output must use one explicit target for both the runtime and
    // the protoc plugin rather than inheriting properties from the runner.
    const host_target = if (transpile_c) target else b.graph.host;
    const optimize = b.standardOptimizeOption(.{});
    const sanitizers: Sanitizers = .{
        .thread = b.option(bool, "sanitize-thread", "Enable ThreadSanitizer"),
        .c = if (b.option(bool, "sanitize-c", "Enable full C undefined behavior detection")) |enabled|
            if (enabled) .full else .off
        else
            null,
    };
    const libxev_dependency = b.dependency("libxev", .{
        .target = target,
        .optimize = optimize,
    });
    const nghttp2_dependency = b.dependency("nghttp2", .{});
    const cares_dependency = b.dependency("cares", .{});
    const cpucycles_dependency = b.dependency("cpucycles", .{});
    const nanozlog_dependency = b.dependency("nanozlog", .{
        .target = target,
        .optimize = optimize,
    });
    const enable_tls = b.option(bool, "tls", "Enable TLS transport support through mbedTLS") orelse false;
    const enable_protobuf = b.option(
        bool,
        "protobuf",
        "Enable protobuf runtimes, typed adapters, code generation, examples, and tests",
    ) orelse (b.pkg_hash.len == 0);
    const protobuf_dependency = if (enable_protobuf)
        b.lazyDependency("protobuf", .{
            .target = target,
            .optimize = optimize,
        }) orelse return
    else
        null;
    const host_protobuf_dependency = if (enable_protobuf)
        b.lazyDependency("protobuf", .{
            .target = host_target,
            .optimize = optimize,
        }) orelse return
    else
        null;
    const enable_gperftools = b.option(
        bool,
        "gperftools",
        "Expose gperftools profiling APIs (enables tcmalloc by default)",
    ) orelse false;
    const enable_tcmalloc = b.option(
        bool,
        "tcmalloc",
        "Replace the process C allocator with tcmalloc (defaults to gperftools)",
    ) orelse enable_gperftools;
    const grpc_proto_dependency = if (enable_protobuf)
        b.lazyDependency("grpc_proto", .{}) orelse return
    else
        null;
    if (target.result.os.tag != .linux) {
        @panic("DNS support is currently limited to Linux");
    }
    if ((enable_gperftools or enable_tcmalloc) and target.result.os.tag != .linux) {
        @panic("gperftools support is currently limited to Linux");
    }
    if (enable_tcmalloc and sanitizers.thread == true) {
        @panic("tcmalloc is incompatible with ThreadSanitizer");
    }
    const grpc_lite_options = b.addOptions();
    grpc_lite_options.addOption([]const u8, "version", manifest.version);
    grpc_lite_options.addOption(bool, "tls", enable_tls);
    grpc_lite_options.addOption(bool, "sanitize_thread", sanitizers.thread == true);
    const grpc_lite = b.addModule("grpc_lite", .{
        .root_source_file = b.path("src/root.zig"),
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    applySanitizers(grpc_lite, sanitizers);
    const grpc_lite_gperftools = if (enable_gperftools or enable_tcmalloc) blk: {
        const options = b.addOptions();
        options.addOption(bool, "cpu_profiler", enable_gperftools);
        options.addOption(bool, "tcmalloc", enable_tcmalloc);
        break :blk b.addModule("grpc_lite_gperftools", .{
            .root_source_file = b.path("src/gperftools.zig"),
            .target = target,
            .optimize = optimize,
            .imports = &.{
                .{ .name = "grpc_lite", .module = grpc_lite },
                .{ .name = "grpc_lite_gperftools_options", .module = options.createModule() },
            },
        });
    } else null;
    const gperftools_dependency = if (enable_gperftools or enable_tcmalloc)
        b.lazyDependency("gperftools", .{}) orelse return
    else
        null;
    const mbedtls_dependency = if (enable_tls)
        b.lazyDependency("mbedtls", .{}) orelse return
    else
        null;
    const native = addNativeDependencies(
        b,
        nghttp2_dependency.path(""),
        cares_dependency.path(""),
        if (mbedtls_dependency) |dependency| dependency.path("") else null,
        if (gperftools_dependency) |dependency| dependency.path("") else null,
        target,
        optimize,
        sanitizers,
        enable_gperftools,
        enable_tcmalloc,
    );

    const xev = libxev_dependency.module("xev");
    const protobuf = if (protobuf_dependency) |dependency| dependency.module("protobuf") else null;
    applySanitizers(xev, sanitizers);
    if (protobuf) |module| {
        applySanitizers(module, sanitizers);
        b.modules.put(
            b.graph.arena,
            b.dupe("grpc_lite_protobuf_runtime"),
            module,
        ) catch @panic("OOM");
    }
    grpc_lite.addOptions("grpc_lite_options", grpc_lite_options);
    grpc_lite.addIncludePath(nghttp2_dependency.path("lib/includes"));
    grpc_lite.addIncludePath(native.nghttp2_include);
    grpc_lite.addObjectFile(native.nghttp2_archive);
    grpc_lite.addIncludePath(cares_dependency.path("include"));
    grpc_lite.addIncludePath(native.cares_include);
    grpc_lite.addObjectFile(native.cares_archive);
    addCpuCycles(b, grpc_lite, cpucycles_dependency, target);
    if (mbedtls_dependency) |dependency| {
        grpc_lite.addIncludePath(dependency.path("include"));
        grpc_lite.addIncludePath(b.path("tools"));
        grpc_lite.addObjectFile(native.mbedtls_archive.?);
    }
    grpc_lite.addImport("xev", xev);
    grpc_lite.addImport("nanozlog", nanozlog_dependency.module("nanozlog"));
    const test_step = b.step("test", "Run unit tests");
    const cpp_codegen = if (host_protobuf_dependency) |dependency|
        addCppCodegenSupport(b, dependency, test_step, host_target, optimize, transpile_c)
    else
        null;

    if (gperftools_dependency) |dependency| {
        grpc_lite.addObjectFile(native.gperftools_archive.?);
        grpc_lite.addObjectFile(native.gperftools_force_link.?);
        grpc_lite.link_libcpp = true;
        grpc_lite.omit_frame_pointer = false;
        xev.omit_frame_pointer = false;

        grpc_lite_gperftools.?.addIncludePath(dependency.path("src"));
        applySanitizers(grpc_lite_gperftools.?, sanitizers);
        grpc_lite_gperftools.?.omit_frame_pointer = false;

        const gperftools_test_module = b.createModule(.{
            .root_source_file = b.path("tests/gperftools_test.zig"),
            .target = target,
            .optimize = optimize,
            .imports = &.{.{ .name = "grpc_lite_gperftools", .module = grpc_lite_gperftools.? }},
        });
        applySanitizers(gperftools_test_module, sanitizers);
        gperftools_test_module.omit_frame_pointer = false;
        const gperftools_tests = b.addTest(.{
            .name = "gperftools-integration",
            .root_module = gperftools_test_module,
        });
        test_step.dependOn(&b.addRunArtifact(gperftools_tests).step);

        if (enable_gperftools) {
            const gperftools_env_module = b.createModule(.{
                .root_source_file = b.path("tests/gperftools_env_test.zig"),
                .target = target,
                .optimize = optimize,
                .imports = &.{.{ .name = "grpc_lite", .module = grpc_lite }},
            });
            applySanitizers(gperftools_env_module, sanitizers);
            gperftools_env_module.omit_frame_pointer = false;
            const gperftools_env_test = b.addExecutable(.{
                .name = "gperftools-env-test",
                .root_module = gperftools_env_module,
            });
            const run_gperftools_env_test = b.addSystemCommand(&.{"bash"});
            run_gperftools_env_test.addFileArg(b.path("tests/run_gperftools_env_test.sh"));
            run_gperftools_env_test.addArtifactArg(gperftools_env_test);
            _ = run_gperftools_env_test.addOutputDirectoryArg("profiles");
            run_gperftools_env_test.addArg(if (enable_tcmalloc) "true" else "false");
            test_step.dependOn(&run_gperftools_env_test.step);
        }
    }

    if (target.result.os.tag == .linux) {
        grpc_lite.linkSystemLibrary("pthread", .{});
        grpc_lite.linkSystemLibrary("dl", .{});
        grpc_lite.linkSystemLibrary("rt", .{});
    }

    const library = b.addLibrary(.{
        .name = "grpc_lite",
        .root_module = grpc_lite,
    });
    if (transpile_c) {
        const install_c_source = b.addInstallFileWithDir(
            library.getEmittedBin(),
            .{ .custom = "src" },
            "grpc_lite.c",
        );
        const install_plugin_source = if (cpp_codegen) |support|
            b.addInstallFileWithDir(
                support.plugin.getEmittedBin(),
                .{ .custom = "src" },
                "protoc-gen-grpc_lite_cpp.c",
            )
        else
            null;
        const zig_header_path = b.pathJoin(&.{ b.graph.zig_lib_directory.path.?, "zig.h" });
        const install_zig_header = b.addInstallFileWithDir(
            .{ .cwd_relative = zig_header_path },
            .header,
            "zig.h",
        );
        const transpile_c_step = b.step("transpile-c", "Emit the grpc-lite implementation as C source");
        transpile_c_step.dependOn(&install_c_source.step);
        if (install_plugin_source) |install| transpile_c_step.dependOn(&install.step);
        transpile_c_step.dependOn(&install_zig_header.step);
        return;
    }
    const shared_library = b.addLibrary(.{
        .name = "grpc_lite",
        .linkage = .dynamic,
        .root_module = grpc_lite,
        .version = .{ .major = 1, .minor = 0, .patch = 0 },
    });
    const package_static_library = b.addSystemCommand(&.{"bash"});
    package_static_library.addFileArg(b.path("tools/package_static_library.sh"));
    package_static_library.addFileArg(library.getEmittedBin());
    const packaged_static_library = package_static_library.addOutputFileArg("libgrpc_lite.a");
    package_static_library.addArg(b.graph.zig_exe);
    const install_library = b.addInstallFileWithDir(
        packaged_static_library,
        .lib,
        "libgrpc_lite.a",
    );
    const install_shared_library = b.addInstallArtifact(shared_library, .{});
    const install_c_header = b.addInstallHeaderFile(
        b.path("include/grpc_lite/grpc_lite.h"),
        "grpc_lite/grpc_lite.h",
    );
    b.getInstallStep().dependOn(&install_library.step);
    b.getInstallStep().dependOn(&install_shared_library.step);
    b.getInstallStep().dependOn(&install_c_header.step);

    const install_c_sdk = b.step(
        "install-c-sdk",
        "Install the C header, static library, and shared library",
    );
    install_c_sdk.dependOn(&install_library.step);
    install_c_sdk.dependOn(&install_shared_library.step);
    install_c_sdk.dependOn(&install_c_header.step);
    b.getInstallStep().dependOn(&b.addInstallDirectory(.{
        .source_dir = b.path("include/grpcpp"),
        .install_dir = .header,
        .install_subdir = "grpcpp",
    }).step);
    b.getInstallStep().dependOn(&b.addInstallHeaderFile(
        b.path("include/grpc_lite/grpc_lite.hpp"),
        "grpc_lite/grpc_lite.hpp",
    ).step);
    b.getInstallStep().dependOn(&b.addInstallDirectory(.{
        .source_dir = b.path("include/grpc_lite/cpp"),
        .install_dir = .header,
        .install_subdir = "grpc_lite/cpp",
    }).step);
    b.getInstallStep().dependOn(&b.addInstallFileWithDir(
        b.path("cmake/grpc_liteConfig.cmake"),
        .lib,
        "cmake/grpc_lite/grpc_liteConfig.cmake",
    ).step);
    b.getInstallStep().dependOn(&b.addInstallFileWithDir(
        b.path("cmake/grpc_liteConfigVersion.cmake"),
        .lib,
        "cmake/grpc_lite/grpc_liteConfigVersion.cmake",
    ).step);

    const unit_tests = b.addTest(.{
        .root_module = grpc_lite,
    });
    const run_unit_tests = b.addRunArtifact(unit_tests);
    const coverage_tests = b.addTest(.{
        .root_module = grpc_lite,
        .use_llvm = true,
    });
    const install_coverage_tests = b.addInstallFileWithDir(
        coverage_tests.getEmittedBin(),
        .{ .custom = "coverage" },
        "grpc-lite-tests",
    );
    const coverage_bin_step = b.step("coverage-bin", "Build the core test binary for coverage");
    coverage_bin_step.dependOn(&install_coverage_tests.step);

    const public_api_module = b.createModule(.{
        .root_source_file = b.path("tests/public_api_test.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "grpc_lite", .module = grpc_lite }},
    });
    applySanitizers(public_api_module, sanitizers);
    const public_api_tests = b.addTest(.{
        .name = "public-api",
        .root_module = public_api_module,
    });
    const run_public_api_tests = b.addRunArtifact(public_api_tests);

    const c_api_smoke_module = b.createModule(.{
        .target = target,
        .optimize = optimize,
        .link_libc = true,
    });
    applySanitizers(c_api_smoke_module, sanitizers);
    c_api_smoke_module.addIncludePath(b.path("include"));
    c_api_smoke_module.addCSourceFile(.{
        .file = b.path("tests/c_api_smoke.c"),
        .flags = &.{ "-std=c11", "-Wall", "-Wextra", "-Werror" },
    });
    linkCApiTestLibrary(c_api_smoke_module, packaged_static_library, shared_library, sanitizers);
    const c_api_smoke = b.addExecutable(.{
        .name = "c-api-smoke",
        .root_module = c_api_smoke_module,
    });
    const run_c_api_smoke = b.addRunArtifact(c_api_smoke);

    const cpp_c_api_smoke_module = b.createModule(.{
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .link_libcpp = true,
    });
    applySanitizers(cpp_c_api_smoke_module, sanitizers);
    cpp_c_api_smoke_module.addIncludePath(b.path("include"));
    cpp_c_api_smoke_module.addCSourceFile(.{
        .file = b.path("tests/cpp_c_api_smoke.cc"),
        .flags = &.{ "-std=c++17", "-Wall", "-Wextra", "-Werror" },
    });
    linkCApiTestLibrary(cpp_c_api_smoke_module, packaged_static_library, shared_library, sanitizers);
    const cpp_c_api_smoke = b.addExecutable(.{
        .name = "cpp-c-api-smoke",
        .root_module = cpp_c_api_smoke_module,
    });
    const run_cpp_c_api_smoke = b.addRunArtifact(cpp_c_api_smoke);

    const cpp_native_api_test_module = b.createModule(.{
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .link_libcpp = true,
    });
    applySanitizers(cpp_native_api_test_module, sanitizers);
    cpp_native_api_test_module.addIncludePath(b.path("include"));
    cpp_native_api_test_module.addCSourceFile(.{
        .file = b.path("tests/cpp_native_api_test.cc"),
        .flags = &.{ "-std=c++17", "-Wall", "-Wextra", "-Werror" },
    });
    linkCApiTestLibrary(cpp_native_api_test_module, packaged_static_library, shared_library, sanitizers);
    const cpp_native_api_test = b.addExecutable(.{
        .name = "cpp-native-api-test",
        .root_module = cpp_native_api_test_module,
    });
    const run_cpp_native_api_test = b.addRunArtifact(cpp_native_api_test);

    const grpcpp_facade_test_module = b.createModule(.{
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .link_libcpp = true,
    });
    applySanitizers(grpcpp_facade_test_module, sanitizers);
    grpcpp_facade_test_module.addIncludePath(b.path("include"));
    grpcpp_facade_test_module.addCSourceFile(.{
        .file = b.path("tests/grpcpp_facade_test.cc"),
        .flags = &.{ "-std=c++17", "-Wall", "-Wextra", "-Werror" },
    });
    linkCApiTestLibrary(grpcpp_facade_test_module, packaged_static_library, shared_library, sanitizers);
    const grpcpp_facade_test = b.addExecutable(.{
        .name = "grpcpp-facade-test",
        .root_module = grpcpp_facade_test_module,
    });
    const run_grpcpp_facade_test = b.addRunArtifact(grpcpp_facade_test);

    const grpcpp_streaming_facade_test_module = b.createModule(.{
        .target = target,
        .optimize = optimize,
        .link_libc = true,
        .link_libcpp = true,
    });
    applySanitizers(grpcpp_streaming_facade_test_module, sanitizers);
    grpcpp_streaming_facade_test_module.addIncludePath(b.path("include"));
    grpcpp_streaming_facade_test_module.addCSourceFile(.{
        .file = b.path("tests/grpcpp_streaming_facade_test.cc"),
        .flags = &.{ "-std=c++17", "-fno-exceptions", "-Wall", "-Wextra", "-Werror" },
    });
    linkCApiTestLibrary(grpcpp_streaming_facade_test_module, packaged_static_library, shared_library, sanitizers);
    const grpcpp_streaming_facade_test = b.addExecutable(.{
        .name = "grpcpp-streaming-facade-test",
        .root_module = grpcpp_streaming_facade_test_module,
    });
    const run_grpcpp_streaming_facade_test = b.addRunArtifact(grpcpp_streaming_facade_test);

    if (cpp_codegen) |support| {
        const grpcpp_generated_test_module = b.createModule(.{
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .link_libcpp = true,
        });
        applySanitizers(grpcpp_generated_test_module, sanitizers);
        grpcpp_generated_test_module.addIncludePath(b.path("include"));
        grpcpp_generated_test_module.addIncludePath(b.path("tests/codegen"));
        grpcpp_generated_test_module.addIncludePath(support.generated);
        grpcpp_generated_test_module.addCSourceFile(.{
            .file = support.generated.path(b, "cpp_codegen.grpc.pb.cc"),
            .flags = &.{ "-std=c++17", "-fno-exceptions", "-Wall", "-Wextra", "-Werror" },
        });
        grpcpp_generated_test_module.addCSourceFile(.{
            .file = b.path("tests/grpcpp_generated_test.cc"),
            .flags = &.{ "-std=c++17", "-fno-exceptions", "-Wall", "-Wextra", "-Werror" },
        });
        linkCApiTestLibrary(grpcpp_generated_test_module, packaged_static_library, shared_library, sanitizers);
        const grpcpp_generated_test = b.addExecutable(.{
            .name = "grpcpp-generated-test",
            .root_module = grpcpp_generated_test_module,
        });
        test_step.dependOn(&b.addRunArtifact(grpcpp_generated_test).step);

        const grpcpp_generated_server_test_module = b.createModule(.{
            .target = target,
            .optimize = optimize,
            .link_libc = true,
            .link_libcpp = true,
        });
        applySanitizers(grpcpp_generated_server_test_module, sanitizers);
        grpcpp_generated_server_test_module.addIncludePath(b.path("include"));
        grpcpp_generated_server_test_module.addIncludePath(b.path("tests/codegen"));
        grpcpp_generated_server_test_module.addIncludePath(support.generated);
        grpcpp_generated_server_test_module.addCSourceFile(.{
            .file = support.generated.path(b, "cpp_codegen.grpc.pb.cc"),
            .flags = &.{ "-std=c++17", "-fno-exceptions", "-Wall", "-Wextra", "-Werror" },
        });
        grpcpp_generated_server_test_module.addCSourceFile(.{
            .file = b.path("tests/grpcpp_generated_server_test.cc"),
            .flags = &.{ "-std=c++17", "-fno-exceptions", "-Wall", "-Wextra", "-Werror" },
        });
        linkCApiTestLibrary(grpcpp_generated_server_test_module, packaged_static_library, shared_library, sanitizers);
        const grpcpp_generated_server_test = b.addExecutable(.{
            .name = "grpcpp-generated-server-test",
            .root_module = grpcpp_generated_server_test_module,
        });
        test_step.dependOn(&b.addRunArtifact(grpcpp_generated_server_test).step);
    }

    const cpp_e2e_server = addExample(
        b,
        "grpc-lite-cpp-e2e-server",
        "tests/cpp_e2e_server.zig",
        grpc_lite,
    );
    const install_cpp_e2e_server = b.addInstallArtifact(cpp_e2e_server, .{});
    const cpp_e2e_server_step = b.step(
        "install-cpp-e2e-server",
        "Install the C++ package E2E fixture server",
    );
    cpp_e2e_server_step.dependOn(&install_cpp_e2e_server.step);

    test_step.dependOn(&run_unit_tests.step);
    test_step.dependOn(&run_public_api_tests.step);
    test_step.dependOn(&run_c_api_smoke.step);
    test_step.dependOn(&run_cpp_c_api_smoke.step);
    test_step.dependOn(&run_cpp_native_api_test.step);
    test_step.dependOn(&run_grpcpp_facade_test.step);
    test_step.dependOn(&run_grpcpp_streaming_facade_test.step);

    if (!enable_protobuf) return;
    addProtobufSupport(
        b,
        protobuf_codegen,
        protobuf_dependency.?,
        grpc_proto_dependency.?,
        protobuf.?,
        grpc_lite,
        test_step,
        target,
        optimize,
        sanitizers,
    );
}

const CppCodegenSupport = struct {
    plugin: *std.Build.Step.Compile,
    generated: std.Build.LazyPath,
};

fn addCppCodegenSupport(
    b: *std.Build,
    host_protobuf_dependency: *std.Build.Dependency,
    test_step: *std.Build.Step,
    host_target: std.Build.ResolvedTarget,
    optimize: std.builtin.OptimizeMode,
    transpile_c: bool,
) CppCodegenSupport {
    const host_protobuf = host_protobuf_dependency.module("protobuf");
    const plugin_schema_files = b.addWriteFiles();
    const plugin_schema_root = plugin_schema_files.add("root.zig",
        \\const compiler = @import("google/protobuf/compiler.pb.zig");
        \\pub const CodeGeneratorRequest = compiler.CodeGeneratorRequest;
        \\pub const CodeGeneratorResponse = compiler.CodeGeneratorResponse;
    );
    _ = plugin_schema_files.addCopyFile(
        host_protobuf_dependency.path("bootstrapped-generator/google/protobuf.pb.zig"),
        "google/protobuf.pb.zig",
    );
    _ = plugin_schema_files.addCopyFile(
        host_protobuf_dependency.path("bootstrapped-generator/google/protobuf/compiler.pb.zig"),
        "google/protobuf/compiler.pb.zig",
    );
    const protobuf_plugin = b.createModule(.{
        .root_source_file = plugin_schema_root,
        .target = host_target,
        .optimize = optimize,
        .imports = &.{.{ .name = "protobuf", .module = host_protobuf }},
    });
    const grpc_lite_cpp_plugin_module = b.createModule(.{
        .root_source_file = b.path("tools/protoc-gen-grpc-lite-cpp/main.zig"),
        .target = host_target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "protobuf", .module = host_protobuf },
            .{ .name = "protobuf_plugin", .module = protobuf_plugin },
        },
    });
    const grpc_lite_cpp_plugin = b.addExecutable(.{
        .name = "protoc-gen-grpc_lite_cpp",
        .root_module = grpc_lite_cpp_plugin_module,
    });
    if (!transpile_c) b.installArtifact(grpc_lite_cpp_plugin);

    const grpc_lite_cpp_plugin_test_module = b.createModule(.{
        .root_source_file = b.path("tools/protoc-gen-grpc-lite-cpp/generator.zig"),
        .target = b.graph.host,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "protobuf", .module = host_protobuf },
            .{ .name = "protobuf_plugin", .module = protobuf_plugin },
        },
    });
    const grpc_lite_cpp_plugin_tests = b.addTest(.{
        .name = "grpc-lite-cpp-plugin",
        .root_module = grpc_lite_cpp_plugin_test_module,
    });
    test_step.dependOn(&b.addRunArtifact(grpc_lite_cpp_plugin_tests).step);

    const protoc_dependency = host_protobuf_dependency.builder.lazyDependency(
        protocDependencyName(),
        .{},
    ) orelse @panic("protoc dependency is unavailable");
    const generate_grpcpp = std.Build.Step.Run.create(b, "run protoc for C++ service glue");
    generate_grpcpp.addFileArg(protoc_dependency.path(if (builtin.os.tag == .windows)
        "bin/protoc.exe"
    else
        "bin/protoc"));
    generate_grpcpp.addPrefixedArtifactArg(
        "--plugin=protoc-gen-grpc_lite_cpp=",
        grpc_lite_cpp_plugin,
    );
    const generated_grpcpp = generate_grpcpp.addPrefixedOutputDirectoryArg(
        "--grpc_lite_cpp_out=",
        "generated-grpcpp",
    );
    generate_grpcpp.addPrefixedDirectoryArg("-I", b.path("proto"));
    generate_grpcpp.addFileArg(b.path("proto/cpp_codegen.proto"));

    const gen_grpcpp_step = b.step("gen-grpcpp", "Generate synchronous C++ service glue");
    gen_grpcpp_step.dependOn(&generate_grpcpp.step);
    return .{ .plugin = grpc_lite_cpp_plugin, .generated = generated_grpcpp };
}

const Sanitizers = struct {
    thread: ?bool,
    c: ?std.zig.SanitizeC,

    fn enabled(self: Sanitizers) bool {
        return self.thread == true or self.c == .full;
    }
};

fn applySanitizers(module: *std.Build.Module, sanitizers: Sanitizers) void {
    module.sanitize_thread = sanitizers.thread;
    module.sanitize_c = sanitizers.c;
    if (sanitizers.enabled()) module.omit_frame_pointer = false;
}

fn addProtobufSupport(
    b: *std.Build,
    comptime protobuf_build: type,
    protobuf_dependency: *std.Build.Dependency,
    grpc_proto_dependency: *std.Build.Dependency,
    protobuf: *std.Build.Module,
    grpc_lite: *std.Build.Module,
    test_step: *std.Build.Step,
    target: std.Build.ResolvedTarget,
    optimize: std.builtin.OptimizeMode,
    sanitizers: Sanitizers,
) void {
    const generate_proto = protobuf_build.RunProtocStep.create(
        protobuf_dependency.builder,
        target,
        .{
            .destination_directory = b.path(".zig-cache/generated"),
            .source_files = &.{b.path("proto/echo.proto")},
            .include_directories = &.{b.path("proto")},
        },
    );
    const generate_proto_step = b.step("gen-proto", "Generate Zig protobuf sources");
    generate_proto_step.dependOn(&generate_proto.step);

    const generate_interop_proto = protobuf_build.RunProtocStep.create(
        protobuf_dependency.builder,
        target,
        .{
            .destination_directory = b.path(".zig-cache/generated-interop"),
            .source_files = &.{
                grpc_proto_dependency.path("grpc/testing/empty.proto"),
                grpc_proto_dependency.path("grpc/testing/messages.proto"),
                grpc_proto_dependency.path("grpc/testing/test.proto"),
            },
            .include_directories = &.{grpc_proto_dependency.path("")},
        },
    );
    const generate_interop_proto_step = b.step(
        "gen-interop-proto",
        "Generate official gRPC interop protobuf sources",
    );
    generate_interop_proto_step.dependOn(&generate_interop_proto.step);

    const install_cpp_interop_protos = b.step(
        "install-cpp-interop-protos",
        "Install official protobuf sources for C++ interop tests",
    );
    inline for (.{ "empty.proto", "messages.proto", "test.proto" }) |name| {
        const install_proto = b.addInstallFileWithDir(
            grpc_proto_dependency.path("grpc/testing/" ++ name),
            .{ .custom = "share/grpc-lite/interop-proto/grpc/testing" },
            name,
        );
        install_cpp_interop_protos.dependOn(&install_proto.step);
    }

    const demo_proto = b.createModule(.{
        .root_source_file = b.path(".zig-cache/generated/demo.pb.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "protobuf", .module = protobuf }},
    });
    applySanitizers(demo_proto, sanitizers);
    const interop_proto = b.createModule(.{
        .root_source_file = b.path(".zig-cache/generated-interop/grpc/testing.pb.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "protobuf", .module = protobuf }},
    });
    applySanitizers(interop_proto, sanitizers);
    const grpc_lite_protobuf = b.addModule("grpc_lite_protobuf", .{
        .root_source_file = b.path("src/protobuf_adapter.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "grpc_lite", .module = grpc_lite },
            .{ .name = "protobuf", .module = protobuf },
        },
    });
    applySanitizers(grpc_lite_protobuf, sanitizers);

    const protobuf_test_module = b.createModule(.{
        .root_source_file = b.path("tests/protobuf_codegen_test.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "demo_proto", .module = demo_proto }},
    });
    applySanitizers(protobuf_test_module, sanitizers);
    const protobuf_tests = b.addTest(.{
        .name = "protobuf-integration",
        .root_module = protobuf_test_module,
    });
    protobuf_tests.step.dependOn(&generate_proto.step);
    const run_protobuf_tests = b.addRunArtifact(protobuf_tests);

    const protobuf_adapter_test_module = b.createModule(.{
        .root_source_file = b.path("tests/protobuf_adapter_test.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "grpc_lite", .module = grpc_lite },
            .{ .name = "grpc_lite_protobuf", .module = grpc_lite_protobuf },
            .{ .name = "demo_proto", .module = demo_proto },
        },
    });
    applySanitizers(protobuf_adapter_test_module, sanitizers);
    const protobuf_adapter_tests = b.addTest(.{
        .name = "protobuf-adapter",
        .root_module = protobuf_adapter_test_module,
    });
    protobuf_adapter_tests.step.dependOn(&generate_proto.step);
    const run_protobuf_adapter_tests = b.addRunArtifact(protobuf_adapter_tests);

    const official_proto_test_module = b.createModule(.{
        .root_source_file = b.path("tests/official/protobuf_test.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{.{ .name = "grpc_testing", .module = interop_proto }},
    });
    applySanitizers(official_proto_test_module, sanitizers);
    const official_proto_tests = b.addTest(.{
        .name = "official-protobuf",
        .root_module = official_proto_test_module,
    });
    official_proto_tests.step.dependOn(&generate_interop_proto.step);
    const run_official_proto_tests = b.addRunArtifact(official_proto_tests);

    const interop_client_test_module = b.createModule(.{
        .root_source_file = b.path("tests/official/interop_client.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "grpc_lite", .module = grpc_lite },
            .{ .name = "grpc_testing", .module = interop_proto },
        },
    });
    applySanitizers(interop_client_test_module, sanitizers);
    const interop_client_tests = b.addTest(.{
        .name = "interop-client-parser",
        .root_module = interop_client_test_module,
    });
    interop_client_tests.step.dependOn(&generate_interop_proto.step);
    const run_interop_client_tests = b.addRunArtifact(interop_client_tests);

    const benchmark_server_test_module = b.createModule(.{
        .root_source_file = b.path("benchmarks/server.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "grpc_lite", .module = grpc_lite },
            .{ .name = "grpc_lite_protobuf", .module = grpc_lite_protobuf },
            .{ .name = "demo_proto", .module = demo_proto },
        },
    });
    applySanitizers(benchmark_server_test_module, sanitizers);
    const benchmark_server_tests = b.addTest(.{
        .name = "benchmark-server-parser",
        .root_module = benchmark_server_test_module,
    });
    benchmark_server_tests.step.dependOn(&generate_proto.step);
    const run_benchmark_server_tests = b.addRunArtifact(benchmark_server_tests);

    const benchmark_client_test_module = b.createModule(.{
        .root_source_file = b.path("benchmarks/client.zig"),
        .target = target,
        .optimize = optimize,
        .imports = &.{
            .{ .name = "grpc_lite", .module = grpc_lite },
            .{ .name = "grpc_lite_protobuf", .module = grpc_lite_protobuf },
            .{ .name = "demo_proto", .module = demo_proto },
        },
    });
    applySanitizers(benchmark_client_test_module, sanitizers);
    const benchmark_client_tests = b.addTest(.{
        .name = "benchmark-client",
        .root_module = benchmark_client_test_module,
    });
    benchmark_client_tests.step.dependOn(&generate_proto.step);
    const run_benchmark_client_tests = b.addRunArtifact(benchmark_client_tests);

    test_step.dependOn(&run_protobuf_tests.step);
    test_step.dependOn(&run_protobuf_adapter_tests.step);
    test_step.dependOn(&run_official_proto_tests.step);
    test_step.dependOn(&run_interop_client_tests.step);
    test_step.dependOn(&run_benchmark_server_tests.step);
    test_step.dependOn(&run_benchmark_client_tests.step);

    const echo_server = addExample(b, "grpc-lite-echo-server", "examples/echo_server.zig", grpc_lite);
    echo_server.root_module.addImport("grpc_lite_protobuf", grpc_lite_protobuf);
    echo_server.root_module.addImport("demo_proto", demo_proto);
    echo_server.step.dependOn(&generate_proto.step);
    const echo_client = addExample(b, "grpc-lite-echo-client", "examples/echo_client.zig", grpc_lite);
    echo_client.root_module.addImport("grpc_lite_protobuf", grpc_lite_protobuf);
    echo_client.root_module.addImport("demo_proto", demo_proto);
    echo_client.step.dependOn(&generate_proto.step);
    b.installArtifact(echo_server);
    b.installArtifact(echo_client);

    const interop_server = addExample(
        b,
        "grpc-lite-interop-server",
        "tests/official/interop_server.zig",
        grpc_lite,
    );
    interop_server.root_module.addImport("grpc_testing", interop_proto);
    interop_server.step.dependOn(&generate_interop_proto.step);
    const interop_client = addExample(
        b,
        "grpc-lite-interop-client",
        "tests/official/interop_client.zig",
        grpc_lite,
    );
    interop_client.root_module.addImport("grpc_testing", interop_proto);
    interop_client.step.dependOn(&generate_interop_proto.step);
    b.installArtifact(interop_server);
    b.installArtifact(interop_client);

    const benchmark_server = addExample(
        b,
        "grpc-lite-benchmark-server",
        "benchmarks/server.zig",
        grpc_lite,
    );
    benchmark_server.root_module.addImport("grpc_lite_protobuf", grpc_lite_protobuf);
    benchmark_server.root_module.addImport("demo_proto", demo_proto);
    benchmark_server.step.dependOn(&generate_proto.step);
    const benchmark_client = addExample(
        b,
        "grpc-lite-benchmark-client",
        "benchmarks/client.zig",
        grpc_lite,
    );
    benchmark_client.root_module.addImport("grpc_lite_protobuf", grpc_lite_protobuf);
    benchmark_client.root_module.addImport("demo_proto", demo_proto);
    benchmark_client.step.dependOn(&generate_proto.step);
    b.installArtifact(benchmark_server);
    b.installArtifact(benchmark_client);
}

fn addExample(
    b: *std.Build,
    name: []const u8,
    source: []const u8,
    grpc_lite: *std.Build.Module,
) *std.Build.Step.Compile {
    const module = b.createModule(.{
        .root_source_file = b.path(source),
        .target = grpc_lite.resolved_target,
        .optimize = grpc_lite.optimize,
        .imports = &.{.{ .name = "grpc_lite", .module = grpc_lite }},
    });
    module.sanitize_thread = grpc_lite.sanitize_thread;
    module.sanitize_c = grpc_lite.sanitize_c;
    module.omit_frame_pointer = grpc_lite.omit_frame_pointer;
    const executable = b.addExecutable(.{
        .name = name,
        .root_module = module,
    });
    return executable;
}

const NativeDependencies = struct {
    nghttp2_archive: std.Build.LazyPath,
    nghttp2_include: std.Build.LazyPath,
    cares_archive: std.Build.LazyPath,
    cares_include: std.Build.LazyPath,
    mbedtls_archive: ?std.Build.LazyPath,
    gperftools_archive: ?std.Build.LazyPath,
    gperftools_force_link: ?std.Build.LazyPath,
};

fn linkCApiTestLibrary(
    module: *std.Build.Module,
    static_library: std.Build.LazyPath,
    shared_library: *std.Build.Step.Compile,
    sanitizers: Sanitizers,
) void {
    if (sanitizers.thread == true) {
        module.addObjectFile(static_library);
    } else {
        module.linkLibrary(shared_library);
    }
}

fn protocDependencyName() []const u8 {
    if (builtin.os.tag == .windows) return "protoc-win64";
    const os = switch (builtin.os.tag) {
        .linux => "linux",
        .macos => "osx",
        else => @panic("protoc is unavailable for this host OS"),
    };
    const arch = switch (builtin.cpu.arch) {
        .x86_64 => "x86_64",
        .x86 => "x86_32",
        .aarch64, .aarch64_be => "aarch_64",
        .s390x => "s390",
        else => @panic("protoc is unavailable for this host architecture"),
    };
    return std.fmt.comptimePrint("protoc-{s}-{s}", .{ os, arch });
}

fn addNativeDependencies(
    b: *std.Build,
    nghttp2_source_dir: std.Build.LazyPath,
    cares_source_dir: std.Build.LazyPath,
    mbedtls_source_dir: ?std.Build.LazyPath,
    gperftools_source_dir: ?std.Build.LazyPath,
    target: std.Build.ResolvedTarget,
    optimize: std.builtin.OptimizeMode,
    sanitizers: Sanitizers,
    enable_gperftools: bool,
    enable_tcmalloc: bool,
) NativeDependencies {
    const target_triple = target.query.zigTriple(b.allocator) catch @panic("OOM");
    const cc = b.fmt("{s} cc -target {s}", .{ b.graph.zig_exe, target_triple });
    const cxx = b.fmt("{s} c++ -target {s}", .{ b.graph.zig_exe, target_triple });
    const cmake_build_type = switch (optimize) {
        .Debug => "Debug",
        .ReleaseSafe => "RelWithDebInfo",
        .ReleaseFast, .ReleaseSmall => "Release",
    };

    const build_nghttp2 = addNativeBuild(
        b,
        "nghttp2",
        nghttp2_source_dir,
        cmake_build_type,
        cc,
        cxx,
        target_triple,
        sanitizers,
        false,
        false,
    );
    const build_cares = addNativeBuild(
        b,
        "cares",
        cares_source_dir,
        cmake_build_type,
        cc,
        cxx,
        target_triple,
        sanitizers,
        false,
        false,
    );
    const build_mbedtls = if (mbedtls_source_dir) |source_dir|
        addNativeBuild(
            b,
            "mbedtls",
            source_dir,
            cmake_build_type,
            cc,
            cxx,
            target_triple,
            sanitizers,
            false,
            false,
        )
    else
        null;

    const build_gperftools = if (gperftools_source_dir) |source_dir|
        addNativeBuild(
            b,
            "gperftools",
            source_dir,
            cmake_build_type,
            cc,
            cxx,
            target_triple,
            sanitizers,
            enable_gperftools,
            enable_tcmalloc,
        )
    else
        null;

    return .{
        .nghttp2_archive = build_nghttp2.path(b, "lib/libnghttp2.a"),
        .nghttp2_include = build_nghttp2.path(b, "lib/includes"),
        .cares_archive = build_cares.path(b, "lib/libcares.a"),
        .cares_include = build_cares.path(b, "include"),
        .mbedtls_archive = if (build_mbedtls) |output| output.path(b, "libmbedtls_combined.a") else null,
        .gperftools_archive = if (build_gperftools) |output| output.path(b, "libgrpc_lite_gperftools.a") else null,
        .gperftools_force_link = if (build_gperftools) |output| output.path(b, "gperftools_force_link.o") else null,
    };
}

fn addNativeBuild(
    b: *std.Build,
    name: []const u8,
    source_dir: std.Build.LazyPath,
    cmake_build_type: []const u8,
    cc: []const u8,
    cxx: []const u8,
    target_triple: []const u8,
    sanitizers: Sanitizers,
    enable_gperftools: bool,
    enable_tcmalloc: bool,
) std.Build.LazyPath {
    const run = b.addSystemCommand(&.{"bash"});
    run.addFileArg(b.path("tools/build_native.sh"));
    run.addArg(name);
    run.addDirectoryArg(source_dir);
    const output = run.addOutputDirectoryArg(name);
    run.addArgs(&.{
        cmake_build_type,
        cc,
        cxx,
        b.graph.zig_exe,
        target_triple,
    });
    run.addFileArg(b.path("tools/gperftools_force_link.c"));
    run.addFileArg(b.path("tools/mbedtls_user_config.h"));
    run.addArgs(&.{
        if (sanitizers.thread == true) "true" else "false",
        if (sanitizers.c == .full) "true" else "false",
        if (enable_gperftools) "true" else "false",
        if (enable_tcmalloc) "true" else "false",
    });
    return output;
}

fn addCpuCycles(
    b: *std.Build,
    module: *std.Build.Module,
    dependency: *std.Build.Dependency,
    target: std.Build.ResolvedTarget,
) void {
    const options: []const []const u8 = switch (target.result.cpu.arch) {
        .x86_64 => &.{
            "amd64-tsc",
            "amd64-tscasm",
            "default-monotonic",
            "default-gettimeofday",
            "default-zero",
        },
        .x86 => &.{
            "x86-tsc",
            "x86-tscasm",
            "default-monotonic",
            "default-gettimeofday",
            "default-zero",
        },
        .aarch64 => &.{
            "arm64-vct",
            "default-monotonic",
            "default-gettimeofday",
            "default-zero",
        },
        else => &.{
            "default-monotonic",
            "default-gettimeofday",
            "default-zero",
        },
    };

    var declarations: []const u8 = "";
    var entries: []const u8 = "";
    for (options) |option| {
        const symbol = b.dupe(option);
        std.mem.replaceScalar(u8, symbol, '-', '_');
        declarations = b.fmt(
            \\{s}extern long long cpucycles_ticks_{s}_setup(void);
            \\extern long long cpucycles_ticks_{s}(void);
            \\extern void cpucycles_ticks_{s}_close(void);
            \\
        , .{ declarations, symbol, symbol, symbol });
        entries = b.fmt(
            \\{s}{{ "{s}", cpucycles_ticks_{s}_setup, cpucycles_ticks_{s}, cpucycles_ticks_{s}_close }},
        , .{ entries, option, symbol, symbol, symbol });
    }
    const generated = b.addWriteFiles();
    const options_include = generated.add("options.inc", b.fmt(
        \\#define NUMOPTIONS {d}
        \\#define DEFAULTOPTION (NUMOPTIONS-1)
        \\
        \\{s}
        \\static struct {{
        \\  const char *implementation;
        \\  long long (*ticks_setup)(void);
        \\  long long (*ticks)(void);
        \\  void (*ticks_close)(void);
        \\}} options[NUMOPTIONS] = {{
        \\{s}}};
        \\
    , .{ options.len, declarations, entries }));

    module.addIncludePath(options_include.dirname());
    module.addIncludePath(dependency.path("cpucycles"));
    module.addCSourceFile(.{
        .file = dependency.path("cpucycles/wrapper.c"),
        .flags = &.{ "-std=gnu99", "-D_GNU_SOURCE=1", "-fwrapv", "-fvisibility=hidden" },
    });
    for (options) |option| {
        const symbol = b.dupe(option);
        std.mem.replaceScalar(u8, symbol, '-', '_');
        module.addCSourceFile(.{
            .file = dependency.path(b.fmt("cpucycles/{s}.c", .{option})),
            .flags = &.{
                "-std=gnu99",
                "-D_GNU_SOURCE=1",
                "-fwrapv",
                "-fvisibility=hidden",
                b.fmt("-Dticks=cpucycles_ticks_{s}", .{symbol}),
                b.fmt("-Dticks_setup=cpucycles_ticks_{s}_setup", .{symbol}),
                b.fmt("-Dticks_close=cpucycles_ticks_{s}_close", .{symbol}),
            },
        });
    }
}
