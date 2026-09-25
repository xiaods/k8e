#ifndef GRPCPP_CHANNEL_H
#define GRPCPP_CHANNEL_H

#include <grpc_lite/grpc_lite.hpp>
#include <grpcpp/channel_arguments.h>
#include <grpcpp/client_context.h>
#include <grpcpp/impl/blocking_call.h>
#include <grpcpp/security/credentials.h>
#include <grpcpp/support/status.h>

#include <memory>
#include <string>

namespace grpc {

class ChannelInterface {
 public:
  virtual ~ChannelInterface() = default;
  virtual Status CallUnary(const std::string& method, ClientContext* context,
                           const std::string& request,
                           std::string* response) = 0;

  virtual std::shared_ptr<internal::BlockingCallState> OpenRawStream(
      const std::string&, ClientContext* context) {
    return internal::BlockingCallState::Failed(
        context, {StatusCode::UNIMPLEMENTED,
                  "channel does not support server streaming"});
  }
};

class Channel final : public ChannelInterface,
                      public std::enable_shared_from_this<Channel> {
 public:
  ~Channel() override {
    Shutdown();
    Wait();
  }
  Channel(const Channel&) = delete;
  Channel& operator=(const Channel&) = delete;

  Status CallUnary(const std::string& method, ClientContext* context,
                   const std::string& request,
                   std::string* response) override {
    if (!construction_status_.ok()) return construction_status_;
    if (context == nullptr || response == nullptr) {
      return {StatusCode::INVALID_ARGUMENT, "null unary call argument"};
    }
    auto state = OpenRawStream(method, context);
    Status status = state->SendAndClose(request);
    std::string payload;
    const bool has_response = status.ok() && state->Read(&payload);
    std::string extra_payload;
    const bool has_extra_response = status.ok() && state->Read(&extra_payload);
    if (has_extra_response) state->Cancel();
    status = state->Finish();
    if (has_extra_response) {
      return {StatusCode::INTERNAL, "unary call returned multiple responses"};
    }
    if (!status.ok()) return status;
    if (!has_response) {
      return {StatusCode::INTERNAL, "unary response is missing"};
    }
    *response = std::move(payload);
    return {};
  }

  std::shared_ptr<internal::BlockingCallState> OpenRawStream(
      const std::string& method, ClientContext* context) override {
    if (context == nullptr) {
      return internal::BlockingCallState::Failed(
          context, {StatusCode::INVALID_ARGUMENT, "null client context"});
    }
    if (!construction_status_.ok()) {
      return internal::BlockingCallState::Failed(context,
                                                 construction_status_);
    }
    return internal::BlockingCallState::Open(channel_, method, context,
                                             compression_,
                                             weak_from_this().lock());
  }

  void Shutdown() {
    if (channel_ready_) channel_.Shutdown();
  }
  void Wait() {
    if (channel_ready_) channel_.Wait();
  }

 private:
  friend std::shared_ptr<Channel> CreateChannel(
      const std::string&, const std::shared_ptr<class ChannelCredentials>&);
  friend std::shared_ptr<Channel> CreateCustomChannel(
      const std::string&, const std::shared_ptr<class ChannelCredentials>&,
      const ChannelArguments&);

  Channel(const std::string& target,
          const std::shared_ptr<ChannelCredentials>& credentials,
          const ChannelArguments& arguments) {
    if (credentials == nullptr || !credentials->is_insecure()) {
      construction_status_ = {
          StatusCode::UNIMPLEMENTED,
          "only insecure channel credentials are supported"};
      return;
    }
    if (!arguments.status_.ok()) {
      construction_status_ = arguments.status_;
      return;
    }
    grpc_lite::ChannelOptions options = arguments.options_;
    const grpc_lite::Error error = grpc_lite::Channel::CreateManaged(
        nullptr, target, std::move(options), &channel_);
    if (!error.ok()) {
      construction_status_ = internal::ErrorStatus(error);
      return;
    }
    compression_ = arguments.compression_;
    channel_ready_ = true;
  }

  grpc_lite::Channel channel_;
  grpc_lite::Compression compression_ = grpc_lite::Compression::Identity;
  Status construction_status_;
  bool channel_ready_ = false;
};

}  // namespace grpc

#endif
