const std = @import("std");

const c = @import("mbedtls_c.zig").api;

const queue_capacity = 64 * 1024;
const personalization = "grpc-lite";
const h2_protocols = [_:null]?[*:0]const u8{ "h2", null };
// HTTP/2 permits TLS 1.2 ECDHE AEAD suites and all TLS 1.3 suites below.
// Keeping an explicit list prevents mbedTLS defaults from enabling RFC 7540 Appendix A suites.
const http2_ciphersuites = [_]c_int{
    c.MBEDTLS_TLS1_3_AES_128_GCM_SHA256,
    c.MBEDTLS_TLS1_3_AES_256_GCM_SHA384,
    c.MBEDTLS_TLS1_3_CHACHA20_POLY1305_SHA256,
    c.MBEDTLS_TLS_ECDHE_ECDSA_WITH_AES_128_GCM_SHA256,
    c.MBEDTLS_TLS_ECDHE_ECDSA_WITH_AES_256_GCM_SHA384,
    c.MBEDTLS_TLS_ECDHE_ECDSA_WITH_CHACHA20_POLY1305_SHA256,
    c.MBEDTLS_TLS_ECDHE_RSA_WITH_AES_128_GCM_SHA256,
    c.MBEDTLS_TLS_ECDHE_RSA_WITH_AES_256_GCM_SHA384,
    c.MBEDTLS_TLS_ECDHE_RSA_WITH_CHACHA20_POLY1305_SHA256,
    0,
};
var psa_state: std.atomic.Value(u8) = .init(0);

pub const Config = struct {
    allocator: std.mem.Allocator = undefined,
    entropy: c.mbedtls_entropy_context = undefined,
    drbg: c.mbedtls_ctr_drbg_context = undefined,
    ssl: c.mbedtls_ssl_config = undefined,
    certificate: c.mbedtls_x509_crt = undefined,
    ca_certificate: c.mbedtls_x509_crt = undefined,
    private_key: c.mbedtls_pk_context = undefined,
    initialized: bool = false,

    pub fn createClient(allocator: std.mem.Allocator, ca_pem: []const u8) !*Config {
        const self = try allocator.create(Config);
        self.* = .{ .allocator = allocator };
        errdefer allocator.destroy(self);
        try self.init(.client);
        errdefer self.deinit();

        try parseCertificate(allocator, &self.certificate, ca_pem);
        c.mbedtls_ssl_conf_ca_chain(&self.ssl, &self.certificate, null);
        c.mbedtls_ssl_conf_authmode(&self.ssl, c.MBEDTLS_SSL_VERIFY_REQUIRED);
        return self;
    }

    pub fn createServer(
        allocator: std.mem.Allocator,
        certificate_chain_pem: []const u8,
        private_key_pem: []const u8,
        ca_certificates_pem: ?[]const u8,
        require_client_certificate: bool,
    ) !*Config {
        const self = try allocator.create(Config);
        self.* = .{ .allocator = allocator };
        errdefer allocator.destroy(self);
        try self.init(.server);
        errdefer self.deinit();

        try parseCertificate(allocator, &self.certificate, certificate_chain_pem);
        if (ca_certificates_pem) |ca_pem| {
            try parseCertificate(allocator, &self.ca_certificate, ca_pem);
            c.mbedtls_ssl_conf_ca_chain(&self.ssl, &self.ca_certificate, null);
            if (require_client_certificate) c.mbedtls_ssl_conf_authmode(&self.ssl, c.MBEDTLS_SSL_VERIFY_REQUIRED);
        }
        try parsePrivateKey(allocator, &self.private_key, private_key_pem, &self.drbg);
        if (c.mbedtls_pk_check_pair(
            &self.certificate.pk,
            &self.private_key,
            c.mbedtls_ctr_drbg_random,
            &self.drbg,
        ) != 0) return error.CertificateKeyMismatch;
        if (c.mbedtls_ssl_conf_own_cert(&self.ssl, &self.certificate, &self.private_key) != 0)
            return error.TlsConfigurationFailed;
        return self;
    }

    pub fn destroy(self: *Config) void {
        const allocator = self.allocator;
        self.deinit();
        allocator.destroy(self);
    }

    fn deinit(self: *Config) void {
        if (!self.initialized) return;
        c.mbedtls_ssl_config_free(&self.ssl);
        c.mbedtls_pk_free(&self.private_key);
        c.mbedtls_x509_crt_free(&self.certificate);
        c.mbedtls_x509_crt_free(&self.ca_certificate);
        c.mbedtls_ctr_drbg_free(&self.drbg);
        c.mbedtls_entropy_free(&self.entropy);
        self.initialized = false;
    }

    const Endpoint = enum { client, server };

    fn init(self: *Config, endpoint: Endpoint) !void {
        try ensurePsaInitialized();
        c.mbedtls_entropy_init(&self.entropy);
        c.mbedtls_ctr_drbg_init(&self.drbg);
        c.mbedtls_ssl_config_init(&self.ssl);
        c.mbedtls_x509_crt_init(&self.certificate);
        c.mbedtls_x509_crt_init(&self.ca_certificate);
        c.mbedtls_pk_init(&self.private_key);
        self.initialized = true;
        errdefer self.deinit();

        if (c.mbedtls_ctr_drbg_seed(
            &self.drbg,
            c.mbedtls_entropy_func,
            &self.entropy,
            personalization,
            personalization.len,
        ) != 0) return error.RandomInitializationFailed;
        if (c.mbedtls_ssl_config_defaults(
            &self.ssl,
            if (endpoint == .client) c.MBEDTLS_SSL_IS_CLIENT else c.MBEDTLS_SSL_IS_SERVER,
            c.MBEDTLS_SSL_TRANSPORT_STREAM,
            c.MBEDTLS_SSL_PRESET_DEFAULT,
        ) != 0) return error.TlsConfigurationFailed;
        c.mbedtls_ssl_conf_min_tls_version(&self.ssl, c.MBEDTLS_SSL_VERSION_TLS1_2);
        c.mbedtls_ssl_conf_ciphersuites(&self.ssl, &http2_ciphersuites);
        c.mbedtls_ssl_conf_rng(&self.ssl, c.mbedtls_ctr_drbg_random, &self.drbg);
        if (c.mbedtls_ssl_conf_alpn_protocols(&self.ssl, @ptrCast(@constCast(&h2_protocols))) != 0)
            return error.TlsConfigurationFailed;
    }
};

fn ensurePsaInitialized() !void {
    while (true) switch (psa_state.load(.acquire)) {
        0 => {
            if (psa_state.cmpxchgStrong(0, 1, .acq_rel, .acquire) != null) continue;
            if (c.psa_crypto_init() != c.PSA_SUCCESS) {
                psa_state.store(0, .release);
                return error.RandomInitializationFailed;
            }
            psa_state.store(2, .release);
            return;
        },
        1 => std.atomic.spinLoopHint(),
        2 => return,
        else => unreachable,
    };
}

pub const Result = union(enum) {
    complete,
    bytes: usize,
    want_read,
    want_write,
    peer_closed,
    transport_eof,
    failed: c_int,
    alpn_mismatch,
};

pub const Session = struct {
    const State = enum { handshaking, active, failed };

    allocator: std.mem.Allocator = undefined,
    ssl: c.mbedtls_ssl_context = undefined,
    inbound: ByteQueue = .{},
    outbound: ByteQueue = .{},
    transport_eof: bool = false,
    initialized: bool = false,
    pending_write: [c.MBEDTLS_SSL_OUT_CONTENT_LEN]u8 = undefined,
    pending_write_length: usize = 0,
    state: State = .handshaking,

    pub fn create(
        allocator: std.mem.Allocator,
        config: *const Config,
        hostname: ?[]const u8,
    ) !*Session {
        const self = try allocator.create(Session);
        self.* = .{ .allocator = allocator };
        errdefer allocator.destroy(self);
        c.mbedtls_ssl_init(&self.ssl);
        self.initialized = true;
        errdefer self.deinit();

        if (c.mbedtls_ssl_setup(&self.ssl, &config.ssl) != 0)
            return error.TlsSessionInitializationFailed;
        c.mbedtls_ssl_set_bio(&self.ssl, self, bioSend, bioRecv, null);
        if (hostname) |name| {
            if (name.len == 0 or name.len > c.MBEDTLS_SSL_MAX_HOST_NAME_LEN)
                return error.InvalidServerName;
            var name_z: [c.MBEDTLS_SSL_MAX_HOST_NAME_LEN + 1]u8 = undefined;
            @memcpy(name_z[0..name.len], name);
            name_z[name.len] = 0;
            if (c.mbedtls_ssl_set_hostname(&self.ssl, @ptrCast(&name_z)) != 0)
                return error.InvalidServerName;
        }
        return self;
    }

    pub fn destroy(self: *Session) void {
        const allocator = self.allocator;
        self.deinit();
        allocator.destroy(self);
    }

    fn deinit(self: *Session) void {
        if (!self.initialized) return;
        c.mbedtls_ssl_free(&self.ssl);
        self.initialized = false;
    }

    pub fn feedCiphertext(self: *Session, bytes: []const u8) !void {
        try self.inbound.append(bytes);
    }

    pub fn markTransportEof(self: *Session) void {
        self.transport_eof = true;
    }

    pub fn ciphertext(self: *const Session) []const u8 {
        return self.outbound.readable();
    }

    pub fn consumeCiphertext(self: *Session, count: usize) void {
        self.outbound.consume(count);
    }

    pub fn handshake(self: *Session) Result {
        if (self.state == .active) return .complete;
        if (self.state == .failed) return .{ .failed = c.MBEDTLS_ERR_SSL_BAD_INPUT_DATA };
        const result = c.mbedtls_ssl_handshake(&self.ssl);
        if (result == 0) {
            if (!self.negotiatedHttp2()) {
                self.state = .failed;
                return .alpn_mismatch;
            }
            self.state = .active;
        } else if (result != c.MBEDTLS_ERR_SSL_WANT_READ and result != c.MBEDTLS_ERR_SSL_WANT_WRITE) {
            self.state = .failed;
        }
        return classify(result, false);
    }

    pub fn read(self: *Session, output: []u8) !Result {
        if (self.state != .active) return error.TlsHandshakeIncomplete;
        if (output.len == 0) return .{ .bytes = 0 };
        const result = c.mbedtls_ssl_read(&self.ssl, output.ptr, output.len);
        if (result == 0 and self.transport_eof) return .transport_eof;
        if (result < 0 and result != c.MBEDTLS_ERR_SSL_WANT_READ and result != c.MBEDTLS_ERR_SSL_WANT_WRITE and result != c.MBEDTLS_ERR_SSL_PEER_CLOSE_NOTIFY)
            self.state = .failed;
        return classify(result, true);
    }

    pub fn beginWrite(self: *Session, input: []const u8) !Result {
        if (self.state != .active) return error.TlsHandshakeIncomplete;
        if (self.pending_write_length != 0) return error.TlsWriteInProgress;
        if (input.len == 0) return .{ .bytes = 0 };
        self.pending_write_length = @min(input.len, self.pending_write.len);
        @memcpy(self.pending_write[0..self.pending_write_length], input[0..self.pending_write_length]);
        return try self.continueWrite();
    }

    pub fn continueWrite(self: *Session) !Result {
        if (self.state != .active) return error.TlsHandshakeIncomplete;
        std.debug.assert(self.pending_write_length != 0);
        const result = c.mbedtls_ssl_write(
            &self.ssl,
            &self.pending_write,
            self.pending_write_length,
        );
        if (result > 0) self.pending_write_length = 0;
        if (result < 0 and result != c.MBEDTLS_ERR_SSL_WANT_READ and result != c.MBEDTLS_ERR_SSL_WANT_WRITE)
            self.state = .failed;
        return classify(result, true);
    }

    pub fn hasPendingWrite(self: *const Session) bool {
        return self.pending_write_length != 0;
    }

    pub fn closeNotify(self: *Session) !Result {
        if (self.state != .active) return error.TlsHandshakeIncomplete;
        if (self.pending_write_length != 0) return error.TlsWriteInProgress;
        return classify(c.mbedtls_ssl_close_notify(&self.ssl), false);
    }

    pub fn negotiatedHttp2(self: *const Session) bool {
        const protocol = c.mbedtls_ssl_get_alpn_protocol(&self.ssl);
        return protocol != null and std.mem.eql(u8, std.mem.span(protocol), "h2");
    }

    fn bioSend(context: ?*anyopaque, buffer: [*c]const u8, length: usize) callconv(.c) c_int {
        const self: *Session = @ptrCast(@alignCast(context.?));
        const written = self.outbound.appendPartial(buffer[0..length]);
        if (written == 0) return c.MBEDTLS_ERR_SSL_WANT_WRITE;
        return @intCast(written);
    }

    fn bioRecv(context: ?*anyopaque, buffer: [*c]u8, length: usize) callconv(.c) c_int {
        const self: *Session = @ptrCast(@alignCast(context.?));
        const available = self.inbound.readable();
        if (available.len == 0)
            return if (self.transport_eof) 0 else c.MBEDTLS_ERR_SSL_WANT_READ;
        const read_length = @min(length, available.len);
        @memcpy(buffer[0..read_length], available[0..read_length]);
        self.inbound.consume(read_length);
        return @intCast(read_length);
    }
};

const ByteQueue = struct {
    bytes: [queue_capacity]u8 = undefined,
    start: usize = 0,
    end: usize = 0,

    fn append(self: *ByteQueue, input: []const u8) !void {
        if (input.len > self.bytes.len - (self.end - self.start)) return error.TlsBufferFull;
        std.debug.assert(self.appendPartial(input) == input.len);
    }

    fn appendPartial(self: *ByteQueue, input: []const u8) usize {
        self.compact();
        const count = @min(input.len, self.bytes.len - self.end);
        @memcpy(self.bytes[self.end..][0..count], input[0..count]);
        self.end += count;
        return count;
    }

    fn readable(self: *const ByteQueue) []const u8 {
        return self.bytes[self.start..self.end];
    }

    fn consume(self: *ByteQueue, count: usize) void {
        std.debug.assert(count <= self.end - self.start);
        self.start += count;
        if (self.start == self.end) {
            self.start = 0;
            self.end = 0;
        }
    }

    fn compact(self: *ByteQueue) void {
        if (self.start == 0) return;
        std.mem.copyForwards(u8, self.bytes[0 .. self.end - self.start], self.bytes[self.start..self.end]);
        self.end -= self.start;
        self.start = 0;
    }
};

fn classify(result: c_int, has_bytes: bool) Result {
    if (result > 0 and has_bytes) return .{ .bytes = @intCast(result) };
    if (result == 0) return .complete;
    return switch (result) {
        c.MBEDTLS_ERR_SSL_WANT_READ => .want_read,
        c.MBEDTLS_ERR_SSL_WANT_WRITE => .want_write,
        c.MBEDTLS_ERR_SSL_PEER_CLOSE_NOTIFY => .peer_closed,
        else => .{ .failed = result },
    };
}

fn parseCertificate(allocator: std.mem.Allocator, certificate: *c.mbedtls_x509_crt, pem: []const u8) !void {
    const pem_z = try allocator.dupeZ(u8, pem);
    defer allocator.free(pem_z);
    if (c.mbedtls_x509_crt_parse(certificate, pem_z.ptr, pem_z.len + 1) != 0)
        return error.InvalidCertificate;
}

fn parsePrivateKey(
    allocator: std.mem.Allocator,
    private_key: *c.mbedtls_pk_context,
    pem: []const u8,
    drbg: *c.mbedtls_ctr_drbg_context,
) !void {
    const pem_z = try allocator.dupeZ(u8, pem);
    defer {
        @memset(pem_z, 0);
        allocator.free(pem_z);
    }
    if (c.mbedtls_pk_parse_key(
        private_key,
        pem_z.ptr,
        pem_z.len + 1,
        null,
        0,
        c.mbedtls_ctr_drbg_random,
        drbg,
    ) != 0) return error.InvalidPrivateKey;
}

test "byte queue compacts consumed input" {
    var queue: ByteQueue = .{};
    try queue.append("first");
    queue.consume(5);
    try queue.append("second");
    try std.testing.expectEqualStrings("second", queue.readable());
}

test "byte queue rejects input beyond its bound" {
    var queue: ByteQueue = .{};
    const input = try std.testing.allocator.alloc(u8, queue_capacity + 1);
    defer std.testing.allocator.free(input);
    try std.testing.expectError(error.TlsBufferFull, queue.append(input));
}

test "client and server negotiate h2 and exchange plaintext" {
    const certificate = @embedFile("testdata/localhost-cert.pem");
    const private_key = @embedFile("testdata/localhost-key.pem");

    const client_config = try Config.createClient(std.testing.allocator, certificate);
    defer client_config.destroy();
    const server_config = try Config.createServer(std.testing.allocator, certificate, private_key, null, false);
    defer server_config.destroy();

    const client = try Session.create(std.testing.allocator, client_config, "localhost");
    defer client.destroy();
    const server = try Session.create(std.testing.allocator, server_config, null);
    defer server.destroy();

    var client_complete = false;
    var server_complete = false;
    for (0..32) |_| {
        if (!client_complete) client_complete = try handshakeStep(client);
        try transferCiphertext(client, server);
        if (!server_complete) server_complete = try handshakeStep(server);
        try transferCiphertext(server, client);
        if (client_complete and server_complete) break;
    }
    try std.testing.expect(client_complete);
    try std.testing.expect(server_complete);
    try std.testing.expect(client.negotiatedHttp2());
    try std.testing.expect(server.negotiatedHttp2());

    const message = "hello over TLS";
    const write_result = try client.beginWrite(message);
    try std.testing.expectEqual(message.len, switch (write_result) {
        .bytes => |count| count,
        else => return error.UnexpectedTlsResult,
    });
    try transferCiphertext(client, server);
    var plaintext: [64]u8 = undefined;
    const read_result = try server.read(&plaintext);
    const read_length = switch (read_result) {
        .bytes => |count| count,
        else => return error.UnexpectedTlsResult,
    };
    try std.testing.expectEqualStrings(message, plaintext[0..read_length]);
}

test "client rejects a certificate for a different hostname" {
    const certificate = @embedFile("testdata/localhost-cert.pem");
    const private_key = @embedFile("testdata/localhost-key.pem");
    const client_config = try Config.createClient(std.testing.allocator, certificate);
    defer client_config.destroy();
    const server_config = try Config.createServer(std.testing.allocator, certificate, private_key, null, false);
    defer server_config.destroy();
    const client = try Session.create(std.testing.allocator, client_config, "wrong.example");
    defer client.destroy();
    const server = try Session.create(std.testing.allocator, server_config, null);
    defer server.destroy();

    var rejected = false;
    for (0..32) |_| {
        switch (client.handshake()) {
            .failed => rejected = true,
            .complete => return error.UnexpectedTlsResult,
            else => {},
        }
        try transferCiphertext(client, server);
        if (rejected) break;
        _ = server.handshake();
        try transferCiphertext(server, client);
    }
    try std.testing.expect(rejected);
}

fn handshakeStep(session: *Session) !bool {
    return switch (session.handshake()) {
        .complete => true,
        .want_read, .want_write => false,
        else => error.TlsHandshakeFailed,
    };
}

fn transferCiphertext(source: *Session, destination: *Session) !void {
    const bytes = source.ciphertext();
    try destination.feedCiphertext(bytes);
    source.consumeCiphertext(bytes.len);
}
