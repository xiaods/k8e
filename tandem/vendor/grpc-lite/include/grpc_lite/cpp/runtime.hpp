#ifndef GRPC_LITE_CPP_RUNTIME_HPP
#define GRPC_LITE_CPP_RUNTIME_HPP

#include <grpc_lite/cpp/status.hpp>

#include <utility>

namespace grpc_lite {

// A Runtime must outlive every handle created with it.
class Runtime {
 public:
  Runtime() = default;
  ~Runtime() { grpc_lite_runtime_destroy(runtime_); }
  Runtime(const Runtime&) = delete;
  Runtime& operator=(const Runtime&) = delete;

  Runtime(Runtime&& other) noexcept
      : runtime_(std::exchange(other.runtime_, nullptr)) {}
  Runtime& operator=(Runtime&& other) noexcept {
    if (this != &other) {
      grpc_lite_runtime_destroy(runtime_);
      runtime_ = std::exchange(other.runtime_, nullptr);
    }
    return *this;
  }

  static Error Create(Runtime* out) {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    grpc_lite_runtime* runtime = nullptr;
    const Error error(grpc_lite_runtime_create(&runtime));
    if (!error.ok()) return error;
    *out = Runtime(runtime);
    return {};
  }

 private:
  friend class Channel;
  explicit Runtime(grpc_lite_runtime* runtime) : runtime_(runtime) {}

  grpc_lite_runtime* runtime_ = nullptr;
};

}  // namespace grpc_lite

#endif
