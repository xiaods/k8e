//! Lightweight gRPC core runtime for Zig.

const std = @import("std");
const build_options = @import("grpc_lite_options");

const c = @import("c.zig");
const c_api = @import("c_api.zig");
const cares_adapter = @import("cares_adapter.zig");
const deadline = @import("deadline.zig");
const fast_clock = @import("fast_clock.zig");
const version_info = @import("version.zig");
const runtime = @import("runtime.zig");

comptime {
    @export(&c_api.grpc_lite_abi_version, .{ .name = "grpc_lite_abi_version" });
    @export(&c_api.grpc_lite_library_version, .{ .name = "grpc_lite_library_version" });
    @export(&c_api.grpc_lite_features, .{ .name = "grpc_lite_features" });
    @export(&c_api.grpc_lite_error_string, .{ .name = "grpc_lite_error_string" });
    @export(&c_api.grpc_lite_runtime_create, .{ .name = "grpc_lite_runtime_create" });
    @export(&c_api.grpc_lite_runtime_destroy, .{ .name = "grpc_lite_runtime_destroy" });
    @export(&c_api.grpc_lite_metadata_create, .{ .name = "grpc_lite_metadata_create" });
    @export(&c_api.grpc_lite_metadata_destroy, .{ .name = "grpc_lite_metadata_destroy" });
    @export(&c_api.grpc_lite_metadata_add, .{ .name = "grpc_lite_metadata_add" });
    @export(&c_api.grpc_lite_metadata_count, .{ .name = "grpc_lite_metadata_count" });
    @export(&c_api.grpc_lite_metadata_at, .{ .name = "grpc_lite_metadata_at" });
    @export(&c_api.grpc_lite_metadata_view_count, .{ .name = "grpc_lite_metadata_view_count" });
    @export(&c_api.grpc_lite_metadata_view_at, .{ .name = "grpc_lite_metadata_view_at" });
    @export(&c_api.grpc_lite_channel_create, .{ .name = "grpc_lite_channel_create" });
    @export(&c_api.grpc_lite_channel_create_managed, .{ .name = "grpc_lite_channel_create_managed" });
    @export(&c_api.grpc_lite_channel_shutdown, .{ .name = "grpc_lite_channel_shutdown" });
    @export(&c_api.grpc_lite_channel_wait, .{ .name = "grpc_lite_channel_wait" });
    @export(&c_api.grpc_lite_channel_destroy, .{ .name = "grpc_lite_channel_destroy" });
    @export(&c_api.grpc_lite_channel_call_unary, .{ .name = "grpc_lite_channel_call_unary" });
    @export(&c_api.grpc_lite_channel_open_stream, .{ .name = "grpc_lite_channel_open_stream" });
    @export(&c_api.grpc_lite_client_stream_send, .{ .name = "grpc_lite_client_stream_send" });
    @export(&c_api.grpc_lite_client_stream_close_send, .{ .name = "grpc_lite_client_stream_close_send" });
    @export(&c_api.grpc_lite_client_stream_cancel, .{ .name = "grpc_lite_client_stream_cancel" });
    @export(&c_api.grpc_lite_client_stream_resume_receive, .{ .name = "grpc_lite_client_stream_resume_receive" });
    @export(&c_api.grpc_lite_client_stream_destroy, .{ .name = "grpc_lite_client_stream_destroy" });
    @export(&c_api.grpc_lite_server_create, .{ .name = "grpc_lite_server_create" });
    @export(&c_api.grpc_lite_server_register_stream, .{ .name = "grpc_lite_server_register_stream" });
    @export(&c_api.grpc_lite_server_start, .{ .name = "grpc_lite_server_start" });
    @export(&c_api.grpc_lite_server_port, .{ .name = "grpc_lite_server_port" });
    @export(&c_api.grpc_lite_server_shutdown, .{ .name = "grpc_lite_server_shutdown" });
    @export(&c_api.grpc_lite_server_shutdown_gracefully, .{ .name = "grpc_lite_server_shutdown_gracefully" });
    @export(&c_api.grpc_lite_server_wait, .{ .name = "grpc_lite_server_wait" });
    @export(&c_api.grpc_lite_server_destroy, .{ .name = "grpc_lite_server_destroy" });
    @export(&c_api.grpc_lite_server_stream_id, .{ .name = "grpc_lite_server_stream_id" });
    @export(&c_api.grpc_lite_server_stream_retain, .{ .name = "grpc_lite_server_stream_retain" });
    @export(&c_api.grpc_lite_server_call_clone, .{ .name = "grpc_lite_server_call_clone" });
    @export(&c_api.grpc_lite_server_call_destroy, .{ .name = "grpc_lite_server_call_destroy" });
    @export(&c_api.grpc_lite_server_call_id, .{ .name = "grpc_lite_server_call_id" });
    @export(&c_api.grpc_lite_server_call_is_cancelled, .{ .name = "grpc_lite_server_call_is_cancelled" });
    @export(&c_api.grpc_lite_server_call_is_terminal, .{ .name = "grpc_lite_server_call_is_terminal" });
    @export(&c_api.grpc_lite_server_call_abort, .{ .name = "grpc_lite_server_call_abort" });
    @export(&c_api.grpc_lite_server_call_send_initial_metadata, .{ .name = "grpc_lite_server_call_send_initial_metadata" });
    @export(&c_api.grpc_lite_server_call_send, .{ .name = "grpc_lite_server_call_send" });
    @export(&c_api.grpc_lite_server_call_finish, .{ .name = "grpc_lite_server_call_finish" });
    @export(&c_api.grpc_lite_server_call_resume_receive, .{ .name = "grpc_lite_server_call_resume_receive" });
    @export(&c_api.grpc_lite_server_context_request_metadata, .{ .name = "grpc_lite_server_context_request_metadata" });
    @export(&c_api.grpc_lite_server_context_request_compression, .{ .name = "grpc_lite_server_context_request_compression" });
    @export(&c_api.grpc_lite_server_context_has_deadline, .{ .name = "grpc_lite_server_context_has_deadline" });
    @export(&c_api.grpc_lite_server_context_remaining_time_ns, .{ .name = "grpc_lite_server_context_remaining_time_ns" });
    @export(&c_api.grpc_lite_unary_result_destroy, .{ .name = "grpc_lite_unary_result_destroy" });
    @export(&c_api.grpc_lite_unary_result_status_code, .{ .name = "grpc_lite_unary_result_status_code" });
    @export(&c_api.grpc_lite_unary_result_status_message, .{ .name = "grpc_lite_unary_result_status_message" });
    @export(&c_api.grpc_lite_unary_result_payload, .{ .name = "grpc_lite_unary_result_payload" });
    @export(&c_api.grpc_lite_unary_result_response_compression, .{ .name = "grpc_lite_unary_result_response_compression" });
    @export(&c_api.grpc_lite_unary_result_metadata_count, .{ .name = "grpc_lite_unary_result_metadata_count" });
    @export(&c_api.grpc_lite_unary_result_metadata_at, .{ .name = "grpc_lite_unary_result_metadata_at" });
}
pub const message = @import("message.zig");

pub const call = @import("call.zig");
pub const channel = @import("channel.zig");
pub const compression = @import("compression.zig");
pub const frame = @import("frame.zig");
pub const log = @import("log.zig");
pub const Logger = @import("logger.zig").Logger;
pub const LogLevel = @import("logger.zig").Level;
pub const metadata = @import("metadata.zig");
pub const server = @import("server.zig");
pub const service = @import("service.zig");
pub const status = @import("status.zig");
pub const stream = @import("stream.zig");
pub const xev = @import("xev");

pub const CallOptions = call.Options;
pub const CallResult = call.Result;
pub const AsyncCallResult = call.AsyncResult;
pub const UnaryCallCallbacks = call.Callbacks;
pub const Compression = compression.Compression;
pub const Channel = channel.Channel;
pub const ChannelOptions = channel.Options;
pub const ReconnectOptions = channel.ReconnectOptions;
pub const ClientTlsOptions = channel.TlsOptions;
pub const Metadata = metadata.Metadata;
pub const MetadataEntry = metadata.Entry;
pub const Server = server.Server;
pub const ServerOptions = server.Options;
pub const ServerTlsOptions = server.TlsOptions;
pub const ServerLocalAddress = server.LocalAddress;
pub const ServerContext = service.ServerContext;
pub const Status = status.Status;
pub const StatusCode = status.Code;
pub const StreamBufferLimits = stream.BufferLimits;
pub const StreamOptions = stream.Options;
pub const StreamSendOptions = stream.SendOptions;
pub const StreamReceiveAction = stream.ReceiveAction;
pub const ClientStream = stream.ClientStream;
pub const ClientStreamCallbacks = stream.ClientCallbacks;
pub const ServerStream = stream.ServerStream;
pub const ServerCall = stream.ServerCall;
pub const ServerCallId = stream.ServerCallId;
pub const ServerTerminalReason = stream.ServerTerminalReason;
pub const ServerInitialMetadataMode = stream.InitialMetadataMode;
pub const ServerStreamHandler = stream.ServerHandler;
pub const UnaryHandler = service.UnaryHandler;
pub const UnaryResponse = service.UnaryResponse;
pub const Runtime = runtime.Runtime;

pub const version = version_info.string;

pub const internal = struct {
    pub const fastNowNs = fast_clock.now;
    pub const fastClockImplementation = fast_clock.implementation;
    pub const fastClockFallbackReason = fast_clock.fallbackReason;
    pub const fastClockUsesCpuCycles = fast_clock.usesCpuCycles;
};

test "version is available" {
    _ = try std.SemanticVersion.parse(version);
}

test {
    _ = c;
    _ = c_api;
    _ = cares_adapter;
    _ = call;
    _ = channel;
    _ = compression;
    _ = deadline;
    _ = frame;
    _ = fast_clock;
    _ = log;
    _ = metadata;
    _ = runtime;
    _ = message;
    _ = server;
    _ = service;
    _ = status;
    _ = stream;
    _ = xev;
    _ = version_info;
    if (build_options.tls) _ = @import("tls_record.zig");
}
