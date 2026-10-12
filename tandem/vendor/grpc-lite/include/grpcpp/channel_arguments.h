#ifndef GRPCPP_CHANNEL_ARGUMENTS_H
#define GRPCPP_CHANNEL_ARGUMENTS_H

#include <grpc_lite/cpp/channel.hpp>
#include <grpcpp/support/status.h>

#include <cmath>
#include <cstdint>
#include <limits>
#include <utility>

enum grpc_compression_algorithm {
  GRPC_COMPRESS_NONE = 0,
  GRPC_COMPRESS_DEFLATE = 1,
  GRPC_COMPRESS_GZIP = 2,
};

namespace grpc {

using CompressionAlgorithm = grpc_compression_algorithm;
inline constexpr auto GRPC_COMPRESS_NONE = ::GRPC_COMPRESS_NONE;
inline constexpr auto GRPC_COMPRESS_DEFLATE = ::GRPC_COMPRESS_DEFLATE;
inline constexpr auto GRPC_COMPRESS_GZIP = ::GRPC_COMPRESS_GZIP;

class ChannelArguments {
 public:
  ChannelArguments() { options_.allow_initial_offline = true; }

  void SetAllowInitialOffline(bool allow) {
    options_.allow_initial_offline = allow;
  }
  void SetInitialReconnectBackoffMs(int milliseconds) {
    SetBackoffMs(milliseconds, &options_.initial_backoff_ns);
  }
  void SetMinReconnectBackoffMs(int milliseconds) {
    // grpc-lite has one initial/minimum backoff value.
    SetBackoffMs(milliseconds, &options_.initial_backoff_ns);
  }
  void SetMaxReconnectBackoffMs(int milliseconds) {
    SetBackoffMs(milliseconds, &options_.max_backoff_ns);
  }
  void SetReconnectBackoffMultiplier(double multiplier) {
    if (!std::isfinite(multiplier) || multiplier < 1 ||
        multiplier > static_cast<double>(
                         std::numeric_limits<std::uint32_t>::max()) /
                         1000.0) {
      status_ = {StatusCode::INVALID_ARGUMENT,
                 "invalid reconnect backoff multiplier"};
      return;
    }
    options_.multiplier_millis =
        static_cast<std::uint32_t>(multiplier * 1000.0);
  }
  void SetReconnectBackoffJitter(double jitter) {
    if (!std::isfinite(jitter) || jitter < 0 || jitter > 1) {
      status_ = {StatusCode::INVALID_ARGUMENT,
                 "invalid reconnect backoff jitter"};
      return;
    }
    options_.jitter_percent = static_cast<std::uint32_t>(jitter * 100.0);
  }
  void SetCompressionAlgorithm(grpc_compression_algorithm algorithm) {
    if (algorithm == GRPC_COMPRESS_NONE) {
      compression_ = grpc_lite::Compression::Identity;
    } else if (algorithm == GRPC_COMPRESS_GZIP) {
      compression_ = grpc_lite::Compression::Gzip;
    } else {
      status_ = {StatusCode::UNIMPLEMENTED,
                 "only identity and gzip compression are supported"};
    }
  }
  void SetDefaultCompressionAlgorithm(grpc_compression_algorithm algorithm) {
    SetCompressionAlgorithm(algorithm);
  }
  void SetLogger(grpc_lite::Logger logger) {
    options_.logger = std::move(logger);
  }

 private:
  friend class Channel;

  void SetBackoffMs(int milliseconds, std::uint64_t* output) {
    if (milliseconds < 0) {
      status_ = {StatusCode::INVALID_ARGUMENT,
                 "reconnect backoff must not be negative"};
      return;
    }
    *output = static_cast<std::uint64_t>(milliseconds) * UINT64_C(1000000);
  }

  grpc_lite::ChannelOptions options_;
  grpc_lite::Compression compression_ = grpc_lite::Compression::Identity;
  Status status_;
};

}  // namespace grpc

#endif
