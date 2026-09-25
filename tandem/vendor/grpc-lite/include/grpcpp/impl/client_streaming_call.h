#ifndef GRPCPP_IMPL_CLIENT_STREAMING_CALL_H
#define GRPCPP_IMPL_CLIENT_STREAMING_CALL_H

#include <grpcpp/impl/client_unary_call.h>
#include <grpcpp/support/sync_stream.h>

#include <memory>
#include <string>

namespace grpc::internal {

template <class Request, class Response>
std::unique_ptr<ClientReader<Response>> BlockingServerStreamingCall(
    ChannelInterface* channel, const RpcMethod& method, ClientContext* context,
    const Request& request) {
  std::shared_ptr<BlockingCallState> state;
  if (channel == nullptr || context == nullptr) {
    state = BlockingCallState::Failed(
        context, {StatusCode::INVALID_ARGUMENT,
                  "null server streaming call argument"});
  } else {
    std::string request_bytes;
    if (!request.SerializeToString(&request_bytes)) {
      state = BlockingCallState::Failed(
          context, {StatusCode::INTERNAL, "failed to serialize request"});
    } else {
      state = channel->OpenRawStream(method.name(), context);
      const Status status = state->SendAndClose(request_bytes);
      (void)status;
    }
  }
  return std::unique_ptr<ClientReader<Response>>(
      new ClientReader<Response>(std::move(state)));
}

template <class Request, class Response>
std::unique_ptr<ClientWriter<Request>> BlockingClientStreamingCall(
    ChannelInterface* channel, const RpcMethod& method, ClientContext* context,
    Response* response) {
  std::shared_ptr<BlockingCallState> state;
  if (channel == nullptr || context == nullptr || response == nullptr) {
    state = BlockingCallState::Failed(
        context, {StatusCode::INVALID_ARGUMENT,
                  "null client streaming call argument"});
  } else {
    state = channel->OpenRawStream(method.name(), context);
  }
  auto parse = [](const std::string& payload, void* output,
                  Status* parse_status) {
    return ParseMessage(payload, static_cast<Response*>(output), parse_status,
                        "response");
  };
  return std::unique_ptr<ClientWriter<Request>>(
      new ClientWriter<Request>(std::move(state), response, std::move(parse)));
}

template <class Request, class Response>
std::unique_ptr<ClientReaderWriter<Request, Response>>
BlockingBidiStreamingCall(ChannelInterface* channel, const RpcMethod& method,
                          ClientContext* context) {
  std::shared_ptr<BlockingCallState> state;
  if (channel == nullptr || context == nullptr) {
    state = BlockingCallState::Failed(
        context,
        {StatusCode::INVALID_ARGUMENT, "null bidirectional call argument"});
  } else {
    state = channel->OpenRawStream(method.name(), context);
  }
  return std::unique_ptr<ClientReaderWriter<Request, Response>>(
      new ClientReaderWriter<Request, Response>(std::move(state)));
}

}  // namespace grpc::internal

#endif
