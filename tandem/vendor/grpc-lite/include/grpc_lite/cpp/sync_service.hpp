#ifndef GRPC_LITE_CPP_SYNC_SERVICE_HPP
#define GRPC_LITE_CPP_SYNC_SERVICE_HPP

#include <grpc_lite/cpp/typed_service.hpp>
#include <grpcpp/server_context.h>
#include <grpcpp/support/status.h>
#include <grpcpp/support/sync_stream.h>

#include <atomic>
#include <cstdint>
#include <functional>
#include <memory>
#include <string_view>
#include <utility>

namespace grpc_lite {

class ServerExecutor {
 public:
  using Task = std::function<void()>;

  virtual ~ServerExecutor() = default;
  virtual bool Submit(std::string_view method, Task task) noexcept = 0;
};

class ServerAdmission {
 public:
  virtual ~ServerAdmission() = default;
  virtual Status Admit(std::string_view method,
                       const ServerContext& context) noexcept = 0;
};

struct SynchronousServiceOptions {
  Compression response_compression = Compression::Identity;
  ServerAdmission* admission = nullptr;
};

namespace internal {

class CallAdmissionGate {
 public:
  bool BeginStart() noexcept {
    State expected = State::Queued;
    return state_.compare_exchange_strong(expected, State::Starting,
                                          std::memory_order_acq_rel,
                                          std::memory_order_acquire);
  }

  bool CommitStart() noexcept {
    State expected = State::Starting;
    return state_.compare_exchange_strong(expected, State::Running,
                                          std::memory_order_acq_rel,
                                          std::memory_order_acquire);
  }

  void Stop() noexcept {
    State state = state_.load(std::memory_order_acquire);
    while (state == State::Queued || state == State::Starting) {
      if (state_.compare_exchange_weak(state, State::Stopped,
                                       std::memory_order_acq_rel,
                                       std::memory_order_acquire)) {
        return;
      }
    }
  }

 private:
  enum class State : std::uint8_t { Queued, Starting, Running, Stopped };
  std::atomic<State> state_{State::Queued};
};

inline grpc_compression_algorithm GrpcCompression(Compression compression) {
  return compression == Compression::Gzip ? GRPC_COMPRESS_GZIP
                                          : GRPC_COMPRESS_NONE;
}

inline Compression NativeCompression(grpc_compression_algorithm compression) {
  return compression == GRPC_COMPRESS_GZIP ? Compression::Gzip
                                           : Compression::Identity;
}

inline Status NativeStatus(const grpc::Status& status) {
  return {static_cast<StatusCode>(status.error_code()), status.error_message()};
}

inline Error CopyMetadata(const grpc::ServerContext::MetadataMap& entries,
                          Metadata* metadata) {
  Error error = Metadata::Create(metadata);
  if (!error.ok()) return error;
  for (const auto& entry : entries) {
    error = metadata->Add(entry.first, entry.second);
    if (!error.ok()) return error;
  }
  return {};
}

template <class Call>
Error SendInitialMetadata(Call& call, const grpc::ServerContext& context) {
  Metadata metadata;
  const Error error = CopyMetadata(
      grpc::internal::ServerContextAccess::InitialMetadata(context), &metadata);
  if (!error.ok()) return error;
  return call.SendInitialMetadata(
      &metadata, NativeCompression(
                     grpc::internal::ServerContextAccess::Compression(context)));
}

template <class Call>
Error Finish(Call& call, const grpc::ServerContext& context,
             const grpc::Status& status) {
  Metadata trailing;
  const Error error = CopyMetadata(
      grpc::internal::ServerContextAccess::TrailingMetadata(context), &trailing);
  if (!error.ok()) {
    return call.Finish({StatusCode::Internal, "failed to copy metadata"});
  }
  return call.Finish(NativeStatus(status), &trailing);
}

template <class Request, class Response, class Handler>
void DispatchUnary(ServerExecutor& executor, std::string_view method,
                   SynchronousServiceOptions options,
                   const ServerContext& native_context, Request request,
                   UnaryCall<Response> call, Handler handler) noexcept {
  if (options.admission) {
    Status status = options.admission->Admit(method, native_context);
    if (!status.ok()) {
      call.Finish(std::move(status));
      return;
    }
  }
  struct State {
    State(const ServerContext& native_context, Request request,
          UnaryCall<Response> call, Handler handler,
          SynchronousServiceOptions options)
        : call(std::move(call)),
          context(grpc::internal::ServerContextAccess::Create(
              native_context, [this] { return this->call.cancelled(); },
              GrpcCompression(options.response_compression))),
          request(std::move(request)),
          handler(std::move(handler)) {}

    void Run() {
      if (!admission.BeginStart() || !admission.CommitStart()) return;
      Response response;
      const grpc::Status status = handler(&context, &request, &response);
      if (call.cancelled()) return;
      const Error metadata_error = SendInitialMetadata(call, context);
      if (!metadata_error.ok()) {
        call.Finish({StatusCode::Internal, "failed to send initial metadata"});
        return;
      }
      if (status.ok()) {
        Metadata trailing;
        const Error trailing_error = CopyMetadata(
            grpc::internal::ServerContextAccess::TrailingMetadata(context),
            &trailing);
        if (!trailing_error.ok()) {
          call.Finish({StatusCode::Internal, "failed to copy metadata"});
          return;
        }
        call.Finish(std::move(response), {}, &trailing,
                    NativeCompression(
                        grpc::internal::ServerContextAccess::Compression(
                            context)));
      } else {
        Finish(call, context, status);
      }
    }

    UnaryCall<Response> call;
    grpc::ServerContext context;
    Request request;
    Handler handler;
    CallAdmissionGate admission;
  };

  auto state = std::make_shared<State>(native_context, std::move(request),
                                       std::move(call), std::move(handler), options);
  const std::weak_ptr<State> observer = state;
  state->call.SetOnCancel([observer] {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  state->call.SetOnTerminal([observer](ServerTerminalReason) {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  if (!executor.Submit(method, [state] { state->Run(); })) {
    state->admission.Stop();
    state->call.Finish(
        {StatusCode::ResourceExhausted, "executor rejected the call"});
  }
}

template <class Request, class Response, class Handler>
void DispatchServerStreaming(ServerExecutor& executor, std::string_view method,
                             SynchronousServiceOptions options,
                              const ServerContext& native_context, Request request,
                              ServerStreamingCall<Response> call,
                              Handler handler) noexcept {
  if (options.admission) {
    Status status = options.admission->Admit(method, native_context);
    if (!status.ok()) {
      call.Finish(std::move(status));
      return;
    }
  }
  struct State {
    State(const ServerContext& native_context, Request request,
          ServerStreamingCall<Response> call, Handler handler,
          SynchronousServiceOptions options)
        : call(std::move(call)),
          context(grpc::internal::ServerContextAccess::Create(
              native_context, [this] { return this->call.cancelled(); },
              GrpcCompression(options.response_compression))),
          request(std::move(request)),
          handler(std::move(handler)) {}

    Error TryWrite(const Response& response) {
      if (!initial_metadata_sent) {
        const Error error = SendInitialMetadata(call, context);
        if (!error.ok()) return error;
        initial_metadata_sent = true;
      }
      return call.TryWrite(
          response, NativeCompression(
                        grpc::internal::ServerContextAccess::Compression(context)));
    }

    void Run() {
      if (!admission.BeginStart() || !admission.CommitStart()) return;
      auto writer = grpc::internal::ServerWriterAccess<Response>::Create(
          &context, [this](const Response& response) { return TryWrite(response); },
          [this] { return call.WaitForWritable(); });
      const grpc::Status status = handler(&context, &request, &writer);
      if (call.cancelled()) return;
      if (!initial_metadata_sent) {
        const Error error = SendInitialMetadata(call, context);
        if (!error.ok()) {
          call.Finish(
              {StatusCode::Internal, "failed to send initial metadata"});
          return;
        }
        initial_metadata_sent = true;
      }
      Finish(call, context, status);
    }

    ServerStreamingCall<Response> call;
    grpc::ServerContext context;
    Request request;
    Handler handler;
    bool initial_metadata_sent = false;
    CallAdmissionGate admission;
  };

  auto state = std::make_shared<State>(native_context, std::move(request),
                                       std::move(call), std::move(handler), options);
  const std::weak_ptr<State> observer = state;
  state->call.SetOnCancel([observer] {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  state->call.SetOnTerminal([observer](ServerTerminalReason) {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  if (!executor.Submit(method, [state] { state->Run(); })) {
    state->admission.Stop();
    state->call.Finish(
        {StatusCode::ResourceExhausted, "executor rejected the call"});
  }
}

template <class Request, class Response, class Handler>
void DispatchClientStreaming(ServerExecutor& executor, std::string_view method,
                             SynchronousServiceOptions options,
                             const ServerContext& native_context,
                             ClientStreamingCall<Request, Response> call,
                             Handler handler) noexcept {
  if (options.admission) {
    Status status = options.admission->Admit(method, native_context);
    if (!status.ok()) {
      call.Finish(std::move(status));
      return;
    }
  }
  struct State {
    State(const ServerContext& native_context,
          ClientStreamingCall<Request, Response> call, Handler handler,
          SynchronousServiceOptions options)
        : call(std::move(call)),
          context(grpc::internal::ServerContextAccess::Create(
              native_context, [this] { return this->call.cancelled(); },
              GrpcCompression(options.response_compression))),
          handler(std::move(handler)) {}

    void Run() {
      if (!admission.BeginStart() || !admission.CommitStart()) return;
      auto reader = grpc::internal::ServerReaderAccess<Request>::Create(
          &context, [this](Request* request) { return call.Read(request); });
      Response response;
      const grpc::Status status = handler(&context, &reader, &response);
      if (call.cancelled() || call.terminal()) return;
      const Error metadata_error = SendInitialMetadata(call, context);
      if (!metadata_error.ok()) {
        call.Finish({StatusCode::Internal,
                     "failed to send initial metadata"});
        return;
      }
      if (status.ok()) {
        Metadata trailing;
        const Error trailing_error = CopyMetadata(
            grpc::internal::ServerContextAccess::TrailingMetadata(context),
            &trailing);
        if (!trailing_error.ok()) {
          call.Finish({StatusCode::Internal, "failed to copy metadata"});
          return;
        }
        call.Finish(std::move(response), {}, &trailing,
                    NativeCompression(
                        grpc::internal::ServerContextAccess::Compression(
                            context)));
      } else {
        Finish(call, context, status);
      }
    }

    ClientStreamingCall<Request, Response> call;
    grpc::ServerContext context;
    Handler handler;
    CallAdmissionGate admission;
  };

  auto state = std::make_shared<State>(native_context, std::move(call),
                                       std::move(handler), options);
  const std::weak_ptr<State> observer = state;
  state->call.SetOnCancel([observer] {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  state->call.SetOnTerminal([observer](ServerTerminalReason) {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  if (!executor.Submit(method, [state] { state->Run(); })) {
    state->admission.Stop();
    state->call.Finish(
        {StatusCode::ResourceExhausted, "executor rejected the call"});
  }
}

template <class Request, class Response, class Handler>
void DispatchBidirectionalStreaming(
    ServerExecutor& executor, std::string_view method,
    SynchronousServiceOptions options, const ServerContext& native_context,
    BidirectionalStreamingCall<Request, Response> call,
    Handler handler) noexcept {
  if (options.admission) {
    Status status = options.admission->Admit(method, native_context);
    if (!status.ok()) {
      call.Finish(std::move(status));
      return;
    }
  }
  struct State {
    State(const ServerContext& native_context,
          BidirectionalStreamingCall<Request, Response> call, Handler handler,
          SynchronousServiceOptions options)
        : call(std::move(call)),
          context(grpc::internal::ServerContextAccess::Create(
              native_context, [this] { return this->call.cancelled(); },
              GrpcCompression(options.response_compression))),
          handler(std::move(handler)) {}

    Error TryWrite(const Response& response) {
      if (!initial_metadata_sent) {
        const Error error = SendInitialMetadata(call, context);
        if (!error.ok()) return error;
        initial_metadata_sent = true;
      }
      return call.TryWrite(
          response, NativeCompression(
                        grpc::internal::ServerContextAccess::Compression(
                            context)));
    }

    void Run() {
      if (!admission.BeginStart() || !admission.CommitStart()) return;
      auto stream =
          grpc::internal::ServerReaderWriterAccess<Response, Request>::Create(
              &context,
              [this](Request* request) { return call.Read(request); },
              [this](const Response& response) { return TryWrite(response); },
              [this] { return call.WaitForWritable(); });
      const grpc::Status status = handler(&context, &stream);
      if (call.cancelled() || call.terminal()) return;
      if (!initial_metadata_sent) {
        const Error error = SendInitialMetadata(call, context);
        if (!error.ok()) {
          call.Finish({StatusCode::Internal,
                       "failed to send initial metadata"});
          return;
        }
        initial_metadata_sent = true;
      }
      Finish(call, context, status);
    }

    BidirectionalStreamingCall<Request, Response> call;
    grpc::ServerContext context;
    Handler handler;
    bool initial_metadata_sent = false;
    CallAdmissionGate admission;
  };

  auto state = std::make_shared<State>(native_context, std::move(call),
                                       std::move(handler), options);
  const std::weak_ptr<State> observer = state;
  state->call.SetOnCancel([observer] {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  state->call.SetOnTerminal([observer](ServerTerminalReason) {
    if (const auto state = observer.lock()) state->admission.Stop();
  });
  if (!executor.Submit(method, [state] { state->Run(); })) {
    state->admission.Stop();
    state->call.Finish(
        {StatusCode::ResourceExhausted, "executor rejected the call"});
  }
}

}  // namespace internal
}  // namespace grpc_lite

#endif
