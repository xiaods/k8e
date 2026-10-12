const std = @import("std");
const builtin = @import("builtin");
const demo = @import("demo_proto");
const grpc = @import("grpc_lite");
const grpc_pb = @import("grpc_lite_protobuf");

const max_message_size = 4 * 1024 * 1024;
const protobuf_field_overhead = 5;
const max_payload_bytes = max_message_size - protobuf_field_overhead;
const rpc_timeout_ns = 5 * std.time.ns_per_s;
const drain_timeout_ns = 5 * std.time.ns_per_s;
const cancel_timeout_ns = 2 * std.time.ns_per_s;
const max_stream_latency_samples = 1_000_000;

const initial_stream_window_bytes = 64 * 1024;
const write_high_watermark_bytes = 1024 * 1024;
const write_low_watermark_bytes = 512 * 1024;

const raw_unary_path = "/grpc.lite.Benchmark/Unary";
const raw_bidi_path = "/grpc.lite.Benchmark/Bidi";

const EchoApi = demo.EchoService(void, error{});
const StreamingEchoApi = demo.StreamingEchoService(void, error{});
const TypedStream = grpc_pb.ClientStream(demo.EchoRequest, demo.EchoReply);
const TypedView = grpc_pb.ClientStreamView(demo.EchoRequest);

const Scenario = enum {
    unary,
    bidi_ping_pong,
    bidi_throughput,

    fn jsonName(self: Scenario) []const u8 {
        return switch (self) {
            .unary => "unary",
            .bidi_ping_pong => "bidi-ping-pong",
            .bidi_throughput => "bidi-throughput",
        };
    }
};

const Transport = enum {
    raw,
    typed,

    fn jsonName(self: Transport) []const u8 {
        return @tagName(self);
    }
};

const PayloadPattern = enum {
    repeated,
    deterministic_random,

    fn jsonName(self: PayloadPattern) []const u8 {
        return switch (self) {
            .repeated => "repeated",
            .deterministic_random => "deterministic-random",
        };
    }
};

const Config = struct {
    host: []const u8 = "127.0.0.1",
    port: u16 = 50061,
    scenario: Scenario = .unary,
    transport: Transport = .raw,
    warmup_ns: u64 = std.time.ns_per_s,
    duration_ns: u64 = 3 * std.time.ns_per_s,
    streams: usize = 1,
    channels: usize = 1,
    server_reactors: usize = 1,
    pipeline: usize = 32,
    payload_bytes: usize = 128,
    payload_pattern: PayloadPattern = .repeated,
    compression: grpc.Compression = .identity,
    output: ?[]const u8 = null,
    compact: bool = false,
};

const Phase = enum(u8) {
    setup,
    warmup,
    measurement,
    stop,
};

const Timing = struct {
    io: std.Io,
    phase: std.atomic.Value(u8) = .init(@intFromEnum(Phase.setup)),
    measure_start_ns: std.atomic.Value(u64) = .init(0),
    measure_end_ns: std.atomic.Value(u64) = .init(0),

    fn loadPhase(self: *const Timing) Phase {
        return @enumFromInt(self.phase.load(.acquire));
    }

    fn storePhase(self: *Timing, phase: Phase) void {
        self.phase.store(@intFromEnum(phase), .release);
    }

    fn requestIsMeasured(self: *const Timing, now_ns: u64) bool {
        return self.loadPhase() == .measurement and
            now_ns >= self.measure_start_ns.load(.acquire) and
            now_ns < self.measure_end_ns.load(.acquire);
    }
};

const Measurements = struct {
    const LatencySample = struct {
        ns: u64,
        weight: f64 = 1,
    };

    completed: u64 = 0,
    exchange_errors: u64 = 0,
    stream_errors: u64 = 0,
    eligible_latency_count: u64 = 0,
    observed_min_latency_ns: u64 = std.math.maxInt(u64),
    observed_max_latency_ns: u64 = 0,
    sampling_seed: u64 = 0,
    latency_samples: std.ArrayList(LatencySample) = .empty,

    fn deinit(self: *Measurements, allocator: std.mem.Allocator) void {
        self.latency_samples.deinit(allocator);
    }

    fn addLatency(self: *Measurements, latency_ns: u64, capacity: usize) void {
        self.eligible_latency_count += 1;
        self.observed_min_latency_ns = @min(self.observed_min_latency_ns, latency_ns);
        self.observed_max_latency_ns = @max(self.observed_max_latency_ns, latency_ns);
        if (self.latency_samples.items.len < capacity) {
            self.latency_samples.appendAssumeCapacity(.{ .ns = latency_ns });
            return;
        }
        const slot = mix64(self.eligible_latency_count ^ self.sampling_seed) % self.eligible_latency_count;
        if (slot < capacity) self.latency_samples.items[@intCast(slot)] = .{ .ns = latency_ns };
    }

    fn recordExchange(
        self: *Measurements,
        started_in_measurement: bool,
        valid: bool,
        latency_ns: u64,
        capacity: usize,
    ) void {
        if (!started_in_measurement) return;
        if (!valid) {
            self.exchange_errors += 1;
            return;
        }
        self.completed += 1;
        self.addLatency(latency_ns, capacity);
    }

    fn mergeShard(
        self: *Measurements,
        allocator: std.mem.Allocator,
        shard: *const Measurements,
    ) !void {
        self.completed += shard.completed;
        self.exchange_errors += shard.exchange_errors;
        self.stream_errors += shard.stream_errors;
        self.eligible_latency_count += shard.eligible_latency_count;
        if (shard.eligible_latency_count != 0) {
            self.observed_min_latency_ns = @min(
                self.observed_min_latency_ns,
                shard.observed_min_latency_ns,
            );
            self.observed_max_latency_ns = @max(
                self.observed_max_latency_ns,
                shard.observed_max_latency_ns,
            );
        }
        if (shard.latency_samples.items.len == 0) return;

        const new_len = self.latency_samples.items.len + shard.latency_samples.items.len;
        try self.latency_samples.ensureTotalCapacity(allocator, new_len);
        const weight = @as(f64, @floatFromInt(shard.eligible_latency_count)) /
            @as(f64, @floatFromInt(shard.latency_samples.items.len));
        for (shard.latency_samples.items) |sample| {
            self.latency_samples.appendAssumeCapacity(.{ .ns = sample.ns, .weight = weight });
        }
    }
};

const TypedUnaryWorker = struct {
    channel: *grpc.Channel,
    timing: *Timing,
    payload: []const u8,
    compression: grpc.Compression,
    latency_capacity: usize,
    measurements: Measurements = .{},

    fn run(self: *TypedUnaryWorker) void {
        while (self.timing.loadPhase() == .setup) std.atomic.spinLoopHint();
        var typed_client = grpc_pb.ServiceClient(EchoApi).init(self.channel);

        while (self.timing.loadPhase() != .stop) {
            const started_ns = nowNs(self.timing.io);
            const measured = self.timing.requestIsMeasured(started_ns);
            const valid = self.callTyped(&typed_client) catch {
                if (measured) self.measurements.exchange_errors += 1;
                continue;
            };
            const finished_ns = nowNs(self.timing.io);
            self.measurements.recordExchange(
                measured,
                valid,
                finished_ns -| started_ns,
                self.latency_capacity,
            );
        }
    }

    fn callTyped(
        self: *TypedUnaryWorker,
        client: *grpc_pb.ServiceClient(EchoApi),
    ) !bool {
        var result = try client.callUnary(
            std.heap.c_allocator,
            "Echo",
            .{ .message = self.payload },
            .{
                .timeout_ns = rpc_timeout_ns,
                .request_compression = self.compression,
            },
        );
        defer result.deinit();
        return result.raw.status.isOk() and result.response != null and
            validatePayload(result.response.?.message, self.payload);
    }
};

const RawUnarySlot = struct {
    channel: *grpc.Channel,
    timing: *Timing,
    payload: []const u8,
    compression: grpc.Compression,
    latency_capacity: usize,
    current: Timestamp = .{ .started_ns = 0, .measured = false },
    measurements: Measurements = .{},
    done: std.atomic.Value(bool) = .init(false),

    fn start(self: *RawUnarySlot) void {
        const started_ns = nowNs(self.timing.io);
        const measured = rawUnaryMeasurementEligibilityAt(self.timing, started_ns) orelse {
            self.done.store(true, .release);
            return;
        };
        self.current = .{
            .started_ns = started_ns,
            .measured = measured,
        };
        self.channel.callUnaryAsync(
            raw_unary_path,
            self.payload,
            .{
                .timeout_ns = rpc_timeout_ns,
                .request_compression = self.compression,
            },
            .{ .context = self, .on_complete = onComplete },
        ) catch {
            self.measurements.exchange_errors += 1;
            self.done.store(true, .release);
        };
    }

    fn onComplete(context: ?*anyopaque, result: grpc.AsyncCallResult) void {
        const self: *RawUnarySlot = @ptrCast(@alignCast(context.?));
        const finished_ns = nowNs(self.timing.io);
        self.measurements.recordExchange(
            self.current.measured,
            result.status.isOk() and validatePayload(result.payload, self.payload),
            finished_ns -| self.current.started_ns,
            self.latency_capacity,
        );
        self.start();
    }
};

const Timestamp = struct {
    started_ns: u64,
    measured: bool,
};

const StreamState = struct {
    timing: *Timing,
    transport: Transport,
    payload: []const u8,
    compression: grpc.Compression,
    pipeline: usize,
    timestamps: []Timestamp,
    head: usize = 0,
    tail: usize = 0,
    inflight: usize = 0,
    pending: ?Timestamp = null,
    raw_stream: ?grpc.ClientStream = null,
    typed_stream: ?TypedStream = null,
    measurements: Measurements = .{},
    latency_capacity: usize,
    terminal_status: grpc.StatusCode = .unknown,
    terminal_error: bool = false,
    terminal_count: usize = 0,
    done: std.atomic.Value(bool) = .init(false),

    fn deinit(self: *StreamState, allocator: std.mem.Allocator) void {
        if (self.raw_stream) |*stream| stream.deinit();
        if (self.typed_stream) |*stream| stream.deinit();
        self.measurements.deinit(std.heap.page_allocator);
        allocator.free(self.timestamps);
    }

    fn bootstrap(self: *StreamState) !void {
        const timestamp: Timestamp = .{
            .started_ns = nowNs(self.timing.io),
            .measured = false,
        };
        try self.sendReserved(timestamp);
    }

    fn sendReserved(self: *StreamState, timestamp: Timestamp) !void {
        std.debug.assert(self.inflight < self.pipeline);
        self.timestamps[self.tail] = timestamp;
        self.tail = (self.tail + 1) % self.timestamps.len;
        self.inflight += 1;
        self.sendOne() catch |err| {
            self.tail = if (self.tail == 0) self.timestamps.len - 1 else self.tail - 1;
            self.inflight -= 1;
            if (err == error.WouldBlock) {
                self.pending = timestamp;
                return;
            }
            return err;
        };
    }

    fn sendOne(self: *StreamState) !void {
        switch (self.transport) {
            .raw => try self.raw_stream.?.send(
                self.payload,
                .{ .compression = self.compression },
            ),
            .typed => try self.typed_stream.?.send(
                std.heap.page_allocator,
                .{ .message = self.payload },
                .{ .compression = self.compression },
            ),
        }
    }

    fn refill(self: *StreamState) void {
        if (!self.sendingActive()) return;
        if (self.pending != null) return;

        while (self.inflight < self.pipeline) {
            const started_ns = nowNs(self.timing.io);
            if (!self.sendingActiveAt(started_ns)) return;
            const timestamp: Timestamp = .{
                .started_ns = started_ns,
                .measured = self.timing.requestIsMeasured(started_ns),
            };
            self.sendReserved(timestamp) catch |err| {
                if (self.stoppingAfterClose(err)) return;
                self.failStream(timestamp.measured);
                return;
            };
            if (self.pending != null) return;
        }
    }

    fn onWritable(self: *StreamState) void {
        if (!self.sendingActive()) {
            self.pending = null;
            return;
        }
        if (self.pending) |timestamp| {
            self.pending = null;
            self.sendReserved(timestamp) catch |err| {
                if (self.stoppingAfterClose(err)) return;
                self.failStream(timestamp.measured);
                return;
            };
            if (self.pending != null) return;
        }
        self.refill();
    }

    fn sendingActive(self: *const StreamState) bool {
        return self.sendingActiveAt(nowNs(self.timing.io));
    }

    fn sendingActiveAt(self: *const StreamState, current_ns: u64) bool {
        return switch (self.timing.loadPhase()) {
            .warmup => true,
            .measurement => current_ns < self.timing.measure_end_ns.load(.acquire),
            .setup, .stop => false,
        };
    }

    fn stoppingAfterClose(self: *const StreamState, err: anyerror) bool {
        return self.timing.loadPhase() == .stop and
            (err == error.SendClosed or err == error.StreamClosed);
    }

    fn onResponse(self: *StreamState, response: []const u8) grpc.StreamReceiveAction {
        const finished_ns = nowNs(self.timing.io);
        if (self.inflight == 0) {
            self.cancel();
            return .continue_receiving;
        }

        const timestamp = self.timestamps[self.head];
        self.head = (self.head + 1) % self.timestamps.len;
        self.inflight -= 1;
        const valid = validatePayload(response, self.payload);
        self.measurements.recordExchange(
            timestamp.measured,
            valid,
            finished_ns -| timestamp.started_ns,
            self.latency_capacity,
        );
        if (!valid) {
            self.cancel();
            return .continue_receiving;
        }
        self.refill();
        return .continue_receiving;
    }

    fn failStream(self: *StreamState, measured: bool) void {
        if (measured) self.measurements.exchange_errors += 1;
        self.terminal_error = true;
        self.cancel();
    }

    fn cancel(self: *StreamState) void {
        switch (self.transport) {
            .raw => self.raw_stream.?.cancel(),
            .typed => self.typed_stream.?.cancel(),
        }
    }

    fn finishTerminal(self: *StreamState, status: grpc.Status, adapter_error: ?anyerror) void {
        self.terminal_count += 1;
        if (self.terminal_count != 1) self.terminal_error = true;
        self.terminal_status = status.code;
        if (adapter_error != null) self.terminal_error = true;
        self.done.store(true, .release);
    }

    fn rawMessage(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        response: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *StreamState = @ptrCast(@alignCast(context.?));
        return self.onResponse(response);
    }

    fn rawWritable(context: ?*anyopaque, _: grpc.ClientStream) void {
        const self: *StreamState = @ptrCast(@alignCast(context.?));
        self.onWritable();
    }

    fn rawTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *StreamState = @ptrCast(@alignCast(context.?));
        self.finishTerminal(status, null);
    }

    fn typedMessage(
        context: ?*anyopaque,
        _: TypedView,
        response: *const demo.EchoReply,
        _: grpc.Compression,
    ) !grpc.StreamReceiveAction {
        const self: *StreamState = @ptrCast(@alignCast(context.?));
        return self.onResponse(response.message);
    }

    fn typedWritable(context: ?*anyopaque, _: TypedView) !void {
        const self: *StreamState = @ptrCast(@alignCast(context.?));
        self.onWritable();
    }

    fn typedTerminal(
        context: ?*anyopaque,
        _: TypedView,
        status: grpc.Status,
        _: *const grpc.Metadata,
        adapter_error: ?anyerror,
    ) void {
        const self: *StreamState = @ptrCast(@alignCast(context.?));
        self.finishTerminal(status, adapter_error);
    }
};

const LatencyJson = struct {
    p50_us: f64,
    p95_us: f64,
    p99_us: f64,
    min_us: f64,
    max_us: f64,
    mean_us: f64,
};

const ResultJson = struct {
    host: []const u8,
    port: u16,
    scenario: []const u8,
    transport: []const u8,
    api_mode: []const u8,
    client_execution_model: []const u8,
    compression: []const u8,
    payload_pattern: []const u8,
    load_model: []const u8,
    operation_unit: []const u8,
    throughput_scope: []const u8,
    latency_scope: []const u8,
    channel_count: usize,
    server_reactor_count: usize,
    grpc_lite_version: []const u8,
    client_optimize_mode: []const u8,
    client_target_arch: []const u8,
    client_target_os: []const u8,
    clock_source: []const u8,
    clock_uses_cpucycles: bool,
    clock_fallback_reason: []const u8,
    warmup_ns: u64,
    duration_ns: u64,
    streams: usize,
    pipeline: usize,
    payload_bytes: usize,
    initial_stream_window_bytes: u32,
    write_high_watermark_bytes: usize,
    write_low_watermark_bytes: usize,
    completed: u64,
    errors: u64,
    exchange_errors: u64,
    stream_errors: u64,
    operations_per_second: f64,
    application_bytes_per_second: f64,
    sampled_latency_count: usize,
    eligible_latency_count: u64,
    latency_sample_limit: usize,
    latency_sampling: []const u8,
    latency: LatencyJson,
};

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const config = parseArgs(args) catch |err| {
        std.debug.print("invalid arguments: {s}\n", .{@errorName(err)});
        return err;
    };
    run(init, config) catch |err| {
        std.debug.print("benchmark failed: {s}\n", .{@errorName(err)});
        return err;
    };
}

fn run(init: std.process.Init, config: Config) !void {
    const target = try std.fmt.allocPrint(init.gpa, "{s}:{d}", .{ config.host, config.port });
    defer init.gpa.free(target);
    const payload = try init.gpa.alloc(u8, config.payload_bytes);
    defer init.gpa.free(payload);
    fillPayload(payload, config.payload_pattern);

    const channels = try init.gpa.alloc(grpc.Channel, config.channels);
    defer init.gpa.free(channels);
    var initialized_channels: usize = 0;
    defer for (channels[0..initialized_channels]) |*channel| channel.deinit();
    for (channels) |*channel| {
        channel.* = try grpc.Channel.init(init.gpa, target, .{});
        initialized_channels += 1;
    }
    var timing: Timing = .{ .io = init.io };
    var measurements = switch (config.scenario) {
        .unary => try runUnary(init, config, payload, channels, &timing),
        .bidi_ping_pong, .bidi_throughput => try runBidi(
            init,
            config,
            payload,
            channels,
            &timing,
        ),
    };
    defer measurements.deinit(init.gpa);

    const result = makeResult(config, &measurements);
    try writeResult(init, config, result);
    if (result.completed == 0) return error.NoCompletedExchanges;
    if (result.errors != 0) return error.BenchmarkErrors;
}

fn runUnary(
    init: std.process.Init,
    config: Config,
    payload: []const u8,
    channels: []grpc.Channel,
    timing: *Timing,
) !Measurements {
    return switch (config.transport) {
        .raw => runRawUnary(init, config, payload, channels, timing),
        .typed => runTypedUnary(init, config, payload, channels, timing),
    };
}

fn runRawUnary(
    init: std.process.Init,
    config: Config,
    payload: []const u8,
    channels: []grpc.Channel,
    timing: *Timing,
) !Measurements {
    const slots = try init.gpa.alloc(RawUnarySlot, config.streams);
    defer init.gpa.free(slots);
    var initialized: usize = 0;
    defer for (slots[0..initialized]) |*slot| {
        slot.measurements.deinit(std.heap.page_allocator);
    };

    const latency_capacity = @max(@as(usize, 1), max_stream_latency_samples / config.streams);
    for (slots, 0..) |*slot, slot_index| {
        slot.* = .{
            .channel = &channels[slot_index % channels.len],
            .timing = timing,
            .payload = payload,
            .compression = config.compression,
            .latency_capacity = latency_capacity,
            .measurements = .{ .sampling_seed = slot_index + 1 },
        };
        initialized += 1;
        try slot.measurements.latency_samples.ensureTotalCapacityPrecise(
            std.heap.page_allocator,
            latency_capacity,
        );
    }

    const warmup_start_ns = nowNs(timing.io);
    timing.storePhase(.warmup);
    for (slots) |*slot| slot.start();
    try sleepUntil(timing.io, warmup_start_ns +| config.warmup_ns);
    startMeasurement(timing, config.duration_ns);
    try sleepUntil(timing.io, timing.measure_end_ns.load(.acquire));
    timing.storePhase(.stop);
    try waitForRawUnarySlots(timing.io, slots, channels);

    var result: Measurements = .{};
    errdefer result.deinit(init.gpa);
    for (slots) |*slot| try result.mergeShard(init.gpa, &slot.measurements);
    return result;
}

fn runTypedUnary(
    init: std.process.Init,
    config: Config,
    payload: []const u8,
    channels: []grpc.Channel,
    timing: *Timing,
) !Measurements {
    const workers = try init.gpa.alloc(TypedUnaryWorker, config.streams);
    defer init.gpa.free(workers);
    const threads = try init.gpa.alloc(std.Thread, config.streams);
    defer init.gpa.free(threads);

    var spawned: usize = 0;
    const latency_capacity = @max(@as(usize, 1), max_stream_latency_samples / config.streams);
    errdefer {
        timing.storePhase(.stop);
        for (threads[0..spawned]) |thread| thread.join();
        for (workers[0..spawned]) |*worker| worker.measurements.deinit(std.heap.page_allocator);
    }
    for (workers, threads, 0..) |*worker, *thread, worker_index| {
        worker.* = .{
            .channel = &channels[worker_index % channels.len],
            .timing = timing,
            .payload = payload,
            .compression = config.compression,
            .latency_capacity = latency_capacity,
            .measurements = .{ .sampling_seed = worker_index + 1 },
        };
        worker.measurements.latency_samples.ensureTotalCapacityPrecise(
            std.heap.page_allocator,
            latency_capacity,
        ) catch |err| {
            worker.measurements.deinit(std.heap.page_allocator);
            return err;
        };
        thread.* = std.Thread.spawn(.{}, TypedUnaryWorker.run, .{worker}) catch |err| {
            worker.measurements.deinit(std.heap.page_allocator);
            return err;
        };
        spawned += 1;
    }

    try runPhases(timing, config.warmup_ns, config.duration_ns);
    for (threads) |thread| thread.join();
    spawned = 0;
    defer for (workers) |*worker| worker.measurements.deinit(std.heap.page_allocator);

    var result: Measurements = .{};
    errdefer result.deinit(init.gpa);
    for (workers) |*worker| {
        try result.mergeShard(init.gpa, &worker.measurements);
    }
    return result;
}

fn rawUnaryMeasurementEligibilityAt(timing: *const Timing, current_ns: u64) ?bool {
    return switch (timing.loadPhase()) {
        .warmup => false,
        .measurement => if (current_ns >= timing.measure_start_ns.load(.acquire) and
            current_ns < timing.measure_end_ns.load(.acquire)) true else null,
        .setup, .stop => null,
    };
}

fn waitForRawUnarySlots(io: std.Io, slots: []const RawUnarySlot, channels: []grpc.Channel) !void {
    const drain_deadline = nowNs(io) +| drain_timeout_ns;
    while (!allRawUnarySlotsDone(slots)) {
        if (nowNs(io) >= drain_deadline) break;
        try std.Io.sleep(io, .fromMilliseconds(1), .awake);
    }
    if (allRawUnarySlotsDone(slots)) return;

    for (channels) |*channel| channel.shutdown();
    for (channels) |*channel| channel.wait();
    std.debug.assert(allRawUnarySlotsDone(slots));
    return error.UnaryDrainTimeout;
}

fn allRawUnarySlotsDone(slots: []const RawUnarySlot) bool {
    for (slots) |*slot| if (!slot.done.load(.acquire)) return false;
    return true;
}

fn runBidi(
    init: std.process.Init,
    config: Config,
    payload: []const u8,
    channels: []grpc.Channel,
    timing: *Timing,
) !Measurements {
    const states = try init.gpa.alloc(StreamState, config.streams);
    defer init.gpa.free(states);
    var initialized: usize = 0;
    defer for (states[0..initialized]) |*state| state.deinit(init.gpa);

    const latency_capacity = @max(@as(usize, 1), max_stream_latency_samples / config.streams);
    for (states, 0..) |*state, stream_index| {
        const channel = &channels[stream_index % channels.len];
        state.* = .{
            .timing = timing,
            .transport = config.transport,
            .payload = payload,
            .compression = config.compression,
            .pipeline = config.pipeline,
            .timestamps = try init.gpa.alloc(Timestamp, config.pipeline),
            .latency_capacity = latency_capacity,
            .measurements = .{ .sampling_seed = stream_index + 1 },
        };
        initialized += 1;
        try state.measurements.latency_samples.ensureTotalCapacityPrecise(
            std.heap.page_allocator,
            latency_capacity,
        );
        switch (config.transport) {
            .raw => state.raw_stream = try channel.openStream(
                raw_bidi_path,
                .{
                    .timeout_ns = config.warmup_ns +| config.duration_ns +|
                        drain_timeout_ns +| rpc_timeout_ns,
                    .send_compression = config.compression,
                },
                .{
                    .context = state,
                    .on_message = StreamState.rawMessage,
                    .on_writable = StreamState.rawWritable,
                    .on_terminal = StreamState.rawTerminal,
                },
            ),
            .typed => {
                var typed_client = grpc_pb.ServiceClient(StreamingEchoApi).init(channel);
                state.typed_stream = try typed_client.openStream(
                    std.heap.page_allocator,
                    "Chat",
                    .{
                        .timeout_ns = config.warmup_ns +| config.duration_ns +|
                            drain_timeout_ns +| rpc_timeout_ns,
                        .send_compression = config.compression,
                    },
                    .{
                        .context = state,
                        .on_message = StreamState.typedMessage,
                        .on_writable = StreamState.typedWritable,
                        .on_terminal = StreamState.typedTerminal,
                    },
                );
            },
        }
    }

    const warmup_start_ns = nowNs(timing.io);
    timing.storePhase(.warmup);
    for (states) |*state| try state.bootstrap();
    try sleepUntil(timing.io, warmup_start_ns +| config.warmup_ns);
    startMeasurement(timing, config.duration_ns);
    try sleepUntil(timing.io, timing.measure_end_ns.load(.acquire));
    timing.storePhase(.stop);

    for (states) |*state| switch (config.transport) {
        .raw => state.raw_stream.?.closeSend() catch |err| {
            if (err != error.StreamClosed) return err;
        },
        .typed => state.typed_stream.?.closeSend() catch |err| {
            if (err != error.StreamClosed) return err;
        },
    };
    try waitForStreams(timing.io, states);

    var result: Measurements = .{};
    errdefer result.deinit(init.gpa);
    for (states, 0..) |*state, stream_index| {
        try result.mergeShard(init.gpa, &state.measurements);
        if (state.terminal_count != 1 or state.terminal_status != .ok or state.terminal_error) {
            result.stream_errors += 1;
            std.debug.print(
                "stream {d} terminal: count={d} status={t} local_error={}\n",
                .{ stream_index, state.terminal_count, state.terminal_status, state.terminal_error },
            );
        }
    }
    return result;
}

fn runPhases(timing: *Timing, warmup_ns: u64, duration_ns: u64) !void {
    const warmup_start_ns = nowNs(timing.io);
    timing.storePhase(.warmup);
    try sleepUntil(timing.io, warmup_start_ns +| warmup_ns);
    startMeasurement(timing, duration_ns);
    try sleepUntil(timing.io, timing.measure_end_ns.load(.acquire));
    timing.storePhase(.stop);
}

fn startMeasurement(timing: *Timing, duration_ns: u64) void {
    const start_ns = nowNs(timing.io);
    timing.measure_start_ns.store(start_ns, .release);
    timing.measure_end_ns.store(start_ns +| duration_ns, .release);
    timing.storePhase(.measurement);
}

fn waitForStreams(io: std.Io, states: []StreamState) !void {
    const drain_deadline = nowNs(io) +| drain_timeout_ns;
    while (!allStreamsDone(states)) {
        if (nowNs(io) >= drain_deadline) break;
        try std.Io.sleep(io, .fromMilliseconds(1), .awake);
    }
    if (allStreamsDone(states)) return;

    for (states) |*state| if (!state.done.load(.acquire)) state.cancel();
    const cancel_deadline = nowNs(io) +| cancel_timeout_ns;
    while (!allStreamsDone(states)) {
        if (nowNs(io) >= cancel_deadline) return error.StreamDrainTimeout;
        try std.Io.sleep(io, .fromMilliseconds(1), .awake);
    }
}

fn allStreamsDone(states: []const StreamState) bool {
    for (states) |*state| if (!state.done.load(.acquire)) return false;
    return true;
}

fn sleepUntil(io: std.Io, deadline_ns: u64) !void {
    while (true) {
        const current_ns = nowNs(io);
        if (current_ns >= deadline_ns) return;
        try std.Io.sleep(io, .fromNanoseconds(@intCast(deadline_ns - current_ns)), .awake);
    }
}

fn nowNs(io: std.Io) u64 {
    return grpc.internal.fastNowNs(io);
}

fn validatePayload(response: []const u8, expected: []const u8) bool {
    return std.mem.eql(u8, response, expected);
}

fn fillPayload(payload: []u8, pattern: PayloadPattern) void {
    switch (pattern) {
        .repeated => @memset(payload, 0x5a),
        .deterministic_random => {
            var state: u32 = 0x6d2b79f5;
            for (payload) |*byte| {
                state = state *% 1_664_525 +% 1_013_904_223;
                byte.* = @truncate(state >> 24);
            }
        },
    }
}

fn makeResult(config: Config, measurements: *Measurements) ResultJson {
    std.mem.sortUnstable(
        Measurements.LatencySample,
        measurements.latency_samples.items,
        {},
        struct {
            fn lessThan(_: void, lhs: Measurements.LatencySample, rhs: Measurements.LatencySample) bool {
                return lhs.ns < rhs.ns;
            }
        }.lessThan,
    );
    const count = measurements.latency_samples.items.len;
    var weighted_sum: f64 = 0;
    var total_weight: f64 = 0;
    for (measurements.latency_samples.items) |sample| {
        weighted_sum += @as(f64, @floatFromInt(sample.ns)) * sample.weight;
        total_weight += sample.weight;
    }
    const duration: f64 = @floatFromInt(config.duration_ns);
    const completed: f64 = @floatFromInt(measurements.completed);
    const payload_bytes: f64 = @floatFromInt(config.payload_bytes);
    return .{
        .host = config.host,
        .port = config.port,
        .scenario = config.scenario.jsonName(),
        .transport = "grpc-http2-insecure",
        .api_mode = config.transport.jsonName(),
        .client_execution_model = clientExecutionModel(config),
        .compression = config.compression.name(),
        .payload_pattern = config.payload_pattern.jsonName(),
        .load_model = "closed-loop-fixed-concurrency",
        .operation_unit = if (config.scenario == .unary) "rpc" else "message-exchange",
        .throughput_scope = "exchanges-started-in-measurement-window",
        .latency_scope = "full-latency-for-exchanges-started-in-measurement-window",
        .channel_count = config.channels,
        .server_reactor_count = config.server_reactors,
        .grpc_lite_version = grpc.version,
        .client_optimize_mode = @tagName(builtin.mode),
        .client_target_arch = @tagName(builtin.cpu.arch),
        .client_target_os = @tagName(builtin.os.tag),
        .clock_source = grpc.internal.fastClockImplementation(),
        .clock_uses_cpucycles = grpc.internal.fastClockUsesCpuCycles(),
        .clock_fallback_reason = grpc.internal.fastClockFallbackReason(),
        .warmup_ns = config.warmup_ns,
        .duration_ns = config.duration_ns,
        .streams = config.streams,
        .pipeline = config.pipeline,
        .payload_bytes = config.payload_bytes,
        .initial_stream_window_bytes = initial_stream_window_bytes,
        .write_high_watermark_bytes = write_high_watermark_bytes,
        .write_low_watermark_bytes = write_low_watermark_bytes,
        .completed = measurements.completed,
        .errors = measurements.exchange_errors + measurements.stream_errors,
        .exchange_errors = measurements.exchange_errors,
        .stream_errors = measurements.stream_errors,
        .operations_per_second = completed * @as(f64, @floatFromInt(std.time.ns_per_s)) / duration,
        .application_bytes_per_second = completed * payload_bytes * 2.0 *
            @as(f64, @floatFromInt(std.time.ns_per_s)) / duration,
        .sampled_latency_count = count,
        .eligible_latency_count = measurements.eligible_latency_count,
        .latency_sample_limit = max_stream_latency_samples,
        .latency_sampling = "weighted-per-worker-deterministic-reservoir",
        .latency = .{
            .p50_us = nsToUs(weightedPercentile(measurements.latency_samples.items, 50)),
            .p95_us = nsToUs(weightedPercentile(measurements.latency_samples.items, 95)),
            .p99_us = nsToUs(weightedPercentile(measurements.latency_samples.items, 99)),
            .min_us = nsToUs(if (measurements.eligible_latency_count == 0)
                0
            else
                measurements.observed_min_latency_ns),
            .max_us = nsToUs(if (measurements.eligible_latency_count == 0)
                0
            else
                measurements.observed_max_latency_ns),
            .mean_us = if (total_weight == 0) 0 else weighted_sum / total_weight / 1000.0,
        },
    };
}

fn clientExecutionModel(config: Config) []const u8 {
    if (config.scenario == .unary and config.transport == .typed) return "blocking-workers";
    return "event-driven";
}

fn weightedPercentile(sorted: []const Measurements.LatencySample, percent: u8) u64 {
    if (sorted.len == 0) return 0;
    var total_weight: f64 = 0;
    for (sorted) |sample| total_weight += sample.weight;
    const target = total_weight * @as(f64, @floatFromInt(percent)) / 100.0;
    var cumulative: f64 = 0;
    for (sorted) |sample| {
        cumulative += sample.weight;
        if (cumulative >= target) return sample.ns;
    }
    return sorted[sorted.len - 1].ns;
}

fn mix64(value: u64) u64 {
    var mixed = value +% 0x9e3779b97f4a7c15;
    mixed = (mixed ^ (mixed >> 30)) *% 0xbf58476d1ce4e5b9;
    mixed = (mixed ^ (mixed >> 27)) *% 0x94d049bb133111eb;
    return mixed ^ (mixed >> 31);
}

fn nsToUs(value: u64) f64 {
    return @as(f64, @floatFromInt(value)) / 1000.0;
}

fn writeResult(init: std.process.Init, config: Config, result: ResultJson) !void {
    const json = try encodeResult(init.gpa, result, config.compact);
    defer init.gpa.free(json);
    try std.Io.File.writeStreamingAll(.stdout(), init.io, json);
    if (config.output) |path| {
        var file = if (std.fs.path.isAbsolute(path))
            try std.Io.Dir.createFileAbsolute(init.io, path, .{})
        else
            try std.Io.Dir.createFile(.cwd(), init.io, path, .{});
        defer file.close(init.io);
        try std.Io.File.writeStreamingAll(file, init.io, json);
    }
}

fn encodeResult(allocator: std.mem.Allocator, result: ResultJson, compact: bool) ![]u8 {
    var writer: std.Io.Writer.Allocating = .init(allocator);
    errdefer writer.deinit();
    try std.json.Stringify.value(
        result,
        .{ .whitespace = if (compact) .minified else .indent_2 },
        &writer.writer,
    );
    try writer.writer.writeByte('\n');
    return writer.toOwnedSlice();
}

fn parseArgs(args: []const []const u8) !Config {
    var config: Config = .{};
    var seen: Seen = .{};
    var index: usize = 1;
    while (index < args.len) : (index += 1) {
        const arg = args[index];
        if (std.mem.eql(u8, arg, "--compact")) {
            if (seen.compact) return error.DuplicateOption;
            seen.compact = true;
            config.compact = true;
            continue;
        }
        if (try optionValue(args, &index, arg, "--host")) |value| {
            try markSeen(&seen.host);
            config.host = try parseHost(value);
        } else if (try optionValue(args, &index, arg, "--port")) |value| {
            try markSeen(&seen.port);
            config.port = try parsePositiveInt(u16, value);
        } else if (try optionValue(args, &index, arg, "--scenario")) |value| {
            try markSeen(&seen.scenario);
            config.scenario = try parseScenario(value);
        } else if (try optionValue(args, &index, arg, "--transport")) |value| {
            try markSeen(&seen.transport);
            config.transport = try parseTransport(value);
        } else if (try optionValue(args, &index, arg, "--warmup")) |value| {
            try markSeen(&seen.warmup);
            config.warmup_ns = try parseDuration(value);
        } else if (try optionValue(args, &index, arg, "--duration")) |value| {
            try markSeen(&seen.duration);
            config.duration_ns = try parseDuration(value);
        } else if (try optionValue(args, &index, arg, "--streams")) |value| {
            try markSeen(&seen.streams);
            config.streams = try parsePositiveInt(usize, value);
        } else if (try optionValue(args, &index, arg, "--channels")) |value| {
            try markSeen(&seen.channels);
            config.channels = try parsePositiveInt(usize, value);
        } else if (try optionValue(args, &index, arg, "--server-reactors")) |value| {
            try markSeen(&seen.server_reactors);
            config.server_reactors = try parsePositiveInt(usize, value);
        } else if (try optionValue(args, &index, arg, "--pipeline")) |value| {
            try markSeen(&seen.pipeline);
            config.pipeline = try parsePositiveInt(usize, value);
        } else if (try optionValue(args, &index, arg, "--payload-bytes")) |value| {
            try markSeen(&seen.payload_bytes);
            config.payload_bytes = try parsePositiveInt(usize, value);
            if (config.payload_bytes > max_payload_bytes) return error.PayloadTooLarge;
        } else if (try optionValue(args, &index, arg, "--payload-pattern")) |value| {
            try markSeen(&seen.payload_pattern);
            config.payload_pattern = try parsePayloadPattern(value);
        } else if (try optionValue(args, &index, arg, "--compression")) |value| {
            try markSeen(&seen.compression);
            config.compression = try parseCompression(value);
        } else if (try optionValue(args, &index, arg, "--output")) |value| {
            try markSeen(&seen.output);
            if (std.mem.indexOfScalar(u8, value, 0) != null) return error.InvalidOutput;
            config.output = value;
        } else {
            return error.UnknownArgument;
        }
    }
    if (config.channels > config.streams) return error.ChannelsExceedStreams;
    if (config.scenario != .bidi_throughput) config.pipeline = 1;
    return config;
}

const Seen = struct {
    host: bool = false,
    port: bool = false,
    scenario: bool = false,
    transport: bool = false,
    warmup: bool = false,
    duration: bool = false,
    streams: bool = false,
    channels: bool = false,
    server_reactors: bool = false,
    pipeline: bool = false,
    payload_bytes: bool = false,
    payload_pattern: bool = false,
    compression: bool = false,
    output: bool = false,
    compact: bool = false,
};

fn markSeen(seen: *bool) !void {
    if (seen.*) return error.DuplicateOption;
    seen.* = true;
}

fn optionValue(
    args: []const []const u8,
    index: *usize,
    arg: []const u8,
    name: []const u8,
) !?[]const u8 {
    if (std.mem.eql(u8, arg, name)) {
        index.* += 1;
        if (index.* >= args.len or std.mem.startsWith(u8, args[index.*], "--")) {
            return error.MissingOptionValue;
        }
        if (args[index.*].len == 0) return error.InvalidOptionValue;
        return args[index.*];
    }
    if (arg.len > name.len and std.mem.startsWith(u8, arg, name) and arg[name.len] == '=') {
        const value = arg[name.len + 1 ..];
        if (value.len == 0) return error.InvalidOptionValue;
        return value;
    }
    return null;
}

fn parseHost(value: []const u8) ![]const u8 {
    if (value.len == 0 or std.mem.indexOfScalar(u8, value, 0) != null) return error.InvalidHost;
    _ = std.Io.net.IpAddress.parseIp4(value, 0) catch return error.InvalidHost;
    return value;
}

fn parsePositiveInt(comptime T: type, value: []const u8) !T {
    if (!allDigits(value)) return error.InvalidInteger;
    const parsed = std.fmt.parseInt(T, value, 10) catch return error.InvalidInteger;
    if (parsed == 0) return error.ZeroValue;
    return parsed;
}

fn parseScenario(value: []const u8) !Scenario {
    if (std.mem.eql(u8, value, "unary")) return .unary;
    if (std.mem.eql(u8, value, "bidi-ping-pong")) return .bidi_ping_pong;
    if (std.mem.eql(u8, value, "bidi-throughput")) return .bidi_throughput;
    return error.InvalidScenario;
}

fn parseTransport(value: []const u8) !Transport {
    if (std.mem.eql(u8, value, "raw")) return .raw;
    if (std.mem.eql(u8, value, "typed")) return .typed;
    return error.InvalidTransport;
}

fn parseCompression(value: []const u8) !grpc.Compression {
    if (std.mem.eql(u8, value, "identity")) return .identity;
    if (std.mem.eql(u8, value, "gzip")) return .gzip;
    return error.InvalidCompression;
}

fn parsePayloadPattern(value: []const u8) !PayloadPattern {
    if (std.mem.eql(u8, value, "repeated")) return .repeated;
    if (std.mem.eql(u8, value, "deterministic-random")) return .deterministic_random;
    return error.InvalidPayloadPattern;
}

fn parseDuration(value: []const u8) !u64 {
    const suffixes = [_]struct { name: []const u8, scale: u64, decimals: usize }{
        .{ .name = "ns", .scale = 1, .decimals = 0 },
        .{ .name = "us", .scale = std.time.ns_per_us, .decimals = 3 },
        .{ .name = "ms", .scale = std.time.ns_per_ms, .decimals = 6 },
        .{ .name = "s", .scale = std.time.ns_per_s, .decimals = 9 },
    };
    for (suffixes) |suffix| {
        if (!std.mem.endsWith(u8, value, suffix.name)) continue;
        const number = value[0 .. value.len - suffix.name.len];
        if (number.len == 0) return error.InvalidDuration;
        const dot = std.mem.indexOfScalar(u8, number, '.');
        const whole_text = if (dot) |position| number[0..position] else number;
        if (!allDigits(whole_text)) return error.InvalidDuration;
        const whole = std.fmt.parseInt(u64, whole_text, 10) catch return error.InvalidDuration;
        var result = std.math.mul(u64, whole, suffix.scale) catch return error.InvalidDuration;
        if (dot) |position| {
            const fraction = number[position + 1 ..];
            if (!allDigits(fraction) or fraction.len > suffix.decimals) return error.InvalidDuration;
            const fraction_value = std.fmt.parseInt(u64, fraction, 10) catch return error.InvalidDuration;
            var divisor: u64 = 1;
            for (0..fraction.len) |_| divisor *= 10;
            const scaled = std.math.mul(u64, fraction_value, suffix.scale) catch return error.InvalidDuration;
            result = std.math.add(u64, result, scaled / divisor) catch return error.InvalidDuration;
        }
        if (result == 0) return error.ZeroValue;
        return result;
    }
    return error.InvalidDuration;
}

fn allDigits(value: []const u8) bool {
    if (value.len == 0) return false;
    for (value) |byte| if (byte < '0' or byte > '9') return false;
    return true;
}

test "parse duration suffixes and reject malformed values" {
    try std.testing.expectEqual(@as(u64, 1), try parseDuration("1ns"));
    try std.testing.expectEqual(@as(u64, 1500), try parseDuration("1.5us"));
    try std.testing.expectEqual(@as(u64, 250 * std.time.ns_per_ms), try parseDuration("250ms"));
    try std.testing.expectEqual(@as(u64, 3 * std.time.ns_per_s), try parseDuration("3s"));
    try std.testing.expectError(error.ZeroValue, parseDuration("0s"));
    try std.testing.expectError(error.InvalidDuration, parseDuration("1"));
    try std.testing.expectError(error.InvalidDuration, parseDuration(".5s"));
    try std.testing.expectError(error.InvalidDuration, parseDuration("1.1ns"));
}

test "parse benchmark client defaults and options" {
    const defaults = try parseArgs(&.{"client"});
    try std.testing.expectEqualStrings("127.0.0.1", defaults.host);
    try std.testing.expectEqual(@as(u16, 50061), defaults.port);
    try std.testing.expectEqual(Scenario.unary, defaults.scenario);
    try std.testing.expectEqual(Transport.raw, defaults.transport);
    try std.testing.expectEqual(@as(usize, 1), defaults.channels);
    try std.testing.expectEqual(PayloadPattern.repeated, defaults.payload_pattern);
    try std.testing.expectEqual(@as(usize, 1), defaults.pipeline);

    const config = try parseArgs(&.{
        "client",
        "--host=127.0.0.2",
        "--port",
        "60000",
        "--scenario=bidi-ping-pong",
        "--transport=typed",
        "--warmup=500ms",
        "--duration",
        "2s",
        "--streams=4",
        "--channels=2",
        "--server-reactors=4",
        "--pipeline=99",
        "--payload-bytes=1024",
        "--payload-pattern=deterministic-random",
        "--compression=gzip",
        "--output=result.json",
        "--compact",
    });
    try std.testing.expectEqual(Scenario.bidi_ping_pong, config.scenario);
    try std.testing.expectEqual(Transport.typed, config.transport);
    try std.testing.expectEqual(@as(usize, 2), config.channels);
    try std.testing.expectEqual(@as(usize, 4), config.server_reactors);
    try std.testing.expectEqual(PayloadPattern.deterministic_random, config.payload_pattern);
    try std.testing.expectEqual(@as(usize, 1), config.pipeline);
    try std.testing.expect(config.compact);
}

test "reject malformed benchmark client options" {
    try std.testing.expectError(error.UnknownArgument, parseArgs(&.{ "client", "--other" }));
    try std.testing.expectError(error.MissingOptionValue, parseArgs(&.{ "client", "--host" }));
    try std.testing.expectError(error.InvalidHost, parseArgs(&.{ "client", "--host=localhost" }));
    try std.testing.expectError(error.ZeroValue, parseArgs(&.{ "client", "--port=0" }));
    try std.testing.expectError(error.ZeroValue, parseArgs(&.{ "client", "--streams=0" }));
    try std.testing.expectError(error.ZeroValue, parseArgs(&.{ "client", "--channels=0" }));
    try std.testing.expectError(error.ZeroValue, parseArgs(&.{ "client", "--server-reactors=0" }));
    try std.testing.expectError(error.ChannelsExceedStreams, parseArgs(&.{
        "client",
        "--streams=2",
        "--channels=3",
    }));
    try std.testing.expectError(error.InvalidPayloadPattern, parseArgs(&.{
        "client",
        "--payload-pattern=other",
    }));
    try std.testing.expectError(error.PayloadTooLarge, parseArgs(&.{
        "client",
        "--payload-bytes=4194300",
    }));
    try std.testing.expectError(error.DuplicateOption, parseArgs(&.{
        "client",
        "--transport=raw",
        "--transport=typed",
    }));
}

test "payload patterns are stable and distinct" {
    var repeated: [16]u8 = undefined;
    fillPayload(&repeated, .repeated);
    try std.testing.expectEqualSlices(u8, &([_]u8{0x5a} ** 16), &repeated);

    var first: [16]u8 = undefined;
    var second: [16]u8 = undefined;
    fillPayload(&first, .deterministic_random);
    fillPayload(&second, .deterministic_random);
    try std.testing.expectEqualSlices(u8, &first, &second);
    try std.testing.expect(!std.mem.eql(u8, &first, &repeated));
}

test "async unary slot accounting follows start eligibility through drain" {
    var measurements: Measurements = .{};
    defer measurements.deinit(std.testing.allocator);
    try measurements.latency_samples.ensureTotalCapacityPrecise(std.testing.allocator, 3);

    measurements.recordExchange(false, true, 10, 3);
    measurements.recordExchange(true, true, 20, 3);
    measurements.recordExchange(true, false, 30, 3);

    try std.testing.expectEqual(@as(u64, 1), measurements.completed);
    try std.testing.expectEqual(@as(u64, 1), measurements.exchange_errors);
    try std.testing.expectEqual(@as(u64, 1), measurements.eligible_latency_count);
    try std.testing.expectEqual(@as(u64, 20), measurements.latency_samples.items[0].ns);
}

test "async unary slots classify start windows and stop at boundaries" {
    var timing: Timing = .{ .io = std.testing.io };
    try std.testing.expectEqual(@as(?bool, null), rawUnaryMeasurementEligibilityAt(&timing, 99));
    timing.storePhase(.warmup);
    try std.testing.expectEqual(@as(?bool, false), rawUnaryMeasurementEligibilityAt(&timing, 99));
    timing.measure_start_ns.store(10, .release);
    timing.measure_end_ns.store(100, .release);
    timing.storePhase(.measurement);
    try std.testing.expectEqual(@as(?bool, null), rawUnaryMeasurementEligibilityAt(&timing, 9));
    try std.testing.expectEqual(@as(?bool, true), rawUnaryMeasurementEligibilityAt(&timing, 99));
    try std.testing.expectEqual(@as(?bool, null), rawUnaryMeasurementEligibilityAt(&timing, 100));
    timing.storePhase(.stop);
    try std.testing.expectEqual(@as(?bool, null), rawUnaryMeasurementEligibilityAt(&timing, 99));
}

test "measurement merge preserves observed extrema beyond reservoir" {
    var first: Measurements = .{};
    defer first.deinit(std.testing.allocator);
    try first.latency_samples.ensureTotalCapacityPrecise(std.testing.allocator, 1);
    first.addLatency(100, 1);
    first.addLatency(10, 1);

    var second: Measurements = .{};
    defer second.deinit(std.testing.allocator);
    try second.latency_samples.ensureTotalCapacityPrecise(std.testing.allocator, 1);
    second.addLatency(200, 1);
    second.addLatency(1_000, 1);

    var merged: Measurements = .{};
    defer merged.deinit(std.testing.allocator);
    try merged.mergeShard(std.testing.allocator, &first);
    try merged.mergeShard(std.testing.allocator, &second);
    try std.testing.expectEqual(@as(u64, 10), merged.observed_min_latency_ns);
    try std.testing.expectEqual(@as(u64, 1_000), merged.observed_max_latency_ns);
    try std.testing.expectEqual(@as(u64, 4), merged.eligible_latency_count);
    const result = makeResult(.{}, &merged);
    try std.testing.expectEqual(@as(f64, 0.01), result.latency.min_us);
    try std.testing.expectEqual(@as(f64, 1), result.latency.max_us);
}

test "percentile uses nearest rank" {
    const values = [_]Measurements.LatencySample{
        .{ .ns = 1 }, .{ .ns = 2 }, .{ .ns = 3 }, .{ .ns = 4 }, .{ .ns = 5 },
        .{ .ns = 6 }, .{ .ns = 7 }, .{ .ns = 8 }, .{ .ns = 9 }, .{ .ns = 10 },
    };
    try std.testing.expectEqual(@as(u64, 5), weightedPercentile(&values, 50));
    try std.testing.expectEqual(@as(u64, 10), weightedPercentile(&values, 95));
    try std.testing.expectEqual(@as(u64, 10), weightedPercentile(&values, 99));
    try std.testing.expectEqual(@as(u64, 0), weightedPercentile(&.{}, 50));
}

test "weighted percentile preserves shard contribution" {
    const samples = [_]Measurements.LatencySample{
        .{ .ns = 1, .weight = 9 },
        .{ .ns = 10, .weight = 1 },
    };
    try std.testing.expectEqual(@as(u64, 1), weightedPercentile(&samples, 50));
    try std.testing.expectEqual(@as(u64, 10), weightedPercentile(&samples, 95));
}

test "result JSON has stable benchmark shape" {
    const result: ResultJson = .{
        .host = "127.0.0.1",
        .port = 50061,
        .scenario = "unary",
        .transport = "grpc-http2-insecure",
        .api_mode = "raw",
        .client_execution_model = "event-driven",
        .compression = "identity",
        .payload_pattern = "repeated",
        .load_model = "closed-loop-fixed-concurrency",
        .operation_unit = "rpc",
        .throughput_scope = "exchanges-started-in-measurement-window",
        .latency_scope = "full-latency-for-exchanges-started-in-measurement-window",
        .channel_count = 1,
        .server_reactor_count = 1,
        .grpc_lite_version = grpc.version,
        .client_optimize_mode = "ReleaseFast",
        .client_target_arch = "x86_64",
        .client_target_os = "linux",
        .clock_source = "amd64-tsc",
        .clock_uses_cpucycles = true,
        .clock_fallback_reason = "none",
        .warmup_ns = 1,
        .duration_ns = 2,
        .streams = 1,
        .pipeline = 32,
        .payload_bytes = 128,
        .initial_stream_window_bytes = initial_stream_window_bytes,
        .write_high_watermark_bytes = write_high_watermark_bytes,
        .write_low_watermark_bytes = write_low_watermark_bytes,
        .completed = 3,
        .errors = 0,
        .exchange_errors = 0,
        .stream_errors = 0,
        .operations_per_second = 1.5,
        .application_bytes_per_second = 384,
        .sampled_latency_count = 3,
        .eligible_latency_count = 3,
        .latency_sample_limit = max_stream_latency_samples,
        .latency_sampling = "weighted-per-worker-deterministic-reservoir",
        .latency = .{
            .p50_us = 1,
            .p95_us = 2,
            .p99_us = 3,
            .min_us = 1,
            .max_us = 3,
            .mean_us = 2,
        },
    };
    const json = try encodeResult(std.testing.allocator, result, true);
    defer std.testing.allocator.free(json);
    var parsed = try std.json.parseFromSlice(std.json.Value, std.testing.allocator, json, .{});
    defer parsed.deinit();
    const object = parsed.value.object;
    try std.testing.expect(object.contains("scenario"));
    try std.testing.expectEqualStrings(
        "event-driven",
        object.get("client_execution_model").?.string,
    );
    try std.testing.expect(object.contains("operations_per_second"));
    try std.testing.expectEqual(@as(i64, 1), object.get("server_reactor_count").?.integer);
    try std.testing.expect(object.contains("sampled_latency_count"));
    try std.testing.expect(object.contains("latency"));
    try std.testing.expectEqual(@as(usize, 1), std.mem.count(u8, json, "\n"));
}

test "unary execution model distinguishes raw and typed clients" {
    try std.testing.expectEqualStrings("event-driven", clientExecutionModel(.{
        .scenario = .unary,
        .transport = .raw,
    }));
    try std.testing.expectEqualStrings("blocking-workers", clientExecutionModel(.{
        .scenario = .unary,
        .transport = .typed,
    }));
    try std.testing.expectEqualStrings("event-driven", clientExecutionModel(.{
        .scenario = .bidi_ping_pong,
        .transport = .typed,
    }));
}
