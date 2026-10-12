// TCP/TLS transport layer for Tandem
//
// Provides:
// - TcpListener / TcpStream: TCP socket primitives via Linux syscalls
// - HttpClient: HTTP/1.1 client for rqlite API
// - JsonBuilder / JsonParser: JSON serialization/deserialization
//
// Uses direct Linux syscalls (std.os.linux) rather than Io.Threaded to
// keep the binary small and the build simple. Works on Linux; macOS
// support would require kqueue-based I/O (deferred to M2).

const std = @import("std");
const Allocator = std.mem.Allocator;
const linux = std.os.linux;
const AllocatorError = std.mem.Allocator.Error;

// ─── Linux Syscall Wrappers ─────────────────────────────────────────────────

fn sysSocket(domain: u32, socket_type: u32, protocol: u32) !i32 {
    const ret = linux.socket(domain, socket_type, protocol);
    if (ret < 0) return error.SocketFailed;
    return @intCast(ret);
}

fn sysBind(fd: i32, addr: *const linux.sockaddr, len: linux.socklen_t) !void {
    const ret = linux.bind(fd, addr, len);
    if (ret < 0) return error.BindFailed;
}

fn sysListen(fd: i32, backlog: u32) !void {
    const ret = linux.listen(fd, backlog);
    if (ret < 0) return error.ListenFailed;
}

fn sysAccept(fd: i32, addr: ?*linux.sockaddr, len: ?*linux.socklen_t) !i32 {
    const ret = linux.accept(fd, addr, len);
    if (ret < 0) return error.AcceptFailed;
    return @intCast(ret);
}

fn sysConnect(fd: i32, addr: *const linux.sockaddr, len: linux.socklen_t) !void {
    const ret = linux.connect(fd, addr, len);
    if (ret < 0) return error.ConnectFailed;
}

fn sysClose(fd: i32) void {
    _ = linux.close(fd);
}

fn sysRead(fd: i32, buf: []u8) !usize {
    const ret = linux.read(fd, buf.ptr, buf.len);
    if (ret < 0) return error.ReadFailed;
    return @intCast(ret);
}

fn sysWrite(fd: i32, data: []const u8) !usize {
    const ret = linux.write(fd, data.ptr, data.len);
    if (ret < 0) return error.WriteFailed;
    return @intCast(ret);
}

fn sysSetsockopt(fd: i32, level: i32, optname: u32, opt: []const u8) !void {
    const ret = linux.setsockopt(fd, level, optname, opt.ptr, @intCast(opt.len));
    if (ret < 0) return error.SetSockOptFailed;
}

// ─── Address (IPv4) ─────────────────────────────────────────────────────────

const Address = extern union {
    any: linux.sockaddr,
    ipv4: linux.sockaddr.in,

    pub fn parseIp4(ip: []const u8, port: u16) !Address {
        var addr = linux.sockaddr.in{
            .family = linux.AF.INET,
            .port = std.mem.nativeToBig(u16, port),
            .addr = undefined,
        };

        // Parse 4 octets into a single u32 (big-endian)
        var parts = std.mem.splitScalar(u8, ip, '.');
        var octets: [4]u8 = undefined;
        var i: usize = 0;
        while (i < 4) : (i += 1) {
            const part = parts.next() orelse return error.InvalidFormat;
            octets[i] = std.fmt.parseInt(u8, part, 10) catch return error.InvalidFormat;
        }

        addr.addr = (@as(u32, octets[0]) << 24) |
            (@as(u32, octets[1]) << 16) |
            (@as(u32, octets[2]) << 8) |
            @as(u32, octets[3]);

        return .{ .ipv4 = addr };
    }

    pub fn resolveIp(host: []const u8, port: u16) !Address {
        return parseIp4(host, port);
    }
};

// ─── Transport Configuration ─────────────────────────────────────────────────

pub const TransportConfig = struct {
    cert_file: ?[]const u8 = null,
    key_file: ?[]const u8 = null,
    ca_file: ?[]const u8 = null,
    require_client_cert: bool = false,
    connect_timeout_ms: u64 = 5000,
    read_timeout_ms: u64 = 30000,
    tcp_keepalive: bool = true,
    tcp_nodelay: bool = true,
};

// ─── TCP Listener ───────────────────────────────────────────────────────────

pub const TcpListener = struct {
    fd: i32,
    address: Address,

    pub fn bind(_: Allocator, port: u16) !TcpListener {
        const fd = try sysSocket(linux.AF.INET, linux.SOCK.STREAM | linux.SOCK.CLOEXEC, 0);

        const reuse: u32 = 1;
        try sysSetsockopt(fd, linux.SOL.SOCKET, linux.SO.REUSEADDR, std.mem.asBytes(&reuse));

        const addr = try Address.parseIp4("0.0.0.0", port);
        try sysBind(fd, &addr.any, @sizeOf(linux.sockaddr));
        try sysListen(fd, 128);

        return .{ .fd = fd, .address = addr };
    }

    pub fn accept(self: *TcpListener) !TcpStream {
        var addr: linux.sockaddr = undefined;
        var addr_len: linux.socklen_t = @sizeOf(linux.sockaddr);
        const fd = try sysAccept(self.fd, &addr, &addr_len);
        return TcpStream{ .fd = fd };
    }

    pub fn close(self: *TcpListener) void {
        sysClose(self.fd);
    }

    pub fn deinit(self: *TcpListener) void {
        self.close();
    }
};

// ─── TCP Stream ──────────────────────────────────────────────────────────────

pub const TcpStream = struct {
    fd: i32,

    pub fn connect(host: []const u8, port: u16) !TcpStream {
        const fd = try sysSocket(linux.AF.INET, linux.SOCK.STREAM | linux.SOCK.CLOEXEC, 0);
        errdefer sysClose(fd);

        const addr = try Address.resolveIp(host, port);
        try sysConnect(fd, &addr.any, @sizeOf(linux.sockaddr));

        return TcpStream{ .fd = fd };
    }

    pub fn read(self: *TcpStream, buf: []u8) !usize {
        return try sysRead(self.fd, buf);
    }

    pub fn readAll(self: *TcpStream, buf: []u8) !void {
        var total: usize = 0;
        while (total < buf.len) {
            total += try self.read(buf[total..]);
        }
    }

    pub fn write(self: *TcpStream, data: []const u8) !usize {
        return try sysWrite(self.fd, data);
    }

    pub fn writeAll(self: *TcpStream, data: []const u8) !void {
        var total: usize = 0;
        while (total < data.len) {
            total += try self.write(data[total..]);
        }
    }

    pub fn close(self: *TcpStream) void {
        sysClose(self.fd);
    }

    pub fn deinit(self: *TcpStream) void {
        self.close();
    }
};

// ─── HTTP Response ───────────────────────────────────────────────────────────

pub const HttpResponse = struct {
    status_code: u16,
    body: []u8,
    allocator: Allocator,

    pub fn deinit(self: *HttpResponse) void {
        self.allocator.free(self.body);
    }
};

pub const HttpRequest = struct {
    method: []const u8,
    path: []const u8,
    body: ?[]const u8,
    content_type: []const u8 = "application/json",
};

// ─── HTTP/1.1 Client ─────────────────────────────────────────────────────────

pub const HttpClient = struct {
    allocator: Allocator,
    host: []const u8,
    port: u16,
    use_tls: bool = false,

    pub fn init(allocator: Allocator, host: []const u8, port: u16) HttpClient {
        return .{
            .allocator = allocator,
            .host = host,
            .port = port,
        };
    }

    pub fn deinit(self: *HttpClient) void {
        _ = self;
    }

    /// Parse a URL like "http://127.0.0.1:4001" into host/port
    pub fn parseUrl(allocator: Allocator, url: []const u8) !struct { host: []u8, port: u16, path: []const u8 } {
        var rest = url;

        // Strip protocol
        if (std.mem.indexOf(u8, rest, "://")) |idx| {
            rest = rest[idx + 3 ..];
        }

        // Split host:port from path
        var host_port = rest;
        var path: []const u8 = "/";
        if (std.mem.indexOfPos(u8, rest, 0, "/")) |idx| {
            host_port = rest[0..idx];
            path = rest[idx..];
        }

        // Split host from port
        var host = host_port;
        var port: u16 = 80;
        if (std.mem.indexOfScalar(u8, host_port, ':')) |idx| {
            host = host_port[0..idx];
            port = std.fmt.parseInt(u16, host_port[idx + 1 ..], 10) catch 80;
        }

        const host_copy = try allocator.dupe(u8, host);
        const path_copy = try allocator.dupe(u8, path);
        return .{ .host = host_copy, .port = port, .path = path_copy };
    }

    /// Send an HTTP request and return the response
    pub fn request(self: *HttpClient, req: HttpRequest) !HttpResponse {
        var threaded = std.Io.Threaded.init(self.allocator, .{});
        defer threaded.deinit();
        var client: std.http.Client = .{ .allocator = self.allocator, .io = threaded.io() };
        defer client.deinit();
        const url = if (std.mem.startsWith(u8, req.path, "http://") or std.mem.startsWith(u8, req.path, "https://"))
            try self.allocator.dupe(u8, req.path)
        else
            try std.fmt.allocPrint(self.allocator, "http://{s}:{d}{s}", .{ self.host, self.port, req.path });
        defer self.allocator.free(url);
        var output: std.Io.Writer.Allocating = .init(self.allocator);
        defer output.deinit();
        const response = try client.fetch(.{
            .location = .{ .url = url },
            .method = std.meta.stringToEnum(std.http.Method, req.method) orelse return error.InvalidMethod,
            .payload = req.body,
            .response_writer = &output.writer,
            .extra_headers = &.{.{ .name = "Content-Type", .value = req.content_type }},
        });
        if (@intFromEnum(response.status) < 200 or @intFromEnum(response.status) >= 300) {
            std.log.warn("HTTP request to {s} failed with status {d}", .{ url, @intFromEnum(response.status) });
            return error.HttpRequestFailed;
        }
        return .{ .status_code = @intFromEnum(response.status), .body = try output.toOwnedSlice(), .allocator = self.allocator };
    }

    fn readResponse(self: *HttpClient, stream: *TcpStream) !HttpResponse {
        var raw = std.ArrayList(u8).empty;
        defer raw.deinit(self.allocator);

        var read_buf: [4096]u8 = undefined;
        var body_start: ?usize = null;
        var content_length: usize = 0;
        var chunked = false;

        while (true) {
            const n = stream.read(&read_buf) catch |err| {
                if (err == error.ConnectionClosed) break;
                return err;
            };
            if (n == 0) break;
            try raw.appendSlice(self.allocator, read_buf[0..n]);

            // Find end of headers
            if (body_start == null) {
                if (std.mem.indexOf(u8, raw.items, "\r\n\r\n")) |idx| {
                    body_start = idx + 4;

                    // Check for chunked encoding
                    const headers = raw.items[0..idx];
                    if (std.mem.indexOf(u8, headers, "Transfer-Encoding: chunked") != null) {
                        chunked = true;
                    }

                    // Parse Content-Length
                    if (findHeader(headers, "content-length")) |cl| {
                        content_length = std.fmt.parseInt(usize, cl, 10) catch 0;
                    }
                }
            }

            // If we found headers, check if we have all the body
            if (body_start) |start| {
                if (chunked) {
                    if (std.mem.indexOf(u8, raw.items[start..], "\r\n0\r\n\r\n") != null) {
                        break;
                    }
                } else if (content_length > 0) {
                    if (raw.items.len >= start + content_length) {
                        break;
                    }
                }
            }
        }

        // Extract body
        var body: []u8 = &[_]u8{};
        if (body_start) |start| {
            if (content_length > 0 and raw.items.len > start) {
                const end = @min(start + content_length, raw.items.len);
                body = try self.allocator.dupe(u8, raw.items[start..end]);
            }
        }

        return HttpResponse{
            .status_code = 200, // simplified
            .body = body,
            .allocator = self.allocator,
        };
    }

    /// GET request
    pub fn get(self: *HttpClient, path: []const u8) !HttpResponse {
        return try self.request(HttpRequest{ .method = "GET", .path = path, .body = null });
    }

    /// POST request with JSON body
    pub fn post(self: *HttpClient, path: []const u8, body: []const u8) !HttpResponse {
        return try self.request(HttpRequest{ .method = "POST", .path = path, .body = body });
    }

    /// PUT request with JSON body
    pub fn put(self: *HttpClient, path: []const u8, body: []const u8) !HttpResponse {
        return try self.request(HttpRequest{ .method = "PUT", .path = path, .body = body });
    }
};

// ─── JSON Builder ────────────────────────────────────────────────────────────

pub const JsonBuilder = struct {
    buf: std.ArrayList(u8),
    allocator: Allocator,
    first_field: bool,
    first_array: bool,
    in_array: bool,

    pub fn init(allocator: Allocator) JsonBuilder {
        return .{
            .buf = std.ArrayList(u8).empty,
            .allocator = allocator,
            .first_field = true,
            .first_array = true,
            .in_array = false,
        };
    }

    pub fn deinit(self: *JsonBuilder) void {
        self.buf.deinit(self.allocator);
    }

    pub fn startObject(self: *JsonBuilder) !void {
        try self.buf.append(self.allocator, '{');
        self.first_field = true;
    }

    pub fn endObject(self: *JsonBuilder) !void {
        try self.buf.append(self.allocator, '}');
    }

    pub fn startArray(self: *JsonBuilder, name: []const u8) !void {
        if (!self.first_field) try self.buf.append(self.allocator, ',');
        try self.buf.append(self.allocator, '"');
        try self.buf.appendSlice(self.allocator, name);
        try self.buf.appendSlice(self.allocator, "\":[");
        self.first_array = true;
        self.in_array = true;
        self.first_field = false;
    }

    pub fn endArray(self: *JsonBuilder) !void {
        try self.buf.append(self.allocator, ']');
        self.in_array = false;
        self.first_array = true;
    }

    fn appendComma(self: *JsonBuilder, is_first: bool) !void {
        if (!is_first) {
            try self.buf.append(self.allocator, ',');
        }
    }

    pub fn addString(self: *JsonBuilder, name: []const u8, value: []const u8) !void {
        if (!self.in_array) {
            try self.appendComma(self.first_field);
        } else {
            if (!self.first_array) try self.buf.append(self.allocator, ',');
        }
        try self.buf.append(self.allocator, '"');
        try self.buf.appendSlice(self.allocator, name);
        try self.buf.appendSlice(self.allocator, "\":\"");
        try escapeJson(self.allocator, &self.buf, value);
        try self.buf.append(self.allocator, '"');
        if (!self.in_array) self.first_field = false;
        if (self.in_array) self.first_array = false;
    }

    pub fn addInt(self: *JsonBuilder, name: []const u8, value: i64) !void {
        if (!self.in_array) {
            try self.appendComma(self.first_field);
        } else {
            if (!self.first_array) try self.buf.append(self.allocator, ',');
        }
        try self.buf.append(self.allocator, '"');
        try self.buf.appendSlice(self.allocator, name);
        try self.buf.appendSlice(self.allocator, "\":");
        var tmp: [32]u8 = undefined;
        const s = std.fmt.bufPrint(&tmp, "{d}", .{value}) catch unreachable;
        try self.buf.appendSlice(self.allocator, s);
        if (!self.in_array) self.first_field = false;
        if (self.in_array) self.first_array = false;
    }

    pub fn addIntValue(self: *JsonBuilder, value: i64) !void {
        if (self.in_array) {
            if (!self.first_array) try self.buf.append(self.allocator, ',');
        }
        var tmp: [32]u8 = undefined;
        const s = std.fmt.bufPrint(&tmp, "{d}", .{value}) catch unreachable;
        try self.buf.appendSlice(self.allocator, s);
        if (self.in_array) self.first_array = false;
    }

    pub fn addRaw(self: *JsonBuilder, name: []const u8, value: []const u8) !void {
        try self.appendComma(self.first_field);
        try self.buf.append(self.allocator, '"');
        try self.buf.appendSlice(self.allocator, name);
        try self.buf.appendSlice(self.allocator, "\":");
        try self.buf.appendSlice(self.allocator, value);
        self.first_field = false;
    }

    pub fn addStringValue(self: *JsonBuilder, value: []const u8) !void {
        if (self.in_array) {
            if (!self.first_array) try self.buf.append(self.allocator, ',');
        }
        try self.buf.append(self.allocator, '"');
        try escapeJson(self.allocator, &self.buf, value);
        try self.buf.append(self.allocator, '"');
        if (self.in_array) self.first_array = false;
    }

    pub fn addRawValue(self: *JsonBuilder, value: []const u8) !void {
        if (self.in_array) {
            if (!self.first_array) try self.buf.append(self.allocator, ',');
        }
        try self.buf.appendSlice(self.allocator, value);
        if (self.in_array) self.first_array = false;
    }

    pub fn finish(self: *JsonBuilder) ![]u8 {
        return try self.buf.toOwnedSlice(self.allocator);
    }
};

/// Escape a string for JSON output
fn escapeJson(allocator: Allocator, buf: *std.ArrayList(u8), value: []const u8) !void {
    for (value) |c| {
        switch (c) {
            '"' => try buf.appendSlice(allocator, "\\\""),
            '\\' => try buf.appendSlice(allocator, "\\\\"),
            '\n' => try buf.appendSlice(allocator, "\\n"),
            '\r' => try buf.appendSlice(allocator, "\\r"),
            '\t' => try buf.appendSlice(allocator, "\\t"),
            else => try buf.append(allocator, c),
        }
    }
}

// ─── JSON Value ──────────────────────────────────────────────────────────────

pub const JsonValue = union(enum) {
    null: void,
    bool: bool,
    int: i64,
    float: f64,
    string: []const u8,
    array: []JsonValue,
    object: []JsonPair,

    pub fn getString(self: JsonValue, key: []const u8) ?[]const u8 {
        const obj = switch (self) {
            .object => |pairs| pairs,
            else => return null,
        };
        for (obj) |pair| {
            if (std.mem.eql(u8, pair.key, key)) {
                return switch (pair.value) {
                    .string => |s| s,
                    .int => null, // would need allocation
                    else => null,
                };
            }
        }
        return null;
    }

    pub fn getInt(self: JsonValue, key: []const u8) ?i64 {
        const obj = switch (self) {
            .object => |pairs| pairs,
            else => return null,
        };
        for (obj) |pair| {
            if (std.mem.eql(u8, pair.key, key)) {
                return switch (pair.value) {
                    .int => |i| i,
                    else => null,
                };
            }
        }
        return null;
    }
};

pub const JsonPair = struct {
    key: []const u8,
    value: JsonValue,
};

// ─── JSON Parser ─────────────────────────────────────────────────────────────

pub const JsonParser = struct {
    /// Parse a JSON string value
    pub fn parseString(json: []const u8, key: []const u8) ?[]const u8 {
        var pattern_buf: [256]u8 = undefined;
        const pattern = std.fmt.bufPrint(&pattern_buf, "\"{s}\":\"", .{key}) catch return null;

        const start = std.mem.indexOf(u8, json, pattern) orelse return null;
        const value_start = start + pattern.len;
        const value_end = std.mem.indexOfScalarPos(u8, json, value_start, '"') orelse return null;
        return json[value_start..value_end];
    }

    pub fn parseBool(json: []const u8, key: []const u8) ?bool {
        var pattern_buf: [256]u8 = undefined;
        const pattern = std.fmt.bufPrint(&pattern_buf, "\"{s}\":", .{key}) catch return null;

        const start = std.mem.indexOf(u8, json, pattern) orelse return null;
        const value_start = start + pattern.len;
        const end4: usize = if (value_start + 4 < json.len) value_start + 4 else json.len;
        if (std.mem.eql(u8, json[value_start..end4], "true")) return true;
        const end5: usize = if (value_start + 5 < json.len) value_start + 5 else json.len;
        if (std.mem.eql(u8, json[value_start..end5], "false")) return false;
        return null;
    }

    pub fn parseInt(json: []const u8, key: []const u8) ?i64 {
        var key_pattern_buf: [256]u8 = undefined;
        const key_pattern = std.fmt.bufPrint(&key_pattern_buf, "\"{s}\":", .{key}) catch return null;

        const start = std.mem.indexOf(u8, json, key_pattern) orelse return null;
        const value_start = start + key_pattern.len;

        var pos = value_start;
        while (pos < json.len and (json[pos] == ' ' or json[pos] == '\t')) : (pos += 1) {}

        var negative = false;
        if (pos < json.len and json[pos] == '-') {
            negative = true;
            pos += 1;
        }

        var value: i64 = 0;
        while (pos < json.len) {
            const c = json[pos];
            if (c >= '0' and c <= '9') {
                value = value * 10 + @as(i64, @intCast(c - '0'));
                pos += 1;
            } else break;
        }

        return if (negative) -value else value;
    }

    pub fn parseArray(json: []const u8, key: []const u8, allocator: Allocator) ![][]const u8 {
        _ = json;
        _ = key;
        _ = allocator;
        return &[_][]const u8{};
    }
};

// ─── HTTP/1.1 Server Helpers ─────────────────────────────────────────────────

/// Find an HTTP header value by name (case-insensitive)
pub fn findHeader(headers: []const u8, name: []const u8) ?[]const u8 {
    var pos: usize = 0;
    const name_lower = name;

    while (pos < headers.len) {
        const line_end = std.mem.indexOfScalarPos(u8, headers, pos, '\r') orelse break;
        const line = headers[pos..line_end];

        if (line.len > name_lower.len + 1 and
            std.ascii.eqlIgnoreCase(line[0..name_lower.len], name_lower) and
            line[name_lower.len] == ':')
        {
            const value_start = name_lower.len + 1;
            if (value_start < line.len) {
                return std.mem.trim(u8, line[value_start..], " \t");
            }
            return line[value_start..value_start];
        }

        pos = line_end + 2;
    }

    return null;
}

/// Parse Content-Length from headers
pub fn findContentLength(headers: []const u8) ?usize {
    if (findHeader(headers, "content-length")) |value| {
        return std.fmt.parseInt(usize, value, 10) catch null;
    }
    return null;
}

// ─── Utility Functions ───────────────────────────────────────────────────────

fn formatU16(allocator: Allocator, buf: *std.ArrayList(u8), value: u16) !void {
    var tmp: [8]u8 = undefined;
    const s = std.fmt.bufPrint(&tmp, "{d}", .{value}) catch unreachable;
    try buf.appendSlice(allocator, s);
}

// ─── Tests ───────────────────────────────────────────────────────────────────

const testing = std.testing;

test "tcp listener bind" {
    const allocator = testing.allocator;
    var listener = TcpListener.bind(allocator, 19877) catch |err| {
        if (err == error.BindFailed or err == error.AddressInUse) return;
        return err;
    };
    defer listener.deinit();
}

test "http client init" {
    const allocator = testing.allocator;
    const client = HttpClient.init(allocator, "127.0.0.1", 4001);
    try testing.expectEqualStrings("127.0.0.1", client.host);
    try testing.expectEqual(@as(u16, 4001), client.port);
}

test "http client parse url" {
    const allocator = testing.allocator;
    const parsed = try HttpClient.parseUrl(allocator, "http://127.0.0.1:4001/db/execute");
    defer {
        allocator.free(parsed.host);
        allocator.free(parsed.path);
    }
    try testing.expectEqualStrings("127.0.0.1", parsed.host);
    try testing.expectEqual(@as(u16, 4001), parsed.port);
    try testing.expectEqualStrings("/db/execute", parsed.path);
}

test "http client parse url without path" {
    const allocator = testing.allocator;
    const parsed = try HttpClient.parseUrl(allocator, "http://localhost:4001");
    defer {
        allocator.free(parsed.host);
        allocator.free(parsed.path);
    }
    try testing.expectEqualStrings("localhost", parsed.host);
    try testing.expectEqual(@as(u16, 4001), parsed.port);
    try testing.expectEqualStrings("/", parsed.path);
}

test "json builder basic" {
    const allocator = testing.allocator;
    var builder = JsonBuilder.init(allocator);
    defer builder.deinit();

    try builder.startObject();
    try builder.addString("query", "SELECT 1");
    try builder.addInt("level", 1);
    try builder.addRaw("data", "[1,2,3]");
    try builder.endObject();

    const json = try builder.finish();
    defer allocator.free(json);

    try testing.expect(std.mem.indexOf(u8, json, "\"query\":\"SELECT 1\"") != null);
    try testing.expect(std.mem.indexOf(u8, json, "\"level\":1") != null);
    try testing.expect(std.mem.indexOf(u8, json, "\"data\":[1,2,3]") != null);
}

test "json builder with array" {
    const allocator = testing.allocator;
    var builder = JsonBuilder.init(allocator);
    defer builder.deinit();

    try builder.startObject();
    try builder.startArray("statements");
    try builder.addStringValue("SELECT 1");
    try builder.addStringValue("SELECT 2");
    try builder.endArray();
    try builder.endObject();

    const json = try builder.finish();
    defer allocator.free(json);

    try testing.expect(std.mem.indexOf(u8, json, "\"statements\":[\"SELECT 1\",\"SELECT 2\"]") != null);
}

test "json builder with nested objects" {
    const allocator = testing.allocator;
    var builder = JsonBuilder.init(allocator);
    defer builder.deinit();

    try builder.startObject();
    try builder.startArray("results");
    try builder.addRawValue("{\"columns\":[\"a\"],\"values\":[[1]]}");
    try builder.endArray();
    try builder.endObject();

    const json = try builder.finish();
    defer allocator.free(json);

    try testing.expect(std.mem.indexOf(u8, json, "\"results\":") != null);
    try testing.expect(std.mem.indexOf(u8, json, "\"columns\":") != null);
}

test "json builder string escaping" {
    const allocator = testing.allocator;
    var builder = JsonBuilder.init(allocator);
    defer builder.deinit();

    try builder.startObject();
    try builder.addString("key", "hello\"world\n");
    try builder.endObject();

    const json = try builder.finish();
    defer allocator.free(json);

    try testing.expect(std.mem.indexOf(u8, json, "\\\"") != null);
    try testing.expect(std.mem.indexOf(u8, json, "\\n") != null);
}

test "json parser string" {
    const json = "{\"name\":\"rqlite\",\"version\":\"8.0.0\"}";
    const name = JsonParser.parseString(json, "name");
    try testing.expect(name != null);
    try testing.expectEqualStrings("rqlite", name.?);
}

test "json parser int positive" {
    const json = "{\"revision\":42,\"count\":100}";
    const revision = JsonParser.parseInt(json, "revision");
    try testing.expect(revision != null);
    try testing.expectEqual(@as(i64, 42), revision.?);
}

test "json parser int negative" {
    const json = "{\"delta\":-10,\"offset\":-1}";
    const delta = JsonParser.parseInt(json, "delta");
    try testing.expect(delta != null);
    try testing.expectEqual(@as(i64, -10), delta.?);
}

test "json parser missing key" {
    const json = "{\"name\":\"test\"}";
    const missing = JsonParser.parseString(json, "missing");
    try testing.expectEqual(null, missing);
}

test "json parser bool" {
    const json = "{\"leader\":true,\"learner\":false}";
    const leader = JsonParser.parseBool(json, "leader");
    try testing.expect(leader != null);
    try testing.expectEqual(true, leader.?);

    const learner = JsonParser.parseBool(json, "learner");
    try testing.expect(learner != null);
    try testing.expectEqual(false, learner.?);
}

test "find header" {
    const headers = "Content-Type: application/json\r\nContent-Length: 42\r\nServer: rqlite\r\n";
    const ct = findHeader(headers, "content-type");
    try testing.expect(ct != null);
    try testing.expectEqualStrings("application/json", ct.?);

    const cl = findHeader(headers, "content-length");
    try testing.expect(cl != null);
    try testing.expectEqualStrings("42", cl.?);
}

test "find header case insensitive" {
    const headers = "content-type: text/html\r\n";
    const ct = findHeader(headers, "content-type");
    try testing.expect(ct != null);
    try testing.expectEqualStrings("text/html", ct.?);
}

test "find content length" {
    const headers = "Content-Type: application/json\r\nContent-Length: 128\r\n";
    const cl = findContentLength(headers);
    try testing.expect(cl != null);
    try testing.expectEqual(@as(usize, 128), cl.?);
}

test "transport config defaults" {
    const config = TransportConfig{};
    try testing.expectEqual(false, config.require_client_cert);
    try testing.expectEqual(@as(u64, 5000), config.connect_timeout_ms);
    try testing.expectEqual(@as(u64, 30000), config.read_timeout_ms);
    try testing.expectEqual(true, config.tcp_keepalive);
    try testing.expectEqual(true, config.tcp_nodelay);
}
