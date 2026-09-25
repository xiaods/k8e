#include "cpp_codegen.grpc.pb.h"

#include <cassert>
#include <functional>
#include <string_view>
#include <string>
#include <type_traits>

namespace {

class FakeChannel final : public grpc::ChannelInterface {
 public:
  grpc::Status CallUnary(const std::string& method, grpc::ClientContext*,
                         const std::string& request,
                         std::string* response) override {
    assert(method == "/demo.EchoService/Echo");
    *response = request + " response";
    return grpc::Status::OK;
  }
};

}  // namespace

using ServerStreamSignature =
    std::unique_ptr<grpc::ClientReader<demo::EchoReply>> (
        demo::EchoService::StubInterface::*)(grpc::ClientContext*,
                                             const demo::EchoRequest&);
using ClientStreamSignature =
    std::unique_ptr<grpc::ClientWriter<demo::EchoRequest>> (
        demo::EchoService::StubInterface::*)(grpc::ClientContext*,
                                             demo::EchoReply*);
using BidiStreamSignature =
    std::unique_ptr<grpc::ClientReaderWriter<demo::EchoRequest,
                                             demo::EchoReply>> (
        demo::EchoService::StubInterface::*)(grpc::ClientContext*);
static_assert(std::is_same_v<decltype(&demo::EchoService::StubInterface::ServerStream),
                             ServerStreamSignature>);
static_assert(std::is_same_v<decltype(&demo::EchoService::StubInterface::ClientStream),
                             ClientStreamSignature>);
static_assert(std::is_same_v<decltype(&demo::EchoService::StubInterface::BidiStream),
                             BidiStreamSignature>);

class CompileEventService final : public demo::EchoService::EventService {
 public:
  void Echo(const grpc_lite::ServerContext&, demo::EchoRequest,
            grpc_lite::UnaryCall<demo::EchoReply>) noexcept override {}
  void ServerStream(
      const grpc_lite::ServerContext&, demo::EchoRequest,
      grpc_lite::ServerStreamingCall<demo::EchoReply>) noexcept override {}
  void ClientStream(
      const grpc_lite::ServerContext&,
      grpc_lite::ClientStreamingCall<demo::EchoRequest,
                                     demo::EchoReply>) noexcept override {}
  void BidiStream(
      const grpc_lite::ServerContext&,
      grpc_lite::BidirectionalStreamingCall<demo::EchoRequest,
                                            demo::EchoReply>) noexcept override {}
};

class CompileCollisionEventService final
    : public demo::CollisionService::EventService {
 public:
#define GRPC_LITE_COLLISION_OVERRIDE(name)                              \
  void name(const grpc_lite::ServerContext&, demo::EchoRequest,         \
            grpc_lite::UnaryCall<demo::EchoReply>) noexcept override {}
  GRPC_LITE_COLLISION_OVERRIDE(class_)
  GRPC_LITE_COLLISION_OVERRIDE(class__)
  GRPC_LITE_COLLISION_OVERRIDE(Service)
  GRPC_LITE_COLLISION_OVERRIDE(Service_)
  GRPC_LITE_COLLISION_OVERRIDE(Register_)
  GRPC_LITE_COLLISION_OVERRIDE(Register__)
  GRPC_LITE_COLLISION_OVERRIDE(EventService_)
  GRPC_LITE_COLLISION_OVERRIDE(CreateEventService)
  GRPC_LITE_COLLISION_OVERRIDE(grpc_lite_registration_0__)
  GRPC_LITE_COLLISION_OVERRIDE(grpc_lite_registration_0___)
#undef GRPC_LITE_COLLISION_OVERRIDE
};

using UnaryServiceSignature = grpc::Status (demo::EchoService::Service::*)(
    grpc::ServerContext*, const demo::EchoRequest*, demo::EchoReply*);
using StreamingServiceSignature = grpc::Status (demo::EchoService::Service::*)(
    grpc::ServerContext*, const demo::EchoRequest*,
    grpc::ServerWriter<demo::EchoReply>*);
using ClientStreamingServiceSignature =
    grpc::Status (demo::EchoService::Service::*)(
        grpc::ServerContext*, grpc::ServerReader<demo::EchoRequest>*,
        demo::EchoReply*);
using BidiStreamingServiceSignature =
    grpc::Status (demo::EchoService::Service::*)(
        grpc::ServerContext*,
        grpc::ServerReaderWriter<demo::EchoReply, demo::EchoRequest>*);
static_assert(std::is_same_v<decltype(&demo::EchoService::Service::Echo),
                             UnaryServiceSignature>);
static_assert(
    std::is_same_v<decltype(&demo::EchoService::Service::ServerStream),
                   StreamingServiceSignature>);
static_assert(
    std::is_same_v<decltype(&demo::EchoService::Service::ClientStream),
                   ClientStreamingServiceSignature>);
static_assert(std::is_same_v<decltype(&demo::EchoService::Service::BidiStream),
                             BidiStreamingServiceSignature>);

static_assert(std::is_abstract_v<grpc_lite::ServerExecutor>);

void TestAdmissionGate() {
  grpc_lite::internal::CallAdmissionGate cancelled_queued;
  cancelled_queued.Stop();
  assert(!cancelled_queued.BeginStart());

  grpc_lite::internal::CallAdmissionGate cancelled_starting;
  assert(cancelled_starting.BeginStart());
  cancelled_starting.Stop();
  assert(!cancelled_starting.CommitStart());

  grpc_lite::internal::CallAdmissionGate admitted;
  assert(admitted.BeginStart());
  assert(admitted.CommitStart());
  admitted.Stop();
}

class CompileExecutor final : public grpc_lite::ServerExecutor {
 public:
  bool Submit(std::string_view method, Task task) noexcept override {
    assert(!method.empty());
    task();
    return true;
  }
};

int main() {
  TestAdmissionGate();
  auto channel = std::make_shared<FakeChannel>();
  assert(std::string(demo::EchoService::service_full_name()) == "demo.EchoService");
  auto stub = demo::EchoService::NewStub(channel, grpc::StubOptions{});
  demo::EchoRequest request;
  request.set_message("request");
  demo::EchoReply response;
  grpc::ClientContext context;
  const grpc::Status status = stub->Echo(&context, request, &response);
  assert(status.ok());
  assert(response.message() == "request response");
  demo::EchoService::Service service;
  CompileExecutor executor;
  auto first_adapter = service.CreateEventService(executor);
  auto second_adapter = service.CreateEventService(executor);
  assert(first_adapter != nullptr);
  assert(second_adapter != nullptr);
  assert(first_adapter.get() != second_adapter.get());
  return 0;
}
