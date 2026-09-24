#include "grpc/testing/test.grpc.pb.h"

#include <grpcpp/grpcpp.h>

#include <array>
#include <chrono>
#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <memory>
#include <string>
#include <string_view>
#include <vector>

namespace {

constexpr std::array<int, 4> kRequestSizes{27182, 8, 1828, 45904};
constexpr std::array<int, 4> kResponseSizes{31415, 9, 2653, 58979};

struct Config {
  std::string host = "127.0.0.1";
  int port = 10000;
  std::string test_case = "client_streaming";
};

Config ParseArgs(int argc, char** argv) {
  Config config;
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument.rfind("--server_host=", 0) == 0) {
      config.host = std::string(argument.substr(14));
    } else if (argument.rfind("--server_port=", 0) == 0) {
      config.port = std::atoi(argv[index] + 14);
    } else if (argument.rfind("--test_case=", 0) == 0) {
      config.test_case = std::string(argument.substr(12));
    } else if (argument != "--use_tls=false") {
      std::cerr << "unsupported argument: " << argument << '\n';
      std::exit(2);
    }
  }
  return config;
}

void SetDeadline(grpc::ClientContext* context) {
  context->set_deadline(std::chrono::system_clock::now() +
                        std::chrono::seconds(60));
}

void RequireStatus(const grpc::Status& status) {
  if (status.ok()) return;
  std::cerr << "unexpected RPC status " << static_cast<int>(status.error_code())
            << ": " << status.error_message() << '\n';
  std::exit(1);
}

void FillPayload(grpc::testing::Payload* payload, int size) {
  payload->set_type(grpc::testing::COMPRESSABLE);
  payload->set_body(std::string(static_cast<std::size_t>(size), '\0'));
}

grpc::testing::StreamingOutputCallRequest MakeOutputRequest(int request_size,
                                                             int response_size) {
  grpc::testing::StreamingOutputCallRequest request;
  request.set_response_type(grpc::testing::COMPRESSABLE);
  request.add_response_parameters()->set_size(response_size);
  FillPayload(request.mutable_payload(), request_size);
  return request;
}

void CheckOutput(const grpc::testing::StreamingOutputCallResponse& response,
                 int expected_size) {
  if (!response.has_payload() ||
      response.payload().body().size() != expected_size) {
    std::cerr << "unexpected streaming response payload size\n";
    std::exit(1);
  }
}

void ClientStreaming(grpc::testing::TestService::StubInterface* stub) {
  grpc::ClientContext context;
  SetDeadline(&context);
  grpc::testing::StreamingInputCallResponse response;
  auto writer = stub->StreamingInputCall(&context, &response);
  for (const int size : kRequestSizes) {
    grpc::testing::StreamingInputCallRequest request;
    FillPayload(request.mutable_payload(), size);
    if (!writer->Write(request)) {
      std::cerr << "client-streaming write failed\n";
      std::exit(1);
    }
  }
  if (!writer->WritesDone()) {
    std::cerr << "client-streaming half-close failed\n";
    std::exit(1);
  }
  RequireStatus(writer->Finish());
  if (response.aggregated_payload_size() != 74922) {
    std::cerr << "unexpected aggregated payload size\n";
    std::exit(1);
  }
}

void ServerStreaming(grpc::testing::TestService::StubInterface* stub) {
  grpc::ClientContext context;
  SetDeadline(&context);
  grpc::testing::StreamingOutputCallRequest request;
  request.set_response_type(grpc::testing::COMPRESSABLE);
  for (const int size : kResponseSizes) {
    request.add_response_parameters()->set_size(size);
  }
  auto reader = stub->StreamingOutputCall(&context, request);
  grpc::testing::StreamingOutputCallResponse response;
  std::size_t index = 0;
  while (reader->Read(&response)) {
    if (index >= kResponseSizes.size()) {
      std::cerr << "too many server-streaming responses\n";
      std::exit(1);
    }
    CheckOutput(response, kResponseSizes[index++]);
  }
  RequireStatus(reader->Finish());
  if (index != kResponseSizes.size()) {
    std::cerr << "missing server-streaming responses\n";
    std::exit(1);
  }
}

void PingPong(grpc::testing::TestService::StubInterface* stub) {
  grpc::ClientContext context;
  SetDeadline(&context);
  auto stream = stub->FullDuplexCall(&context);
  for (std::size_t index = 0; index < kRequestSizes.size(); ++index) {
    const auto request =
        MakeOutputRequest(kRequestSizes[index], kResponseSizes[index]);
    if (!stream->Write(request)) {
      std::cerr << "ping-pong write failed\n";
      std::exit(1);
    }
    grpc::testing::StreamingOutputCallResponse response;
    if (!stream->Read(&response)) {
      std::cerr << "ping-pong response is missing\n";
      std::exit(1);
    }
    CheckOutput(response, kResponseSizes[index]);
  }
  if (!stream->WritesDone()) {
    std::cerr << "ping-pong half-close failed\n";
    std::exit(1);
  }
  grpc::testing::StreamingOutputCallResponse extra;
  if (stream->Read(&extra)) {
    std::cerr << "unexpected extra ping-pong response\n";
    std::exit(1);
  }
  RequireStatus(stream->Finish());
}

void EmptyStream(grpc::testing::TestService::StubInterface* stub) {
  grpc::ClientContext context;
  SetDeadline(&context);
  auto stream = stub->FullDuplexCall(&context);
  if (!stream->WritesDone()) {
    std::cerr << "empty-stream half-close failed\n";
    std::exit(1);
  }
  grpc::testing::StreamingOutputCallResponse response;
  if (stream->Read(&response)) {
    std::cerr << "empty stream returned a response\n";
    std::exit(1);
  }
  RequireStatus(stream->Finish());
}

}  // namespace

int main(int argc, char** argv) {
  const Config config = ParseArgs(argc, argv);
  grpc::ChannelArguments arguments;
  arguments.SetAllowInitialOffline(false);
  auto channel = grpc::CreateCustomChannel(
      config.host + ":" + std::to_string(config.port),
      grpc::InsecureChannelCredentials(), arguments);
  auto stub = grpc::testing::TestService::NewStub(channel);

  if (config.test_case == "client_streaming") {
    ClientStreaming(stub.get());
  } else if (config.test_case == "server_streaming") {
    ServerStreaming(stub.get());
  } else if (config.test_case == "ping_pong") {
    PingPong(stub.get());
  } else if (config.test_case == "empty_stream") {
    EmptyStream(stub.get());
  } else {
    std::cerr << "unsupported test case: " << config.test_case << '\n';
    return 2;
  }

  channel->Shutdown();
  channel->Wait();
  std::cout << "C++ generated interop case passed: " << config.test_case
            << '\n';
  return 0;
}
