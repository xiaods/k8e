const std = @import("std");
const Compression = @import("compression.zig").Compression;

pub const header_size = 5;

pub fn encode(allocator: std.mem.Allocator, payload: []const u8) ![]u8 {
    return encodeWithCompression(allocator, payload, .identity);
}

pub fn encodeWithCompression(
    allocator: std.mem.Allocator,
    payload: []const u8,
    compression: Compression,
) ![]u8 {
    if (compression == .identity) return encodePayload(allocator, payload, false);

    var output = try std.Io.Writer.Allocating.initCapacity(allocator, 64);
    defer output.deinit();
    const history = try allocator.alloc(u8, std.compress.flate.max_window_len);
    defer allocator.free(history);
    var compressor = std.compress.flate.Compress.init(
        &output.writer,
        history,
        .gzip,
        .default,
    ) catch return error.OutOfMemory;
    compressor.writer.writeAll(payload) catch return error.OutOfMemory;
    compressor.finish() catch return error.OutOfMemory;
    return encodePayload(allocator, output.written(), true);
}

fn encodePayload(allocator: std.mem.Allocator, payload: []const u8, compressed: bool) ![]u8 {
    if (payload.len > std.math.maxInt(u32)) return error.MessageTooLarge;

    const frame = try allocator.alloc(u8, header_size + payload.len);
    frame[0] = @intFromBool(compressed);
    const length: u32 = @intCast(payload.len);
    frame[1] = @truncate(length >> 24);
    frame[2] = @truncate(length >> 16);
    frame[3] = @truncate(length >> 8);
    frame[4] = @truncate(length);
    @memcpy(frame[header_size..], payload);
    return frame;
}

pub const Decoder = struct {
    pub const DecodedMessage = struct {
        /// Allocated by the decoder allocator and owned by the caller.
        payload: []u8,
        /// Complete wire size, including the five-byte message header.
        consumed_bytes: usize,
        compressed: bool,
    };

    allocator: std.mem.Allocator,
    max_message_size: usize,
    compression: Compression,
    buffer: std.ArrayList(u8) = .empty,
    offset: usize = 0,

    pub fn init(allocator: std.mem.Allocator, max_message_size: usize) Decoder {
        return initWithCompression(allocator, max_message_size, .identity);
    }

    pub fn initWithCompression(
        allocator: std.mem.Allocator,
        max_message_size: usize,
        compression: Compression,
    ) Decoder {
        return .{
            .allocator = allocator,
            .max_message_size = max_message_size,
            .compression = compression,
        };
    }

    pub fn deinit(self: *Decoder) void {
        self.buffer.deinit(self.allocator);
        self.* = undefined;
    }

    pub fn feed(self: *Decoder, bytes: []const u8) !void {
        return self.feedBounded(bytes, std.math.maxInt(usize));
    }

    /// Appends bytes only when the total unconsumed wire data remains within limit.
    pub fn feedBounded(self: *Decoder, bytes: []const u8, max_buffered_bytes: usize) !void {
        const buffered = self.bufferedBytes();
        if (buffered > max_buffered_bytes or bytes.len > max_buffered_bytes - buffered) {
            return error.BufferLimitExceeded;
        }

        if (self.offset == self.buffer.items.len) {
            self.buffer.clearRetainingCapacity();
            self.offset = 0;
        } else if (self.offset > 0 and self.offset >= self.buffer.items.len / 2) {
            const remaining = self.buffer.items[self.offset..];
            std.mem.copyForwards(u8, self.buffer.items[0..remaining.len], remaining);
            self.buffer.shrinkRetainingCapacity(remaining.len);
            self.offset = 0;
        }
        try self.buffer.appendSlice(self.allocator, bytes);
    }

    /// Returns the exact number of unconsumed wire bytes held by the decoder.
    pub fn bufferedBytes(self: *const Decoder) usize {
        return self.buffer.items.len - self.offset;
    }

    pub fn nextMessage(self: *Decoder) !?DecodedMessage {
        const available = self.buffer.items[self.offset..];
        if (available.len < header_size) return null;
        if (available[0] > 1) return error.InvalidCompressedFlag;
        const compressed = available[0] == 1;
        if (compressed and self.compression != .gzip) return error.CompressionMismatch;

        const length = (@as(u32, available[1]) << 24) |
            (@as(u32, available[2]) << 16) |
            (@as(u32, available[3]) << 8) |
            @as(u32, available[4]);
        if (!compressed and length > self.max_message_size) return error.MessageTooLarge;

        const end = header_size + @as(usize, length);
        if (available.len < end) return null;

        const payload = if (compressed)
            try decompressGzip(
                self.allocator,
                available[header_size..end],
                self.max_message_size,
            )
        else
            try self.allocator.dupe(u8, available[header_size..end]);
        self.offset += end;
        return .{
            .payload = payload,
            .consumed_bytes = end,
            .compressed = compressed,
        };
    }

    pub fn next(self: *Decoder) !?[]u8 {
        const message = try self.nextMessage() orelse return null;
        return message.payload;
    }

    pub fn finish(self: *const Decoder) !void {
        if (self.bufferedBytes() != 0) return error.TruncatedFrame;
    }
};

pub fn decodeUnary(
    allocator: std.mem.Allocator,
    bytes: []const u8,
    max_message_size: usize,
) ![]u8 {
    return decodeUnaryWithCompression(allocator, bytes, max_message_size, .identity);
}

pub fn decodeUnaryWithCompression(
    allocator: std.mem.Allocator,
    bytes: []const u8,
    max_message_size: usize,
    compression: Compression,
) ![]u8 {
    var decoder = Decoder.initWithCompression(allocator, max_message_size, compression);
    defer decoder.deinit();

    try decoder.feed(bytes);
    const payload = try decoder.next() orelse return error.TruncatedFrame;
    errdefer allocator.free(payload);
    if (try decoder.next()) |extra| {
        allocator.free(extra);
        return error.ExpectedUnaryMessage;
    }
    try decoder.finish();
    return payload;
}

fn decompressGzip(
    allocator: std.mem.Allocator,
    payload: []const u8,
    max_message_size: usize,
) ![]u8 {
    var input: std.Io.Reader = .fixed(payload);
    const history = try allocator.alloc(u8, std.compress.flate.max_window_len);
    defer allocator.free(history);
    var decompressor: std.compress.flate.Decompress = .init(&input, .gzip, history);
    const limit: std.Io.Limit = if (max_message_size == std.math.maxInt(usize))
        .unlimited
    else
        .limited(max_message_size + 1);
    const decompressed = decompressor.reader.allocRemaining(allocator, limit) catch |err| switch (err) {
        error.OutOfMemory => return error.OutOfMemory,
        error.StreamTooLong => return error.MessageTooLarge,
        else => return error.MalformedCompressedMessage,
    };
    errdefer allocator.free(decompressed);
    if (decompressed.len > max_message_size) return error.MessageTooLarge;
    if (input.seek != input.end) return error.MalformedCompressedMessage;
    return decompressed;
}

test "frame round trips empty and binary payloads" {
    for ([_][]const u8{ "", "abc\x00xyz" }) |payload| {
        const encoded = try encode(std.testing.allocator, payload);
        defer std.testing.allocator.free(encoded);
        const decoded = try decodeUnary(std.testing.allocator, encoded, 1024);
        defer std.testing.allocator.free(decoded);
        try std.testing.expectEqualSlices(u8, payload, decoded);
    }
}

test "frame round trips a multi-byte payload length" {
    const payload = try std.testing.allocator.alloc(u8, 70_000);
    defer std.testing.allocator.free(payload);
    @memset(payload, 0x5a);

    const encoded = try encode(std.testing.allocator, payload);
    defer std.testing.allocator.free(encoded);
    const decoded = try decodeUnary(std.testing.allocator, encoded, payload.len);
    defer std.testing.allocator.free(decoded);
    try std.testing.expectEqualSlices(u8, payload, decoded);
}

test "gzip frame round trips and marks compressed payload" {
    const encoded = try encodeWithCompression(std.testing.allocator, "compress me compress me", .gzip);
    defer std.testing.allocator.free(encoded);
    try std.testing.expectEqual(@as(u8, 1), encoded[0]);

    const decoded = try decodeUnaryWithCompression(std.testing.allocator, encoded, 1024, .gzip);
    defer std.testing.allocator.free(decoded);
    try std.testing.expectEqualStrings("compress me compress me", decoded);
}

test "gzip decoding rejects malformed data and compression mismatches" {
    try std.testing.expectError(
        error.MalformedCompressedMessage,
        decodeUnaryWithCompression(
            std.testing.allocator,
            &.{ 1, 0, 0, 0, 3, 1, 2, 3 },
            1024,
            .gzip,
        ),
    );
    try std.testing.expectError(
        error.CompressionMismatch,
        decodeUnaryWithCompression(std.testing.allocator, &.{ 1, 0, 0, 0, 0 }, 1024, .identity),
    );
    const uncompressed = try decodeUnaryWithCompression(
        std.testing.allocator,
        &.{ 0, 0, 0, 0, 0 },
        1024,
        .gzip,
    );
    defer std.testing.allocator.free(uncompressed);
    try std.testing.expectEqual(@as(usize, 0), uncompressed.len);
}

test "gzip decoding enforces decompressed message size" {
    const encoded = try encodeWithCompression(std.testing.allocator, "123456789", .gzip);
    defer std.testing.allocator.free(encoded);
    try std.testing.expectError(
        error.MessageTooLarge,
        decodeUnaryWithCompression(std.testing.allocator, encoded, 8, .gzip),
    );
}

test "decoder accepts fragmented input and multiple messages" {
    const first = try encode(std.testing.allocator, "first");
    defer std.testing.allocator.free(first);
    const second = try encode(std.testing.allocator, "second");
    defer std.testing.allocator.free(second);

    var decoder = Decoder.init(std.testing.allocator, 1024);
    defer decoder.deinit();
    try decoder.feed(first[0..2]);
    try std.testing.expectEqual(@as(?[]u8, null), try decoder.next());
    try decoder.feed(first[2..]);
    try decoder.feed(second);

    const first_payload = (try decoder.next()).?;
    defer std.testing.allocator.free(first_payload);
    const second_payload = (try decoder.next()).?;
    defer std.testing.allocator.free(second_payload);
    try std.testing.expectEqualStrings("first", first_payload);
    try std.testing.expectEqualStrings("second", second_payload);
    try decoder.finish();
}

test "streaming decoder reports wire consumption and compression" {
    const identity = try encode(std.testing.allocator, "first");
    defer std.testing.allocator.free(identity);
    const gzip = try encodeWithCompression(std.testing.allocator, "second second second", .gzip);
    defer std.testing.allocator.free(gzip);
    const empty = try encode(std.testing.allocator, "");
    defer std.testing.allocator.free(empty);

    var decoder = Decoder.initWithCompression(std.testing.allocator, 1024, .gzip);
    defer decoder.deinit();

    try decoder.feed(identity[0..2]);
    try std.testing.expectEqual(@as(usize, 2), decoder.bufferedBytes());
    try std.testing.expectEqual(@as(?Decoder.DecodedMessage, null), try decoder.nextMessage());
    try decoder.feed(identity[2..6]);
    try std.testing.expectEqual(@as(?Decoder.DecodedMessage, null), try decoder.nextMessage());
    try decoder.feed(identity[6..]);
    try decoder.feed(gzip[0..header_size]);
    try decoder.feed(gzip[header_size .. gzip.len - 1]);

    const first = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(first.payload);
    try std.testing.expectEqualStrings("first", first.payload);
    try std.testing.expectEqual(identity.len, first.consumed_bytes);
    try std.testing.expect(!first.compressed);
    try std.testing.expectEqual(gzip.len - 1, decoder.bufferedBytes());
    try std.testing.expectEqual(@as(?Decoder.DecodedMessage, null), try decoder.nextMessage());

    try decoder.feed(gzip[gzip.len - 1 ..]);
    try decoder.feed(empty);
    const second = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(second.payload);
    try std.testing.expectEqualStrings("second second second", second.payload);
    try std.testing.expectEqual(gzip.len, second.consumed_bytes);
    try std.testing.expect(second.compressed);

    const third = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(third.payload);
    try std.testing.expectEqual(@as(usize, 0), third.payload.len);
    try std.testing.expectEqual(header_size, third.consumed_bytes);
    try std.testing.expect(!third.compressed);
    try std.testing.expectEqual(@as(usize, 0), decoder.bufferedBytes());
    try decoder.finish();
}

test "decoder enforces buffered byte admission atomically" {
    var decoder = Decoder.init(std.testing.allocator, 1024);
    defer decoder.deinit();

    try decoder.feedBounded(&.{ 0, 0, 0 }, header_size);
    try std.testing.expectEqual(@as(usize, 3), decoder.bufferedBytes());
    try std.testing.expectError(error.BufferLimitExceeded, decoder.feedBounded(&.{ 0, 0, 0 }, header_size));
    try std.testing.expectEqual(@as(usize, 3), decoder.bufferedBytes());
    try decoder.feedBounded(&.{ 0, 0 }, header_size);

    const first = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(first.payload);
    try std.testing.expectEqual(@as(usize, 0), decoder.bufferedBytes());

    try decoder.feedBounded(&.{ 0, 0, 0, 0, 0 }, header_size);
    const second = (try decoder.nextMessage()).?;
    defer std.testing.allocator.free(second.payload);
    try std.testing.expectEqual(@as(usize, 0), decoder.bufferedBytes());
    try decoder.finish();
}

test "decoder reports malformed frames" {
    var decoder = Decoder.init(std.testing.allocator, 3);
    defer decoder.deinit();

    try decoder.feed(&.{ 1, 0, 0, 0, 0 });
    try std.testing.expectError(error.CompressionMismatch, decoder.next());

    decoder.buffer.clearRetainingCapacity();
    decoder.offset = 0;
    try decoder.feed(&.{ 0, 0, 0, 0, 4 });
    try std.testing.expectError(error.MessageTooLarge, decoder.next());

    decoder.buffer.clearRetainingCapacity();
    decoder.offset = 0;
    try decoder.feed(&.{ 2, 0, 0, 0, 0 });
    try std.testing.expectError(error.InvalidCompressedFlag, decoder.next());
}

test "decoder enforces decompressed size limits" {
    const payload = try std.testing.allocator.alloc(u8, 4096);
    defer std.testing.allocator.free(payload);
    @memset(payload, 'a');
    const encoded = try encodeWithCompression(std.testing.allocator, payload, .gzip);
    defer std.testing.allocator.free(encoded);
    const compressed_length = encoded.len - header_size;
    try std.testing.expect(compressed_length < payload.len);

    var decompressed_decoder = Decoder.initWithCompression(std.testing.allocator, compressed_length, .gzip);
    defer decompressed_decoder.deinit();
    try decompressed_decoder.feed(encoded);
    try std.testing.expectError(error.MessageTooLarge, decompressed_decoder.nextMessage());
}

test "decoder finish rejects fragmented header and payload" {
    var header_decoder = Decoder.init(std.testing.allocator, 1024);
    defer header_decoder.deinit();
    try header_decoder.feed(&.{ 0, 0, 0, 0 });
    try std.testing.expectError(error.TruncatedFrame, header_decoder.finish());

    var payload_decoder = Decoder.init(std.testing.allocator, 1024);
    defer payload_decoder.deinit();
    try payload_decoder.feed(&.{ 0, 0, 0, 0, 2, 'a' });
    try std.testing.expectEqual(@as(?Decoder.DecodedMessage, null), try payload_decoder.nextMessage());
    try std.testing.expectError(error.TruncatedFrame, payload_decoder.finish());
}

test "unary decoding rejects truncated and repeated messages" {
    try std.testing.expectError(
        error.TruncatedFrame,
        decodeUnary(std.testing.allocator, &.{ 0, 0, 0, 0, 1 }, 1024),
    );

    const first = try encode(std.testing.allocator, "one");
    defer std.testing.allocator.free(first);
    const second = try encode(std.testing.allocator, "two");
    defer std.testing.allocator.free(second);
    const combined = try std.mem.concat(std.testing.allocator, u8, &.{ first, second });
    defer std.testing.allocator.free(combined);
    try std.testing.expectError(
        error.ExpectedUnaryMessage,
        decodeUnary(std.testing.allocator, combined, 1024),
    );
}
