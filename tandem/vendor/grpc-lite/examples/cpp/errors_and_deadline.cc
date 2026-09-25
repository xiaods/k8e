#include "echo.grpc.pb.h"
#include "example_support.h"

#include <grpcpp/grpcpp.h>

#include <chrono>
#include <memory>

int main(int argc, char** argv) {
  if (argc != 2) {
    std::cerr << "usage: grpc_lite_cpp_errors_and_deadline HOST:PORT\n";
    return 2;
  }

  auto channel = example::CreateChannel(argv[1]);
  auto stub = demo::EchoService::NewStub(channel);
  demo::EchoRequest request;
  request.set_message("request");

  demo::EchoReply missing_response;
  missing_response.set_message("unchanged");
  grpc::ClientContext missing_context;
  const grpc::Status missing =
      stub->Missing(&missing_context, request, &missing_response);
  if (!example::CheckStatus(missing, grpc::StatusCode::UNIMPLEMENTED,
                            "Missing")) {
    return 1;
  }
  if (!example::Check(missing_response.message() == "unchanged",
                      "failed RPC modified its response")) {
    return 1;
  }

  demo::EchoReply deadline_response;
  grpc::ClientContext deadline_context;
  deadline_context.set_deadline(std::chrono::system_clock::now() -
                                std::chrono::seconds(1));
  const auto started = std::chrono::steady_clock::now();
  const grpc::Status deadline =
      stub->Echo(&deadline_context, request, &deadline_response);
  const auto elapsed = std::chrono::steady_clock::now() - started;
  if (!example::CheckStatus(deadline, grpc::StatusCode::DEADLINE_EXCEEDED,
                            "expired-deadline Echo")) {
    return 1;
  }
  if (!example::Check(elapsed < std::chrono::seconds(2),
                      "expired deadline did not finish promptly")) {
    return 1;
  }
  std::cout << "Verified UNIMPLEMENTED and DEADLINE_EXCEEDED statuses\n";
  return 0;
}
