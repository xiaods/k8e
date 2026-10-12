#include <grpc_lite/grpc_lite.hpp>

#include <atomic>
#include <cassert>
#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <memory>
#include <mutex>
#include <string>
#include <type_traits>
#include <utility>

using grpc_lite::Channel;
using grpc_lite::ClientStream;
using grpc_lite::Metadata;
using grpc_lite::Runtime;
using grpc_lite::Server;
using grpc_lite::ServerCall;

static_assert(!std::is_copy_constructible_v<Runtime>);
static_assert(std::is_nothrow_move_constructible_v<Runtime>);
static_assert(!std::is_copy_constructible_v<Metadata>);
static_assert(std::is_nothrow_move_constructible_v<Metadata>);
static_assert(!std::is_copy_constructible_v<Channel>);
static_assert(std::is_nothrow_move_constructible_v<Channel>);
static_assert(!std::is_copy_constructible_v<ClientStream>);
static_assert(std::is_nothrow_move_constructible_v<ClientStream>);
static_assert(!std::is_copy_constructible_v<Server>);
static_assert(std::is_nothrow_move_constructible_v<Server>);
static_assert(!std::is_copy_constructible_v<ServerCall>);
static_assert(std::is_nothrow_move_constructible_v<ServerCall>);

namespace {

void TestMetadata() {
  Metadata metadata;
  assert(Metadata::Create(&metadata).ok());
  assert(metadata.Add("x-order", "first").ok());
  assert(metadata.Add("x-order", "second").ok());
  const std::string binary("a\0b", 3);
  assert(metadata.Add("trace-bin", binary).ok());

  grpc_lite::MetadataEntries entries;
  assert(metadata.CopyEntries(&entries).ok());
  assert(entries.size() == 3);
  assert(entries[0] == std::make_pair(std::string("x-order"),
                                      std::string("first")));
  assert(entries[1] == std::make_pair(std::string("x-order"),
                                      std::string("second")));
  assert(entries[2].first == "trace-bin");
  assert(entries[2].second == binary);

  std::string key;
  std::string value;
  assert(metadata.At(2, &key, &value).ok());
  assert(key == "trace-bin");
  assert(value == binary);
}

void TestInvalidObjects() {
  ServerCall call;
  ServerCall clone;
  assert(call.id() == 0);
  assert(!call.cancelled());
  assert(!call.terminal());
  call.Abort();
  assert(call.Clone(&clone).code() == grpc_lite::ErrorCode::InvalidState);
  assert(call.Send("x").code() == grpc_lite::ErrorCode::InvalidState);
  assert(call.ResumeReceive().code() == grpc_lite::ErrorCode::InvalidState);

  auto logger_token = std::make_shared<int>(1);
  std::weak_ptr<int> weak = logger_token;
  grpc_lite::ChannelOptions options;
  assert(options.max_response_header_list_size == UINT64_C(65536));
  options.max_response_header_list_size = 4096;
  options.logger = [token = std::move(logger_token)](
                       grpc_lite::LogLevel, std::string_view) {};
  Channel channel;
  const grpc_lite::Error error =
      Channel::CreateManaged(nullptr, "invalid", std::move(options), &channel);
  assert(error.code() == grpc_lite::ErrorCode::InvalidArgument);
  options.logger = {};
  assert(weak.expired());
}

struct RoundTripState {
  std::mutex mutex;
  std::condition_variable completed;
  bool done = false;
  bool client_ok = false;
  bool server_context_ok = false;
  bool server_terminal = false;
  std::size_t started_call_id = 0;
  std::size_t terminal_call_id = 0;
  grpc_lite::MetadataEntries headers;
  grpc_lite::MetadataEntries trailers;
  std::string response;
};

void TestRoundTrip() {
  Runtime runtime;
  assert(Runtime::Create(&runtime).ok());

  Metadata initial_metadata;
  Metadata trailing_metadata;
  assert(Metadata::Create(&initial_metadata).ok());
  assert(Metadata::Create(&trailing_metadata).ok());
  assert(initial_metadata.Add("x-header", "one").ok());
  assert(initial_metadata.Add("x-header", "two").ok());
  assert(trailing_metadata.Add("trace-bin", std::string("\0\1", 2)).ok());

  RoundTripState state;
  std::atomic<int> log_count{0};
  Server server;
  grpc_lite::ServerOptions server_options;
  assert(server_options.max_connections == UINT64_C(1024));
  assert(server_options.max_concurrent_streams_per_connection == 100);
  assert(server_options.cleartext_preface_timeout_ns == UINT64_C(10000000000));
  assert(server_options.connection_idle_timeout_ns == UINT64_C(300000000000));
  server_options.max_connections = UINT64_C(2048);
  server_options.max_concurrent_streams_per_connection = 128;
  server_options.logger = [&log_count](grpc_lite::LogLevel,
                                        std::string_view) {
    log_count.fetch_add(1, std::memory_order_relaxed);
  };
  assert(Server::Create(std::move(server_options), &server).ok());

  grpc_lite::ServerStreamCallbacks server_callbacks;
  server_callbacks.on_start = [&state](grpc_lite::ServerStream& stream,
                                        const grpc_lite::ServerContext&) {
    ServerCall call;
    assert(stream.Retain(&call).ok());
    state.started_call_id = call.id();
  };
  server_callbacks.on_message =
      [&state, &initial_metadata](grpc_lite::ServerStream& stream,
                                  const grpc_lite::ServerContext& context,
                                  std::string payload,
                                  grpc_lite::Compression compression) {
        state.server_context_ok =
            payload == std::string("request\0bytes", 13) &&
            compression == grpc_lite::Compression::Identity &&
            context.request_compression() == grpc_lite::Compression::Identity &&
            context.request_metadata().size() == 2 &&
            context.request_metadata()[0].first == "x-client" &&
            context.request_metadata()[0].second == "first" &&
            context.request_metadata()[1].first == "x-client-bin" &&
            context.request_metadata()[1].second == std::string("\0x", 2);
        ServerCall call;
        assert(stream.Retain(&call).ok());
        assert(call.id() != 0);
        assert(call.SendInitialMetadata(&initial_metadata).ok());
        return grpc_lite::ReceiveAction::Continue;
      };
  server_callbacks.on_remote_end =
      [&trailing_metadata](grpc_lite::ServerStream& stream,
                           const grpc_lite::ServerContext&) {
        ServerCall call;
        assert(stream.Retain(&call).ok());
        assert(call.Send(std::string("response\0bytes", 14)).ok());
        assert(call.Finish({}, &trailing_metadata).ok());
      };
  server_callbacks.on_terminal =
      [&state](std::size_t call_id, grpc_lite::ServerTerminalReason reason) {
        std::lock_guard<std::mutex> lock(state.mutex);
        state.server_terminal =
            reason == grpc_lite::ServerTerminalReason::Completed;
        state.terminal_call_id = call_id;
      };
  assert(server.RegisterStream("/test.Native/Echo", {},
                               std::move(server_callbacks))
             .ok());
  assert(server.Start().ok());
  std::uint32_t port = 0;
  assert(server.Port(&port).ok());
  assert(port != 0);

  auto logger_token = std::make_shared<int>(2);
  std::weak_ptr<int> logger_weak = logger_token;
  {
    grpc_lite::ChannelOptions channel_options;
    channel_options.logger =
        [token = std::move(logger_token), &log_count](
            grpc_lite::LogLevel, std::string_view) {
          assert(*token == 2);
          log_count.fetch_add(1, std::memory_order_relaxed);
        };
    Channel channel;
    assert(Channel::CreateManaged(
               &runtime, "127.0.0.1:" + std::to_string(port),
               std::move(channel_options), &channel)
               .ok());
    channel_options.logger = {};
    assert(!logger_weak.expired());

    Metadata request_metadata;
    assert(Metadata::Create(&request_metadata).ok());
    assert(request_metadata.Add("x-client", "first").ok());
    assert(request_metadata.Add("x-client-bin", std::string("\0x", 2)).ok());
    grpc_lite::ClientStreamOptions stream_options;
    stream_options.metadata = &request_metadata;
    grpc_lite::ClientStreamCallbacks client_callbacks;
    client_callbacks.on_headers = [&state](grpc_lite::MetadataEntries entries) {
      std::lock_guard<std::mutex> lock(state.mutex);
      state.headers = std::move(entries);
    };
    client_callbacks.on_message =
        [&state](std::string payload, grpc_lite::Compression compression) {
          std::lock_guard<std::mutex> lock(state.mutex);
          state.response = std::move(payload);
          assert(compression == grpc_lite::Compression::Identity);
          return grpc_lite::ReceiveAction::Continue;
        };
    client_callbacks.on_terminal =
        [&state](grpc_lite::Status status,
                 grpc_lite::MetadataEntries trailers) {
          std::lock_guard<std::mutex> lock(state.mutex);
          state.client_ok = status.ok();
          state.trailers = std::move(trailers);
          state.done = true;
          state.completed.notify_one();
        };

    ClientStream stream;
    assert(ClientStream::Open(channel, "/test.Native/Echo", stream_options,
                              std::move(client_callbacks), &stream)
               .ok());
    assert(stream.Send(std::string("request\0bytes", 13)).ok());
    assert(stream.CloseSend().ok());

    std::unique_lock<std::mutex> lock(state.mutex);
    assert(state.completed.wait_for(lock, std::chrono::seconds(5),
                                    [&state] { return state.done; }));
    lock.unlock();
    channel.Shutdown();
    channel.Wait();
  }
  assert(logger_weak.expired());

  server.ShutdownGracefully(UINT64_C(1000000000));
  server.Wait();
  assert(state.client_ok);
  assert(state.server_context_ok);
  assert(state.server_terminal);
  assert(state.started_call_id != 0);
  assert(state.terminal_call_id == state.started_call_id);
  assert(state.headers.size() == 2);
  assert(state.headers[0].second == "one");
  assert(state.headers[1].second == "two");
  assert(state.response == std::string("response\0bytes", 14));
  assert(state.trailers.size() == 1);
  assert(state.trailers[0].first == "trace-bin");
  assert(state.trailers[0].second == std::string("\0\1", 2));
  assert(log_count.load(std::memory_order_relaxed) > 0);
}

}  // namespace

int main() {
  TestMetadata();
  TestInvalidObjects();
  TestRoundTrip();
  return 0;
}
