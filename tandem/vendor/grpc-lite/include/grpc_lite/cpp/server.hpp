#ifndef GRPC_LITE_CPP_SERVER_HPP
#define GRPC_LITE_CPP_SERVER_HPP

#include <grpc_lite/cpp/channel.hpp>
#include <grpc_lite/cpp/metadata.hpp>

#include <cstddef>
#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <utility>
#include <vector>

namespace grpc_lite {

enum class ServerTerminalReason : std::uint32_t {
  Completed = GRPC_LITE_SERVER_TERMINAL_COMPLETED,
  PeerCancelled = GRPC_LITE_SERVER_TERMINAL_PEER_CANCELLED,
  DeadlineExceeded = GRPC_LITE_SERVER_TERMINAL_DEADLINE_EXCEEDED,
  ServerShutdown = GRPC_LITE_SERVER_TERMINAL_SERVER_SHUTDOWN,
  TransportError = GRPC_LITE_SERVER_TERMINAL_TRANSPORT_ERROR,
  LocalError = GRPC_LITE_SERVER_TERMINAL_LOCAL_ERROR,
};

struct ServerOptions {
  std::string host = "127.0.0.1";
  std::uint32_t port = 0;
  std::uint32_t reactor_count = 1;
  std::uint64_t max_message_size = UINT64_C(4194304);
  std::uint64_t max_inbound_buffer_size = UINT64_C(8388608);
  std::uint64_t max_outbound_buffer_size = UINT64_C(8388608);
  Logger logger;
  std::uint64_t max_connections = UINT64_C(1024);
  std::uint64_t cleartext_preface_timeout_ns = UINT64_C(10000000000);
  std::uint64_t connection_idle_timeout_ns = UINT64_C(300000000000);
  std::uint32_t max_concurrent_streams_per_connection = 100;
};

struct ServerMethodOptions {
  bool receive_initially_paused = false;
  bool explicit_initial_metadata = true;
};

class ServerCall {
 public:
  ServerCall() = default;
  ~ServerCall() { grpc_lite_server_call_destroy(call_); }
  ServerCall(const ServerCall&) = delete;
  ServerCall& operator=(const ServerCall&) = delete;

  ServerCall(ServerCall&& other) noexcept
      : call_(std::exchange(other.call_, nullptr)) {}
  ServerCall& operator=(ServerCall&& other) noexcept {
    if (this != &other) {
      grpc_lite_server_call_destroy(call_);
      call_ = std::exchange(other.call_, nullptr);
    }
    return *this;
  }

  Error Clone(ServerCall* out) const {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    if (call_ == nullptr) return Error(ErrorCode::InvalidState);
    grpc_lite_server_call* call = nullptr;
    const Error error(grpc_lite_server_call_clone(call_, &call));
    if (!error.ok()) return error;
    *out = ServerCall(call);
    return {};
  }

  std::size_t id() const {
    return call_ == nullptr ? 0 : grpc_lite_server_call_id(call_);
  }
  bool cancelled() const {
    return call_ != nullptr && grpc_lite_server_call_is_cancelled(call_) != 0;
  }
  bool terminal() const {
    return call_ != nullptr && grpc_lite_server_call_is_terminal(call_) != 0;
  }
  void Abort() { grpc_lite_server_call_abort(call_); }

  Error SendInitialMetadata(
      const Metadata* metadata = nullptr,
      Compression compression = Compression::Identity) {
    if (call_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_server_call_send_initial_metadata(
        call_, metadata == nullptr ? nullptr : metadata->metadata_,
        static_cast<std::uint32_t>(compression)));
  }
  Error Send(const std::string& payload,
             Compression compression = Compression::Identity) {
    if (call_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_server_call_send(
        call_, internal::Bytes(payload),
        static_cast<std::uint32_t>(compression)));
  }
  Error Finish(Status status, const Metadata* trailing_metadata = nullptr) {
    if (call_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_server_call_finish(
        call_, static_cast<std::uint32_t>(status.code()),
        internal::Bytes(status.message()),
        trailing_metadata == nullptr ? nullptr : trailing_metadata->metadata_));
  }
  Error ResumeReceive() {
    if (call_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_server_call_resume_receive(call_));
  }

 private:
  friend class ServerStream;
  explicit ServerCall(grpc_lite_server_call* call) : call_(call) {}
  grpc_lite_server_call* call_ = nullptr;
};

class ServerStream {
 public:
  Error Retain(ServerCall* out) const {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    grpc_lite_server_call* call = nullptr;
    const Error error(grpc_lite_server_stream_retain(stream_, &call));
    if (!error.ok()) return error;
    *out = ServerCall(call);
    return {};
  }

 private:
  friend class Server;
  explicit ServerStream(grpc_lite_server_stream* stream) : stream_(stream) {}
  grpc_lite_server_stream* stream_;
};

class ServerContext {
 public:
  const MetadataEntries& request_metadata() const { return request_metadata_; }
  Compression request_compression() const { return request_compression_; }
  bool has_deadline() const { return has_deadline_; }
  std::uint64_t remaining_time_ns() const { return remaining_time_ns_; }

 private:
  friend class Server;
  explicit ServerContext(const grpc_lite_server_context* context)
      : request_metadata_(internal::CopyMetadataView(
            grpc_lite_server_context_request_metadata(context))),
        request_compression_(static_cast<Compression>(
            grpc_lite_server_context_request_compression(context))),
        has_deadline_(grpc_lite_server_context_has_deadline(context) != 0),
        remaining_time_ns_(
            grpc_lite_server_context_remaining_time_ns(context)) {}

  MetadataEntries request_metadata_;
  Compression request_compression_ = Compression::Identity;
  bool has_deadline_ = false;
  std::uint64_t remaining_time_ns_ = 0;
};

struct ServerStreamCallbacks {
  std::function<void(ServerStream&, const ServerContext&)> on_start;
  std::function<ReceiveAction(ServerStream&, const ServerContext&, std::string,
                              Compression)>
      on_message;
  std::function<void(ServerStream&, const ServerContext&)> on_remote_end;
  std::function<void(ServerStream&, const ServerContext&)> on_writable;
  std::function<void(ServerStream&, const ServerContext&)> on_cancel;
  std::function<void(std::size_t, ServerTerminalReason)> on_terminal;
};

// Callbacks run concurrently on reactor threads and must not block or throw.
class Server {
 public:
  Server() = default;
  ~Server() { Reset(); }
  Server(const Server&) = delete;
  Server& operator=(const Server&) = delete;

  Server(Server&& other) noexcept
      : server_(std::exchange(other.server_, nullptr)),
        logger_(std::move(other.logger_)),
        methods_(std::move(other.methods_)) {}
  Server& operator=(Server&& other) noexcept {
    if (this != &other) {
      Reset();
      server_ = std::exchange(other.server_, nullptr);
      logger_ = std::move(other.logger_);
      methods_ = std::move(other.methods_);
    }
    return *this;
  }

  static Error Create(ServerOptions options, Server* out) {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    auto logger = options.logger
                      ? std::make_unique<LoggerState>(std::move(options.logger))
                      : nullptr;
    grpc_lite_logger c_logger = GRPC_LITE_LOGGER_INIT;
    if (logger != nullptr) {
      c_logger.user_data = logger.get();
      c_logger.log = &Log;
    }
    grpc_lite_server_options c_options = GRPC_LITE_SERVER_OPTIONS_INIT;
    c_options.host = internal::Bytes(options.host);
    c_options.port = options.port;
    c_options.reactor_count = options.reactor_count;
    c_options.max_message_size = options.max_message_size;
    c_options.max_inbound_buffer_size = options.max_inbound_buffer_size;
    c_options.max_outbound_buffer_size = options.max_outbound_buffer_size;
    c_options.logger = logger == nullptr ? nullptr : &c_logger;
    c_options.max_connections = options.max_connections;
    c_options.cleartext_preface_timeout_ns =
        options.cleartext_preface_timeout_ns;
    c_options.connection_idle_timeout_ns = options.connection_idle_timeout_ns;
    c_options.max_concurrent_streams_per_connection =
        options.max_concurrent_streams_per_connection;

    grpc_lite_server* server = nullptr;
    const Error error(grpc_lite_server_create(&c_options, &server));
    if (!error.ok()) return error;
    *out = Server(server, std::move(logger));
    return {};
  }

  Error RegisterStream(std::string method, ServerMethodOptions options,
                       ServerStreamCallbacks callbacks) {
    if (server_ == nullptr) return Error(ErrorCode::InvalidState);
    methods_.push_back(
        std::make_unique<MethodState>(std::move(callbacks)));
    grpc_lite_server_method_options c_options =
        GRPC_LITE_SERVER_METHOD_OPTIONS_INIT;
    c_options.receive_initially_paused = options.receive_initially_paused;
    c_options.explicit_initial_metadata = options.explicit_initial_metadata;
    grpc_lite_server_method_callbacks c_callbacks =
        GRPC_LITE_SERVER_METHOD_CALLBACKS_INIT;
    c_callbacks.user_data = methods_.back().get();
    c_callbacks.on_start = &OnStart;
    c_callbacks.on_message = &OnMessage;
    c_callbacks.on_remote_end = &OnRemoteEnd;
    c_callbacks.on_writable = &OnWritable;
    c_callbacks.on_cancel = &OnCancel;
    c_callbacks.on_terminal = &OnTerminal;
    const Error error(grpc_lite_server_register_stream(
        server_, internal::Bytes(method), &c_options, &c_callbacks));
    if (!error.ok()) methods_.pop_back();
    return error;
  }

  Error Start() {
    if (server_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_server_start(server_));
  }
  Error Port(std::uint32_t* out) const {
    if (server_ == nullptr) return Error(ErrorCode::InvalidState);
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    return Error(grpc_lite_server_port(server_, out));
  }
  void Shutdown() { grpc_lite_server_shutdown(server_); }
  void ShutdownGracefully(std::uint64_t timeout_ns) {
    grpc_lite_server_shutdown_gracefully(server_, timeout_ns);
  }
  void Wait() { grpc_lite_server_wait(server_); }

 private:
  struct LoggerState {
    explicit LoggerState(Logger value) : callback(std::move(value)) {}
    Logger callback;
  };
  struct MethodState {
    explicit MethodState(ServerStreamCallbacks value)
        : callbacks(std::move(value)) {}
    ServerStreamCallbacks callbacks;
  };

  Server(grpc_lite_server* server, std::unique_ptr<LoggerState> logger)
      : server_(server), logger_(std::move(logger)) {}

  static void Log(void* user_data, std::uint32_t level,
                  grpc_lite_bytes_view message) noexcept {
    const auto* data = message.data == nullptr
                           ? ""
                           : reinterpret_cast<const char*>(message.data);
    static_cast<LoggerState*>(user_data)->callback(
        static_cast<LogLevel>(level), {data, message.size});
  }
  static void OnStart(void* user_data, grpc_lite_server_stream* stream,
                       const grpc_lite_server_context* context) noexcept {
    auto& callback = static_cast<MethodState*>(user_data)->callbacks.on_start;
    ServerStream borrowed(stream);
    ServerContext copy(context);
    if (callback) callback(borrowed, copy);
  }
  static std::uint32_t OnMessage(void* user_data,
                                 grpc_lite_server_stream* stream,
                                  const grpc_lite_server_context* context,
                                  grpc_lite_bytes_view payload,
                                  std::uint32_t compression) noexcept {
    auto& callback = static_cast<MethodState*>(user_data)->callbacks.on_message;
    ServerStream borrowed(stream);
    ServerContext copy(context);
    std::string payload_copy = internal::CopyBytes(payload);
    if (!callback) return GRPC_LITE_RECEIVE_CONTINUE;
    return static_cast<std::uint32_t>(callback(
        borrowed, copy, std::move(payload_copy),
        static_cast<Compression>(compression)));
  }
  static void OnRemoteEnd(void* user_data, grpc_lite_server_stream* stream,
                           const grpc_lite_server_context* context) noexcept {
    auto& callback =
        static_cast<MethodState*>(user_data)->callbacks.on_remote_end;
    ServerStream borrowed(stream);
    ServerContext copy(context);
    if (callback) callback(borrowed, copy);
  }
  static void OnWritable(void* user_data, grpc_lite_server_stream* stream,
                          const grpc_lite_server_context* context) noexcept {
    auto& callback = static_cast<MethodState*>(user_data)->callbacks.on_writable;
    ServerStream borrowed(stream);
    ServerContext copy(context);
    if (callback) callback(borrowed, copy);
  }
  static void OnCancel(void* user_data, grpc_lite_server_stream* stream,
                        const grpc_lite_server_context* context) noexcept {
    auto& callback = static_cast<MethodState*>(user_data)->callbacks.on_cancel;
    ServerStream borrowed(stream);
    ServerContext copy(context);
    if (callback) callback(borrowed, copy);
  }
  static void OnTerminal(void* user_data, std::size_t call_id,
                          std::uint32_t reason) noexcept {
    auto& callback = static_cast<MethodState*>(user_data)->callbacks.on_terminal;
    if (callback) {
      callback(call_id, static_cast<ServerTerminalReason>(reason));
    }
  }

  void Reset() {
    grpc_lite_server_destroy(server_);
    server_ = nullptr;
    methods_.clear();
    logger_.reset();
  }

  grpc_lite_server* server_ = nullptr;
  std::unique_ptr<LoggerState> logger_;
  std::vector<std::unique_ptr<MethodState>> methods_;
};

}  // namespace grpc_lite

#endif
