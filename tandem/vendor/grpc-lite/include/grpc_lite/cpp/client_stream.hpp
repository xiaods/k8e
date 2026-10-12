#ifndef GRPC_LITE_CPP_CLIENT_STREAM_HPP
#define GRPC_LITE_CPP_CLIENT_STREAM_HPP

#include <grpc_lite/cpp/channel.hpp>
#include <grpc_lite/cpp/metadata.hpp>

#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <utility>

namespace grpc_lite {

struct ClientStreamOptions {
  const Metadata* metadata = nullptr;
  bool has_timeout = false;
  Compression send_compression = Compression::Identity;
  std::uint64_t timeout_ns = 0;
  std::uint64_t max_message_size = UINT64_C(4194304);
  std::uint64_t max_inbound_buffer_size = UINT64_C(8388608);
  std::uint64_t max_outbound_buffer_size = UINT64_C(8388608);
};

struct ClientStreamCallbacks {
  std::function<void(MetadataEntries)> on_headers;
  std::function<ReceiveAction(std::string, Compression)> on_message;
  std::function<void()> on_remote_end;
  std::function<void()> on_writable;
  std::function<void(Status, MetadataEntries)> on_terminal;
};

// Callbacks run on transport threads and must not block or throw. Destroy outside
// callbacks.
class ClientStream {
 public:
  ClientStream() = default;
  ~ClientStream() { Reset(); }
  ClientStream(const ClientStream&) = delete;
  ClientStream& operator=(const ClientStream&) = delete;

  ClientStream(ClientStream&& other) noexcept
      : stream_(std::exchange(other.stream_, nullptr)),
        callbacks_(std::move(other.callbacks_)) {}
  ClientStream& operator=(ClientStream&& other) noexcept {
    if (this != &other) {
      Reset();
      stream_ = std::exchange(other.stream_, nullptr);
      callbacks_ = std::move(other.callbacks_);
    }
    return *this;
  }

  static Error Open(Channel& channel, std::string method,
                    ClientStreamOptions options,
                    ClientStreamCallbacks callbacks, ClientStream* out) {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    auto state = std::make_unique<CallbackState>(std::move(callbacks));
    grpc_lite_client_stream_options c_options =
        GRPC_LITE_CLIENT_STREAM_OPTIONS_INIT;
    c_options.metadata = options.metadata == nullptr
                             ? nullptr
                             : options.metadata->metadata_;
    c_options.has_timeout = options.has_timeout;
    c_options.send_compression =
        static_cast<std::uint32_t>(options.send_compression);
    c_options.timeout_ns = options.timeout_ns;
    c_options.max_message_size = options.max_message_size;
    c_options.max_inbound_buffer_size = options.max_inbound_buffer_size;
    c_options.max_outbound_buffer_size = options.max_outbound_buffer_size;

    grpc_lite_client_stream_callbacks c_callbacks =
        GRPC_LITE_CLIENT_STREAM_CALLBACKS_INIT;
    c_callbacks.user_data = state.get();
    c_callbacks.on_headers = &OnHeaders;
    c_callbacks.on_message = &OnMessage;
    c_callbacks.on_remote_end = &OnRemoteEnd;
    c_callbacks.on_writable = &OnWritable;
    c_callbacks.on_terminal = &OnTerminal;

    grpc_lite_client_stream* stream = nullptr;
    const Error error(grpc_lite_channel_open_stream(
        channel.channel_, internal::Bytes(method), &c_options, &c_callbacks,
        &stream));
    if (!error.ok()) return error;
    *out = ClientStream(stream, std::move(state));
    return {};
  }

  Error Send(const std::string& payload,
             Compression compression = Compression::Identity) {
    if (stream_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_client_stream_send(
        stream_, internal::Bytes(payload),
        static_cast<std::uint32_t>(compression)));
  }
  Error CloseSend() {
    if (stream_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_client_stream_close_send(stream_));
  }
  void Cancel() { grpc_lite_client_stream_cancel(stream_); }
  Error ResumeReceive() {
    if (stream_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_client_stream_resume_receive(stream_));
  }

 private:
  struct CallbackState {
    explicit CallbackState(ClientStreamCallbacks value)
        : callbacks(std::move(value)) {}
    ClientStreamCallbacks callbacks;
  };

  ClientStream(grpc_lite_client_stream* stream,
               std::unique_ptr<CallbackState> callbacks)
      : stream_(stream), callbacks_(std::move(callbacks)) {}

  static void OnHeaders(void* user_data, grpc_lite_client_stream*,
                         const grpc_lite_metadata_view* headers) noexcept {
    auto& callback = static_cast<CallbackState*>(user_data)->callbacks.on_headers;
    MetadataEntries copy = internal::CopyMetadataView(headers);
    if (callback) callback(std::move(copy));
  }
  static std::uint32_t OnMessage(void* user_data, grpc_lite_client_stream*,
                                  grpc_lite_bytes_view payload,
                                  std::uint32_t compression) noexcept {
    auto& callback = static_cast<CallbackState*>(user_data)->callbacks.on_message;
    std::string copy = internal::CopyBytes(payload);
    if (!callback) return GRPC_LITE_RECEIVE_CONTINUE;
    return static_cast<std::uint32_t>(
        callback(std::move(copy), static_cast<Compression>(compression)));
  }
  static void OnRemoteEnd(void* user_data,
                          grpc_lite_client_stream*) noexcept {
    auto& callback =
        static_cast<CallbackState*>(user_data)->callbacks.on_remote_end;
    if (callback) callback();
  }
  static void OnWritable(void* user_data,
                         grpc_lite_client_stream*) noexcept {
    auto& callback =
        static_cast<CallbackState*>(user_data)->callbacks.on_writable;
    if (callback) callback();
  }
  static void OnTerminal(void* user_data, grpc_lite_client_stream*,
                          std::int32_t status_code,
                          grpc_lite_bytes_view status_message,
                          const grpc_lite_metadata_view* trailing_metadata) noexcept {
    auto& callback =
        static_cast<CallbackState*>(user_data)->callbacks.on_terminal;
    Status status(static_cast<StatusCode>(status_code),
                  internal::CopyBytes(status_message));
    MetadataEntries copy = internal::CopyMetadataView(trailing_metadata);
    if (callback) callback(std::move(status), std::move(copy));
  }

  void Reset() {
    grpc_lite_client_stream_destroy(stream_);
    stream_ = nullptr;
    callbacks_.reset();
  }

  grpc_lite_client_stream* stream_ = nullptr;
  std::unique_ptr<CallbackState> callbacks_;
};

}  // namespace grpc_lite

#endif
