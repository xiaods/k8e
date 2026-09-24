//! Optional gperftools integration enabled with `-Dgperftools` or `-Dtcmalloc`.

const std = @import("std");
const grpc_lite = @import("grpc_lite");
const options = @import("grpc_lite_gperftools_options");

comptime {
    _ = grpc_lite;
}

const c = @cImport({
    @cInclude("gperftools/profiler.h");
    @cInclude("gperftools/heap-profiler.h");
    @cInclude("gperftools/malloc_extension_c.h");
});

/// Uses tcmalloc when `has_tcmalloc` is true, otherwise the system C allocator.
pub const allocator = std.heap.c_allocator;
pub const has_cpu_profiler = options.cpu_profiler;
pub const has_tcmalloc = options.tcmalloc;
pub const has_heap_profiler = has_cpu_profiler and has_tcmalloc;

pub const CpuProfilerError = error{ProfilerStartFailed};

/// Starts process-wide CPU profiling and writes samples to `path`.
pub fn startCpuProfiler(path: [:0]const u8) CpuProfilerError!void {
    if (!has_cpu_profiler) @compileError("CPU profiling requires -Dgperftools=true");
    if (c.ProfilerStart(path.ptr) == 0) return error.ProfilerStartFailed;
}

/// Flushes the currently active process-wide CPU profile.
pub fn flushCpuProfiler() void {
    if (!has_cpu_profiler) @compileError("CPU profiling requires -Dgperftools=true");
    c.ProfilerFlush();
}

/// Stops process-wide CPU profiling and flushes its output.
pub fn stopCpuProfiler() void {
    if (!has_cpu_profiler) @compileError("CPU profiling requires -Dgperftools=true");
    c.ProfilerStop();
}

pub fn cpuProfilerRunning() bool {
    if (!has_cpu_profiler) @compileError("CPU profiling requires -Dgperftools=true");
    return c.ProfilingIsEnabledForAllThreads() != 0;
}

/// Starts process-wide heap profiling with the given output prefix.
pub fn startHeapProfiler(prefix: [:0]const u8) void {
    if (!has_heap_profiler) @compileError("heap profiling requires both -Dgperftools=true and -Dtcmalloc=true");
    c.HeapProfilerStart(prefix.ptr);
}

/// Writes the current heap profile to the next file for the active prefix.
pub fn dumpHeapProfile(reason: [:0]const u8) void {
    if (!has_heap_profiler) @compileError("heap profiling requires both -Dgperftools=true and -Dtcmalloc=true");
    c.HeapProfilerDump(reason.ptr);
}

/// Stops process-wide heap profiling.
pub fn stopHeapProfiler() void {
    if (!has_heap_profiler) @compileError("heap profiling requires both -Dgperftools=true and -Dtcmalloc=true");
    c.HeapProfilerStop();
}

pub fn heapProfilerRunning() bool {
    if (!has_heap_profiler) @compileError("heap profiling requires both -Dgperftools=true and -Dtcmalloc=true");
    return c.IsHeapProfilerRunning() != 0;
}

pub fn getNumericProperty(name: [:0]const u8) ?usize {
    if (!has_tcmalloc) @compileError("MallocExtension requires -Dtcmalloc=true");
    var value: usize = 0;
    return if (c.MallocExtension_GetNumericProperty(name.ptr, &value) != 0) value else null;
}

pub fn owns(pointer: *const anyopaque) bool {
    if (!has_tcmalloc) @compileError("MallocExtension requires -Dtcmalloc=true");
    return c.MallocExtension_GetOwnership(pointer) == c.MallocExtension_kOwned;
}

pub fn releaseFreeMemory() void {
    if (!has_tcmalloc) @compileError("MallocExtension requires -Dtcmalloc=true");
    c.MallocExtension_ReleaseFreeMemory();
}

pub fn setGuardedSamplingInterval(interval: i64) void {
    if (!has_heap_profiler) @compileError("guarded sampling requires both -Dgperftools=true and -Dtcmalloc=true");
    c.MallocExtension_SetGuardedSamplingInterval(interval);
}

pub fn guardedSamplingInterval() i64 {
    if (!has_heap_profiler) @compileError("guarded sampling requires both -Dgperftools=true and -Dtcmalloc=true");
    return c.MallocExtension_GetGuardedSamplingInterval();
}

pub fn activateGuardedSampling() void {
    if (!has_heap_profiler) @compileError("guarded sampling requires both -Dgperftools=true and -Dtcmalloc=true");
    c.MallocExtension_ActivateGuardedSampling();
}
