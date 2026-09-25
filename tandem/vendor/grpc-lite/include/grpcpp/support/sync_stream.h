#ifndef GRPCPP_SUPPORT_SYNC_STREAM_H
#define GRPCPP_SUPPORT_SYNC_STREAM_H

#include <grpcpp/impl/blocking_call.h>
#include <grpcpp/server_context.h>

#include <climits>
#include <functional>
#include <memory>
#include <string>
#include <utility>

namespace grpc {

namespace internal {
template <class Response>
class ServerWriterAccess;
template <class Request>
class ServerReaderAccess;
template <class Response, class Request>
class ServerReaderWriterAccess;

template <class Message>
bool ParseMessage(const std::string& payload, Message* message,
                  Status* parse_status, const char* description) {
  if (payload.size() > static_cast<std::size_t>(INT_MAX)) {
    *parse_status = {StatusCode::RESOURCE_EXHAUSTED,
                     std::string(description) + " is too large to parse"};
    return false;
  }
  Message parsed;
  if (!parsed.ParseFromArray(payload.data(), static_cast<int>(payload.size()))) {
    *parse_status = {StatusCode::INTERNAL,
                     std::string("failed to parse ") + description};
    return false;
  }
  *message = std::move(parsed);
  return true;
}

template <class Message>
bool WriteMessage(const Message& message,
                  const std::shared_ptr<BlockingCallState>& state,
                  Status* write_status) {
  std::string payload;
  if (!message.SerializeToString(&payload)) {
    *write_status = {StatusCode::INTERNAL, "failed to serialize request"};
    state->Cancel();
    return false;
  }
  *write_status = state->Send(payload);
  return write_status->ok();
}
}  // namespace internal

template <class Response>
class ServerWriter final {
 public:
  ServerWriter(const ServerWriter&) = delete;
  ServerWriter& operator=(const ServerWriter&) = delete;

  bool Write(const Response& response) {
    while (!context_->IsCancelled()) {
      const grpc_lite::Error error = try_write_(response);
      if (error.ok()) return true;
      if (error.code() != grpc_lite::ErrorCode::WouldBlock ||
          !wait_for_writable_()) {
        return false;
      }
    }
    return false;
  }

 private:
  friend class internal::ServerWriterAccess<Response>;
  ServerWriter(ServerContext* context,
               std::function<grpc_lite::Error(const Response&)> try_write,
               std::function<bool()> wait_for_writable)
      : context_(context),
        try_write_(std::move(try_write)),
        wait_for_writable_(std::move(wait_for_writable)) {}

  ServerContext* context_;
  std::function<grpc_lite::Error(const Response&)> try_write_;
  std::function<bool()> wait_for_writable_;
};

template <class Request>
class ServerReader final {
 public:
  ServerReader(const ServerReader&) = delete;
  ServerReader& operator=(const ServerReader&) = delete;

  bool Read(Request* request) {
    return request != nullptr && !context_->IsCancelled() && read_(request);
  }

 private:
  friend class internal::ServerReaderAccess<Request>;
  ServerReader(ServerContext* context, std::function<bool(Request*)> read)
      : context_(context), read_(std::move(read)) {}

  ServerContext* context_;
  std::function<bool(Request*)> read_;
};

template <class Response, class Request>
class ServerReaderWriter final {
 public:
  ServerReaderWriter(const ServerReaderWriter&) = delete;
  ServerReaderWriter& operator=(const ServerReaderWriter&) = delete;

  bool Read(Request* request) {
    return request != nullptr && !context_->IsCancelled() && read_(request);
  }

  bool Write(const Response& response) {
    while (!context_->IsCancelled()) {
      const grpc_lite::Error error = try_write_(response);
      if (error.ok()) return true;
      if (error.code() != grpc_lite::ErrorCode::WouldBlock ||
          !wait_for_writable_()) {
        return false;
      }
    }
    return false;
  }

 private:
  friend class internal::ServerReaderWriterAccess<Response, Request>;
  ServerReaderWriter(ServerContext* context,
                     std::function<bool(Request*)> read,
                     std::function<grpc_lite::Error(const Response&)> try_write,
                     std::function<bool()> wait_for_writable)
      : context_(context),
        read_(std::move(read)),
        try_write_(std::move(try_write)),
        wait_for_writable_(std::move(wait_for_writable)) {}

  ServerContext* context_;
  std::function<bool(Request*)> read_;
  std::function<grpc_lite::Error(const Response&)> try_write_;
  std::function<bool()> wait_for_writable_;
};

namespace internal {

template <class Response>
class ServerWriterAccess {
 public:
  static ServerWriter<Response> Create(
      ServerContext* context,
      std::function<grpc_lite::Error(const Response&)> try_write,
      std::function<bool()> wait_for_writable) {
    return ServerWriter<Response>(context, std::move(try_write),
                                  std::move(wait_for_writable));
  }
};

template <class Request>
class ServerReaderAccess {
 public:
  static ServerReader<Request> Create(ServerContext* context,
                                      std::function<bool(Request*)> read) {
    return ServerReader<Request>(context, std::move(read));
  }
};

template <class Response, class Request>
class ServerReaderWriterAccess {
 public:
  static ServerReaderWriter<Response, Request> Create(
      ServerContext* context, std::function<bool(Request*)> read,
      std::function<grpc_lite::Error(const Response&)> try_write,
      std::function<bool()> wait_for_writable) {
    return ServerReaderWriter<Response, Request>(
        context, std::move(read), std::move(try_write),
        std::move(wait_for_writable));
  }
};

}  // namespace internal

template <class Response>
class ClientReader final {
 public:
  explicit ClientReader(std::shared_ptr<internal::BlockingCallState> state)
      : state_(std::move(state)) {}
  ~ClientReader() {
    if (!finished_) {
      state_->Cancel();
      Finish();
    }
  }
  ClientReader(const ClientReader&) = delete;
  ClientReader& operator=(const ClientReader&) = delete;

  bool Read(Response* response) {
    if (response == nullptr || finished_ || !parse_status_.ok()) return false;
    std::string payload;
    if (!state_->Read(&payload)) {
      read_exhausted_ = true;
      return false;
    }
    if (!internal::ParseMessage(payload, response, &parse_status_, "response")) {
      state_->Cancel();
      return false;
    }
    return true;
  }

  Status Finish() {
    if (finished_) return final_status_;
    if (!read_exhausted_) state_->Cancel();
    final_status_ = state_->Finish();
    if (!parse_status_.ok()) final_status_ = parse_status_;
    finished_ = true;
    return final_status_;
  }

 private:
  std::shared_ptr<internal::BlockingCallState> state_;
  Status final_status_;
  Status parse_status_;
  bool read_exhausted_ = false;
  bool finished_ = false;
};

template <class Request>
class ClientWriter final {
 public:
  ClientWriter(std::shared_ptr<internal::BlockingCallState> state,
               void* response,
               std::function<bool(const std::string&, void*, Status*)> parse)
      : state_(std::move(state)), response_(response), parse_(std::move(parse)) {}
  ~ClientWriter() {
    if (!finished_) {
      state_->Cancel();
      Finish();
    }
  }
  ClientWriter(const ClientWriter&) = delete;
  ClientWriter& operator=(const ClientWriter&) = delete;

  bool Write(const Request& request) {
    if (writes_done_ || finished_ || !write_status_.ok()) return false;
    return internal::WriteMessage(request, state_, &write_status_);
  }

  bool WritesDone() {
    if (finished_) return false;
    if (writes_done_) return write_status_.ok();
    writes_done_ = true;
    if (write_status_.ok()) write_status_ = state_->CloseSend();
    return write_status_.ok();
  }

  Status Finish() {
    if (finished_) return final_status_;
    if (!writes_done_) WritesDone();
    bool response_missing = false;
    if (write_status_.ok()) {
      std::string payload;
      if (!state_->Read(&payload)) {
        response_missing = true;
      } else if (!parse_(payload, response_, &parse_status_)) {
        state_->Cancel();
      } else {
        std::string extra;
        if (state_->Read(&extra)) {
          parse_status_ = {StatusCode::INTERNAL,
                           "client streaming call returned multiple responses"};
          state_->Cancel();
        }
      }
    }
    final_status_ = state_->Finish();
    if (!write_status_.ok()) final_status_ = write_status_;
    if (!parse_status_.ok()) {
      final_status_ = parse_status_;
    } else if (response_missing && final_status_.ok()) {
      final_status_ = {StatusCode::INTERNAL,
                       "client streaming response is missing"};
    }
    finished_ = true;
    return final_status_;
  }

 private:
  std::shared_ptr<internal::BlockingCallState> state_;
  void* response_;
  std::function<bool(const std::string&, void*, Status*)> parse_;
  Status final_status_;
  Status write_status_;
  Status parse_status_;
  bool writes_done_ = false;
  bool finished_ = false;
};

template <class Request, class Response>
class ClientReaderWriter final {
 public:
  explicit ClientReaderWriter(
      std::shared_ptr<internal::BlockingCallState> state)
      : state_(std::move(state)) {}
  ~ClientReaderWriter() {
    if (!finished_) {
      state_->Cancel();
      Finish();
    }
  }
  ClientReaderWriter(const ClientReaderWriter&) = delete;
  ClientReaderWriter& operator=(const ClientReaderWriter&) = delete;

  bool Write(const Request& request) {
    if (writes_done_ || finished_ || !write_status_.ok()) return false;
    return internal::WriteMessage(request, state_, &write_status_);
  }

  bool WritesDone() {
    if (finished_) return false;
    if (writes_done_) return write_status_.ok();
    writes_done_ = true;
    if (write_status_.ok()) write_status_ = state_->CloseSend();
    return write_status_.ok();
  }

  bool Read(Response* response) {
    if (response == nullptr || finished_ || !parse_status_.ok()) return false;
    std::string payload;
    if (!state_->Read(&payload)) {
      read_exhausted_ = true;
      return false;
    }
    if (!internal::ParseMessage(payload, response, &parse_status_, "response")) {
      state_->Cancel();
      return false;
    }
    return true;
  }

  Status Finish() {
    if (finished_) return final_status_;
    if (!writes_done_) WritesDone();
    if (!read_exhausted_) state_->Cancel();
    final_status_ = state_->Finish();
    if (!write_status_.ok()) final_status_ = write_status_;
    if (!parse_status_.ok()) final_status_ = parse_status_;
    finished_ = true;
    return final_status_;
  }

 private:
  std::shared_ptr<internal::BlockingCallState> state_;
  Status final_status_;
  Status write_status_;
  Status parse_status_;
  bool writes_done_ = false;
  bool read_exhausted_ = false;
  bool finished_ = false;
};

}  // namespace grpc

#endif
