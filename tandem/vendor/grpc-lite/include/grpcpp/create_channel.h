#ifndef GRPCPP_CREATE_CHANNEL_H
#define GRPCPP_CREATE_CHANNEL_H

#include <grpcpp/channel.h>
#include <grpcpp/channel_arguments.h>
#include <grpcpp/security/credentials.h>

namespace grpc {

std::shared_ptr<Channel> CreateChannel(
    const std::string& target,
    const std::shared_ptr<ChannelCredentials>& credentials);

inline std::shared_ptr<Channel> CreateChannel(
    const std::string& target,
    const std::shared_ptr<ChannelCredentials>& credentials) {
  return std::shared_ptr<Channel>(
      new Channel(target, credentials, ChannelArguments{}));
}

inline std::shared_ptr<Channel> CreateCustomChannel(
    const std::string& target,
    const std::shared_ptr<ChannelCredentials>& credentials,
    const ChannelArguments& arguments) {
  return std::shared_ptr<Channel>(new Channel(target, credentials, arguments));
}

}  // namespace grpc

#endif
