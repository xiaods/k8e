const std = @import("std");
const builtin = @import("builtin");

const Io = std.Io;

// This step stays local so loading grpc-lite's build script does not import the
// optional zig-protobuf build package.
/// A protoc invocation backed by zig-protobuf's bootstrapped generator.
pub const RunProtocStep = struct {
    step: std.Build.Step,
    source_files: []std.Build.LazyPath,
    include_directories: []std.Build.LazyPath,
    destination_directory: std.Build.LazyPath,
    generator: *std.Build.Step.Compile,
    generator_bin: std.Build.LazyPath,
    protoc_override_bin: ?std.Build.LazyPath = null,
    preserve_unknown_fields: bool = false,
    verbose: bool = false,

    pub const base_id = .protoc;

    pub const Options = struct {
        source_files: []const std.Build.LazyPath,
        include_directories: []const std.Build.LazyPath = &.{},
        destination_directory: std.Build.LazyPath,
        generator: ?*std.Build.Step.Compile = null,
        protoc: ?std.Build.LazyPath = null,
        preserve_unknown_fields: bool = false,
    };

    pub const StepErr = error{FailedToConvertProtobuf};

    pub fn create(
        owner: *std.Build,
        target: std.Build.ResolvedTarget,
        options: Options,
    ) *RunProtocStep {
        const self = owner.allocator.create(RunProtocStep) catch @panic("OOM");
        const generator = options.generator orelse buildGenerator(owner, target);
        self.* = .{
            .step = std.Build.Step.init(.{
                .id = .check_file,
                .name = "run protoc",
                .owner = owner,
                .makeFn = make,
            }),
            .source_files = dupeLazyPaths(owner, options.source_files),
            .include_directories = dupeLazyPaths(owner, options.include_directories),
            .destination_directory = options.destination_directory.dupe(owner),
            .generator = generator,
            .generator_bin = generator.getEmittedBin(),
            .protoc_override_bin = options.protoc,
            .preserve_unknown_fields = options.preserve_unknown_fields,
        };
        self.step.dependOn(&generator.step);
        return self;
    }

    pub fn createWithGenerator(
        owner: *std.Build,
        generator: *std.Build.Step.Compile,
        options: Options,
    ) *RunProtocStep {
        return create(owner, generator.root_module.resolved_target.?, .{
            .source_files = options.source_files,
            .include_directories = options.include_directories,
            .destination_directory = options.destination_directory,
            .generator = generator,
            .protoc = options.protoc,
            .preserve_unknown_fields = options.preserve_unknown_fields,
        });
    }

    pub fn setName(self: *RunProtocStep, name: []const u8) void {
        self.step.name = name;
    }

    fn make(step: *std.Build.Step, make_options: std.Build.Step.MakeOptions) anyerror!void {
        const b = step.owner;
        const self: *RunProtocStep = @fieldParentPtr("step", step);
        const destination = self.destination_directory.getPath2(b, step);

        var argv: std.ArrayList([]const u8) = .empty;
        const protoc_path = if (self.protoc_override_bin) |path|
            path.getPath2(b, step)
        else
            try getProtocPath(self.generator.step.owner, step);
        try argv.append(b.allocator, protoc_path);
        try argv.append(b.allocator, b.fmt(
            "--plugin=protoc-gen-zig={s}",
            .{self.generator_bin.getPath2(b, step)},
        ));
        try argv.append(b.allocator, if (self.preserve_unknown_fields)
            b.fmt("--zig_out=preserve_unknown_fields=true:{s}", .{destination})
        else
            b.fmt("--zig_out={s}", .{destination}));
        if (!pathExists(b.graph.io, destination)) {
            try Io.Dir.cwd().createDir(b.graph.io, destination, .default_dir);
        }
        for (self.include_directories) |directory| {
            try argv.appendSlice(b.allocator, &.{ "-I", directory.getPath2(b, step) });
        }
        for (self.source_files) |source| {
            try argv.append(b.allocator, source.getPath2(b, step));
        }
        if (self.verbose) {
            std.debug.print("Running protoc:", .{});
            for (argv.items) |arg| std.debug.print(" {s}", .{arg});
            std.debug.print("\n", .{});
        }
        _ = try step.captureChildProcess(b.allocator, make_options.progress_node, argv.items);

        argv = .empty;
        try argv.appendSlice(b.allocator, &.{ b.graph.zig_exe, "fmt", destination });
        step.result_failed_command = null;
        _ = try step.captureChildProcess(b.allocator, make_options.progress_node, argv.items);
    }
};

fn buildGenerator(
    b: *std.Build,
    target: std.Build.ResolvedTarget,
) *std.Build.Step.Compile {
    const protobuf = b.addModule("protobuf", .{
        .root_source_file = b.path("src/protobuf.zig"),
    });
    const generator = b.addExecutable(.{
        .name = "protoc-gen-zig",
        .root_module = b.createModule(.{
            .root_source_file = b.path("bootstrapped-generator/main.zig"),
            .target = target,
            .optimize = .Debug,
            .imports = &.{.{ .name = "protobuf", .module = protobuf }},
        }),
    });
    return generator;
}

fn getProtocPath(owner: *std.Build, step: *std.Build.Step) ![]const u8 {
    const dependency = owner.lazyDependency(protocDependencyName(), .{}) orelse
        return error.FailedToConvertProtobuf;
    const relative_path = if (builtin.os.tag == .windows) "bin/protoc.exe" else "bin/protoc";
    const path = dependency.path(relative_path).getPath2(owner, step);
    if (!pathExists(step.owner.graph.io, path)) return error.FailedToConvertProtobuf;
    return path;
}

fn protocDependencyName() []const u8 {
    if (builtin.os.tag == .windows) return "protoc-win64";
    const os = switch (builtin.os.tag) {
        .linux => "linux",
        .macos => "osx",
        else => @panic("protoc is unavailable for this host OS"),
    };
    const arch = switch (builtin.cpu.arch) {
        .powerpcle, .powerpc64le => "ppcle",
        .x86_64 => "x86_64",
        .x86 => "x86_32",
        .aarch64, .aarch64_be => "aarch_64",
        .s390x => "s390",
        else => @panic("protoc is unavailable for this host architecture"),
    };
    return std.fmt.comptimePrint("protoc-{s}-{s}", .{ os, arch });
}

fn dupeLazyPaths(b: *std.Build, paths: []const std.Build.LazyPath) []std.Build.LazyPath {
    const copies = b.allocator.alloc(std.Build.LazyPath, paths.len) catch @panic("OOM");
    for (copies, paths) |*copy, path| copy.* = path.dupe(b);
    return copies;
}

fn pathExists(io: Io, path: []const u8) bool {
    Io.Dir.cwd().access(io, path, .{}) catch return false;
    return true;
}
