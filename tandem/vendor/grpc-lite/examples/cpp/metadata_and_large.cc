#include "echo.grpc.pb.h"
#include "example_support.h"

#include <grpcpp/grpcpp.h>

#include <memory>
#include <string>

namespace {

bool Echo(demo::EchoService::Stub* stub, const std::string& message) {
  demo::EchoRequest request;
  request.set_message(message);
  demo::EchoReply response;
  grpc::ClientContext context;
  const grpc::Status status = stub->Echo(&context, request, &response);
  return example::CheckStatus(status, grpc::StatusCode::OK, "Echo") &&
         example::Check(response.message() == message,
                        "Echo response payload differs");
}

}  // namespace

int main(int argc, char** argv) {
  if (argc != 2) {
    std::cerr << "usage: grpc_lite_cpp_metadata_and_large HOST:PORT\n";
    return 2;
  }

  auto channel = example::CreateChannel(argv[1]);
  auto stub = demo::EchoService::NewStub(channel);

  demo::EchoRequest request;
  request.set_message("metadata");
  demo::EchoReply response;
  grpc::ClientContext context;
  context.AddMetadata("x-consumer", "generated-stub");
  const grpc::Status status = stub->Echo(&context, request, &response);
  if (!example::CheckStatus(status, grpc::StatusCode::OK, "metadata Echo")) {
    return 1;
  }
  if (!example::Check(response.message() == request.message(),
                      "metadata Echo response payload differs")) {
    return 1;
  }
  if (!example::Check(
          example::HasSingleMetadata(context.GetServerInitialMetadata(),
                                     "x-grpc-lite-service", "demo.EchoService"),
          "missing service initial metadata")) {
    return 1;
  }
  if (!example::Check(
          example::HasSingleMetadata(context.GetServerInitialMetadata(),
                                     "x-consumer-seen", "generated-stub"),
          "server did not observe request metadata")) {
    return 1;
  }
  if (!example::Check(
          example::HasSingleMetadata(context.GetServerTrailingMetadata(),
                                     "x-grpc-lite-method", "Echo"),
          "missing method trailing metadata")) {
    return 1;
  }

  if (!Echo(stub.get(), "")) {
    return 1;
  }
  if (!Echo(stub.get(), std::string(256 * 1024, 'x'))) {
    return 1;
  }
  std::cout << "Verified metadata, empty payload, and 256 KiB payload\n";
  return 0;
}
