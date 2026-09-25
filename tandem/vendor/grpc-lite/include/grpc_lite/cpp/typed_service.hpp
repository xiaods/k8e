#ifndef GRPC_LITE_CPP_TYPED_SERVICE_HPP
#define GRPC_LITE_CPP_TYPED_SERVICE_HPP

#include <grpc_lite/cpp/server.hpp>

#include <condition_variable>
#include <cstddef>
#include <functional>
#include <limits>
#include <memory>
#include <mutex>
#include <optional>
#include <string>
#include <type_traits>
#include <unordered_map>
#include <utility>

namespace grpc_lite {

namespace internal {

class TypedCallState {
 public:
  explicit TypedCallState(ServerCall call) : call_(std::move(call)) {}

  bool cancelled() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return cancelled_ || call_.cancelled();
  }
  bool terminal() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return finish_submitted_ || terminal_ || call_.terminal();
  }
  std::size_t id() const {
    std::lock_guard<std::mutex> lock(mutex_);
    return call_.id();
  }

  Error SendInitialMetadata(const Metadata* metadata, Compression compression) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (terminal_ || finish_submitted_) return Error(ErrorCode::Closed);
    if (initial_metadata_sent_) return Error(ErrorCode::InvalidState);
    const Error error = call_.SendInitialMetadata(metadata, compression);
    if (error.ok()) initial_metadata_sent_ = true;
    return error;
  }

  Error Send(std::string payload, Compression compression) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (terminal_ || finish_submitted_) return Error(ErrorCode::Closed);
    const Error metadata_error = EnsureInitialMetadata(compression);
    if (!metadata_error.ok()) return metadata_error;
    const Error error = call_.Send(payload, compression);
    if (error.code() == ErrorCode::WouldBlock) writable_ = false;
    return error;
  }

  Error Finish(Status status, const Metadata* trailing_metadata) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (terminal_ || finish_submitted_) return Error(ErrorCode::Closed);
    const Error metadata_error = EnsureInitialMetadata(Compression::Identity);
    if (!metadata_error.ok()) return metadata_error;
    const Error error = call_.Finish(std::move(status), trailing_metadata);
    if (error.ok()) finish_submitted_ = true;
    return error;
  }

  Error ResumeReceive() {
    std::lock_guard<std::mutex> lock(mutex_);
    if (terminal_ || finish_submitted_) return Error(ErrorCode::Closed);
    return call_.ResumeReceive();
  }

  void SetOnWritable(std::function<void()> callback) {
    bool invoke = false;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      invoke = writable_ && !cancelled_ && !terminal_;
      if (!terminal_) on_writable_ = callback;
    }
    if (invoke && callback) callback();
  }
  void SetOnCancel(std::function<void()> callback) {
    bool invoke = false;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      invoke = cancelled_;
      if (!invoke && !terminal_) on_cancel_ = callback;
    }
    if (invoke && callback) callback();
  }
  void SetOnTerminal(std::function<void(ServerTerminalReason)> callback) {
    std::optional<ServerTerminalReason> reason;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      reason = terminal_reason_;
      if (!reason) on_terminal_ = callback;
    }
    if (reason && callback) callback(*reason);
  }

  bool WaitForWritable() {
    std::unique_lock<std::mutex> lock(mutex_);
    writable_cv_.wait(lock,
                      [this] { return writable_ || cancelled_ || terminal_; });
    return writable_ && !cancelled_ && !terminal_;
  }

  void OnWritable() {
    std::function<void()> callback;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      writable_ = true;
      callback = on_writable_;
    }
    writable_cv_.notify_all();
    if (callback) callback();
  }

  void OnCancel() {
    std::function<void()> callback;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      cancelled_ = true;
      callback = on_cancel_;
    }
    writable_cv_.notify_all();
    if (callback) callback();
  }

  void OnTerminal(ServerTerminalReason reason) {
    std::function<void(ServerTerminalReason)> callback;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      terminal_ = true;
      terminal_reason_ = reason;
      call_ = {};
      callback = on_terminal_;
      on_writable_ = {};
      on_cancel_ = {};
      on_terminal_ = {};
    }
    writable_cv_.notify_all();
    if (callback) callback(reason);
  }

 private:
  Error EnsureInitialMetadata(Compression compression) {
    if (initial_metadata_sent_) return {};
    const Error error = call_.SendInitialMetadata(nullptr, compression);
    if (error.ok()) initial_metadata_sent_ = true;
    return error;
  }

  mutable std::mutex mutex_;
  std::condition_variable writable_cv_;
  ServerCall call_;
  std::function<void()> on_writable_;
  std::function<void()> on_cancel_;
  std::function<void(ServerTerminalReason)> on_terminal_;
  bool writable_ = true;
  bool cancelled_ = false;
  bool terminal_ = false;
  bool finish_submitted_ = false;
  bool initial_metadata_sent_ = false;
  std::optional<ServerTerminalReason> terminal_reason_;
};

inline Status SendErrorStatus(Error error) {
  if (error.code() == ErrorCode::WouldBlock ||
      error.code() == ErrorCode::OutOfMemory ||
      error.code() == ErrorCode::OutOfRange) {
    return {StatusCode::ResourceExhausted, std::string(error.message())};
  }
  return {StatusCode::Internal, std::string(error.message())};
}

template <class Response>
Error FinishResponse(const std::shared_ptr<TypedCallState>& state,
                     Response response, Status status,
                     const Metadata* trailing_metadata,
                     Compression compression) {
  if (!status.ok()) return state->Finish(std::move(status), trailing_metadata);
  std::string payload;
  if (!response.SerializeToString(&payload)) {
    return state->Finish({StatusCode::Internal, "failed to serialize response"},
                         trailing_metadata);
  }
  const Error send_error = state->Send(std::move(payload), compression);
  if (!send_error.ok()) {
    const Error finish_error =
        state->Finish(SendErrorStatus(send_error), trailing_metadata);
    return finish_error.ok() ? send_error : finish_error;
  }
  return state->Finish({}, trailing_metadata);
}

template <class Request>
class TypedRequestStreamState {
 public:
  explicit TypedRequestStreamState(std::shared_ptr<TypedCallState> call)
      : call_(std::move(call)) {}

  ReceiveAction OnMessage(std::string payload) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (rejected_ || remote_end_ || terminal_) return ReceiveAction::Pause;
    if (message_) {
      rejected_ = true;
      call_->Finish({StatusCode::Internal, "request backpressure violated"},
                    nullptr);
      changed_.notify_all();
      return ReceiveAction::Pause;
    }
    if (payload.size() >
        static_cast<std::size_t>(std::numeric_limits<int>::max())) {
      rejected_ = true;
      call_->Finish(
          {StatusCode::ResourceExhausted, "request is too large to parse"},
          nullptr);
      changed_.notify_all();
      return ReceiveAction::Pause;
    }
    Request parsed;
    if (!parsed.ParseFromArray(payload.data(), static_cast<int>(payload.size()))) {
      rejected_ = true;
      call_->Finish({StatusCode::InvalidArgument, "failed to parse request"},
                    nullptr);
      changed_.notify_all();
      return ReceiveAction::Pause;
    }
    message_.emplace(std::move(parsed));
    changed_.notify_all();
    return ReceiveAction::Pause;
  }

  void OnRemoteEnd() {
    std::lock_guard<std::mutex> lock(mutex_);
    remote_end_ = true;
    changed_.notify_all();
  }

  void OnCancel() {
    std::lock_guard<std::mutex> lock(mutex_);
    cancelled_ = true;
    changed_.notify_all();
  }

  void OnTerminal() {
    std::lock_guard<std::mutex> lock(mutex_);
    terminal_ = true;
    changed_.notify_all();
  }

  // This helper is for application worker threads, never reactor callbacks.
  bool Read(Request* request) {
    if (request == nullptr) return false;
    {
      std::unique_lock<std::mutex> lock(mutex_);
      changed_.wait(lock, [this] {
        return message_.has_value() || remote_end_ || rejected_ || cancelled_ ||
               terminal_;
      });
      if (!message_) return false;
      *request = std::move(*message_);
      message_.reset();
    }
    const Error resume_error = call_->ResumeReceive();
    if (!resume_error.ok() && resume_error.code() != ErrorCode::Closed &&
        resume_error.code() != ErrorCode::Unavailable) {
      call_->Finish({StatusCode::Internal, "failed to resume request stream"},
                    nullptr);
    }
    return true;
  }

  const std::shared_ptr<TypedCallState>& call() const { return call_; }

 private:
  std::shared_ptr<TypedCallState> call_;
  std::mutex mutex_;
  std::condition_variable changed_;
  std::optional<Request> message_;
  bool remote_end_ = false;
  bool rejected_ = false;
  bool cancelled_ = false;
  bool terminal_ = false;
};

}  // namespace internal

template <class Response>
class UnaryCall {
 public:
  UnaryCall() = default;
  explicit UnaryCall(std::shared_ptr<internal::TypedCallState> state)
      : state_(std::move(state)) {}
  UnaryCall(const UnaryCall&) = delete;
  UnaryCall& operator=(const UnaryCall&) = delete;
  UnaryCall(UnaryCall&&) noexcept = default;
  UnaryCall& operator=(UnaryCall&&) noexcept = default;

  bool cancelled() const { return state_ && state_->cancelled(); }
  bool terminal() const { return !state_ || state_->terminal(); }
  Error SendInitialMetadata(
      const Metadata* metadata = nullptr,
      Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->SendInitialMetadata(metadata, compression);
  }
  Error Finish(Response response, Status status = {},
               const Metadata* trailing_metadata = nullptr,
               Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return internal::FinishResponse(state_, std::move(response),
                                    std::move(status), trailing_metadata,
                                    compression);
  }
  Error Finish(Status status, const Metadata* trailing_metadata = nullptr) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->Finish(std::move(status), trailing_metadata);
  }
  void SetOnCancel(std::function<void()> callback) {
    if (state_) state_->SetOnCancel(std::move(callback));
  }
  void SetOnTerminal(std::function<void(ServerTerminalReason)> callback) {
    if (state_) state_->SetOnTerminal(std::move(callback));
  }

 private:
  std::shared_ptr<internal::TypedCallState> state_;
};

template <class Response>
class ServerStreamingCall {
 public:
  ServerStreamingCall() = default;
  explicit ServerStreamingCall(std::shared_ptr<internal::TypedCallState> state)
      : state_(std::move(state)) {}
  ServerStreamingCall(const ServerStreamingCall&) = delete;
  ServerStreamingCall& operator=(const ServerStreamingCall&) = delete;
  ServerStreamingCall(ServerStreamingCall&&) noexcept = default;
  ServerStreamingCall& operator=(ServerStreamingCall&&) noexcept = default;

  bool cancelled() const { return state_ && state_->cancelled(); }
  bool terminal() const { return !state_ || state_->terminal(); }
  Error SendInitialMetadata(
      const Metadata* metadata = nullptr,
      Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->SendInitialMetadata(metadata, compression);
  }
  Error TryWrite(const Response& response,
                 Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    std::string payload;
    if (!response.SerializeToString(&payload)) {
      const Error finish_error = state_->Finish(
          {StatusCode::Internal, "failed to serialize response"}, nullptr);
      return finish_error.ok() ? Error(ErrorCode::Internal) : finish_error;
    }
    return state_->Send(std::move(payload), compression);
  }
  // This helper is for application worker threads, never reactor callbacks.
  bool WaitForWritable() { return state_ && state_->WaitForWritable(); }
  Error Finish(Status status = {},
               const Metadata* trailing_metadata = nullptr) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->Finish(std::move(status), trailing_metadata);
  }
  void SetOnWritable(std::function<void()> callback) {
    if (state_) state_->SetOnWritable(std::move(callback));
  }
  void SetOnCancel(std::function<void()> callback) {
    if (state_) state_->SetOnCancel(std::move(callback));
  }
  void SetOnTerminal(std::function<void(ServerTerminalReason)> callback) {
    if (state_) state_->SetOnTerminal(std::move(callback));
  }

 private:
  std::shared_ptr<internal::TypedCallState> state_;
};

template <class Request, class Response>
class ClientStreamingCall {
 public:
  ClientStreamingCall() = default;
  explicit ClientStreamingCall(
      std::shared_ptr<internal::TypedRequestStreamState<Request>> state)
      : state_(std::move(state)) {}
  ClientStreamingCall(const ClientStreamingCall&) = delete;
  ClientStreamingCall& operator=(const ClientStreamingCall&) = delete;
  ClientStreamingCall(ClientStreamingCall&&) noexcept = default;
  ClientStreamingCall& operator=(ClientStreamingCall&&) noexcept = default;

  bool cancelled() const { return state_ && state_->call()->cancelled(); }
  bool terminal() const { return !state_ || state_->call()->terminal(); }
  bool Read(Request* request) { return state_ && state_->Read(request); }
  Error SendInitialMetadata(
      const Metadata* metadata = nullptr,
      Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->call()->SendInitialMetadata(metadata, compression);
  }
  Error Finish(Response response, Status status = {},
               const Metadata* trailing_metadata = nullptr,
               Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return internal::FinishResponse(state_->call(), std::move(response),
                                    std::move(status), trailing_metadata,
                                    compression);
  }
  Error Finish(Status status, const Metadata* trailing_metadata = nullptr) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->call()->Finish(std::move(status), trailing_metadata);
  }
  void SetOnCancel(std::function<void()> callback) {
    if (state_) state_->call()->SetOnCancel(std::move(callback));
  }
  void SetOnTerminal(std::function<void(ServerTerminalReason)> callback) {
    if (state_) state_->call()->SetOnTerminal(std::move(callback));
  }

 private:
  std::shared_ptr<internal::TypedRequestStreamState<Request>> state_;
};

template <class Request, class Response>
class BidirectionalStreamingCall {
 public:
  BidirectionalStreamingCall() = default;
  explicit BidirectionalStreamingCall(
      std::shared_ptr<internal::TypedRequestStreamState<Request>> state)
      : state_(std::move(state)) {}
  BidirectionalStreamingCall(const BidirectionalStreamingCall&) = delete;
  BidirectionalStreamingCall& operator=(const BidirectionalStreamingCall&) =
      delete;
  BidirectionalStreamingCall(BidirectionalStreamingCall&&) noexcept = default;
  BidirectionalStreamingCall& operator=(BidirectionalStreamingCall&&) noexcept =
      default;

  bool cancelled() const { return state_ && state_->call()->cancelled(); }
  bool terminal() const { return !state_ || state_->call()->terminal(); }
  bool Read(Request* request) { return state_ && state_->Read(request); }
  Error SendInitialMetadata(
      const Metadata* metadata = nullptr,
      Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->call()->SendInitialMetadata(metadata, compression);
  }
  Error TryWrite(const Response& response,
                 Compression compression = Compression::Identity) {
    if (!state_) return Error(ErrorCode::InvalidState);
    std::string payload;
    if (!response.SerializeToString(&payload)) {
      const Error finish_error = state_->call()->Finish(
          {StatusCode::Internal, "failed to serialize response"}, nullptr);
      return finish_error.ok() ? Error(ErrorCode::Internal) : finish_error;
    }
    return state_->call()->Send(std::move(payload), compression);
  }
  bool WaitForWritable() {
    return state_ && state_->call()->WaitForWritable();
  }
  Error Finish(Status status = {},
               const Metadata* trailing_metadata = nullptr) {
    if (!state_) return Error(ErrorCode::InvalidState);
    return state_->call()->Finish(std::move(status), trailing_metadata);
  }
  void SetOnWritable(std::function<void()> callback) {
    if (state_) state_->call()->SetOnWritable(std::move(callback));
  }
  void SetOnCancel(std::function<void()> callback) {
    if (state_) state_->call()->SetOnCancel(std::move(callback));
  }
  void SetOnTerminal(std::function<void(ServerTerminalReason)> callback) {
    if (state_) state_->call()->SetOnTerminal(std::move(callback));
  }

 private:
  std::shared_ptr<internal::TypedRequestStreamState<Request>> state_;
};

namespace internal {

template <class Request, class Response, bool ClientStreaming,
          bool ServerStreaming>
class TypedMethodRegistration {
 public:
  using Call = std::conditional_t<
      ClientStreaming,
      std::conditional_t<ServerStreaming,
                         BidirectionalStreamingCall<Request, Response>,
                         ClientStreamingCall<Request, Response>>,
      std::conditional_t<ServerStreaming, ServerStreamingCall<Response>,
                         UnaryCall<Response>>>;
  using Handler = std::conditional_t<
      ClientStreaming, std::function<void(const ServerContext&, Call)>,
      std::function<void(const ServerContext&, Request, Call)>>;

  TypedMethodRegistration() : impl_(std::make_shared<Impl>()) {}
  ~TypedMethodRegistration() { impl_->Disable(); }
  TypedMethodRegistration(const TypedMethodRegistration&) = delete;
  TypedMethodRegistration& operator=(const TypedMethodRegistration&) = delete;

  Error Register(Server& server, std::string method, Handler handler) {
    return impl_->Register(server, std::move(method), std::move(handler));
  }

 private:
  struct CallState {
    CallState(std::shared_ptr<TypedCallState> call,
              const ServerContext& server_context)
        : typed_call(std::move(call)),
          context(server_context),
          call_id(typed_call->id()) {
      if constexpr (ClientStreaming) {
        request_stream =
            std::make_shared<TypedRequestStreamState<Request>>(typed_call);
      }
    }

    ReceiveAction OnMessage(std::string payload) {
      if constexpr (ClientStreaming) {
        return request_stream->OnMessage(std::move(payload));
      } else {
        std::lock_guard<std::mutex> lock(mutex);
        if (rejected || dispatched) return ReceiveAction::Continue;
        if (request_count != 0) {
          rejected = true;
          typed_call->Finish(
              {StatusCode::InvalidArgument, "duplicate request message"},
              nullptr);
          return ReceiveAction::Continue;
        }
        request_count = 1;
        if (payload.size() > static_cast<std::size_t>(
                                 std::numeric_limits<int>::max())) {
          rejected = true;
          typed_call->Finish(
              {StatusCode::ResourceExhausted,
               "request is too large to parse"},
              nullptr);
          return ReceiveAction::Continue;
        }
        Request parsed;
        if (!parsed.ParseFromArray(payload.data(),
                                   static_cast<int>(payload.size()))) {
          rejected = true;
          typed_call->Finish(
              {StatusCode::InvalidArgument, "failed to parse request"},
              nullptr);
          return ReceiveAction::Continue;
        }
        request.emplace(std::move(parsed));
        return ReceiveAction::Continue;
      }
    }

    std::optional<Request> TakeRequest() {
      std::lock_guard<std::mutex> lock(mutex);
      if (rejected || dispatched) return std::nullopt;
      if (request_count != 1 || !request) {
        rejected = true;
        typed_call->Finish(
            {StatusCode::InvalidArgument, "request message is missing"},
            nullptr);
        return std::nullopt;
      }
      dispatched = true;
      return std::move(request);
    }

    std::mutex mutex;
    std::shared_ptr<TypedCallState> typed_call;
    std::shared_ptr<TypedRequestStreamState<Request>> request_stream;
    ServerContext context;
    std::size_t call_id;
    std::optional<Request> request;
    std::size_t request_count = 0;
    bool rejected = false;
    bool dispatched = false;
  };

  struct Impl : std::enable_shared_from_this<Impl> {
    Error Register(Server& server, std::string method, Handler handler) {
      {
        std::lock_guard<std::mutex> lock(mutex);
        if (registered || registering) return Error(ErrorCode::InvalidState);
        registering = true;
        application_handler = std::move(handler);
      }

      const std::shared_ptr<Impl> self = this->shared_from_this();
      ServerStreamCallbacks callbacks;
      callbacks.on_start = [self](ServerStream& stream,
                                  const ServerContext& context) {
        ServerCall call;
        if (!stream.Retain(&call).ok()) return;
        auto state = std::make_shared<CallState>(
            std::make_shared<TypedCallState>(std::move(call)), context);
        {
          std::lock_guard<std::mutex> lock(self->mutex);
          self->calls[state->call_id] = state;
        }
        if constexpr (ClientStreaming) {
          Handler handler;
          {
            std::lock_guard<std::mutex> lock(self->mutex);
            handler = self->application_handler;
          }
          if (handler) {
            handler(state->context, Call(state->request_stream));
          } else {
            state->typed_call->Finish(
                {StatusCode::Unavailable, "event service is unavailable"},
                nullptr);
          }
        }
      };
      callbacks.on_message =
          [self](ServerStream& stream, const ServerContext&,
                 std::string payload, Compression) {
            const auto state = self->Find(stream);
            return state ? state->OnMessage(std::move(payload))
                         : ReceiveAction::Pause;
          };
      callbacks.on_remote_end = [self](ServerStream& stream,
                                       const ServerContext&) {
        const auto state = self->Find(stream);
        if (!state) return;
        if constexpr (ClientStreaming) {
          state->request_stream->OnRemoteEnd();
        } else {
          std::optional<Request> request = state->TakeRequest();
          if (!request) return;
          Handler handler;
          {
            std::lock_guard<std::mutex> lock(self->mutex);
            handler = self->application_handler;
          }
          if (handler) {
            handler(state->context, std::move(*request),
                    Call(state->typed_call));
          } else {
            state->typed_call->Finish(
                {StatusCode::Unavailable, "event service is unavailable"},
                nullptr);
          }
        }
      };
      callbacks.on_writable = [self](ServerStream& stream,
                                     const ServerContext&) {
        const auto state = self->Find(stream);
        if (state) state->typed_call->OnWritable();
      };
      callbacks.on_cancel = [self](ServerStream& stream,
                                   const ServerContext&) {
        const auto state = self->Find(stream);
        if (!state) return;
        if constexpr (ClientStreaming) state->request_stream->OnCancel();
        state->typed_call->OnCancel();
      };
      callbacks.on_terminal = [self](std::size_t call_id,
                                     ServerTerminalReason reason) {
        std::shared_ptr<CallState> state;
        {
          std::lock_guard<std::mutex> lock(self->mutex);
          const auto found = self->calls.find(call_id);
          if (found == self->calls.end()) return;
          state = std::move(found->second);
          self->calls.erase(found);
        }
        if constexpr (ClientStreaming) state->request_stream->OnTerminal();
        state->typed_call->OnTerminal(reason);
      };

      const Error error = server.RegisterStream(std::move(method), {},
                                                std::move(callbacks));
      std::lock_guard<std::mutex> lock(mutex);
      if (error.ok()) {
        registered = true;
      } else {
        application_handler = {};
      }
      registering = false;
      return error;
    }

    void Disable() {
      std::lock_guard<std::mutex> lock(mutex);
      application_handler = {};
    }

    std::shared_ptr<CallState> Find(ServerStream& stream) {
      ServerCall call;
      if (!stream.Retain(&call).ok()) return {};
      const std::size_t call_id = call.id();
      std::lock_guard<std::mutex> lock(mutex);
      const auto found = calls.find(call_id);
      return found == calls.end() ? nullptr : found->second;
    }

    std::mutex mutex;
    std::unordered_map<std::size_t, std::shared_ptr<CallState>> calls;
    Handler application_handler;
    bool registered = false;
    bool registering = false;
  };

  std::shared_ptr<Impl> impl_;
};

}  // namespace internal

}  // namespace grpc_lite

#endif
