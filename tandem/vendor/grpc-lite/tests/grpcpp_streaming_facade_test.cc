#include <grpc_lite/grpc_lite.hpp>
#include <grpcpp/grpcpp.h>
#include <grpcpp/impl/client_streaming_call.h>
#include <grpcpp/impl/client_unary_call.h>

#include <atomic>
#include <cassert>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <utility>
#include <vector>

namespace {

class Message {
 public:
  explicit Message(std::string value = {}) : value_(std::move(value)) {}

  bool SerializeToString(std::string* output) const {
    *output = value_;
    return true;
  }
  bool ParseFromArray(const void* data, int size) {
    value_.assign(static_cast<const char*>(data), static_cast<std::size_t>(size));
    return true;
  }
  const std::string& value() const { return value_; }

 private:
  std::string value_;
};

struct ServerState {
  std::mutex mutex;
  std::condition_variable changed;
  int hanging_started = 0;
  int hanging_cancelled = 0;
  std::atomic<bool> request_metadata_ok{false};
  std::atomic<bool> request_gzip{false};
};

void WaitFor(std::condition_variable& changed, std::mutex& mutex,
             const std::function<bool()>& predicate) {
  std::unique_lock<std::mutex> lock(mutex);
  assert(changed.wait_for(lock, std::chrono::seconds(5), predicate));
}

}  // namespace

int main() {
  grpc_lite::Metadata initial_metadata;
  grpc_lite::Metadata trailing_metadata;
  assert(grpc_lite::Metadata::Create(&initial_metadata).ok());
  assert(grpc_lite::Metadata::Create(&trailing_metadata).ok());
  assert(initial_metadata.Add("x-initial", "header").ok());
  assert(trailing_metadata.Add("x-trailing", "trailer").ok());

  ServerState server_state;
  grpc_lite::Server server;
  assert(grpc_lite::Server::Create({}, &server).ok());

  grpc_lite::ServerStreamCallbacks stream_callbacks;
  stream_callbacks.on_message =
      [&server_state, &initial_metadata](
          grpc_lite::ServerStream& stream,
          const grpc_lite::ServerContext& context, std::string request,
          grpc_lite::Compression compression) noexcept {
        {
          std::lock_guard<std::mutex> lock(server_state.mutex);
          server_state.request_metadata_ok.store(
              context.request_metadata().size() == 1 &&
              context.request_metadata()[0] ==
                  std::make_pair(std::string("x-client"), std::string("value")),
              std::memory_order_relaxed);
          server_state.request_gzip.store(
              request == "request" &&
                  compression == grpc_lite::Compression::Gzip,
              std::memory_order_relaxed);
        }
        grpc_lite::ServerCall call;
        assert(stream.Retain(&call).ok());
        assert(call.SendInitialMetadata(&initial_metadata).ok());
        return grpc_lite::ReceiveAction::Continue;
      };
  stream_callbacks.on_remote_end =
      [&trailing_metadata](
          grpc_lite::ServerStream& stream,
          const grpc_lite::ServerContext&) noexcept {
        grpc_lite::ServerCall call;
        assert(stream.Retain(&call).ok());
        assert(call.Send("one").ok());
        assert(call.Send("").ok());
        assert(call.Send("three").ok());
        assert(call.Finish({}, &trailing_metadata).ok());
      };
  assert(server.RegisterStream("/test.Facade/Stream", {},
                               std::move(stream_callbacks))
             .ok());

  grpc_lite::ServerStreamCallbacks unary_callbacks;
  unary_callbacks.on_message =
      [](grpc_lite::ServerStream&, const grpc_lite::ServerContext&,
         std::string, grpc_lite::Compression) noexcept {
        return grpc_lite::ReceiveAction::Continue;
      };
  unary_callbacks.on_remote_end =
      [](grpc_lite::ServerStream& stream,
         const grpc_lite::ServerContext&) noexcept {
        grpc_lite::ServerCall call;
        assert(stream.Retain(&call).ok());
        assert(call.Send("request response").ok());
        assert(call.Finish({}).ok());
      };
  assert(server.RegisterStream("/test.Facade/Unary", {},
                               std::move(unary_callbacks))
             .ok());

  grpc_lite::ServerStreamCallbacks duplicate_callbacks;
  duplicate_callbacks.on_message =
      [](grpc_lite::ServerStream&, const grpc_lite::ServerContext&,
         std::string, grpc_lite::Compression) noexcept {
        return grpc_lite::ReceiveAction::Continue;
      };
  duplicate_callbacks.on_remote_end =
      [](grpc_lite::ServerStream& stream,
         const grpc_lite::ServerContext&) noexcept {
        grpc_lite::ServerCall call;
        assert(stream.Retain(&call).ok());
        assert(call.SendInitialMetadata().ok());
        assert(call.Send("first").ok());
        assert(call.Send("second").ok());
        assert(call.Finish({}).ok());
      };
  assert(server.RegisterStream("/test.Facade/Duplicate", {},
                               std::move(duplicate_callbacks))
             .ok());

  grpc_lite::ServerStreamCallbacks hanging_callbacks;
  hanging_callbacks.on_message =
      [&server_state](grpc_lite::ServerStream&,
                      const grpc_lite::ServerContext&, std::string,
                      grpc_lite::Compression) noexcept {
        std::lock_guard<std::mutex> lock(server_state.mutex);
        ++server_state.hanging_started;
        server_state.changed.notify_all();
        return grpc_lite::ReceiveAction::Continue;
      };
  hanging_callbacks.on_cancel =
      [&server_state](grpc_lite::ServerStream&,
                      const grpc_lite::ServerContext&) noexcept {
        std::lock_guard<std::mutex> lock(server_state.mutex);
        ++server_state.hanging_cancelled;
        server_state.changed.notify_all();
      };
  assert(server.RegisterStream("/test.Facade/Hang", {},
                               std::move(hanging_callbacks))
             .ok());

  assert(server.Start().ok());
  std::uint32_t port = 0;
  assert(server.Port(&port).ok());

  std::atomic<int> log_count{0};
  grpc::ChannelArguments arguments;
  arguments.SetAllowInitialOffline(false);
  arguments.SetInitialReconnectBackoffMs(100);
  arguments.SetMaxReconnectBackoffMs(300);
  arguments.SetCompressionAlgorithm(GRPC_COMPRESS_GZIP);
  arguments.SetLogger([&log_count](grpc_lite::LogLevel,
                                   std::string_view) noexcept {
    log_count.fetch_add(1, std::memory_order_relaxed);
  });
  auto channel = grpc::CreateCustomChannel(
      "127.0.0.1:" + std::to_string(port),
      grpc::InsecureChannelCredentials(), arguments);

  grpc::ClientContext stream_context;
  stream_context.AddMetadata("x-client", "value");
  auto reader = grpc::internal::BlockingServerStreamingCall<Message, Message>(
      channel.get(), grpc::internal::RpcMethod("/test.Facade/Stream"),
      &stream_context, Message("request"));
  std::vector<std::string> responses;
  Message response;
  assert(reader->Read(&response));
  responses.push_back(response.value());
  assert(stream_context.GetServerInitialMetadata().find("x-initial")->second ==
         "header");
  while (reader->Read(&response)) responses.push_back(response.value());
  const grpc::Status stream_status = reader->Finish();
  assert(stream_status.ok());
  assert(reader->Finish().ok());
  assert((responses == std::vector<std::string>{"one", "", "three"}));
  assert(stream_context.GetServerInitialMetadata().find("x-initial")->second ==
         "header");
  assert(stream_context.GetServerTrailingMetadata().find("x-trailing")->second ==
         "trailer");
  assert(server_state.request_metadata_ok.load(std::memory_order_relaxed));
  assert(server_state.request_gzip.load(std::memory_order_relaxed));

  grpc::ClientContext pre_cancelled;
  pre_cancelled.TryCancel();
  std::string raw_response = "unchanged";
  grpc::Status status = channel->CallUnary("/test.Facade/Unary",
                                           &pre_cancelled, "request",
                                           &raw_response);
  assert(status.error_code() == grpc::StatusCode::CANCELLED);
  assert(raw_response == "unchanged");

  grpc::ClientContext duplicate_context;
  raw_response = "unchanged";
  status = channel->CallUnary("/test.Facade/Duplicate", &duplicate_context,
                              "request", &raw_response);
  assert(status.error_code() == grpc::StatusCode::INTERNAL);
  assert(raw_response == "unchanged");

  grpc::ClientContext cancellation_context;
  raw_response = "unchanged";
  grpc::Status cancellation_status;
  std::thread unary_thread([&] {
    cancellation_status = channel->CallUnary(
        "/test.Facade/Hang", &cancellation_context, "request", &raw_response);
  });
  WaitFor(server_state.changed, server_state.mutex,
          [&] { return server_state.hanging_started >= 1; });
  cancellation_context.TryCancel();
  unary_thread.join();
  assert(cancellation_status.error_code() == grpc::StatusCode::CANCELLED);
  assert(raw_response == "unchanged");

  grpc::ClientContext early_finish_context;
  auto early_finish_reader =
      grpc::internal::BlockingServerStreamingCall<Message, Message>(
          channel.get(), grpc::internal::RpcMethod("/test.Facade/Hang"),
          &early_finish_context, Message("request"));
  WaitFor(server_state.changed, server_state.mutex,
          [&] { return server_state.hanging_started >= 2; });
  const grpc::Status early_finish_status = early_finish_reader->Finish();
  assert(early_finish_status.error_code() == grpc::StatusCode::CANCELLED);
  WaitFor(server_state.changed, server_state.mutex,
          [&] { return server_state.hanging_cancelled >= 2; });

  grpc::ClientContext deadline_context;
  deadline_context.set_deadline(std::chrono::system_clock::now() +
                                std::chrono::milliseconds(30));
  status = channel->CallUnary("/test.Facade/Hang", &deadline_context,
                              "request", &raw_response);
  assert(status.error_code() == grpc::StatusCode::DEADLINE_EXCEEDED);

  grpc::ClientContext abandoned_context;
  int cancelled_before_abandon = 0;
  {
    std::lock_guard<std::mutex> lock(server_state.mutex);
    cancelled_before_abandon = server_state.hanging_cancelled;
  }
  {
    auto abandoned =
        grpc::internal::BlockingServerStreamingCall<Message, Message>(
            channel.get(), grpc::internal::RpcMethod("/test.Facade/Hang"),
            &abandoned_context, Message("request"));
    WaitFor(server_state.changed, server_state.mutex,
            [&] { return server_state.hanging_started >= 4; });
  }
  WaitFor(server_state.changed, server_state.mutex,
          [&] {
            return server_state.hanging_cancelled > cancelled_before_abandon;
          });

  channel.reset();
  server.ShutdownGracefully(UINT64_C(1000000000));
  server.Wait();
  assert(log_count.load(std::memory_order_relaxed) > 0);
  return 0;
}
