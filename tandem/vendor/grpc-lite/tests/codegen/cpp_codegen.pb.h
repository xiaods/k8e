#ifndef GRPC_LITE_TESTS_CODEGEN_CPP_CODEGEN_PB_H_
#define GRPC_LITE_TESTS_CODEGEN_CPP_CODEGEN_PB_H_

#include <string>
#include <utility>

namespace demo {

class EchoRequest {
 public:
  void set_message(std::string message) { message_ = std::move(message); }
  const std::string& message() const { return message_; }
  bool SerializeToString(std::string* output) const {
    *output = message_;
    return true;
  }
  bool ParseFromArray(const void* data, int size) {
    message_.assign(static_cast<const char*>(data), static_cast<std::size_t>(size));
    return message_ != "!malformed";
  }

 private:
  std::string message_;
};

class EchoReply {
 public:
  void set_message(std::string message) { message_ = std::move(message); }
  const std::string& message() const { return message_; }
  bool SerializeToString(std::string* output) const {
    *output = message_;
    return message_ != "!serialize-error";
  }
  bool ParseFromArray(const void* data, int size) {
    message_.assign(static_cast<const char*>(data), static_cast<std::size_t>(size));
    return true;
  }

 private:
  std::string message_;
};

}  // namespace demo

#endif
