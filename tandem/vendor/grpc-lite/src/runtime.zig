const c = @import("cares_c.zig").api;
const std = @import("std");

const Token = struct {};
var token: Token = .{};
var next_generation: std.atomic.Value(usize) = .init(1);
var active_generation: std.atomic.Value(usize) = .init(0);

pub const Runtime = struct {
    token: *Token,
    generation: usize,

    /// Must be called before the application creates any threads.
    pub fn init() !Runtime {
        const generation = next_generation.fetchAdd(1, .monotonic);
        if (generation == 0) return error.RuntimeGenerationExhausted;
        if (active_generation.cmpxchgStrong(0, generation, .acq_rel, .acquire) != null) {
            return error.RuntimeAlreadyInitialized;
        }
        errdefer active_generation.store(0, .release);
        if (c.ares_library_init(c.ARES_LIB_INIT_ALL) != c.ARES_SUCCESS) {
            return error.ResolverInitializationFailed;
        }
        return .{ .token = &token, .generation = generation };
    }

    /// All channels using this runtime must be deinitialized first.
    pub fn deinit(self: *Runtime) void {
        if (self.token != &token) return;
        if (active_generation.cmpxchgStrong(self.generation, 0, .acq_rel, .acquire) != null) return;
        c.ares_library_cleanup();
        self.* = undefined;
    }

    pub fn isInitialized(self: *const Runtime) bool {
        return self.token == &token and active_generation.load(.acquire) == self.generation;
    }
};

test "runtime has deterministic lifecycle" {
    var runtime = try Runtime.init();
    runtime.deinit();
}

test "stale runtime copies cannot deinitialize a newer generation" {
    var first = try Runtime.init();
    var stale = first;
    first.deinit();

    var second = try Runtime.init();
    stale.deinit();
    try std.testing.expect(second.isInitialized());
    second.deinit();
}
