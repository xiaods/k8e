#ifndef GRPCPP_IMPL_BLOCKING_CALL_H
#define GRPCPP_IMPL_BLOCKING_CALL_H

#include <grpc_lite/grpc_lite.hpp>
#include <grpcpp/client_context.h>
#include <grpcpp/support/status.h>

#include <chrono>
#include <condition_variable>
#include <cstdint>
#include <limits>
#include <memory>
#include <mutex>
#include <string>
#include <utility>

namespace grpc::internal {

inline Status ErrorStatus(grpc_lite::Error error) {
  StatusCode code;
  switch (error.code()) {
    case grpc_lite::ErrorCode::Ok:
      return {};
    case grpc_lite::ErrorCode::InvalidArgument:
      code = StatusCode::INVALID_ARGUMENT;
      break;
    case grpc_lite::ErrorCode::InvalidState:
      code = StatusCode::FAILED_PRECONDITION;
      break;
    case grpc_lite::ErrorCode::OutOfMemory:
      code = StatusCode::RESOURCE_EXHAUSTED;
      break;
    case grpc_lite::ErrorCode::Unsupported:
      code = StatusCode::UNIMPLEMENTED;
      break;
    case grpc_lite::ErrorCode::Unavailable:
    case grpc_lite::ErrorCode::Closed:
      code = StatusCode::UNAVAILABLE;
      break;
    case grpc_lite::ErrorCode::OutOfRange:
      code = StatusCode::OUT_OF_RANGE;
      break;
    case grpc_lite::ErrorCode::WouldBlock:
      code = StatusCode::INTERNAL;
      break;
    case grpc_lite::ErrorCode::Internal:
      code = StatusCode::INTERNAL;
      break;
    default:
      code = StatusCode::UNKNOWN;
      break;
  }
  return {code, std::string(error.message())};
}

inline Status NativeStatus(grpc_lite::Status status) {
  const auto value = static_cast<std::int32_t>(status.code());
  if (value < static_cast<std::int32_t>(StatusCode::OK) ||
      value > static_cast<std::int32_t>(StatusCode::UNAUTHENTICATED)) {
    return {StatusCode::UNKNOWN, status.message()};
  }
  return {static_cast<StatusCode>(value), status.message()};
}

class BlockingCallState final : public CancellationTarget,
                                public std::enable_shared_from_this<BlockingCallState> {
 public:
  static std::shared_ptr<BlockingCallState> Failed(ClientContext* context,
                                                   Status status) {
    auto state = std::shared_ptr<BlockingCallState>(
        new BlockingCallState(context, grpc_lite::Compression::Identity));
    state->SetTerminal(std::move(status), {});
    return state;
  }

  static std::shared_ptr<BlockingCallState> Open(
      grpc_lite::Channel& channel, const std::string& method,
      ClientContext* context, grpc_lite::Compression compression,
      std::shared_ptr<void> channel_owner) {
    auto state = std::shared_ptr<BlockingCallState>(
        new BlockingCallState(context, compression));
    state->channel_owner_ = std::move(channel_owner);
    if (!context->RegisterCancellation(state)) {
      state->SetTerminal(
          {StatusCode::CANCELLED, "call cancelled before start"}, {});
      return state;
    }

    grpc_lite::Metadata metadata;
    grpc_lite::Error error = grpc_lite::Metadata::Create(&metadata);
    if (!error.ok()) {
      state->SetTerminal(ErrorStatus(error), {});
      return state;
    }
    for (const auto& entry : context->metadata_) {
      error = metadata.Add(entry.first, entry.second);
      if (!error.ok()) {
        state->SetTerminal(ErrorStatus(error), {});
        return state;
      }
    }

    grpc_lite::ClientStreamOptions options;
    options.metadata = &metadata;
    options.send_compression = compression;
    if (context->has_deadline_) {
      const long double now = std::chrono::duration<long double>(
                                  std::chrono::system_clock::now()
                                      .time_since_epoch())
                                  .count();
      long double seconds = context->deadline_seconds_ - now;
      if (context->deadline_extreme_ < 0) {
        seconds = -1;
      } else if (context->deadline_extreme_ > 0) {
        seconds = std::numeric_limits<long double>::infinity();
      }
      const long double maximum =
          static_cast<long double>(std::numeric_limits<std::uint64_t>::max()) /
          1000000000.0L;
      options.has_timeout = true;
      if (seconds <= 0) {
        options.timeout_ns = 0;
      } else if (seconds >= maximum) {
        options.timeout_ns = std::numeric_limits<std::uint64_t>::max();
      } else {
        options.timeout_ns =
            static_cast<std::uint64_t>(seconds * 1000000000.0L);
      }
    }

    grpc_lite::ClientStreamCallbacks callbacks;
    callbacks.on_headers = [state](grpc_lite::MetadataEntries entries) noexcept {
      ClientContext::MetadataMap published;
      for (const auto& entry : entries) {
        published.emplace(entry.first, entry.second);
      }
      {
        std::lock_guard<std::mutex> lock(state->mutex_);
        state->initial_metadata_ = std::move(entries);
      }
      state->context_->SetInitialMetadata(std::move(published));
    };
    callbacks.on_message =
        [state](std::string payload, grpc_lite::Compression) noexcept {
          std::lock_guard<std::mutex> lock(state->mutex_);
          state->message_ = std::move(payload);
          state->has_message_ = true;
          state->changed_.notify_all();
          return grpc_lite::ReceiveAction::Pause;
        };
    callbacks.on_writable = [state]() noexcept {
      std::lock_guard<std::mutex> lock(state->mutex_);
      ++state->writable_generation_;
      state->changed_.notify_all();
    };
    callbacks.on_terminal =
        [state](grpc_lite::Status status,
                grpc_lite::MetadataEntries trailers) noexcept {
          state->SetTerminal(NativeStatus(std::move(status)),
                             std::move(trailers));
        };

    grpc_lite::ClientStream stream;
    {
      std::lock_guard<std::mutex> command_lock(state->command_mutex_);
      error = grpc_lite::ClientStream::Open(channel, method, options,
                                             std::move(callbacks), &stream);
      if (error.ok()) {
        state->stream_ = std::make_unique<grpc_lite::ClientStream>(
            std::move(stream));
        if (state->cancel_requested_) state->stream_->Cancel();
      }
    }
    if (!error.ok()) state->SetTerminal(ErrorStatus(error), {});
    return state;
  }

  void Cancel() noexcept override {
    std::lock_guard<std::mutex> command_lock(command_mutex_);
    {
      std::lock_guard<std::mutex> lock(mutex_);
      if (terminal_) return;
      cancel_requested_ = true;
    }
    if (stream_) stream_->Cancel();
  }

  Status Send(const std::string& payload) {
    return RunCommand([&](grpc_lite::ClientStream& stream) {
      return stream.Send(payload, compression_);
    });
  }

  Status CloseSend() {
    return RunCommand(
        [](grpc_lite::ClientStream& stream) { return stream.CloseSend(); });
  }

  Status SendAndClose(const std::string& payload) {
    Status status = Send(payload);
    if (!status.ok()) return status;
    return CloseSend();
  }

  bool Read(std::string* output) {
    if (output == nullptr) return false;
    {
      std::unique_lock<std::mutex> lock(mutex_);
      changed_.wait(lock, [&] { return has_message_ || terminal_; });
      if (!has_message_) return false;
      *output = std::move(message_);
      has_message_ = false;
    }
    std::lock_guard<std::mutex> command_lock(command_mutex_);
    if (stream_) {
      const grpc_lite::Error error = stream_->ResumeReceive();
      if (!error.ok() && error.code() != grpc_lite::ErrorCode::Closed &&
          error.code() != grpc_lite::ErrorCode::Unavailable &&
          error.code() != grpc_lite::ErrorCode::WouldBlock) {
        SetTerminal(ErrorStatus(error), {});
      }
    }
    return true;
  }

  Status Finish() {
    Status status;
    ClientContext::MetadataMap initial;
    ClientContext::MetadataMap trailing;
    {
      std::unique_lock<std::mutex> lock(mutex_);
      changed_.wait(lock, [&] { return terminal_; });
      status = status_;
      for (const auto& entry : initial_metadata_)
        initial.emplace(entry.first, entry.second);
      for (const auto& entry : trailing_metadata_)
        trailing.emplace(entry.first, entry.second);
    }
    if (context_ != nullptr) {
      context_->CompleteCall(this, std::move(initial), std::move(trailing));
    }
    DestroyStream();
    return status;
  }

 private:
  BlockingCallState(ClientContext* context, grpc_lite::Compression compression)
      : context_(context), compression_(compression) {}

  template <typename Command>
  Status RunCommand(Command command) {
    for (;;) {
      std::uint64_t generation;
      {
        std::lock_guard<std::mutex> lock(mutex_);
        if (terminal_) return status_;
        generation = writable_generation_;
      }
      grpc_lite::Error error(grpc_lite::ErrorCode::Closed);
      {
        std::lock_guard<std::mutex> command_lock(command_mutex_);
        if (stream_) error = command(*stream_);
      }
      if (error.ok()) return {};
      if (error.code() == grpc_lite::ErrorCode::Closed) {
        std::unique_lock<std::mutex> lock(mutex_);
        changed_.wait(lock, [&] { return terminal_; });
        return status_;
      }
      if (error.code() != grpc_lite::ErrorCode::WouldBlock) {
        Status status = error.code() == grpc_lite::ErrorCode::OutOfRange
                            ? Status(StatusCode::RESOURCE_EXHAUSTED,
                                     std::string(error.message()))
                            : ErrorStatus(error);
        SetTerminal({status.error_code(),
                     "stream command failed: " + status.error_message()},
                    {});
        return status;
      }
      std::unique_lock<std::mutex> lock(mutex_);
      changed_.wait(lock, [&] {
        return terminal_ || writable_generation_ != generation;
      });
      if (terminal_) return status_;
    }
  }

  void SetTerminal(Status status,
                   grpc_lite::MetadataEntries trailing_metadata) noexcept {
    std::lock_guard<std::mutex> lock(mutex_);
    if (terminal_) return;
    status_ = std::move(status);
    trailing_metadata_ = std::move(trailing_metadata);
    terminal_ = true;
    changed_.notify_all();
  }

  void DestroyStream() {
    std::unique_ptr<grpc_lite::ClientStream> stream;
    {
      std::lock_guard<std::mutex> command_lock(command_mutex_);
      stream = std::move(stream_);
    }
    stream.reset();
    channel_owner_.reset();
  }

  ClientContext* context_;
  grpc_lite::Compression compression_;
  mutable std::mutex mutex_;
  std::condition_variable changed_;
  std::mutex command_mutex_;
  std::unique_ptr<grpc_lite::ClientStream> stream_;
  std::shared_ptr<void> channel_owner_;
  grpc_lite::MetadataEntries initial_metadata_;
  grpc_lite::MetadataEntries trailing_metadata_;
  std::string message_;
  Status status_;
  std::uint64_t writable_generation_ = 0;
  bool has_message_ = false;
  bool terminal_ = false;
  bool cancel_requested_ = false;
};

}  // namespace grpc::internal

#endif
