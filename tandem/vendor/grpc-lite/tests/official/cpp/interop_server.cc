#include "grpc/testing/test.grpc.pb.h"

#include <grpc_lite/grpc_lite.hpp>
#include <grpcpp/grpcpp.h>

#include <cstdint>
#include <cstdlib>
#include <iostream>
#include <mutex>
#include <string>
#include <string_view>
#include <thread>
#include <utility>
#include <vector>

#include <unistd.h>

namespace {

struct Config {
  std::uint32_t port = 10000;
};

Config ParseArgs(int argc, char** argv) {
  Config config;
  for (int index = 1; index < argc; ++index) {
    const std::string_view argument(argv[index]);
    if (argument.rfind("--port=", 0) == 0) {
      config.port = static_cast<std::uint32_t>(std::strtoul(argv[index] + 7,
                                                            nullptr, 10));
    } else if (argument != "--use_tls=false") {
      std::cerr << "unsupported argument: " << argument << '\n';
      std::exit(2);
    }
  }
  return config;
}

class ThreadExecutor final : public grpc_lite::ServerExecutor {
 public:
  ~ThreadExecutor() override {
    std::vector<std::thread> threads;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      threads.swap(threads_);
    }
    for (auto& thread : threads) thread.join();
  }

  bool Submit(std::string_view, Task task) noexcept override {
    std::lock_guard<std::mutex> lock(mutex_);
    threads_.emplace_back(std::move(task));
    return true;
  }

 private:
  std::mutex mutex_;
  std::vector<std::thread> threads_;
};

void FillResponse(grpc::testing::StreamingOutputCallResponse* response,
                  int size) {
  auto* payload = response->mutable_payload();
  payload->set_type(grpc::testing::COMPRESSABLE);
  payload->set_body(std::string(static_cast<std::size_t>(size), '\0'));
}

template <class Writer>
grpc::Status WriteConfiguredResponses(
    const grpc::testing::StreamingOutputCallRequest& request,
    Writer* writer) {
  for (const auto& parameter : request.response_parameters()) {
    if (parameter.size() < 0) {
      return {grpc::StatusCode::INVALID_ARGUMENT, "negative response size"};
    }
    grpc::testing::StreamingOutputCallResponse response;
    FillResponse(&response, parameter.size());
    if (!writer->Write(response)) return grpc::Status::CANCELLED;
  }
  return grpc::Status::OK;
}

class TestService final : public grpc::testing::TestService::Service {
 public:
  grpc::Status StreamingInputCall(
      grpc::ServerContext*,
      grpc::ServerReader<grpc::testing::StreamingInputCallRequest>* reader,
      grpc::testing::StreamingInputCallResponse* response) override {
    std::int64_t aggregate = 0;
    grpc::testing::StreamingInputCallRequest request;
    while (reader->Read(&request)) {
      if (request.has_payload()) {
        aggregate += static_cast<std::int64_t>(request.payload().body().size());
      }
    }
    if (aggregate > INT32_MAX) {
      return {grpc::StatusCode::RESOURCE_EXHAUSTED,
              "aggregated payload is too large"};
    }
    response->set_aggregated_payload_size(static_cast<std::int32_t>(aggregate));
    return grpc::Status::OK;
  }

  grpc::Status StreamingOutputCall(
      grpc::ServerContext*,
      const grpc::testing::StreamingOutputCallRequest* request,
      grpc::ServerWriter<grpc::testing::StreamingOutputCallResponse>* writer)
      override {
    return WriteConfiguredResponses(*request, writer);
  }

  grpc::Status FullDuplexCall(
      grpc::ServerContext*,
      grpc::ServerReaderWriter<grpc::testing::StreamingOutputCallResponse,
                               grpc::testing::StreamingOutputCallRequest>*
          stream) override {
    grpc::testing::StreamingOutputCallRequest request;
    while (stream->Read(&request)) {
      const grpc::Status status = WriteConfiguredResponses(request, stream);
      if (!status.ok()) return status;
    }
    return grpc::Status::OK;
  }

  grpc::Status HalfDuplexCall(
      grpc::ServerContext*,
      grpc::ServerReaderWriter<grpc::testing::StreamingOutputCallResponse,
                               grpc::testing::StreamingOutputCallRequest>*
          stream) override {
    std::vector<grpc::testing::StreamingOutputCallRequest> requests;
    grpc::testing::StreamingOutputCallRequest request;
    while (stream->Read(&request)) requests.push_back(request);
    for (const auto& queued : requests) {
      const grpc::Status status = WriteConfiguredResponses(queued, stream);
      if (!status.ok()) return status;
    }
    return grpc::Status::OK;
  }
};

}  // namespace

int main(int argc, char** argv) {
  const Config config = ParseArgs(argc, argv);
  grpc_lite::ServerOptions options;
  options.port = config.port;
  grpc_lite::Server server;
  const grpc_lite::Error create_error =
      grpc_lite::Server::Create(options, &server);
  if (!create_error.ok()) {
    std::cerr << "failed to create server: " << create_error.message() << '\n';
    return 1;
  }

  ThreadExecutor executor;
  TestService service;
  auto adapter = service.CreateEventService(executor);
  const grpc_lite::Error register_error = adapter->Register(server);
  if (!register_error.ok()) {
    std::cerr << "failed to register service: " << register_error.message()
              << '\n';
    return 1;
  }
  const grpc_lite::Error start_error = server.Start();
  if (!start_error.ok()) {
    std::cerr << "failed to start server: " << start_error.message() << '\n';
    return 1;
  }
  std::uint32_t port = 0;
  if (!server.Port(&port).ok()) return 1;
  std::cout << "grpc-lite C++ generated interop server listening on 127.0.0.1:"
            << port << std::endl;

  for (;;) pause();
}
