const std = @import("std");
const perf = @import("grpc_lite_gperftools");

test "tcmalloc replaces the C allocator" {
    if (!perf.has_tcmalloc) return error.SkipZigTest;
    const memory = try perf.allocator.alloc(u8, 4096);
    defer perf.allocator.free(memory);

    try std.testing.expect(perf.owns(memory.ptr));
    try std.testing.expect(perf.getNumericProperty("generic.current_allocated_bytes") != null);
}

test "CPU profiler is available" {
    if (!perf.has_cpu_profiler) return error.SkipZigTest;
    const pid = std.os.linux.getpid();
    var cpu_path_buffer: [128]u8 = undefined;
    const cpu_profile = try std.fmt.bufPrintZ(
        &cpu_path_buffer,
        "/tmp/grpc-lite-gperftools-test-{d}.prof",
        .{pid},
    );
    std.Io.Dir.deleteFileAbsolute(std.testing.io, cpu_profile) catch {};
    defer std.Io.Dir.deleteFileAbsolute(std.testing.io, cpu_profile) catch {};

    try perf.startCpuProfiler(cpu_profile);
    try std.testing.expect(perf.cpuProfilerRunning());
    perf.flushCpuProfiler();
    perf.stopCpuProfiler();
    try std.testing.expect(!perf.cpuProfilerRunning());
}

test "heap profiler is available with tcmalloc" {
    if (!perf.has_heap_profiler) return error.SkipZigTest;
    const previous_interval = perf.guardedSamplingInterval();
    defer perf.setGuardedSamplingInterval(previous_interval);
    perf.setGuardedSamplingInterval(4096);
    try std.testing.expectEqual(@as(i64, 4096), perf.guardedSamplingInterval());

    const pid = std.os.linux.getpid();
    var heap_prefix_buffer: [128]u8 = undefined;
    const heap_prefix = try std.fmt.bufPrintZ(
        &heap_prefix_buffer,
        "/tmp/grpc-lite-gperftools-test-{d}-heap",
        .{pid},
    );
    var heap_path_buffer: [160]u8 = undefined;
    const heap_profile = try std.fmt.bufPrint(
        &heap_path_buffer,
        "{s}.0001.heap",
        .{heap_prefix},
    );
    std.Io.Dir.deleteFileAbsolute(std.testing.io, heap_profile) catch {};
    defer std.Io.Dir.deleteFileAbsolute(std.testing.io, heap_profile) catch {};

    perf.startHeapProfiler(heap_prefix);
    try std.testing.expect(perf.heapProfilerRunning());
    const memory = try perf.allocator.alloc(u8, 4096);
    defer perf.allocator.free(memory);
    perf.dumpHeapProfile("integration test");
    perf.stopHeapProfiler();
    try std.testing.expect(!perf.heapProfilerRunning());
    std.Io.Dir.accessAbsolute(std.testing.io, heap_profile, .{}) catch return error.TestUnexpectedResult;
}
