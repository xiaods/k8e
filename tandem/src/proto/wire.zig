// Minimal protobuf wire-format encoder/decoder.
// Supports the subset needed for etcd v3 messages.

const std = @import("std");
const Allocator = std.mem.Allocator;

/// Protobuf wire types
pub const WireType = enum(u32) {
    varint = 0,
    fixed64 = 1,
    length_delimited = 2,
    fixed32 = 5,
};

/// Encode a field tag (field_number << 3 | wire_type)
pub fn makeTag(field_number: u32, wire_type: WireType) u64 {
    return (@as(u64, field_number) << 3) | @intFromEnum(wire_type);
}

/// Encode a varint (unsigned LEB128) into an ArrayList
pub fn encodeVarint(buf: *std.ArrayList(u8), allocator: Allocator, value: u64) !void {
    var v = value;
    while (v >= 0x80) {
        try buf.append(allocator, @as(u8, @intCast(v & 0x7f)) | 0x80);
        v >>= 7;
    }
    try buf.append(allocator, @as(u8, @intCast(v)));
}

/// Decode a varint from a slice.
pub fn decodeVarint(data: []const u8) !struct { value: u64, consumed: usize } {
    var result: u64 = 0;
    var shift: u6 = 0;
    var i: usize = 0;
    while (i < data.len) {
        const byte = data[i];
        i += 1;
        result |= @as(u64, @intCast(byte & 0x7f)) << shift;
        if (byte & 0x80 == 0) {
            return .{ .value = result, .consumed = i };
        }
        shift += 7;
        if (shift >= 64) return error.VarintOverflow;
    }
    return error.UnexpectedEndOfVarint;
}

/// ZigZag encoding for sint32/sint64
pub fn zigZagEncode(value: i64) u64 {
    const unsigned = @as(u64, @bitCast(value));
    const sign_ext = @as(u64, @bitCast(value >> 63));
    return (unsigned << 1) ^ sign_ext;
}

pub fn zigZagDecode(value: u64) i64 {
    const sign_bit = value & 1;
    const magnitude = value >> 1;
    if (sign_bit == 1) {
        return @as(i64, @bitCast(~(magnitude)));
    }
    return @as(i64, @bitCast(magnitude));
}

/// Read the next field tag from data.
pub fn readTag(data: []const u8) !struct { field: u32, wire_type: WireType, consumed: usize } {
    const result = try decodeVarint(data);
    const wire_type: WireType = @enumFromInt(@as(u32, @intCast(result.value & 0x07)));
    const field = @as(u32, @intCast(result.value >> 3));
    return .{ .field = field, .wire_type = wire_type, .consumed = result.consumed };
}

/// Read a length-delimited field value from data (after tag).
pub fn readLengthDelimited(data: []const u8) !struct { value: []const u8, consumed: usize } {
    const len_result = try decodeVarint(data);
    const len = @as(usize, @intCast(len_result.value));
    if (data.len < len_result.consumed + len) return error.UnexpectedEndOfField;
    return .{
        .value = data[len_result.consumed .. len_result.consumed + len],
        .consumed = len_result.consumed + len,
    };
}

/// Read a fixed32 field value from data (after tag).
pub fn readFixed32Value(data: []const u8) !struct { value: u32, consumed: usize } {
    if (data.len < 4) return error.UnexpectedEndOfField;
    var raw: [4]u8 = data[0..4].*;
    return .{
        .value = std.mem.readInt(u32, &raw, .little),
        .consumed = 4,
    };
}

/// Read a fixed64 field value from data (after tag).
pub fn readFixed64Value(data: []const u8) !struct { value: u64, consumed: usize } {
    if (data.len < 8) return error.UnexpectedEndOfField;
    var raw: [8]u8 = data[0..8].*;
    return .{
        .value = std.mem.readInt(u64, &raw, .little),
        .consumed = 8,
    };
}

/// Read a signed varint (zigzag decoded) as i64
pub fn readSint64Value(data: []const u8) !struct { value: i64, consumed: usize } {
    const result = try decodeVarint(data);
    return .{ .value = zigZagDecode(result.value), .consumed = result.consumed };
}

/// Read a signed varint (zigzag decoded) as i32
pub fn readSint32Value(data: []const u8) !struct { value: i32, consumed: usize } {
    const result = try decodeVarint(data);
    const decoded = zigZagDecode(result.value);
    return .{ .value = @as(i32, @intCast(decoded)), .consumed = result.consumed };
}

/// Skip a field of any wire type. Returns bytes consumed.
pub fn skipFieldValue(data: []const u8, wire_type: WireType) !usize {
    return switch (wire_type) {
        .varint => blk: {
            const r = try decodeVarint(data);
            break :blk r.consumed;
        },
        .fixed64 => 8,
        .length_delimited => blk: {
            const r = try decodeVarint(data);
            break :blk r.consumed + @as(usize, @intCast(r.value));
        },
        .fixed32 => 4,
    };
}

// ─── Reader ─────────────────────────────────────────────────────────────────

pub const Reader = struct {
    data: []const u8,
    pos: usize = 0,

    pub fn init(data: []const u8) Reader {
        return .{ .data = data };
    }

    pub fn nextTag(self: *Reader) !struct { field: u32, wire_type: WireType } {
        const result = try readTag(self.data[self.pos..]);
        self.pos += result.consumed;
        return .{ .field = result.field, .wire_type = result.wire_type };
    }

    pub fn readU64(self: *Reader) !u64 {
        const result = try decodeVarint(self.data[self.pos..]);
        self.pos += result.consumed;
        return result.value;
    }

    pub fn readI32(self: *Reader) !i32 {
        const result = try readSint32Value(self.data[self.pos..]);
        self.pos += result.consumed;
        return result.value;
    }

    pub fn readI64(self: *Reader) !i64 {
        const result = try readSint64Value(self.data[self.pos..]);
        self.pos += result.consumed;
        return result.value;
    }

    pub fn readBool(self: *Reader) !bool {
        return (try self.readU64()) != 0;
    }

    pub fn readBytes(self: *Reader) ![]const u8 {
        const result = try readLengthDelimited(self.data[self.pos..]);
        self.pos += result.consumed;
        return result.value;
    }

    pub fn readString(self: *Reader) ![]const u8 {
        return try self.readBytes();
    }

    pub fn readFixedU32(self: *Reader) !u32 {
        const result = try readFixed32Value(self.data[self.pos..]);
        self.pos += result.consumed;
        return result.value;
    }

    pub fn readFixedU64(self: *Reader) !u64 {
        const result = try readFixed64Value(self.data[self.pos..]);
        self.pos += result.consumed;
        return result.value;
    }

    pub fn skip(self: *Reader, wire_type: WireType) !void {
        const consumed = try skipFieldValue(self.data[self.pos..], wire_type);
        self.pos += consumed;
    }

    pub fn hasMore(self: *Reader) bool {
        return self.pos < self.data.len;
    }
};

// ─── Writer ─────────────────────────────────────────────────────────────────

/// Writer helper for building protobuf messages
pub const Writer = struct {
    buf: std.ArrayList(u8),
    allocator: Allocator,

    pub fn init(allocator: Allocator) Writer {
        return .{ .buf = std.ArrayList(u8).initCapacity(allocator, 256) catch unreachable, .allocator = allocator };
    }

    pub fn deinit(self: *Writer) void {
        self.buf.deinit(self.allocator);
    }

    /// Write a varint field
    pub fn v(self: *Writer, field_number: u32, value: u64) !void {
        if (value == 0) return;
        const tag = makeTag(field_number, .varint);
        try encodeVarint(&self.buf, self.allocator, tag);
        try encodeVarint(&self.buf, self.allocator, value);
    }

    /// Write a sint32 field (zigzag + varint)
    pub fn sint32(self: *Writer, field_number: u32, value: i32) !void {
        if (value == 0) return;
        const tag = makeTag(field_number, .varint);
        try encodeVarint(&self.buf, self.allocator, tag);
        try encodeVarint(&self.buf, self.allocator, zigZagEncode(value));
    }

    /// Write a sint64 field (zigzag + varint)
    pub fn sint64(self: *Writer, field_number: u32, value: i64) !void {
        if (value == 0) return;
        const tag = makeTag(field_number, .varint);
        try encodeVarint(&self.buf, self.allocator, tag);
        try encodeVarint(&self.buf, self.allocator, zigZagEncode(value));
    }

    /// Write a bool field
    pub fn b(self: *Writer, field_number: u32, value: bool) !void {
        if (!value) return;
        const tag = makeTag(field_number, .varint);
        try encodeVarint(&self.buf, self.allocator, tag);
        try encodeVarint(&self.buf, self.allocator, 1);
    }

    /// Write a bytes field
    pub fn bytes(self: *Writer, field_number: u32, data: []const u8) !void {
        if (data.len == 0) return;
        const tag = makeTag(field_number, .length_delimited);
        try encodeVarint(&self.buf, self.allocator, tag);
        try encodeVarint(&self.buf, self.allocator, data.len);
        try self.buf.appendSlice(self.allocator, data);
    }

    /// Write a string field
    pub fn string(self: *Writer, field_number: u32, data: []const u8) !void {
        try self.bytes(field_number, data);
    }

    /// Write an embedded message field
    pub fn msg(self: *Writer, field_number: u32, msg_bytes: []const u8) !void {
        try self.bytes(field_number, msg_bytes);
    }

    /// Write a fixed32 field
    pub fn fixed32(self: *Writer, field_number: u32, value: u32) !void {
        if (value == 0) return;
        const tag = makeTag(field_number, .fixed32);
        try encodeVarint(&self.buf, self.allocator, tag);
        var raw: [4]u8 = undefined;
        std.mem.writeInt(u32, &raw, value, .little);
        try self.buf.appendSlice(self.allocator, &raw);
    }

    pub fn bytes_(self: *Writer) []const u8 {
        return self.buf.items;
    }

    pub fn toOwnedSlice(self: *Writer) ![]u8 {
        return self.buf.toOwnedSlice(self.allocator);
    }

    pub fn clear(self: *Writer) void {
        self.buf.clearRetainingCapacity();
    }
};

// ─── Tests ───────────────────────────────────────────────────────────────────

const testing = std.testing;

test "varint encode/decode roundtrip" {
    const values = [_]u64{ 0, 1, 127, 128, 255, 300, 16384, 0xFFFFFFFF, 0xFFFFFFFFFFFFFFFF };
    for (values) |v| {
        var buf = std.ArrayList(u8).initCapacity(testing.allocator, 16) catch unreachable;
        defer buf.deinit(testing.allocator);
        try encodeVarint(&buf, testing.allocator, v);
        const result = try decodeVarint(buf.items);
        try testing.expectEqual(v, result.value);
    }
}

test "zigzag encode/decode roundtrip" {
    const values = [_]i64{ 0, -1, 1, -2, 2, -63, 64, -64, 64, -2147483648, 2147483647, -9223372036854775808, 9223372036854775807 };
    for (values) |v| {
        const encoded = zigZagEncode(v);
        const decoded = zigZagDecode(encoded);
        try testing.expectEqual(v, decoded);
    }
}

test "tag encoding" {
    try testing.expectEqual(@as(u64, 8), makeTag(1, .varint));
    try testing.expectEqual(@as(u64, 18), makeTag(2, .length_delimited));
    try testing.expectEqual(@as(u64, 120), makeTag(15, .varint));
}

test "write/read varint field" {
    var w = Writer.init(testing.allocator);
    defer w.deinit();

    try w.v(1, 300);
    try w.v(3, 150);

    var r = Reader.init(w.bytes_());

    const tag1 = try r.nextTag();
    try testing.expectEqual(@as(u32, 1), tag1.field);
    try testing.expectEqual(.varint, tag1.wire_type);
    const val1 = try r.readU64();
    try testing.expectEqual(@as(u64, 300), val1);

    const tag3 = try r.nextTag();
    try testing.expectEqual(@as(u32, 3), tag3.field);
    const val3 = try r.readU64();
    try testing.expectEqual(@as(u64, 150), val3);
}

test "write/read bytes field" {
    var w = Writer.init(testing.allocator);
    defer w.deinit();

    const payload = "hello world";
    try w.bytes(2, payload);

    var r = Reader.init(w.bytes_());
    const tag = try r.nextTag();
    try testing.expectEqual(@as(u32, 2), tag.field);
    try testing.expectEqual(.length_delimited, tag.wire_type);
    const data = try r.readBytes();
    try testing.expectEqualStrings(payload, data);
}

test "write/read sint field" {
    var w = Writer.init(testing.allocator);
    defer w.deinit();

    try w.sint32(1, -1);
    try w.sint64(2, -300);

    var r = Reader.init(w.bytes_());
    _ = try r.nextTag();
    const v1 = try r.readI32();
    try testing.expectEqual(@as(i32, -1), v1);

    _ = try r.nextTag();
    const v2 = try r.readI64();
    try testing.expectEqual(@as(i64, -300), v2);
}

test "skip field" {
    var buf = std.ArrayList(u8).initCapacity(testing.allocator, 64) catch unreachable;
    defer buf.deinit(testing.allocator);

    try encodeVarint(&buf, testing.allocator, makeTag(1, .varint));
    try encodeVarint(&buf, testing.allocator, 42);
    try encodeVarint(&buf, testing.allocator, makeTag(2, .length_delimited));
    try encodeVarint(&buf, testing.allocator, 5);
    try buf.appendSlice(testing.allocator, "hello");

    var r = Reader.init(buf.items);

    const tag1 = try r.nextTag();
    try r.skip(tag1.wire_type);

    const tag2 = try r.nextTag();
    try r.skip(tag2.wire_type);

    try testing.expectEqual(false, r.hasMore());
}
