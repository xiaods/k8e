const std = @import("std");

pub const Entry = struct {
    key: []const u8,
    value: []const u8,
};

pub const IncomingResult = enum { appended, discarded };

pub const OutboundValue = union(enum) {
    borrowed: []const u8,
    owned: []u8,

    pub fn bytes(self: OutboundValue) []const u8 {
        return switch (self) {
            inline else => |value| value,
        };
    }

    pub fn deinit(self: OutboundValue, allocator: std.mem.Allocator) void {
        switch (self) {
            .borrowed => {},
            .owned => |value| allocator.free(value),
        }
    }
};

pub const Metadata = struct {
    allocator: std.mem.Allocator,
    entries: std.ArrayList(Entry) = .empty,

    pub fn init(allocator: std.mem.Allocator) Metadata {
        return .{ .allocator = allocator };
    }

    pub fn deinit(self: *Metadata) void {
        for (self.entries.items) |entry| {
            self.allocator.free(entry.key);
            self.allocator.free(entry.value);
        }
        self.entries.deinit(self.allocator);
        self.* = undefined;
    }

    pub fn append(self: *Metadata, key: []const u8, value: []const u8) !void {
        if (!isApplicationKey(key)) return error.InvalidMetadataKey;
        if (!isBinaryKey(key) and !isValidAsciiValue(value)) return error.InvalidMetadataValue;

        try self.appendOwned(key, value);
    }

    fn appendOwned(self: *Metadata, key: []const u8, value: []const u8) !void {
        const owned_key = try self.allocator.dupe(u8, key);
        errdefer self.allocator.free(owned_key);
        const owned_value = try self.allocator.dupe(u8, value);
        errdefer self.allocator.free(owned_value);

        try self.entries.append(self.allocator, .{
            .key = owned_key,
            .value = owned_value,
        });
    }

    pub fn appendDecoded(self: *Metadata, key: []const u8, value: []const u8) !IncomingResult {
        if (!isApplicationKey(key)) return error.InvalidMetadataKey;
        if (!isBinaryKey(key)) {
            if (!isValidAsciiValue(value)) return .discarded;
            try self.appendOwned(key, value);
            return .appended;
        }

        var decoded_values: std.ArrayList([]u8) = .empty;
        defer {
            for (decoded_values.items) |decoded| self.allocator.free(decoded);
            decoded_values.deinit(self.allocator);
        }
        var values = std.mem.splitScalar(u8, value, ',');
        while (values.next()) |encoded| {
            const decoder = if (std.mem.endsWith(u8, encoded, "="))
                std.base64.standard.Decoder
            else
                std.base64.standard_no_pad.Decoder;
            const decoded = try self.allocator.alloc(u8, try decoder.calcSizeForSlice(encoded));
            errdefer self.allocator.free(decoded);
            try decoder.decode(decoded, encoded);
            try decoded_values.append(self.allocator, decoded);
        }

        for (decoded_values.items) |decoded| try self.appendOwned(key, decoded);
        return .appended;
    }

    pub fn getFirst(self: *const Metadata, key: []const u8) ?[]const u8 {
        for (self.entries.items) |entry| {
            if (std.mem.eql(u8, entry.key, key)) return entry.value;
        }
        return null;
    }

    pub fn items(self: *const Metadata) []const Entry {
        return self.entries.items;
    }
};

pub fn isValidKey(key: []const u8) bool {
    if (key.len == 0 or key[0] == ':') return false;
    for (key) |byte| switch (byte) {
        'a'...'z', '0'...'9', '-', '_', '.' => {},
        else => return false,
    };
    return true;
}

pub fn isApplicationKey(key: []const u8) bool {
    return isValidKey(key) and !std.mem.startsWith(u8, key, "grpc-");
}

pub fn isValidAsciiValue(value: []const u8) bool {
    if (value.len == 0) return false;
    for (value) |byte| if (byte < 0x20 or byte > 0x7e) return false;
    return true;
}

pub fn isBinaryKey(key: []const u8) bool {
    return std.mem.endsWith(u8, key, "-bin");
}

pub fn encodeOutboundValue(allocator: std.mem.Allocator, key: []const u8, value: []const u8) !OutboundValue {
    if (!isBinaryKey(key)) return .{ .borrowed = value };

    const encoded = try allocator.alloc(u8, std.base64.standard.Encoder.calcSize(value.len));
    _ = std.base64.standard.Encoder.encode(encoded, value);
    return .{ .owned = encoded };
}

fn testMetadataAllocations(allocator: std.mem.Allocator) !void {
    var metadata = Metadata.init(allocator);
    defer metadata.deinit();
    try metadata.append("x-request-id", "value");
    _ = try metadata.appendDecoded("trace-bin", "qw==");

    const encoded = try encodeOutboundValue(allocator, "trace-bin", "binary");
    defer encoded.deinit(allocator);
}

fn testMixedOutboundValueCleanup(allocator: std.mem.Allocator) !void {
    const entries = [_]Entry{
        .{ .key = "x-ascii", .value = "borrowed" },
        .{ .key = "first-bin", .value = "first" },
        .{ .key = "second-bin", .value = "second" },
    };
    var values: [entries.len]OutboundValue = undefined;
    var initialized: usize = 0;
    defer for (values[0..initialized]) |value| value.deinit(allocator);

    for (entries) |entry| {
        values[initialized] = try encodeOutboundValue(allocator, entry.key, entry.value);
        initialized += 1;
    }
}

test "metadata owns entries and preserves duplicates" {
    var metadata = Metadata.init(std.testing.allocator);
    defer metadata.deinit();

    var source_key = [_]u8{ 'x', '-', 'r', 'e', 'q', 'u', 'e', 's', 't', '-', 'i', 'd' };
    var source_value = [_]u8{ 'o', 'n', 'e' };
    try metadata.append(&source_key, &source_value);
    try metadata.append("x-request-id", "two");
    @memset(&source_key, 'x');
    @memset(&source_value, 'y');
    try std.testing.expectEqual(@as(usize, 2), metadata.items().len);
    try std.testing.expectEqualStrings("x-request-id", metadata.items()[0].key);
    try std.testing.expectEqualStrings("one", metadata.getFirst("x-request-id").?);
}

test "metadata append is atomic at every allocation failure" {
    for (0..3) |fail_index| {
        var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{
            .fail_index = fail_index,
        });
        var metadata = Metadata.init(failing.allocator());
        defer metadata.deinit();

        try std.testing.expectError(error.OutOfMemory, metadata.append("x-key", "value"));
        try std.testing.expectEqual(@as(usize, 0), metadata.items().len);
    }
}

test "metadata handles every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testMetadataAllocations,
        .{},
    );
}

test "metadata rejects invalid keys" {
    var metadata = Metadata.init(std.testing.allocator);
    defer metadata.deinit();

    try std.testing.expectError(error.InvalidMetadataKey, metadata.append("", "value"));
    try std.testing.expectError(error.InvalidMetadataKey, metadata.append(":path", "value"));
    try std.testing.expectError(error.InvalidMetadataKey, metadata.append("Upper", "value"));
    try std.testing.expectError(error.InvalidMetadataKey, metadata.append("grpc-future", "value"));
    try std.testing.expectError(error.InvalidMetadataValue, metadata.append("x-empty", ""));
    try std.testing.expectError(error.InvalidMetadataValue, metadata.append("x-control", "bad\nvalue"));
}

test "ASCII outbound metadata values are borrowed" {
    var failing = std.testing.FailingAllocator.init(std.testing.allocator, .{
        .fail_index = 0,
    });
    var original = [_]u8{ 'v', 'a', 'l', 'u', 'e' };
    const encoded = try encodeOutboundValue(failing.allocator(), "x-ascii", &original);
    defer encoded.deinit(failing.allocator());

    switch (encoded) {
        .borrowed => |value| {
            try std.testing.expect(value.ptr == original[0..].ptr);
            original[0] = 'V';
            try std.testing.expectEqualStrings("Value", value);
        },
        .owned => return error.TestExpectedBorrowedValue,
    }
}

test "binary outbound metadata values are owned padded base64" {
    const raw = [_]u8{0xab};
    const encoded = try encodeOutboundValue(std.testing.allocator, "trace-bin", &raw);
    defer encoded.deinit(std.testing.allocator);
    switch (encoded) {
        .borrowed => return error.TestExpectedOwnedValue,
        .owned => |value| try std.testing.expectEqualStrings("qw==", value),
    }
}

test "mixed outbound metadata cleanup handles every allocation failure" {
    try std.testing.checkAllAllocationFailures(
        std.testing.allocator,
        testMixedOutboundValueCleanup,
        .{},
    );
}

test "binary metadata codec accepts padded or unpadded input" {
    const raw = [_]u8{0xab};

    var metadata = Metadata.init(std.testing.allocator);
    defer metadata.deinit();
    _ = try metadata.appendDecoded("trace-bin", "q6ur");
    _ = try metadata.appendDecoded("trace-bin", "qw==");
    _ = try metadata.appendDecoded("trace-bin", "qw");
    try std.testing.expectEqualSlices(u8, &.{ 0xab, 0xab, 0xab }, metadata.items()[0].value);
    try std.testing.expectEqualSlices(u8, &raw, metadata.items()[1].value);
    try std.testing.expectEqualSlices(u8, &raw, metadata.items()[2].value);
}

test "binary metadata codec rejects invalid input without appending" {
    var metadata = Metadata.init(std.testing.allocator);
    defer metadata.deinit();

    try std.testing.expectError(error.InvalidCharacter, metadata.appendDecoded("trace-bin", "not base64!"));
    try std.testing.expectError(error.InvalidPadding, metadata.appendDecoded("trace-bin", "A"));
    try std.testing.expectEqual(@as(usize, 0), metadata.items().len);
}

test "binary metadata splits comma-joined values atomically" {
    var metadata = Metadata.init(std.testing.allocator);
    defer metadata.deinit();

    try std.testing.expectEqual(IncomingResult.appended, try metadata.appendDecoded("trace-bin", "qw==,q6ur,qw"));
    try std.testing.expectEqual(@as(usize, 3), metadata.items().len);
    try std.testing.expectEqualSlices(u8, &.{0xab}, metadata.items()[0].value);
    try std.testing.expectEqualSlices(u8, &.{ 0xab, 0xab, 0xab }, metadata.items()[1].value);
    try std.testing.expectEqualSlices(u8, &.{0xab}, metadata.items()[2].value);

    try std.testing.expectError(error.InvalidCharacter, metadata.appendDecoded("trace-bin", "qw,not base64!"));
    try std.testing.expectEqual(@as(usize, 3), metadata.items().len);
}

test "invalid incoming ASCII metadata is discarded" {
    var metadata = Metadata.init(std.testing.allocator);
    defer metadata.deinit();

    try std.testing.expectEqual(IncomingResult.discarded, try metadata.appendDecoded("x-empty", ""));
    try std.testing.expectEqual(IncomingResult.discarded, try metadata.appendDecoded("x-control", "bad\tvalue"));
    try std.testing.expectEqual(IncomingResult.appended, try metadata.appendDecoded("x-visible", " ~"));
    try std.testing.expectEqual(@as(usize, 1), metadata.items().len);
}
