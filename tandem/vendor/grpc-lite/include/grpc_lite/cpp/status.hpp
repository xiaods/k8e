#ifndef GRPC_LITE_CPP_STATUS_HPP
#define GRPC_LITE_CPP_STATUS_HPP

#include <grpc_lite/grpc_lite.h>

#include <cstdint>
#include <string>
#include <string_view>
#include <utility>

namespace grpc_lite {

enum class ErrorCode : std::int32_t {
  Ok = GRPC_LITE_OK,
  InvalidArgument = GRPC_LITE_ERROR_INVALID_ARGUMENT,
  InvalidState = GRPC_LITE_ERROR_INVALID_STATE,
  OutOfMemory = GRPC_LITE_ERROR_OUT_OF_MEMORY,
  Unsupported = GRPC_LITE_ERROR_UNSUPPORTED,
  Unavailable = GRPC_LITE_ERROR_UNAVAILABLE,
  OutOfRange = GRPC_LITE_ERROR_OUT_OF_RANGE,
  Closed = GRPC_LITE_ERROR_CLOSED,
  WouldBlock = GRPC_LITE_ERROR_WOULD_BLOCK,
  Internal = GRPC_LITE_ERROR_INTERNAL,
};

class Error {
 public:
  Error() = default;
  explicit Error(grpc_lite_error code)
      : code_(static_cast<ErrorCode>(code)) {}
  explicit Error(ErrorCode code) : code_(code) {}

  bool ok() const { return code_ == ErrorCode::Ok; }
  ErrorCode code() const { return code_; }
  std::string_view message() const {
    return grpc_lite_error_string(static_cast<grpc_lite_error>(code_));
  }

 private:
  ErrorCode code_ = ErrorCode::Ok;
};

enum class StatusCode : std::int32_t {
  Ok = 0,
  Cancelled = 1,
  Unknown = 2,
  InvalidArgument = 3,
  DeadlineExceeded = 4,
  NotFound = 5,
  AlreadyExists = 6,
  PermissionDenied = 7,
  ResourceExhausted = 8,
  FailedPrecondition = 9,
  Aborted = 10,
  OutOfRange = 11,
  Unimplemented = 12,
  Internal = 13,
  Unavailable = 14,
  DataLoss = 15,
  Unauthenticated = 16,
};

class Status {
 public:
  Status() = default;
  Status(StatusCode code, std::string message)
      : code_(code), message_(std::move(message)) {}

  bool ok() const { return code_ == StatusCode::Ok; }
  StatusCode code() const { return code_; }
  const std::string& message() const { return message_; }

 private:
  StatusCode code_ = StatusCode::Ok;
  std::string message_;
};

enum class Compression : std::uint32_t {
  Identity = GRPC_LITE_COMPRESSION_IDENTITY,
  Gzip = GRPC_LITE_COMPRESSION_GZIP,
};

enum class ReceiveAction : std::uint32_t {
  Continue = GRPC_LITE_RECEIVE_CONTINUE,
  Pause = GRPC_LITE_RECEIVE_PAUSE,
};

namespace internal {

inline grpc_lite_bytes_view Bytes(std::string_view value) {
  return {reinterpret_cast<const std::uint8_t*>(value.data()), value.size()};
}

inline std::string CopyBytes(grpc_lite_bytes_view value) {
  if (value.size == 0) return {};
  return {reinterpret_cast<const char*>(value.data), value.size};
}

}  // namespace internal
}  // namespace grpc_lite

#endif
