// Protobuf wire format tests
const std = @import("std");
const wire = @import("wire.zig");

test "varint edge cases" {
    const testing = std.testing;

    var buf = std.ArrayList(u8).initCapacity(testing.allocator, 16) catch unreachable;
    defer buf.deinit(testing.allocator);
    try wire.encodeVarint(&buf, testing.allocator, 0);
    try testing.expectEqual(@as(usize, 1), buf.items.len);
    try testing.expectEqual(@as(u8, 0), buf.items[0]);

    buf.clearRetainingCapacity();
    try wire.encodeVarint(&buf, testing.allocator, std.math.maxInt(u64));
    const result = try wire.decodeVarint(buf.items);
    try testing.expectEqual(std.math.maxInt(u64), result.value);
}

test "length-delimited field roundtrip" {
    const testing = std.testing;

    var w = wire.Writer.init(testing.allocator);
    defer w.deinit();

    const payload = "\x00\x01\x02\xff\xfe\xfd";
    try w.bytes(5, payload);

    var r = wire.Reader.init(w.bytes_());
    const tag = try r.nextTag();
    try testing.expectEqual(@as(u32, 5), tag.field);
    try testing.expectEqual(.length_delimited, tag.wire_type);
    const data = try r.readBytes();
    try testing.expectEqualSlices(u8, payload, data);
}

test "bool field roundtrip" {
    const testing = std.testing;

    var w = wire.Writer.init(testing.allocator);
    defer w.deinit();

    try w.b(1, true);
    try w.b(2, false); // skipped in proto3

    var r = wire.Reader.init(w.bytes_());
    const tag = try r.nextTag();
    try testing.expectEqual(@as(u32, 1), tag.field);
    const val = try r.readBool();
    try testing.expect(val);

    try testing.expect(!r.hasMore());
}

test "fixed32 field roundtrip" {
    const testing = std.testing;

    var w = wire.Writer.init(testing.allocator);
    defer w.deinit();

    try w.fixed32(1, 0xDEADBEEF);
    try w.fixed32(2, 0); // skipped

    var r = wire.Reader.init(w.bytes_());
    const tag = try r.nextTag();
    try testing.expectEqual(@as(u32, 1), tag.field);
    try testing.expectEqual(.fixed32, tag.wire_type);
    const val = try r.readFixedU32();
    try testing.expectEqual(@as(u32, 0xDEADBEEF), val);
}

test "complex message with multiple fields" {
    const testing = std.testing;

    var w = wire.Writer.init(testing.allocator);
    defer w.deinit();

    try w.v(1, 150);
    try w.string(2, "testing");

    var inner = wire.Writer.init(testing.allocator);
    defer inner.deinit();
    try inner.v(1, 42);
    try w.msg(3, inner.bytes_());

    var r = wire.Reader.init(w.bytes_());

    const tag1 = try r.nextTag();
    try testing.expectEqual(@as(u32, 1), tag1.field);
    try testing.expectEqual(@as(u64, 150), try r.readU64());

    const tag2 = try r.nextTag();
    try testing.expectEqual(@as(u32, 2), tag2.field);
    try testing.expectEqualStrings("testing", try r.readString());

    const tag3 = try r.nextTag();
    try testing.expectEqual(@as(u32, 3), tag3.field);
    const inner_bytes = try r.readBytes();
    var inner_r = wire.Reader.init(inner_bytes);
    _ = try inner_r.nextTag();
    try testing.expectEqual(@as(u64, 42), try inner_r.readU64());
}
