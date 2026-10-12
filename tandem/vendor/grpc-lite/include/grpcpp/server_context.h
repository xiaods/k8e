#ifndef GRPCPP_SERVER_CONTEXT_H
#define GRPCPP_SERVER_CONTEXT_H

#include <grpc_lite/cpp/server.hpp>
#include <grpcpp/channel_arguments.h>

#include <functional>
#include <map>
#include <string>
#include <utility>

namespace grpc {

namespace internal {
class ServerContextAccess;
}

class ServerContext {
 public:
  using MetadataMap = std::multimap<std::string, std::string>;

  ServerContext(const ServerContext&) = delete;
  ServerContext& operator=(const ServerContext&) = delete;

  const MetadataMap& client_metadata() const { return client_metadata_; }
  bool IsCancelled() const { return is_cancelled_ && is_cancelled_(); }

  void AddInitialMetadata(const std::string& key, const std::string& value) {
    initial_metadata_.emplace(key, value);
  }
  void AddTrailingMetadata(const std::string& key, const std::string& value) {
    trailing_metadata_.emplace(key, value);
  }
  void set_compression_algorithm(grpc_compression_algorithm algorithm) {
    compression_ = algorithm;
  }

 private:
  friend class internal::ServerContextAccess;
  ServerContext(const grpc_lite::ServerContext& context,
                std::function<bool()> is_cancelled,
                grpc_compression_algorithm compression)
      : is_cancelled_(std::move(is_cancelled)), compression_(compression) {
    for (const auto& entry : context.request_metadata()) {
      client_metadata_.emplace(entry.first, entry.second);
    }
  }

  MetadataMap client_metadata_;
  MetadataMap initial_metadata_;
  MetadataMap trailing_metadata_;
  std::function<bool()> is_cancelled_;
  grpc_compression_algorithm compression_ = GRPC_COMPRESS_NONE;
};

namespace internal {

class ServerContextAccess {
 public:
  static ServerContext Create(const grpc_lite::ServerContext& context,
                              std::function<bool()> is_cancelled,
                              grpc_compression_algorithm compression) {
    return ServerContext(context, std::move(is_cancelled), compression);
  }
  static const ServerContext::MetadataMap& InitialMetadata(
      const ServerContext& context) {
    return context.initial_metadata_;
  }
  static const ServerContext::MetadataMap& TrailingMetadata(
      const ServerContext& context) {
    return context.trailing_metadata_;
  }
  static grpc_compression_algorithm Compression(const ServerContext& context) {
    return context.compression_;
  }
};

}  // namespace internal
}  // namespace grpc

#endif
