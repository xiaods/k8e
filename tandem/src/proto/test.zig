// Protobuf wire format tests
const std = @import("std");
const wire = @import("wire.zig");
const messages = @import("messages.zig");

test "RangeStreamResponse wraps RangeResponse as field 1" {
    const testing = std.testing;

    // RangeStreamResponse { range_response: RangeResponse { header: { revision: 5 }, count: 3, more: false } }
    const resp = messages.RangeStreamResponse{
        .range = .{
            .header = .{ .revision = 5 },
            .kvs = &[_]messages.KeyValue{},
            .more = false,
            .count = 3,
        },
    };
    const bytes = try resp.encode(testing.allocator);
    defer testing.allocator.free(bytes);

    // Decode and verify the wrapper structure.
    var r = wire.Reader.init(bytes);
    const tag = try r.nextTag();
    try testing.expectEqual(@as(u32, 1), tag.field); // range_response is field 1
    try testing.expectEqual(.length_delimited, tag.wire_type);

    // Inner RangeResponse: header (msg, field 1), more (bool, field 3), count (varint, field 4).
    const inner = try r.readBytes();
    var inner_r = wire.Reader.init(inner);

    const hdr_tag = try inner_r.nextTag();
    try testing.expectEqual(@as(u32, 1), hdr_tag.field); // header
    const hdr_bytes = try inner_r.readBytes();
    var hdr_r = wire.Reader.init(hdr_bytes);
    const rev_tag = try hdr_r.nextTag();
    try testing.expectEqual(@as(u32, 3), rev_tag.field); // revision in ResponseHeader
    try testing.expectEqual(@as(i64, 5), @as(i64, @intCast(try hdr_r.readU64())));

    const count_tag = try inner_r.nextTag();
    try testing.expectEqual(@as(u32, 4), count_tag.field); // count
    try testing.expectEqual(@as(i64, 3), @as(i64, @intCast(try inner_r.readU64())));

    try testing.expect(!inner_r.hasMore());
}

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
