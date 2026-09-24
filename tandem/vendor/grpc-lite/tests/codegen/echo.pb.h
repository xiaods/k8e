#ifndef GRPC_LITE_TESTS_CODEGEN_ECHO_PB_H_
#define GRPC_LITE_TESTS_CODEGEN_ECHO_PB_H_

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

 private:
  std::string message_;
};

class EchoReply {
 public:
  const std::string& message() const { return message_; }
  bool ParseFromArray(const void* data, int size) {
    message_.assign(static_cast<const char*>(data), static_cast<size_t>(size));
    return true;
  }

 private:
  std::string message_;
};

}  // namespace demo

#endif
