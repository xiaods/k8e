#ifndef GRPCPP_SUPPORT_STATUS_H
#define GRPCPP_SUPPORT_STATUS_H

#include <grpcpp/support/status_code_enum.h>

#include <string>
#include <utility>

namespace grpc {

class Status {
 public:
  Status() = default;
  Status(StatusCode code, std::string message)
      : code_(code), message_(std::move(message)) {}
  Status(StatusCode code, std::string message, std::string details)
      : code_(code), message_(std::move(message)), details_(std::move(details)) {}

  bool ok() const { return code_ == StatusCode::OK; }
  StatusCode error_code() const { return code_; }
  const std::string& error_message() const { return message_; }
  const std::string& error_details() const { return details_; }

  static const Status& OK;
  static const Status& CANCELLED;

 private:
  StatusCode code_ = StatusCode::OK;
  std::string message_;
  std::string details_;
};

namespace internal {

inline const Status kStatusOk{};
inline const Status kStatusCancelled{StatusCode::CANCELLED, ""};

}  // namespace internal

inline const Status& Status::OK = internal::kStatusOk;
inline const Status& Status::CANCELLED = internal::kStatusCancelled;

}  // namespace grpc

#endif
