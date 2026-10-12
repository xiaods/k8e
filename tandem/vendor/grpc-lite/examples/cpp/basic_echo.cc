#include "echo.grpc.pb.h"
#include "example_support.h"

#include <grpcpp/grpcpp.h>

#include <memory>
#include <string>

int main(int argc, char** argv) {
  if (argc != 2) {
    std::cerr << "usage: grpc_lite_cpp_basic_echo HOST:PORT\n";
    return 2;
  }

  auto channel = example::CreateChannel(argv[1]);
  auto stub = demo::EchoService::NewStub(channel);
  if (!example::Check(std::string(demo::EchoService::service_full_name()) ==
                          "demo.EchoService",
                      "unexpected generated service name")) {
    return 1;
  }

  const char message[] = "hello\0grpc-lite\n";
  demo::EchoRequest request;
  request.set_message(message, sizeof(message) - 1);
  demo::EchoReply response;
  grpc::ClientContext context;
  const grpc::Status status = stub->Echo(&context, request, &response);
  if (!example::CheckStatus(status, grpc::StatusCode::OK, "Echo")) {
    return 1;
  }
  if (!example::Check(response.message() == request.message(),
                      "Echo response payload differs")) {
    return 1;
  }
  std::cout << "Echoed " << response.message().size() << " bytes\n";
  return 0;
}
