// Test root for storage modules. Keeping the root at src/ lets rqlite resolve
// the shared HTTP transport without escaping Zig's module boundary.
comptime {
    _ = @import("storage/rqlite.zig");
    _ = @import("storage/mvcc.zig");
    _ = @import("integration_test.zig");
}
