#ifndef GRPC_LITE_EXAMPLES_CPP_EXAMPLE_SUPPORT_H_
#define GRPC_LITE_EXAMPLES_CPP_EXAMPLE_SUPPORT_H_

#include <grpcpp/grpcpp.h>

#include <iostream>
#include <iterator>
#include <memory>
#include <string>

namespace example {

inline std::shared_ptr<grpc::Channel> CreateChannel(const std::string& target) {
  grpc::ChannelArguments arguments;
  arguments.SetAllowInitialOffline(false);
  return grpc::CreateCustomChannel(
      target, grpc::InsecureChannelCredentials(), arguments);
}

inline bool Check(bool condition, const std::string& message) {
  if (condition) return true;
  std::cerr << message << '\n';
  return false;
}

inline bool CheckStatus(const grpc::Status& status, grpc::StatusCode expected,
                        const std::string& operation) {
  if (status.error_code() == expected) return true;
  std::cerr << operation << ": expected status " << expected << ", got "
            << status.error_code() << " (" << status.error_message() << ")\n";
  return false;
}

inline bool HasSingleMetadata(const grpc::ClientContext::MetadataMap& metadata,
                              const std::string& key,
                              const std::string& expected) {
  const auto range = metadata.equal_range(key);
  return range.first != range.second && range.first->second == expected &&
         std::next(range.first) == range.second;
}

}  // namespace example

#endif
