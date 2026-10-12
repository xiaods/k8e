// Tandem — etcd v3 protocol compatibility layer backed by rqlite
// Entry point: parses config, initializes storage, starts gRPC server.

const std = @import("std");
const PipelineServer = @import("pipeline.zig").PipelineServer;
const grpc_lite_adapter = @import("server/grpc_lite_adapter.zig");
const grpc_lite = @import("grpc_lite");

/// Configuration for Tandem
pub const Config = struct {
    /// Address to listen on for etcd client connections (gRPC)
    listen_addr: []const u8 = "0.0.0.0:2379",
    /// Address of the rqlite node (HTTP API)
    rqlite_addr: []const u8 = "http://127.0.0.1:4001",
    /// Data directory for local state
    data_dir: []const u8 = "/var/lib/k8e/tandem",
    /// TLS certificate file path
    cert_file: ?[]const u8 = null,
    /// TLS key file path
    key_file: ?[]const u8 = null,
    /// mTLS CA certificate file path
    ca_file: ?[]const u8 = null,
    /// Require client certificate (mTLS)
    require_client_cert: bool = true,
    /// Identifies this instance when it claims ownership of a lease's expiry.
    /// The Supervisor supplies its unique node ID. Standalone launches may
    /// override the listen-address fallback when multiple hosts share it.
    lease_owner: []const u8 = "",

    pub fn fromEnvironment() !Config {
        var config = Config{};
        if (environment("TANDEM_LISTEN_ADDR")) |value| config.listen_addr = value;
        if (environment("TANDEM_RQLITE_ADDR")) |value| config.rqlite_addr = value;
        if (environment("TANDEM_DATA_DIR")) |value| config.data_dir = value;
        if (environment("TANDEM_TLS_CERT_FILE")) |value| config.cert_file = value;
        if (environment("TANDEM_TLS_KEY_FILE")) |value| config.key_file = value;
        if (environment("TANDEM_TLS_CA_FILE")) |value| config.ca_file = value;
        if (environment("TANDEM_TLS_REQUIRE_CLIENT_CERT")) |value| {
            config.require_client_cert = if (std.mem.eql(u8, value, "true") or std.mem.eql(u8, value, "1"))
                true
            else if (std.mem.eql(u8, value, "false") or std.mem.eql(u8, value, "0"))
                false
            else
                return error.InvalidBooleanEnvironmentValue;
        }
        if (environment("TANDEM_LEASE_OWNER")) |value| config.lease_owner = value;
        // Standalone processes retain a fallback identity; managed launches
        // receive the unique rqlite node ID from the Supervisor.
        if (config.lease_owner.len == 0) config.lease_owner = config.listen_addr;
        return config;
    }
};

fn environment(comptime name: [:0]const u8) ?[]const u8 {
    const value = std.c.getenv(name) orelse return null;
    return std.mem.span(value);
}

pub fn main() !void {
    const allocator = std.heap.smp_allocator;
    _ = grpc_lite_adapter;

    const config = try Config.fromEnvironment();

    std.log.info("Tandem starting", .{});
    std.log.info("  listen: {s}", .{config.listen_addr});
    std.log.info("  rqlite: {s}", .{config.rqlite_addr});
    std.log.info("  data:   {s}", .{config.data_dir});

    var server = try PipelineServer.initPersistent(allocator, .{
        .rqlite_url = config.rqlite_addr,
        .data_dir = config.data_dir,
    });
    defer server.deinit();

    // Nothing else reaps expired leases: expiry used to run only from a test
    // probe, so a lease granted through LeaseGrant would have held its keys
    // forever. The identity makes this instance the single expiry owner that
    // KIP-29 requires.
    var stop_reaper = std.atomic.Value(bool).init(false);
    try server.storage.setLeaseOwner(config.lease_owner);
    const reaper = try LeaseReaper.start(allocator, &server, &stop_reaper);
    defer {
        stop_reaper.store(true, .release);
        reaper.join();
        // Hand any lease this instance still owns to the next node, so a
        // failover does not have to wait out a claim that can never succeed.
        server.storage.releaseLeases() catch |err| {
            std.log.warn("could not release lease ownership on shutdown: {s}", .{@errorName(err)});
        };
    }

    try startGrpcLite(allocator, &server, config);
}

/// How often the reaper scans for expired leases. Shorter than the shortest
/// TTL a client is likely to use, so a lease does not outlive its deadline by
/// much. The scan is one indexed read plus a conditional claim per expired
/// lease, so the interval is not a load concern.
const reap_interval_ms: u64 = 500;

/// Background expiry for leases. KIP-29 requires that expiry be executed by
/// exactly one owner rather than on every instance's local clock; ownership is
/// recorded per lease in the `leases.owner` column and taken with a
/// conditional update, so a second instance scanning the same lease loses the
/// claim and skips it.
const LeaseReaper = struct {
    thread: std.Thread,
    allocator: std.mem.Allocator,
    context: *ReaperContext,

    fn start(allocator: std.mem.Allocator, server: *PipelineServer, stop: *std.atomic.Value(bool)) !LeaseReaper {
        // Allocated and filled explicitly rather than through create's
        // initialising overload, whose signature differs between the Zig
        // patch levels this builds against.
        const context = try allocator.create(ReaperContext);
        errdefer allocator.destroy(context);
        context.* = .{ .server = server, .stop = stop };
        return .{
            .thread = try std.Thread.spawn(.{}, reapLoop, .{context}),
            .allocator = allocator,
            .context = context,
        };
    }

    /// Stop the loop and reclaim its context. The caller signals the stop flag
    /// first; the slices keep the context alive exactly as long as the thread
    /// that reads it.
    fn join(self: LeaseReaper) void {
        self.thread.join();
        self.allocator.destroy(self.context);
    }

    const ReaperContext = struct {
        server: *PipelineServer,
        stop: *std.atomic.Value(bool),
    };

    fn reapLoop(context: *ReaperContext) void {
        while (!context.stop.load(.acquire)) {
            // Under the same lock the request path takes. Expiry advances the
            // stored revision and refreshes the cached one, so running it
            // unlocked would let a response header report a revision older than
            // the data a client is about to read — and a watch resuming from
            // that revision would silently miss events.
            context.server.lock();
            const outcome = reapAndPublish(context.server);
            context.server.unlock();
            // A failed scan is logged and retried on the next tick rather than
            // propagated: rqlite may briefly be unreachable while it elects a
            // leader, and killing the datastore over a transient read failure
            // would turn a recoverable blip into an outage.
            if (outcome) |deleted| {
                if (deleted > 0) std.log.info("reaped {d} key(s) from expired leases", .{deleted});
            } else |err| {
                std.log.warn("lease expiry scan failed: {s}", .{@errorName(err)});
            }
            // Sleep in short slices so shutdown is not held up by a full tick.
            // `poll` with no descriptors is the portable wait here: the Zig
            // releases this builds against do not all carry Thread.sleep, and
            // the rest of the storage layer already waits the same way.
            var waited: u64 = 0;
            while (waited < reap_interval_ms and !context.stop.load(.acquire)) : (waited += 50) {
                _ = std.posix.poll(&.{}, 50) catch {};
            }
        }
    }

    fn reapAndPublish(server: *PipelineServer) !i64 {
        try server.storage.syncState();
        // The fan-out must not be gated on the expiry scan succeeding. rqlite
        // is briefly unreachable while it elects a leader, and a scan that
        // fails on the next tick would otherwise stop this instance from
        // delivering commits made by its peers for as long as the blip lasts.
        const deleted = server.storage.expireLeases() catch |err| {
            std.log.warn("lease expiry scan failed: {s}", .{@errorName(err)});
            try server.publishCommittedSinceCursor();
            return 0;
        };
        try server.publishCommittedSinceCursor();
        return deleted;
    }
};

fn startGrpcLite(allocator: std.mem.Allocator, pipeline: *PipelineServer, config: Config) !void {
    const address = try parseListenAddress(config.listen_addr);
    var io_threaded = std.Io.Threaded.init(allocator, .{});
    defer io_threaded.deinit();
    const io = io_threaded.io();

    var cert: ?[]u8 = null;
    var key: ?[]u8 = null;
    var ca: ?[]u8 = null;
    defer if (cert) |value| allocator.free(value);
    defer if (key) |value| allocator.free(value);
    defer if (ca) |value| allocator.free(value);

    if (config.cert_file == null or config.key_file == null) {
        if (config.require_client_cert) return error.TlsCertificateConfigurationRequired;
    } else {
        cert = try readFile(allocator, io, config.cert_file.?);
        key = try readFile(allocator, io, config.key_file.?);
        if (config.ca_file) |path| ca = try readFile(allocator, io, path);
        if (config.require_client_cert and ca == null) return error.ClientCertificateAuthorityRequired;
    }

    const options = grpc_lite.server.Options{
        .host = address.host,
        .port = address.port,
        .tls = if (cert != null and key != null) .{
            .certificate_chain_pem = cert.?,
            .private_key_pem = key.?,
            .ca_certificates_pem = ca,
            .require_client_certificate = config.require_client_cert,
        } else null,
    };

    var watch_endpoint = grpc_lite_adapter.WatchEndpoint.init(allocator, pipeline);
    defer watch_endpoint.deinit();
    var grpc_server = try grpc_lite.server.Server.init(allocator, options);
    defer grpc_server.deinit();
    const paths = [_][]const u8{
        "/etcdserverpb.KV/Range",
        "/etcdserverpb.KV/Put",
        "/etcdserverpb.KV/DeleteRange",
        "/etcdserverpb.KV/Txn",
        "/etcdserverpb.KV/Compact",
        "/etcdserverpb.Lease/LeaseGrant",
        "/etcdserverpb.Lease/LeaseRevoke",
        "/etcdserverpb.Lease/LeaseTimeToLive",
        "/etcdserverpb.Lease/LeaseLeases",
        // The apiserver calls this on every start, from
        // etcd3.New -> CheckClient, to read the endpoint's version and decide
        // whether RequestWatchProgress is supported. Leaving it unrouted made
        // that probe fail on every boot — an error-level log each time — and
        // disabled watch-list initial events. The version and db size are real,
        // and the leader and Raft indices come from rqlite's /status.
        "/etcdserverpb.Maintenance/Status",
    };
    var endpoints: [paths.len]grpc_lite_adapter.UnaryEndpoint = undefined;
    for (paths, 0..) |path, i| {
        endpoints[i] = .{ .pipeline = pipeline, .path = path };
        try grpc_lite_adapter.registerKV(&grpc_server, &endpoints[i]);
    }
    try grpc_lite_adapter.registerWatch(&grpc_server, &watch_endpoint);
    var keepalive_endpoint = grpc_lite_adapter.UnaryEndpoint{ .pipeline = pipeline, .path = "/etcdserverpb.Lease/LeaseKeepAlive" };
    try grpc_lite_adapter.registerStream(&grpc_server, &keepalive_endpoint);
    try grpc_server.start();
    grpc_server.wait();
}

const ListenAddress = struct { host: []const u8, port: u16 };

fn parseListenAddress(address: []const u8) !ListenAddress {
    const separator = std.mem.lastIndexOfScalar(u8, address, ':') orelse return error.InvalidListenAddress;
    const host = address[0..separator];
    const port = try std.fmt.parseInt(u16, address[separator + 1 ..], 10);
    return .{ .host = host, .port = port };
}

fn readFile(allocator: std.mem.Allocator, io: std.Io, path: []const u8) ![]u8 {
    var file = try std.Io.Dir.openFileAbsolute(io, path, .{});
    defer file.close(io);
    const stat = try file.stat(io);
    const contents = try allocator.alloc(u8, @intCast(stat.size));
    errdefer allocator.free(contents);
    _ = try file.readPositionalAll(io, contents, 0);
    return contents;
}

test "config defaults" {
    const testing = std.testing;
    const config = Config{};
    try testing.expectEqualStrings("0.0.0.0:2379", config.listen_addr);
    try testing.expectEqualStrings("http://127.0.0.1:4001", config.rqlite_addr);
    try testing.expectEqualStrings("/var/lib/k8e/tandem", config.data_dir);
    try testing.expectEqual(true, config.require_client_cert);
}
