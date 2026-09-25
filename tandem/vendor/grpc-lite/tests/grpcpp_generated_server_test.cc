#include "cpp_codegen.grpc.pb.h"

#include <grpc_lite/grpc_lite.hpp>
#include <grpcpp/grpcpp.h>

#include <atomic>
#include <cassert>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <functional>
#include <memory>
#include <mutex>
#include <string>
#include <thread>
#include <type_traits>
#include <utility>
#include <vector>

static_assert(std::is_move_constructible_v<
              grpc_lite::UnaryCall<demo::EchoReply>>);
static_assert(!std::is_copy_constructible_v<
              grpc_lite::UnaryCall<demo::EchoReply>>);
static_assert(std::is_move_constructible_v<
              grpc_lite::ServerStreamingCall<demo::EchoReply>>);
static_assert(!std::is_copy_constructible_v<
              grpc_lite::ServerStreamingCall<demo::EchoReply>>);

namespace {

class EventService final : public demo::EchoService::EventService {
 public:
  ~EventService() override { Join(); }

  void Echo(const grpc_lite::ServerContext& context, demo::EchoRequest request,
            grpc_lite::UnaryCall<demo::EchoReply> call) noexcept override {
    AddWorker([this, context, request = std::move(request),
               call = std::move(call)]() mutable {
      if (context.request_metadata().size() == 1 &&
          context.request_metadata()[0] ==
              std::make_pair(std::string("x-request"),
                             std::string("metadata"))) {
        context_ok.store(true, std::memory_order_relaxed);
      }
      call.SetOnCancel(
          [this] { cancellations.fetch_add(1, std::memory_order_relaxed); });
      call.SetOnTerminal([this](grpc_lite::ServerTerminalReason) {
        terminals.fetch_add(1, std::memory_order_relaxed);
      });
      if (request.message() == "hold") {
        call.SetOnTerminal([this](grpc_lite::ServerTerminalReason) {
          cancellation_terminals.fetch_add(1, std::memory_order_relaxed);
        });
        std::lock_guard<std::mutex> lock(held_mutex_);
        held_call_ = std::move(call);
        held_ready_ = true;
        held_changed_.notify_all();
        return;
      }
      grpc_lite::Metadata initial;
      grpc_lite::Metadata trailing;
      assert(grpc_lite::Metadata::Create(&initial).ok());
      assert(grpc_lite::Metadata::Create(&trailing).ok());
      assert(initial.Add("x-initial", "typed").ok());
      assert(trailing.Add("x-trailing", "typed").ok());
      if (request.message() != "gzip") {
        assert(call.SendInitialMetadata(&initial).ok());
      }
      demo::EchoReply response;
      response.set_message(request.message() == "serialize"
                               ? "!serialize-error"
                               : request.message() + " response");
      assert(call.Finish(std::move(response), {}, &trailing,
                         request.message() == "gzip"
                             ? grpc_lite::Compression::Gzip
                             : grpc_lite::Compression::Identity)
                 .ok());
    });
  }

  void ServerStream(
      const grpc_lite::ServerContext& context, demo::EchoRequest request,
      grpc_lite::ServerStreamingCall<demo::EchoReply> call) noexcept override {
    AddWorker([this, context, request = std::move(request),
               call = std::move(call)]() mutable {
      context_copy_ok.store(context.request_metadata().size() == 1,
                            std::memory_order_relaxed);
      call.SetOnTerminal([this](grpc_lite::ServerTerminalReason) {
        terminals.fetch_add(1, std::memory_order_relaxed);
      });
      const int writable_before =
          writable_signals.load(std::memory_order_relaxed);
      call.SetOnWritable([this] {
        writable_signals.fetch_add(1, std::memory_order_relaxed);
      });
      assert(writable_signals.load(std::memory_order_relaxed) ==
             writable_before + 1);
      bool first = true;
      for (const char* suffix : {" one", " two"}) {
        demo::EchoReply response;
        response.set_message(request.message() + suffix);
        grpc_lite::Error error = call.TryWrite(
            response, first ? grpc_lite::Compression::Gzip
                            : grpc_lite::Compression::Identity);
        first = false;
        while (error.code() == grpc_lite::ErrorCode::WouldBlock) {
          if (!call.WaitForWritable()) return;
          error = call.TryWrite(response);
        }
        assert(error.ok());
      }
      assert(call.Finish().ok());
    });
  }

  void ClientStream(
      const grpc_lite::ServerContext&,
      grpc_lite::ClientStreamingCall<demo::EchoRequest, demo::EchoReply>
          call) noexcept override {
    AddWorker([this, call = std::move(call)]() mutable {
      client_stream_starts.fetch_add(1, std::memory_order_release);
      demo::EchoRequest request;
      std::string joined;
      while (call.Read(&request)) {
        if (!joined.empty()) joined += ",";
        joined += request.message();
      }
      if (call.cancelled() || call.terminal()) return;
      if (joined == "error") {
        assert(call
                   .Finish({grpc_lite::StatusCode::InvalidArgument,
                            "client stream rejected"})
                   .ok());
        return;
      }
      demo::EchoReply response;
      response.set_message(joined);
      assert(call.Finish(std::move(response)).ok());
    });
  }

  void BidiStream(
      const grpc_lite::ServerContext&,
      grpc_lite::BidirectionalStreamingCall<demo::EchoRequest,
                                            demo::EchoReply> call) noexcept override {
    AddWorker([call = std::move(call)]() mutable {
      demo::EchoRequest request;
      while (call.Read(&request)) {
        demo::EchoReply response;
        response.set_message(request.message() + " response");
        grpc_lite::Error error = call.TryWrite(response);
        while (error.code() == grpc_lite::ErrorCode::WouldBlock) {
          if (!call.WaitForWritable()) return;
          error = call.TryWrite(response);
        }
        assert(error.ok());
      }
      assert(call.Finish().ok());
    });
  }

  void WaitForHeld() {
    std::unique_lock<std::mutex> lock(held_mutex_);
    assert(held_changed_.wait_for(lock, std::chrono::seconds(5),
                                  [this] { return held_ready_; }));
  }

  void ReleaseHeld() {
    std::lock_guard<std::mutex> lock(held_mutex_);
    held_call_ = {};
  }

  void Join() {
    std::vector<std::thread> workers;
    {
      std::lock_guard<std::mutex> lock(workers_mutex_);
      workers.swap(workers_);
    }
    for (auto& worker : workers) worker.join();
  }

  std::atomic<bool> context_ok{false};
  std::atomic<bool> context_copy_ok{false};
  std::atomic<int> cancellations{0};
  std::atomic<int> terminals{0};
  std::atomic<int> cancellation_terminals{0};
  std::atomic<int> writable_signals{0};
  std::atomic<int> client_stream_starts{0};

 private:
  template <class Work>
  void AddWorker(Work work) {
    std::lock_guard<std::mutex> lock(workers_mutex_);
    workers_.emplace_back(std::move(work));
  }

  std::mutex workers_mutex_;
  std::vector<std::thread> workers_;
  std::mutex held_mutex_;
  std::condition_variable held_changed_;
  grpc_lite::UnaryCall<demo::EchoReply> held_call_;
  bool held_ready_ = false;
};

class QueueExecutor final : public grpc_lite::ServerExecutor {
 public:
  bool Submit(std::string_view method, Task task) noexcept override {
    std::lock_guard<std::mutex> lock(mutex_);
    submitted_methods.push_back(std::string(method));
    if (reject) return false;
    tasks_.push_back(std::move(task));
    changed_.notify_all();
    return true;
  }

  void WaitForTaskCount(std::size_t count) {
    std::unique_lock<std::mutex> lock(mutex_);
    assert(changed_.wait_for(lock, std::chrono::seconds(5),
                              [this, count] { return tasks_.size() >= count; }));
  }

  void WaitForTask() { WaitForTaskCount(1); }

  void RunOne() {
    Task task;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      assert(!tasks_.empty());
      task = std::move(tasks_.front());
      tasks_.erase(tasks_.begin());
    }
    task();
  }

  bool reject = false;
  std::vector<std::string> submitted_methods;

 private:
  std::mutex mutex_;
  std::condition_variable changed_;
  std::vector<Task> tasks_;
};

class RejectAdmission final : public grpc_lite::ServerAdmission {
 public:
  grpc_lite::Status Admit(
      std::string_view method,
      const grpc_lite::ServerContext& context) noexcept override {
    called.store(true, std::memory_order_release);
    assert(method == "/demo.EchoService/Echo");
    assert(context.request_metadata().empty());
    return {grpc_lite::StatusCode::FailedPrecondition, "rejected"};
  }

  std::atomic<bool> called{false};
};

class SynchronousService final : public demo::EchoService::Service {
 public:
  grpc::Status Echo(grpc::ServerContext* context,
                    const demo::EchoRequest* request,
                    demo::EchoReply* response) override {
    unary_calls.fetch_add(1, std::memory_order_relaxed);
    assert(!context->IsCancelled());
    const auto metadata = context->client_metadata().find("x-request");
    assert(metadata != context->client_metadata().end());
    context->AddInitialMetadata("x-initial", "sync");
    context->AddTrailingMetadata("x-trailing", "sync");
    response->set_message(request->message() + " response");
    return grpc::Status::OK;
  }

  grpc::Status ServerStream(
      grpc::ServerContext* context, const demo::EchoRequest* request,
      grpc::ServerWriter<demo::EchoReply>* writer) override {
    streaming_calls.fetch_add(1, std::memory_order_relaxed);
    context->set_compression_algorithm(GRPC_COMPRESS_NONE);
    context->AddInitialMetadata("x-initial", "stream");
    context->AddTrailingMetadata("x-trailing", "stream");
    for (int index = 0; index < 64; ++index) {
      demo::EchoReply response;
      response.set_message(request->message() + std::string(16384, 'x'));
      if (!writer->Write(response)) return grpc::Status::CANCELLED;
    }
    return grpc::Status::OK;
  }

  grpc::Status ClientStream(grpc::ServerContext*,
                            grpc::ServerReader<demo::EchoRequest>* reader,
                            demo::EchoReply* response) override {
    client_streaming_calls.fetch_add(1, std::memory_order_relaxed);
    demo::EchoRequest request;
    std::string joined;
    while (reader->Read(&request)) {
      if (!joined.empty()) joined += ",";
      joined += request.message();
    }
    response->set_message(joined);
    return grpc::Status::OK;
  }

  grpc::Status BidiStream(
      grpc::ServerContext*,
      grpc::ServerReaderWriter<demo::EchoReply, demo::EchoRequest>* stream)
      override {
    bidi_calls.fetch_add(1, std::memory_order_relaxed);
    demo::EchoRequest request;
    while (stream->Read(&request)) {
      demo::EchoReply response;
      response.set_message(request.message() + " response");
      if (!stream->Write(response)) return grpc::Status::CANCELLED;
    }
    return grpc::Status::OK;
  }

  std::atomic<int> unary_calls{0};
  std::atomic<int> streaming_calls{0};
  std::atomic<int> client_streaming_calls{0};
  std::atomic<int> bidi_calls{0};
};

grpc_lite::Status RawCall(
    grpc_lite::Channel& channel, std::vector<std::string> messages,
    std::string method = "/demo.EchoService/Echo") {
  std::mutex mutex;
  std::condition_variable changed;
  bool done = false;
  grpc_lite::Status result;
  grpc_lite::ClientStreamCallbacks callbacks;
  callbacks.on_terminal = [&](grpc_lite::Status status,
                              grpc_lite::MetadataEntries) {
    std::lock_guard<std::mutex> lock(mutex);
    result = std::move(status);
    done = true;
    changed.notify_one();
  };
  grpc_lite::ClientStream stream;
  assert(grpc_lite::ClientStream::Open(
             channel, method, {}, std::move(callbacks), &stream)
             .ok());
  for (const auto& message : messages) assert(stream.Send(message).ok());
  assert(stream.CloseSend().ok());
  std::unique_lock<std::mutex> lock(mutex);
  assert(changed.wait_for(lock, std::chrono::seconds(5),
                          [&] { return done; }));
  return result;
}

void WaitUntil(const std::function<bool()>& predicate) {
  const auto deadline = std::chrono::steady_clock::now() +
                        std::chrono::seconds(5);
  while (!predicate() && std::chrono::steady_clock::now() < deadline) {
    std::this_thread::yield();
  }
  assert(predicate());
}

void TestSynchronousService() {
  grpc_lite::ServerOptions server_options;
  server_options.max_message_size = 32768;
  server_options.max_outbound_buffer_size = 65536;
  grpc_lite::Server server;
  assert(grpc_lite::Server::Create(server_options, &server).ok());
  QueueExecutor executor;
  SynchronousService service;
  grpc_lite::SynchronousServiceOptions service_options;
  service_options.response_compression = grpc_lite::Compression::Gzip;
  auto adapter = service.CreateEventService(executor, service_options);
  assert(adapter->Register(server).ok());
  assert(server.Start().ok());
  std::uint32_t port = 0;
  assert(server.Port(&port).ok());

  grpc::ChannelArguments channel_arguments;
  channel_arguments.SetAllowInitialOffline(false);
  auto channel = grpc::CreateCustomChannel(
      "127.0.0.1:" + std::to_string(port),
      grpc::InsecureChannelCredentials(), channel_arguments);
  auto stub = demo::EchoService::NewStub(channel);

  grpc::ClientContext unary_context;
  unary_context.AddMetadata("x-request", "metadata");
  demo::EchoRequest request;
  request.set_message("sync");
  demo::EchoReply response;
  grpc::Status unary_status;
  std::thread unary_client([&] {
    unary_status = stub->Echo(&unary_context, request, &response);
  });
  executor.WaitForTask();
  std::thread unary_worker([&] { executor.RunOne(); });
  unary_worker.join();
  unary_client.join();
  assert(unary_status.ok());
  assert(response.message() == "sync response");
  assert(unary_context.GetServerInitialMetadata().find("x-initial")->second ==
         "sync");
  assert(unary_context.GetServerTrailingMetadata().find("x-trailing")->second ==
         "sync");

  grpc::ClientContext stream_context;
  stream_context.AddMetadata("x-request", "metadata");
  request.set_message("stream");
  auto reader = stub->ServerStream(&stream_context, request);
  executor.WaitForTask();
  std::atomic<bool> stream_worker_done{false};
  std::thread stream_worker([&] {
    executor.RunOne();
    stream_worker_done.store(true, std::memory_order_release);
  });
  std::this_thread::sleep_for(std::chrono::milliseconds(10));
  assert(!stream_worker_done.load(std::memory_order_acquire));
  int response_count = 0;
  while (reader->Read(&response)) ++response_count;
  assert(reader->Finish().ok());
  stream_worker.join();
  assert(response_count == 64);
  assert(stream_context.GetServerInitialMetadata().find("x-initial")->second ==
         "stream");
  assert(stream_context.GetServerTrailingMetadata().find("x-trailing")->second ==
         "stream");

  grpc::ClientContext client_stream_context;
  auto writer = stub->ClientStream(&client_stream_context, &response);
  request.set_message("sync client one");
  assert(writer->Write(request));
  request.set_message("sync client two");
  assert(writer->Write(request));
  assert(writer->WritesDone());
  executor.WaitForTask();
  std::thread client_stream_worker([&] { executor.RunOne(); });
  assert(writer->Finish().ok());
  client_stream_worker.join();
  assert(response.message() == "sync client one,sync client two");

  grpc::ClientContext bidi_context;
  auto bidi = stub->BidiStream(&bidi_context);
  request.set_message("sync bidi one");
  assert(bidi->Write(request));
  request.set_message("sync bidi two");
  assert(bidi->Write(request));
  assert(bidi->WritesDone());
  executor.WaitForTask();
  std::thread bidi_worker([&] { executor.RunOne(); });
  std::vector<std::string> bidi_responses;
  while (bidi->Read(&response)) bidi_responses.push_back(response.message());
  assert(bidi->Finish().ok());
  bidi_worker.join();
  assert((bidi_responses ==
          std::vector<std::string>{"sync bidi one response",
                                   "sync bidi two response"}));

  grpc::ClientContext cancelled_context;
  cancelled_context.AddMetadata("x-request", "metadata");
  request.set_message("cancelled");
  grpc::Status cancelled_status;
  std::thread cancelled_client([&] {
    cancelled_status = stub->Echo(&cancelled_context, request, &response);
  });
  executor.WaitForTask();
  cancelled_context.TryCancel();
  cancelled_client.join();
  assert(cancelled_status.error_code() == grpc::StatusCode::CANCELLED);
  assert(executor.submitted_methods.front() == "/demo.EchoService/Echo");

  grpc::ClientContext cancelled_stream_context;
  request.set_message("cancelled stream");
  auto cancelled_reader =
      stub->ServerStream(&cancelled_stream_context, request);
  executor.WaitForTaskCount(2);
  cancelled_stream_context.TryCancel();
  server.Shutdown();
  server.Wait();
  assert(!cancelled_reader->Finish().ok());
  executor.RunOne();
  executor.RunOne();
  assert(service.unary_calls.load(std::memory_order_relaxed) == 1);
  assert(service.streaming_calls.load(std::memory_order_relaxed) == 1);
  assert(service.client_streaming_calls.load(std::memory_order_relaxed) == 1);
  assert(service.bidi_calls.load(std::memory_order_relaxed) == 1);

  channel->Shutdown();
  channel->Wait();
  stub.reset();
  channel.reset();
  grpc_lite::Server restarted_server;
  assert(grpc_lite::Server::Create({}, &restarted_server).ok());
  auto restarted_adapter = service.CreateEventService(executor);
  assert(restarted_adapter.get() != adapter.get());
  assert(restarted_adapter->Register(restarted_server).ok());
  assert(restarted_server.Start().ok());
  restarted_server.ShutdownGracefully(UINT64_C(1000000000));
  restarted_server.Wait();
}

void TestSynchronousErrors() {
  for (bool reject : {false, true}) {
    grpc_lite::Server server;
    assert(grpc_lite::Server::Create({}, &server).ok());
    QueueExecutor executor;
    executor.reject = reject;
    demo::EchoService::Service service;
    auto adapter = service.CreateEventService(executor);
    assert(adapter->Register(server).ok());
    assert(server.Start().ok());
    std::uint32_t port = 0;
    assert(server.Port(&port).ok());
    grpc::ChannelArguments channel_arguments;
    channel_arguments.SetAllowInitialOffline(false);
    auto channel = grpc::CreateCustomChannel(
        "127.0.0.1:" + std::to_string(port),
        grpc::InsecureChannelCredentials(), channel_arguments);
    auto stub = demo::EchoService::NewStub(channel);
    grpc::ClientContext context;
    demo::EchoRequest request;
    demo::EchoReply response;
    grpc::Status status;
    std::thread client([&] { status = stub->Echo(&context, request, &response); });
    if (!reject) {
      executor.WaitForTask();
      executor.RunOne();
    }
    client.join();
    assert(status.error_code() ==
           (reject ? grpc::StatusCode::RESOURCE_EXHAUSTED
                   : grpc::StatusCode::UNIMPLEMENTED));
    channel->Shutdown();
    channel->Wait();
    stub.reset();
    channel.reset();
    server.ShutdownGracefully(UINT64_C(1000000000));
    server.Wait();
  }

  grpc_lite::Server server;
  assert(grpc_lite::Server::Create({}, &server).ok());
  QueueExecutor executor;
  RejectAdmission admission;
  demo::EchoService::Service service;
  grpc_lite::SynchronousServiceOptions options;
  options.admission = &admission;
  auto adapter = service.CreateEventService(executor, options);
  assert(adapter->Register(server).ok());
  assert(server.Start().ok());
  std::uint32_t port = 0;
  assert(server.Port(&port).ok());
  grpc::ChannelArguments channel_arguments;
  channel_arguments.SetAllowInitialOffline(false);
  auto channel = grpc::CreateCustomChannel(
      "127.0.0.1:" + std::to_string(port),
      grpc::InsecureChannelCredentials(), channel_arguments);
  auto stub = demo::EchoService::NewStub(channel);
  grpc::ClientContext context;
  demo::EchoRequest request;
  demo::EchoReply response;
  const grpc::Status status = stub->Echo(&context, request, &response);
  assert(status.error_code() == grpc::StatusCode::FAILED_PRECONDITION);
  assert(admission.called.load(std::memory_order_acquire));
  assert(executor.submitted_methods.empty());
  channel->Shutdown();
  channel->Wait();
  stub.reset();
  channel.reset();
  server.ShutdownGracefully(UINT64_C(1000000000));
  server.Wait();
}

}  // namespace

int main() {
  grpc_lite::Server server;
  assert(grpc_lite::Server::Create({}, &server).ok());
  EventService service;
  assert(service.Register(server).ok());
  assert(server.Start().ok());
  std::uint32_t port = 0;
  assert(server.Port(&port).ok());

  grpc::ChannelArguments channel_arguments;
  channel_arguments.SetAllowInitialOffline(false);
  auto channel = grpc::CreateCustomChannel(
      "127.0.0.1:" + std::to_string(port),
      grpc::InsecureChannelCredentials(), channel_arguments);
  auto stub = demo::EchoService::NewStub(channel);

  grpc::ClientContext unary_context;
  unary_context.AddMetadata("x-request", "metadata");
  demo::EchoRequest request;
  request.set_message("unary");
  demo::EchoReply response;
  grpc::Status status = stub->Echo(&unary_context, request, &response);
  assert(status.ok());
  assert(response.message() == "unary response");
  assert(unary_context.GetServerInitialMetadata().find("x-initial")->second ==
         "typed");
  assert(unary_context.GetServerTrailingMetadata().find("x-trailing")->second ==
         "typed");

  grpc::ClientContext serialization_context;
  request.set_message("serialize");
  status = stub->Echo(&serialization_context, request, &response);
  assert(status.error_code() == grpc::StatusCode::INTERNAL);

  grpc::ClientContext gzip_context;
  request.set_message("gzip");
  status = stub->Echo(&gzip_context, request, &response);
  assert(status.ok());
  assert(response.message() == "gzip response");

  grpc::ClientContext stream_context;
  stream_context.AddMetadata("x-request", "metadata");
  request.set_message("stream");
  auto reader = stub->ServerStream(&stream_context, request);
  std::vector<std::string> responses;
  while (reader->Read(&response)) responses.push_back(response.message());
  assert(reader->Finish().ok());
  assert((responses == std::vector<std::string>{"stream one", "stream two"}));
  assert(service.context_ok.load(std::memory_order_relaxed));
  assert(service.context_copy_ok.load(std::memory_order_relaxed));

  grpc::ClientContext client_stream_context;
  auto writer = stub->ClientStream(&client_stream_context, &response);
  request.set_message("client one");
  assert(writer->Write(request));
  request.set_message("client two");
  assert(writer->Write(request));
  assert(writer->WritesDone());
  assert(writer->Finish().ok());
  assert(response.message() == "client one,client two");

  grpc::ClientContext rejected_stream_context;
  response.set_message("unchanged");
  auto rejected_writer =
      stub->ClientStream(&rejected_stream_context, &response);
  request.set_message("error");
  assert(rejected_writer->Write(request));
  assert(rejected_writer->WritesDone());
  const grpc::Status rejected_status = rejected_writer->Finish();
  assert(rejected_status.error_code() == grpc::StatusCode::INVALID_ARGUMENT);
  assert(response.message() == "unchanged");

  const int starts_before_cancel =
      service.client_stream_starts.load(std::memory_order_acquire);
  grpc::ClientContext cancelled_stream_context;
  auto cancelled_writer =
      stub->ClientStream(&cancelled_stream_context, &response);
  WaitUntil([&] {
    return service.client_stream_starts.load(std::memory_order_acquire) >
           starts_before_cancel;
  });
  cancelled_stream_context.TryCancel();
  assert(cancelled_writer->Finish().error_code() ==
         grpc::StatusCode::CANCELLED);

  grpc::ClientContext bidi_context;
  auto bidi = stub->BidiStream(&bidi_context);
  responses.clear();
  std::thread bidi_reader([&] {
    demo::EchoReply bidi_response;
    while (bidi->Read(&bidi_response)) {
      responses.push_back(bidi_response.message());
    }
  });
  request.set_message("bidi one");
  assert(bidi->Write(request));
  request.set_message("bidi two");
  assert(bidi->Write(request));
  assert(bidi->WritesDone());
  bidi_reader.join();
  assert(bidi->Finish().ok());
  assert((responses == std::vector<std::string>{"bidi one response",
                                                "bidi two response"}));

  grpc_lite::Channel raw_channel;
  assert(grpc_lite::Channel::CreateManaged(
             nullptr, "127.0.0.1:" + std::to_string(port), {}, &raw_channel)
             .ok());
  assert(RawCall(raw_channel, {}).code() ==
         grpc_lite::StatusCode::InvalidArgument);
  assert(RawCall(raw_channel, {"!malformed"}).code() ==
         grpc_lite::StatusCode::InvalidArgument);
  assert(RawCall(raw_channel, {"one", "two"}).code() ==
         grpc_lite::StatusCode::InvalidArgument);
  assert(RawCall(raw_channel, {"!malformed"},
                 "/demo.EchoService/ClientStream")
             .code() == grpc_lite::StatusCode::InvalidArgument);

  grpc::ClientContext cancellation_context;
  request.set_message("hold");
  std::thread cancelled_client([&] {
    demo::EchoReply ignored;
    const grpc::Status cancelled =
        stub->Echo(&cancellation_context, request, &ignored);
    assert(cancelled.error_code() == grpc::StatusCode::CANCELLED);
  });
  service.WaitForHeld();
  cancellation_context.TryCancel();
  cancelled_client.join();
  WaitUntil([&] {
    return service.cancellations.load(std::memory_order_relaxed) == 1;
  });
  service.ReleaseHeld();
  service.Join();
  WaitUntil([&] {
    return service.cancellation_terminals.load(std::memory_order_relaxed) == 1;
  });

  raw_channel.Shutdown();
  raw_channel.Wait();
  channel->Shutdown();
  channel->Wait();
  stub.reset();
  channel.reset();
  server.ShutdownGracefully(UINT64_C(1000000000));
  server.Wait();

  grpc_lite::Server partial_server;
  assert(grpc_lite::Server::Create({}, &partial_server).ok());
  grpc_lite::ServerStreamCallbacks occupied_callbacks;
  assert(partial_server
             .RegisterStream("/demo.EchoService/ServerStream", {},
                             std::move(occupied_callbacks))
             .ok());
  {
    EventService partial_service;
    assert(!partial_service.Register(partial_server).ok());
  }
  assert(partial_server.Start().ok());
  assert(partial_server.Port(&port).ok());
  grpc_lite::Channel partial_channel;
  assert(grpc_lite::Channel::CreateManaged(
             nullptr, "127.0.0.1:" + std::to_string(port), {},
             &partial_channel)
             .ok());
  assert(RawCall(partial_channel, {"request"}).code() ==
         grpc_lite::StatusCode::Unavailable);
  partial_channel.Shutdown();
  partial_channel.Wait();
  partial_server.ShutdownGracefully(UINT64_C(1000000000));
  partial_server.Wait();
  TestSynchronousService();
  TestSynchronousErrors();
  return 0;
}
