#ifndef GRPCPP_SECURITY_CREDENTIALS_H
#define GRPCPP_SECURITY_CREDENTIALS_H

#include <memory>

namespace grpc {

class Channel;

class ChannelCredentials {
 public:
  virtual ~ChannelCredentials() = default;

 protected:
  ChannelCredentials() = default;

 private:
  friend class Channel;
  virtual bool is_insecure() const { return false; }
};

namespace internal {
class InsecureCredentials final : public ChannelCredentials {
 private:
  bool is_insecure() const override { return true; }
};
}  // namespace internal

inline std::shared_ptr<ChannelCredentials> InsecureChannelCredentials() {
  return std::make_shared<internal::InsecureCredentials>();
}

}  // namespace grpc

#endif
