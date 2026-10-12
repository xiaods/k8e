const std = @import("std");
const builtin = @import("builtin");
const grpc = @import("grpc_lite");
const testing = @import("grpc_testing");

const large_request_size = 271828;
const large_response_size = 314159;
const max_rpc_timeout_ns = 60 * std.time.ns_per_s;
const streaming_request_sizes = [_]usize{ 27182, 8, 1828, 45904 };
const streaming_response_sizes = [_]usize{ 31415, 9, 2653, 58979 };
const zero_streaming_body = [_]u8{0} ** streaming_request_sizes[streaming_request_sizes.len - 1];
const special_status_message = "\t\ntest with whitespace\r\nand Unicode BMP \xE2\x98\xBA and non-BMP \xF0\x9F\x98\x88\t\n";

const Config = struct {
    server_host: []const u8 = "127.0.0.1",
    server_port: u16 = 10000,
    test_case: []const u8 = "large_unary",
    use_tls: bool = false,
    ca_file: []const u8 = "",
    soak_iterations: usize = 10,
    soak_max_failures: usize = 0,
    soak_overall_timeout_seconds: u64 = 10,
};

pub fn main(init: std.process.Init) !void {
    const args = try init.minimal.args.toSlice(init.arena.allocator());
    const config = parseArgs(args) catch |err| {
        std.debug.print("invalid arguments: {s}\n", .{@errorName(err)});
        return err;
    };
    const ca_pem = if (config.use_tls)
        try std.Io.Dir.cwd().readFileAlloc(init.io, config.ca_file, init.gpa, .limited(4 * 1024 * 1024))
    else
        null;
    defer if (ca_pem) |bytes| init.gpa.free(bytes);
    const channel_options: grpc.ChannelOptions = .{
        .tls = if (ca_pem) |bytes| .{ .ca_certificates_pem = bytes } else null,
    };

    const target = try std.fmt.allocPrint(init.gpa, "{s}:{d}", .{
        config.server_host,
        config.server_port,
    });
    defer init.gpa.free(target);

    if (std.mem.eql(u8, config.test_case, "rpc_soak")) {
        try rpcSoak(init, target, config, channel_options);
        std.debug.print("interop case passed: {s}\n", .{config.test_case});
        return;
    }
    if (std.mem.eql(u8, config.test_case, "channel_soak")) {
        try channelSoak(init, target, config, channel_options);
        std.debug.print("interop case passed: {s}\n", .{config.test_case});
        return;
    }

    var channel = try grpc.Channel.init(init.gpa, target, channel_options);
    defer channel.deinit();

    if (std.mem.eql(u8, config.test_case, "empty_unary")) {
        try emptyUnary(init.gpa, &channel);
    } else if (std.mem.eql(u8, config.test_case, "large_unary")) {
        try largeUnary(init.gpa, &channel, max_rpc_timeout_ns);
    } else if (std.mem.eql(u8, config.test_case, "client_compressed_unary")) {
        try clientCompressedUnary(init.gpa, &channel);
    } else if (std.mem.eql(u8, config.test_case, "server_compressed_unary")) {
        try serverCompressedUnary(init.gpa, &channel);
    } else if (std.mem.eql(u8, config.test_case, "client_streaming")) {
        try clientStreaming(init.gpa, init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "server_streaming")) {
        try serverStreaming(init.gpa, init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "ping_pong")) {
        try pingPong(init.gpa, init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "empty_stream")) {
        try emptyStream(init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "cancel_after_begin")) {
        try cancelAfterBegin(init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "cancel_after_first_response")) {
        try cancelAfterFirstResponse(init.gpa, init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "timeout_on_sleeping_server")) {
        try timeoutOnSleepingServer(init.gpa, init.io, &channel);
    } else if (std.mem.eql(u8, config.test_case, "special_status_message")) {
        try specialStatusMessage(init.gpa, &channel);
    } else if (std.mem.eql(u8, config.test_case, "unimplemented_method")) {
        try expectUnimplemented(init.gpa, &channel, "/grpc.testing.TestService/UnimplementedCall");
    } else if (std.mem.eql(u8, config.test_case, "unimplemented_service")) {
        try expectUnimplemented(init.gpa, &channel, "/grpc.testing.UnimplementedService/UnimplementedCall");
    } else if (std.mem.eql(u8, config.test_case, "goaway")) {
        try goaway(init, &channel);
    } else if (std.mem.eql(u8, config.test_case, "rst_after_header") or
        std.mem.eql(u8, config.test_case, "rst_after_data") or
        std.mem.eql(u8, config.test_case, "rst_during_data"))
    {
        try expectLargeUnaryFailure(init.gpa, &channel);
    } else if (std.mem.eql(u8, config.test_case, "ping") or
        std.mem.eql(u8, config.test_case, "data_frame_padding") or
        std.mem.eql(u8, config.test_case, "no_df_padding_sanity_test"))
    {
        try largeUnary(init.gpa, &channel, max_rpc_timeout_ns);
    } else if (std.mem.eql(u8, config.test_case, "max_streams")) {
        try maxStreams(init.gpa, &channel);
    } else {
        std.debug.print("unsupported test case: {s}\n", .{config.test_case});
        return error.UnsupportedTestCase;
    }
    std.debug.print("interop case passed: {s}\n", .{config.test_case});
}

fn rpcSoak(init: std.process.Init, target: []const u8, config: Config, channel_options: grpc.ChannelOptions) !void {
    const deadline = soakDeadline(init.io, config.soak_overall_timeout_seconds);
    try checkSoakDeadline(init.io, deadline);
    var channel = try grpc.Channel.init(init.gpa, target, channel_options);
    defer channel.deinit();
    try checkSoakDeadline(init.io, deadline);

    var failures: usize = 0;
    for (0..config.soak_iterations) |iteration| {
        const timeout_ns = try soakRpcTimeout(init.io, deadline);
        largeUnary(init.gpa, &channel, timeout_ns) catch |err| {
            try handleSoakFailure(&failures, config.soak_max_failures, iteration, err);
        };
        try checkSoakDeadline(init.io, deadline);
    }
}

fn channelSoak(init: std.process.Init, target: []const u8, config: Config, channel_options: grpc.ChannelOptions) !void {
    const deadline = soakDeadline(init.io, config.soak_overall_timeout_seconds);
    var failures: usize = 0;
    for (0..config.soak_iterations) |iteration| {
        const iteration_error = try channelSoakIteration(init.gpa, init.io, target, deadline, channel_options);
        if (iteration_error) |err| {
            try handleSoakFailure(&failures, config.soak_max_failures, iteration, err);
        }
        try checkSoakDeadline(init.io, deadline);
    }
}

fn channelSoakIteration(
    allocator: std.mem.Allocator,
    io: std.Io,
    target: []const u8,
    deadline: i96,
    channel_options: grpc.ChannelOptions,
) !?anyerror {
    try checkSoakDeadline(io, deadline);
    var channel = grpc.Channel.init(allocator, target, channel_options) catch |err| return @as(?anyerror, err);
    defer channel.deinit();
    const timeout_ns = try soakRpcTimeout(io, deadline);
    largeUnary(allocator, &channel, timeout_ns) catch |err| return @as(?anyerror, err);
    return null;
}

fn soakDeadline(io: std.Io, timeout_seconds: u64) i96 {
    return std.Io.Clock.awake.now(io).nanoseconds +|
        @as(i96, timeout_seconds) * std.time.ns_per_s;
}

fn checkSoakDeadline(io: std.Io, deadline: i96) !void {
    if (std.Io.Clock.awake.now(io).nanoseconds >= deadline) {
        return error.SoakOverallTimeout;
    }
}

fn soakRpcTimeout(io: std.Io, deadline: i96) !u64 {
    return remainingSoakRpcTimeout(std.Io.Clock.awake.now(io).nanoseconds, deadline);
}

fn remainingSoakRpcTimeout(now: i96, deadline: i96) !u64 {
    if (now >= deadline) return error.SoakOverallTimeout;
    return @intCast(@min(deadline -| now, @as(i96, max_rpc_timeout_ns)));
}

fn handleSoakFailure(
    failures: *usize,
    max_failures: usize,
    iteration: usize,
    err: anyerror,
) !void {
    if (err == error.OutOfMemory) return err;
    try recordSoakFailure(failures, max_failures, iteration, err);
}

fn recordSoakFailure(
    failures: *usize,
    max_failures: usize,
    iteration: usize,
    err: anyerror,
) !void {
    failures.* += 1;
    if (!builtin.is_test) {
        std.debug.print("soak iteration {d} failed: {s}\n", .{ iteration + 1, @errorName(err) });
    }
    if (failures.* > max_failures) return error.SoakFailureBudgetExceeded;
}

fn emptyUnary(allocator: std.mem.Allocator, channel: *grpc.Channel) !void {
    var result = try callMessage(
        allocator,
        channel,
        "/grpc.testing.TestService/EmptyCall",
        testing.Empty{},
    );
    defer result.deinit();
    try expectStatus(&result, .ok, "");

    var reader: std.Io.Reader = .fixed(result.payload);
    var response = try testing.Empty.decode(&reader, allocator);
    defer response.deinit(allocator);
}

fn largeUnary(allocator: std.mem.Allocator, channel: *grpc.Channel, timeout_ns: u64) !void {
    var result = try callLargeUnary(allocator, channel, timeout_ns);
    defer result.deinit();
    try expectLargeResponse(allocator, &result);
}

fn callLargeUnary(allocator: std.mem.Allocator, channel: *grpc.Channel, timeout_ns: u64) !grpc.CallResult {
    const body = try allocator.alloc(u8, large_request_size);
    defer allocator.free(body);
    @memset(body, 0);
    const request: testing.SimpleRequest = .{
        .response_type = .COMPRESSABLE,
        .response_size = large_response_size,
        .payload = .{
            .type = .COMPRESSABLE,
            .body = body,
        },
    };
    return callMessageWithOptions(
        allocator,
        channel,
        "/grpc.testing.TestService/UnaryCall",
        request,
        .{ .timeout_ns = timeout_ns },
    );
}

fn expectLargeUnaryFailure(allocator: std.mem.Allocator, channel: *grpc.Channel) !void {
    var result = callLargeUnary(allocator, channel, max_rpc_timeout_ns) catch return;
    defer result.deinit();
    if (result.status.isOk()) return error.ExpectedRpcFailure;
}

fn goaway(init: std.process.Init, channel: *grpc.Channel) !void {
    try largeUnary(init.gpa, channel, max_rpc_timeout_ns);
    try std.Io.sleep(init.io, .fromSeconds(1), .awake);
    try largeUnary(init.gpa, channel, max_rpc_timeout_ns);
}

fn maxStreams(allocator: std.mem.Allocator, channel: *grpc.Channel) !void {
    try largeUnary(allocator, channel, max_rpc_timeout_ns);

    const Worker = struct {
        channel: *grpc.Channel,
        succeeded: bool = false,

        fn run(self: *@This()) void {
            largeUnary(std.heap.page_allocator, self.channel, max_rpc_timeout_ns) catch return;
            self.succeeded = true;
        }
    };
    var workers: [10]Worker = undefined;
    var threads: [10]std.Thread = undefined;
    var spawned: usize = 0;
    for (&workers, &threads) |*worker, *thread| {
        worker.* = .{ .channel = channel };
        thread.* = std.Thread.spawn(.{}, Worker.run, .{worker}) catch |err| {
            for (threads[0..spawned]) |spawned_thread| spawned_thread.join();
            return err;
        };
        spawned += 1;
    }
    for (&threads) |thread| thread.join();
    for (&workers) |worker| {
        if (!worker.succeeded) return error.ConcurrentRpcFailed;
    }
}

const StreamCompletion = struct {
    done: std.atomic.Value(bool) = .init(false),
    callback_error: ?anyerror = null,
    terminal_status: grpc.StatusCode = .unknown,
    terminal_message_buffer: [256]u8 = undefined,
    terminal_message_len: usize = 0,
    terminal_count: usize = 0,

    fn fail(self: *@This(), stream: grpc.ClientStream, err: anyerror) void {
        if (self.callback_error == null) self.callback_error = err;
        stream.cancel();
    }

    fn terminal(self: *@This(), status: grpc.Status) void {
        self.terminal_count += 1;
        if (self.terminal_count == 1) {
            self.terminal_status = status.code;
            self.terminal_message_len = @min(status.message.len, self.terminal_message_buffer.len);
            @memcpy(
                self.terminal_message_buffer[0..self.terminal_message_len],
                status.message[0..self.terminal_message_len],
            );
        } else if (self.callback_error == null) {
            self.callback_error = error.MultipleTerminalCallbacks;
        }
        self.done.store(true, .release);
    }
};

const ClientStreamingState = struct {
    completion: StreamCompletion = .{},
    response_count: usize = 0,
    aggregated_payload_size: i32 = 0,
    scratch: [1024]u8 = undefined,

    fn onMessage(
        context: ?*anyopaque,
        stream: grpc.ClientStream,
        payload: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        var fixed = std.heap.FixedBufferAllocator.init(&self.scratch);
        var reader: std.Io.Reader = .fixed(payload);
        var response = testing.StreamingInputCallResponse.decode(&reader, fixed.allocator()) catch |err| {
            self.completion.fail(stream, err);
            return .continue_receiving;
        };
        defer response.deinit(fixed.allocator());
        if (self.response_count != 0) {
            self.completion.fail(stream, error.UnexpectedResponseCount);
            return .continue_receiving;
        }
        self.response_count = 1;
        self.aggregated_payload_size = response.aggregated_payload_size;
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.completion.terminal(status);
    }
};

const OutputStreamingState = struct {
    completion: StreamCompletion = .{},
    response_count: usize = 0,
    scratch: [64 * 1024]u8 = undefined,

    fn onMessage(
        context: ?*anyopaque,
        stream: grpc.ClientStream,
        payload: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        if (self.response_count >= streaming_response_sizes.len) {
            self.completion.fail(stream, error.UnexpectedResponseCount);
            return .continue_receiving;
        }
        const size = decodeStreamingOutputSize(payload, &self.scratch) catch |err| {
            self.completion.fail(stream, err);
            return .continue_receiving;
        };
        if (size != streaming_response_sizes[self.response_count]) {
            self.completion.fail(stream, error.UnexpectedPayloadSize);
            return .continue_receiving;
        }
        self.response_count += 1;
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.completion.terminal(status);
    }
};

const PingPongState = struct {
    completion: StreamCompletion = .{},
    response_count: usize = 0,
    scratch: [1024 * 1024]u8 = undefined,

    fn onMessage(
        context: ?*anyopaque,
        stream: grpc.ClientStream,
        payload: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        if (self.response_count >= streaming_response_sizes.len) {
            self.completion.fail(stream, error.UnexpectedResponseCount);
            return .continue_receiving;
        }
        var fixed = std.heap.FixedBufferAllocator.init(&self.scratch);
        const size = decodeStreamingOutputSizeWithAllocator(payload, fixed.allocator()) catch |err| {
            self.completion.fail(stream, err);
            return .continue_receiving;
        };
        if (size != streaming_response_sizes[self.response_count]) {
            self.completion.fail(stream, error.UnexpectedPayloadSize);
            return .continue_receiving;
        }

        self.response_count += 1;
        if (self.response_count == streaming_response_sizes.len) {
            stream.closeSend() catch |err| self.completion.fail(stream, err);
        } else {
            fixed = std.heap.FixedBufferAllocator.init(&self.scratch);
            sendStreamingOutputRequest(
                fixed.allocator(),
                stream,
                streaming_request_sizes[self.response_count],
                streaming_response_sizes[self.response_count],
            ) catch |err| self.completion.fail(stream, err);
        }
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.completion.terminal(status);
    }
};

const EmptyStreamState = struct {
    completion: StreamCompletion = .{},
    response_count: usize = 0,

    fn onMessage(
        context: ?*anyopaque,
        stream: grpc.ClientStream,
        _: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.response_count += 1;
        self.completion.fail(stream, error.UnexpectedResponseCount);
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.completion.terminal(status);
    }
};

const CancellationState = struct {
    completion: StreamCompletion = .{},
    response_count: usize = 0,
    cancel_on_message: bool = false,

    fn onMessage(
        context: ?*anyopaque,
        stream: grpc.ClientStream,
        _: []const u8,
        _: grpc.Compression,
    ) grpc.StreamReceiveAction {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.response_count += 1;
        if (self.cancel_on_message) stream.cancel();
        return .continue_receiving;
    }

    fn onTerminal(
        context: ?*anyopaque,
        _: grpc.ClientStream,
        status: grpc.Status,
        _: *const grpc.Metadata,
    ) void {
        const self: *@This() = @ptrCast(@alignCast(context.?));
        self.completion.terminal(status);
    }
};

fn clientStreaming(allocator: std.mem.Allocator, io: std.Io, channel: *grpc.Channel) !void {
    var state: ClientStreamingState = .{};
    var stream = try channel.openStream(
        "/grpc.testing.TestService/StreamingInputCall",
        .{ .timeout_ns = max_rpc_timeout_ns },
        .{
            .context = &state,
            .on_message = ClientStreamingState.onMessage,
            .on_terminal = ClientStreamingState.onTerminal,
        },
    );
    defer stream.deinit();

    for (streaming_request_sizes) |size| {
        try sendStreamingInputRequest(allocator, stream, size);
    }
    try stream.closeSend();
    try waitForStream(io, &state.completion, stream);
    if (state.response_count != 1) return error.UnexpectedResponseCount;
    if (state.aggregated_payload_size != 74922) return error.UnexpectedAggregatedPayloadSize;
}

fn serverStreaming(allocator: std.mem.Allocator, io: std.Io, channel: *grpc.Channel) !void {
    var state: OutputStreamingState = .{};
    var stream = try channel.openStream(
        "/grpc.testing.TestService/StreamingOutputCall",
        .{ .timeout_ns = max_rpc_timeout_ns },
        .{
            .context = &state,
            .on_message = OutputStreamingState.onMessage,
            .on_terminal = OutputStreamingState.onTerminal,
        },
    );
    defer stream.deinit();

    try sendServerStreamingRequest(allocator, stream);
    try stream.closeSend();
    try waitForStream(io, &state.completion, stream);
    if (state.response_count != streaming_response_sizes.len) return error.UnexpectedResponseCount;
}

fn pingPong(allocator: std.mem.Allocator, io: std.Io, channel: *grpc.Channel) !void {
    const state = try allocator.create(PingPongState);
    defer allocator.destroy(state);
    state.* = .{};
    var stream = try channel.openStream(
        "/grpc.testing.TestService/FullDuplexCall",
        .{ .timeout_ns = max_rpc_timeout_ns },
        .{
            .context = state,
            .on_message = PingPongState.onMessage,
            .on_terminal = PingPongState.onTerminal,
        },
    );
    defer stream.deinit();

    try sendStreamingOutputRequest(
        allocator,
        stream,
        streaming_request_sizes[0],
        streaming_response_sizes[0],
    );
    try waitForStream(io, &state.completion, stream);
    if (state.response_count != streaming_response_sizes.len) return error.UnexpectedResponseCount;
}

fn emptyStream(io: std.Io, channel: *grpc.Channel) !void {
    var state: EmptyStreamState = .{};
    var stream = try channel.openStream(
        "/grpc.testing.TestService/FullDuplexCall",
        .{ .timeout_ns = max_rpc_timeout_ns },
        .{
            .context = &state,
            .on_message = EmptyStreamState.onMessage,
            .on_terminal = EmptyStreamState.onTerminal,
        },
    );
    defer stream.deinit();

    try stream.closeSend();
    try waitForStream(io, &state.completion, stream);
    if (state.response_count != 0) return error.UnexpectedResponseCount;
}

fn cancelAfterBegin(io: std.Io, channel: *grpc.Channel) !void {
    var state: CancellationState = .{};
    var stream = try channel.openStream(
        "/grpc.testing.TestService/StreamingInputCall",
        .{ .timeout_ns = max_rpc_timeout_ns },
        .{
            .context = &state,
            .on_message = CancellationState.onMessage,
            .on_terminal = CancellationState.onTerminal,
        },
    );
    defer stream.deinit();

    stream.cancel();
    try waitForStreamStatus(io, &state.completion, stream, .cancelled);
    if (state.response_count != 0) return error.UnexpectedResponseCount;
}

fn cancelAfterFirstResponse(
    allocator: std.mem.Allocator,
    io: std.Io,
    channel: *grpc.Channel,
) !void {
    var state: CancellationState = .{ .cancel_on_message = true };
    var stream = try channel.openStream(
        "/grpc.testing.TestService/FullDuplexCall",
        .{ .timeout_ns = max_rpc_timeout_ns },
        .{
            .context = &state,
            .on_message = CancellationState.onMessage,
            .on_terminal = CancellationState.onTerminal,
        },
    );
    defer stream.deinit();

    try sendStreamingOutputRequest(allocator, stream, 27182, 31415);
    try waitForStreamStatus(io, &state.completion, stream, .cancelled);
    if (state.response_count != 1) return error.UnexpectedResponseCount;
}

fn timeoutOnSleepingServer(
    allocator: std.mem.Allocator,
    io: std.Io,
    channel: *grpc.Channel,
) !void {
    var state: CancellationState = .{};
    var stream = try channel.openStream(
        "/grpc.testing.TestService/FullDuplexCall",
        .{ .timeout_ns = std.time.ns_per_ms },
        .{
            .context = &state,
            .on_message = CancellationState.onMessage,
            .on_terminal = CancellationState.onTerminal,
        },
    );
    defer stream.deinit();

    const request: testing.StreamingOutputCallRequest = .{
        .response_type = .COMPRESSABLE,
        .payload = .{
            .type = .COMPRESSABLE,
            .body = zero_streaming_body[0..27182],
        },
    };
    sendStreamMessage(allocator, stream, request) catch |err| {
        if (err != error.StreamClosed) return err;
    };
    try waitForStreamStatus(io, &state.completion, stream, .deadline_exceeded);
    if (state.response_count != 0) return error.UnexpectedResponseCount;
}

fn waitForStream(io: std.Io, completion: *StreamCompletion, stream: grpc.ClientStream) !void {
    try waitForStreamStatus(io, completion, stream, .ok);
}

fn waitForStreamStatus(
    io: std.Io,
    completion: *StreamCompletion,
    stream: grpc.ClientStream,
    expected_status: grpc.StatusCode,
) !void {
    const deadline = std.Io.Clock.awake.now(io).nanoseconds +| max_rpc_timeout_ns;
    while (!completion.done.load(.acquire)) {
        if (std.Io.Clock.awake.now(io).nanoseconds >= deadline) {
            stream.cancel();
            return error.StreamingRpcTimeout;
        }
        try std.Io.sleep(io, .fromMilliseconds(1), .awake);
    }
    if (completion.terminal_count != 1) return error.UnexpectedTerminalCount;
    if (completion.callback_error) |err| return err;
    if (completion.terminal_status != expected_status) {
        std.debug.print("unexpected streaming RPC status: {t}: {s}\n", .{
            completion.terminal_status,
            completion.terminal_message_buffer[0..completion.terminal_message_len],
        });
        return error.UnexpectedRpcStatus;
    }
}

fn sendStreamingInputRequest(
    allocator: std.mem.Allocator,
    stream: grpc.ClientStream,
    request_size: usize,
) !void {
    const request: testing.StreamingInputCallRequest = .{
        .payload = .{
            .type = .COMPRESSABLE,
            .body = zero_streaming_body[0..request_size],
        },
    };
    try sendStreamMessage(allocator, stream, request);
}

fn sendServerStreamingRequest(allocator: std.mem.Allocator, stream: grpc.ClientStream) !void {
    var parameters: std.ArrayList(testing.ResponseParameters) = .empty;
    defer parameters.deinit(allocator);
    for (streaming_response_sizes) |size| {
        try parameters.append(allocator, .{ .size = @intCast(size) });
    }
    const request: testing.StreamingOutputCallRequest = .{
        .response_type = .COMPRESSABLE,
        .response_parameters = parameters,
    };
    try sendStreamMessage(allocator, stream, request);
}

fn sendStreamingOutputRequest(
    allocator: std.mem.Allocator,
    stream: grpc.ClientStream,
    request_size: usize,
    response_size: usize,
) !void {
    var parameters: std.ArrayList(testing.ResponseParameters) = .empty;
    defer parameters.deinit(allocator);
    try parameters.append(allocator, .{ .size = @intCast(response_size) });
    const request: testing.StreamingOutputCallRequest = .{
        .response_type = .COMPRESSABLE,
        .response_parameters = parameters,
        .payload = .{
            .type = .COMPRESSABLE,
            .body = zero_streaming_body[0..request_size],
        },
    };
    try sendStreamMessage(allocator, stream, request);
}

fn sendStreamMessage(allocator: std.mem.Allocator, stream: grpc.ClientStream, message: anytype) !void {
    var writer: std.Io.Writer.Allocating = .init(allocator);
    defer writer.deinit();
    try message.encode(&writer.writer, allocator);
    try stream.send(writer.written(), .{});
}

fn decodeStreamingOutputSize(payload: []const u8, scratch: []u8) !usize {
    var fixed = std.heap.FixedBufferAllocator.init(scratch);
    return decodeStreamingOutputSizeWithAllocator(payload, fixed.allocator());
}

fn decodeStreamingOutputSizeWithAllocator(
    payload: []const u8,
    allocator: std.mem.Allocator,
) !usize {
    var reader: std.Io.Reader = .fixed(payload);
    var response = try testing.StreamingOutputCallResponse.decode(&reader, allocator);
    defer response.deinit(allocator);
    const response_payload = response.payload orelse return error.MissingPayload;
    if (response_payload.type != .COMPRESSABLE) return error.UnexpectedPayloadType;
    return response_payload.body.len;
}

fn clientCompressedUnary(allocator: std.mem.Allocator, channel: *grpc.Channel) !void {
    const body = try allocator.alloc(u8, large_request_size);
    defer allocator.free(body);
    @memset(body, 0);
    const request: testing.SimpleRequest = .{
        .response_type = .COMPRESSABLE,
        .response_size = large_response_size,
        .payload = .{
            .type = .COMPRESSABLE,
            .body = body,
        },
        .expect_compressed = .{ .value = true },
    };
    var rejected = try callMessage(
        allocator,
        channel,
        "/grpc.testing.TestService/UnaryCall",
        request,
    );
    defer rejected.deinit();
    if (rejected.status.code != .invalid_argument) return error.UnexpectedRpcStatus;

    var result = try callMessageWithOptions(
        allocator,
        channel,
        "/grpc.testing.TestService/UnaryCall",
        request,
        .{
            .timeout_ns = 10 * std.time.ns_per_s,
            .request_compression = .gzip,
        },
    );
    defer result.deinit();
    try expectLargeResponse(allocator, &result);
    if (result.response_compression != .identity) return error.UnexpectedResponseCompression;
}

fn serverCompressedUnary(allocator: std.mem.Allocator, channel: *grpc.Channel) !void {
    const request: testing.SimpleRequest = .{
        .response_type = .COMPRESSABLE,
        .response_size = large_response_size,
        .response_compressed = .{ .value = true },
    };
    var result = try callMessage(
        allocator,
        channel,
        "/grpc.testing.TestService/UnaryCall",
        request,
    );
    defer result.deinit();
    try expectLargeResponse(allocator, &result);
    if (result.response_compression != .gzip) return error.UnexpectedResponseCompression;
}

fn expectLargeResponse(allocator: std.mem.Allocator, result: *const grpc.CallResult) !void {
    try expectStatus(result, .ok, "");
    var reader: std.Io.Reader = .fixed(result.payload);
    var response = try testing.SimpleResponse.decode(&reader, allocator);
    defer response.deinit(allocator);
    const payload = response.payload orelse return error.MissingPayload;
    if (payload.type != .COMPRESSABLE) return error.UnexpectedPayloadType;
    if (payload.body.len != large_response_size) return error.UnexpectedPayloadSize;
    for (payload.body) |byte| {
        if (byte != 0) return error.UnexpectedPayloadContents;
    }
}

fn specialStatusMessage(allocator: std.mem.Allocator, channel: *grpc.Channel) !void {
    const request: testing.SimpleRequest = .{
        .response_status = .{
            .code = @intFromEnum(grpc.StatusCode.unknown),
            .message = special_status_message,
        },
    };
    var result = try callMessage(
        allocator,
        channel,
        "/grpc.testing.TestService/UnaryCall",
        request,
    );
    defer result.deinit();
    try expectStatus(&result, .unknown, special_status_message);
}

fn expectUnimplemented(
    allocator: std.mem.Allocator,
    channel: *grpc.Channel,
    path: []const u8,
) !void {
    var result = try callMessage(allocator, channel, path, testing.Empty{});
    defer result.deinit();
    if (result.status.code != .unimplemented) {
        std.debug.print("expected UNIMPLEMENTED, got {t}: {s}\n", .{
            result.status.code,
            result.status.message,
        });
        return error.UnexpectedRpcStatus;
    }
}

fn callMessage(
    allocator: std.mem.Allocator,
    channel: *grpc.Channel,
    path: []const u8,
    request: anytype,
) !grpc.CallResult {
    return callMessageWithOptions(allocator, channel, path, request, .{
        .timeout_ns = 10 * std.time.ns_per_s,
    });
}

fn callMessageWithOptions(
    allocator: std.mem.Allocator,
    channel: *grpc.Channel,
    path: []const u8,
    request: anytype,
    options: grpc.CallOptions,
) !grpc.CallResult {
    var writer: std.Io.Writer.Allocating = .init(allocator);
    defer writer.deinit();
    try request.encode(&writer.writer, allocator);
    return channel.callUnary(allocator, path, writer.written(), options);
}

fn expectStatus(result: *const grpc.CallResult, code: grpc.StatusCode, message: []const u8) !void {
    if (result.status.code != code or !std.mem.eql(u8, result.status.message, message)) {
        std.debug.print("unexpected RPC status: {t}: {s}\n", .{
            result.status.code,
            result.status.message,
        });
        return error.UnexpectedRpcStatus;
    }
}

fn parseArgs(args: []const []const u8) !Config {
    var config: Config = .{};
    var index: usize = 1;
    while (index < args.len) : (index += 1) {
        const arg = args[index];
        if (std.mem.startsWith(u8, arg, "--server_host=")) {
            config.server_host = arg["--server_host=".len..];
        } else if (std.mem.eql(u8, arg, "--server_host")) {
            index += 1;
            if (index >= args.len) return error.MissingServerHost;
            config.server_host = args[index];
        } else if (std.mem.startsWith(u8, arg, "--server_port=")) {
            config.server_port = try std.fmt.parseInt(u16, arg["--server_port=".len..], 10);
        } else if (std.mem.eql(u8, arg, "--server_port")) {
            index += 1;
            if (index >= args.len) return error.MissingServerPort;
            config.server_port = try std.fmt.parseInt(u16, args[index], 10);
        } else if (std.mem.startsWith(u8, arg, "--test_case=")) {
            config.test_case = arg["--test_case=".len..];
        } else if (std.mem.eql(u8, arg, "--test_case")) {
            index += 1;
            if (index >= args.len) return error.MissingTestCase;
            config.test_case = args[index];
        } else if (std.mem.startsWith(u8, arg, "--use_tls=")) {
            config.use_tls = try parseBool(arg["--use_tls=".len..]);
        } else if (std.mem.eql(u8, arg, "--use_tls")) {
            config.use_tls = true;
        } else if (std.mem.startsWith(u8, arg, "--ca_file=")) {
            config.ca_file = arg["--ca_file=".len..];
        } else if (std.mem.eql(u8, arg, "--ca_file")) {
            index += 1;
            if (index >= args.len) return error.MissingCaFile;
            config.ca_file = args[index];
        } else if (std.mem.startsWith(u8, arg, "--soak_iterations=")) {
            config.soak_iterations = std.fmt.parseInt(
                usize,
                arg["--soak_iterations=".len..],
                10,
            ) catch return error.InvalidSoakIterations;
        } else if (std.mem.eql(u8, arg, "--soak_iterations")) {
            index += 1;
            if (index >= args.len) return error.MissingSoakIterations;
            config.soak_iterations = std.fmt.parseInt(usize, args[index], 10) catch
                return error.InvalidSoakIterations;
        } else if (std.mem.startsWith(u8, arg, "--soak_max_failures=")) {
            config.soak_max_failures = std.fmt.parseInt(
                usize,
                arg["--soak_max_failures=".len..],
                10,
            ) catch return error.InvalidSoakMaxFailures;
        } else if (std.mem.eql(u8, arg, "--soak_max_failures")) {
            index += 1;
            if (index >= args.len) return error.MissingSoakMaxFailures;
            config.soak_max_failures = std.fmt.parseInt(usize, args[index], 10) catch
                return error.InvalidSoakMaxFailures;
        } else if (std.mem.startsWith(u8, arg, "--soak_overall_timeout_seconds=")) {
            config.soak_overall_timeout_seconds = std.fmt.parseInt(
                u64,
                arg["--soak_overall_timeout_seconds=".len..],
                10,
            ) catch return error.InvalidSoakOverallTimeout;
        } else if (std.mem.eql(u8, arg, "--soak_overall_timeout_seconds")) {
            index += 1;
            if (index >= args.len) return error.MissingSoakOverallTimeout;
            config.soak_overall_timeout_seconds = std.fmt.parseInt(u64, args[index], 10) catch
                return error.InvalidSoakOverallTimeout;
        } else {
            return error.UnknownArgument;
        }
    }
    if (config.use_tls and config.ca_file.len == 0) return error.MissingCaFile;
    if (config.soak_iterations == 0) return error.ZeroSoakIterations;
    return config;
}

fn parseBool(value: []const u8) !bool {
    if (std.mem.eql(u8, value, "true")) return true;
    if (std.mem.eql(u8, value, "false")) return false;
    return error.InvalidBoolean;
}

test "parse soak arguments in equals form" {
    const config = try parseArgs(&.{
        "interop-client",
        "--soak_iterations=23",
        "--soak_max_failures=4",
        "--soak_overall_timeout_seconds=90",
    });
    try std.testing.expectEqual(@as(usize, 23), config.soak_iterations);
    try std.testing.expectEqual(@as(usize, 4), config.soak_max_failures);
    try std.testing.expectEqual(@as(u64, 90), config.soak_overall_timeout_seconds);
}

test "parse soak arguments in split form" {
    const config = try parseArgs(&.{
        "interop-client",
        "--soak_iterations",
        "23",
        "--soak_max_failures",
        "4",
        "--soak_overall_timeout_seconds",
        "90",
    });
    try std.testing.expectEqual(@as(usize, 23), config.soak_iterations);
    try std.testing.expectEqual(@as(usize, 4), config.soak_max_failures);
    try std.testing.expectEqual(@as(u64, 90), config.soak_overall_timeout_seconds);
}

test "parse TLS CA arguments" {
    const config = try parseArgs(&.{
        "interop-client",
        "--use_tls=true",
        "--ca_file=/tmp/test-ca.pem",
    });
    try std.testing.expect(config.use_tls);
    try std.testing.expectEqualStrings("/tmp/test-ca.pem", config.ca_file);
}

test "reject zero soak iterations" {
    try std.testing.expectError(
        error.ZeroSoakIterations,
        parseArgs(&.{ "interop-client", "--soak_iterations=0" }),
    );
}

test "reject invalid soak numbers" {
    try std.testing.expectError(
        error.InvalidSoakIterations,
        parseArgs(&.{ "interop-client", "--soak_iterations=invalid" }),
    );
    try std.testing.expectError(
        error.InvalidSoakMaxFailures,
        parseArgs(&.{ "interop-client", "--soak_max_failures=invalid" }),
    );
    try std.testing.expectError(
        error.InvalidSoakOverallTimeout,
        parseArgs(&.{ "interop-client", "--soak_overall_timeout_seconds=invalid" }),
    );
}

test "reject missing soak values" {
    try std.testing.expectError(
        error.MissingSoakIterations,
        parseArgs(&.{ "interop-client", "--soak_iterations" }),
    );
    try std.testing.expectError(
        error.MissingSoakMaxFailures,
        parseArgs(&.{ "interop-client", "--soak_max_failures" }),
    );
    try std.testing.expectError(
        error.MissingSoakOverallTimeout,
        parseArgs(&.{ "interop-client", "--soak_overall_timeout_seconds" }),
    );
}

test "reject unknown interop client argument" {
    try std.testing.expectError(
        error.UnknownArgument,
        parseArgs(&.{ "interop-client", "--unknown=value" }),
    );
}

test "soak controls fail on exceeded budget and elapsed deadline" {
    var failures: usize = 0;
    try recordSoakFailure(&failures, 1, 0, error.RpcFailed);
    try std.testing.expectError(
        error.SoakFailureBudgetExceeded,
        recordSoakFailure(&failures, 1, 1, error.RpcFailed),
    );
    try std.testing.expectEqual(@as(usize, 2), failures);
    try std.testing.expectError(
        error.SoakOverallTimeout,
        checkSoakDeadline(std.testing.io, std.math.minInt(i96)),
    );
}

test "soak RPC timeout uses the remaining overall deadline" {
    const now: i96 = 100 * std.time.ns_per_s;
    try std.testing.expectEqual(
        @as(u64, 5 * std.time.ns_per_s),
        try remainingSoakRpcTimeout(now, now + 5 * std.time.ns_per_s),
    );
    try std.testing.expectEqual(
        @as(u64, max_rpc_timeout_ns),
        try remainingSoakRpcTimeout(now, now + 61 * std.time.ns_per_s),
    );
    try std.testing.expectError(error.SoakOverallTimeout, remainingSoakRpcTimeout(now, now));
}

test "channel soak counts init failures and preserves OOM" {
    const deadline = soakDeadline(std.testing.io, 1);
    const init_error = (try channelSoakIteration(
        std.testing.allocator,
        std.testing.io,
        "invalid-target",
        deadline,
        .{},
    )).?;
    try std.testing.expectEqual(error.InvalidTarget, init_error);

    var failures: usize = 0;
    try handleSoakFailure(&failures, 1, 0, init_error);
    try std.testing.expectEqual(@as(usize, 1), failures);
    try std.testing.expectError(
        error.OutOfMemory,
        handleSoakFailure(&failures, 1, 1, error.OutOfMemory),
    );
    try std.testing.expectEqual(@as(usize, 1), failures);
}
