#ifndef GRPC_LITE_CPP_CHANNEL_HPP
#define GRPC_LITE_CPP_CHANNEL_HPP

#include <grpc_lite/cpp/runtime.hpp>

#include <cstdint>
#include <functional>
#include <memory>
#include <string>
#include <string_view>
#include <utility>

namespace grpc_lite {

enum class LogLevel : std::uint32_t {
  Debug = GRPC_LITE_LOG_DEBUG,
  Info = GRPC_LITE_LOG_INFO,
  Warn = GRPC_LITE_LOG_WARN,
  Error = GRPC_LITE_LOG_ERROR,
};

using Logger = std::function<void(LogLevel, std::string_view)>;
// Logger callbacks may run concurrently on transport threads and must not throw.

struct ChannelOptions {
  bool allow_initial_offline = false;
  std::uint64_t initial_backoff_ns = UINT64_C(1000000000);
  std::uint64_t max_backoff_ns = UINT64_C(120000000000);
  std::uint32_t multiplier_millis = 1600;
  std::uint32_t jitter_percent = 20;
  Logger logger;
  std::uint64_t max_response_header_list_size = UINT64_C(65536);
};

class ClientStream;

class Channel {
 public:
  Channel() = default;
  ~Channel() { Reset(); }
  Channel(const Channel&) = delete;
  Channel& operator=(const Channel&) = delete;

  Channel(Channel&& other) noexcept
      : channel_(std::exchange(other.channel_, nullptr)),
        logger_(std::move(other.logger_)) {}
  Channel& operator=(Channel&& other) noexcept {
    if (this != &other) {
      Reset();
      channel_ = std::exchange(other.channel_, nullptr);
      logger_ = std::move(other.logger_);
    }
    return *this;
  }

  static Error CreateManaged(Runtime* runtime, std::string target,
                             ChannelOptions options, Channel* out) {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    auto logger = options.logger
                      ? std::make_unique<LoggerState>(std::move(options.logger))
                      : nullptr;
    grpc_lite_logger c_logger = GRPC_LITE_LOGGER_INIT;
    if (logger != nullptr) {
      c_logger.user_data = logger.get();
      c_logger.log = &Log;
    }
    grpc_lite_channel_options c_options = GRPC_LITE_CHANNEL_OPTIONS_INIT;
    c_options.allow_initial_offline = options.allow_initial_offline;
    c_options.initial_backoff_ns = options.initial_backoff_ns;
    c_options.max_backoff_ns = options.max_backoff_ns;
    c_options.multiplier_millis = options.multiplier_millis;
    c_options.jitter_percent = options.jitter_percent;
    c_options.logger = logger == nullptr ? nullptr : &c_logger;
    c_options.max_response_header_list_size =
        options.max_response_header_list_size;

    grpc_lite_channel* channel = nullptr;
    const Error error(grpc_lite_channel_create_managed(
        runtime == nullptr ? nullptr : runtime->runtime_, internal::Bytes(target),
        &c_options, &channel));
    if (!error.ok()) return error;
    *out = Channel(channel, std::move(logger));
    return {};
  }

  void Shutdown() { grpc_lite_channel_shutdown(channel_); }
  void Wait() { grpc_lite_channel_wait(channel_); }

 private:
  friend class ClientStream;
  struct LoggerState {
    explicit LoggerState(Logger value) : callback(std::move(value)) {}
    Logger callback;
  };

  Channel(grpc_lite_channel* channel, std::unique_ptr<LoggerState> logger)
      : channel_(channel), logger_(std::move(logger)) {}

  static void Log(void* user_data, std::uint32_t level,
                  grpc_lite_bytes_view message) noexcept {
    const auto* data = message.data == nullptr
                           ? ""
                           : reinterpret_cast<const char*>(message.data);
    static_cast<LoggerState*>(user_data)->callback(
        static_cast<LogLevel>(level), {data, message.size});
  }

  void Reset() {
    grpc_lite_channel_destroy(channel_);
    channel_ = nullptr;
    logger_.reset();
  }

  grpc_lite_channel* channel_ = nullptr;
  std::unique_ptr<LoggerState> logger_;
};

}  // namespace grpc_lite

#endif
