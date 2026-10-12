#ifndef GRPC_LITE_CPP_METADATA_HPP
#define GRPC_LITE_CPP_METADATA_HPP

#include <grpc_lite/cpp/status.hpp>

#include <cstddef>
#include <string>
#include <utility>
#include <vector>

namespace grpc_lite {

using MetadataEntries = std::vector<std::pair<std::string, std::string>>;

class Metadata {
 public:
  Metadata() = default;
  ~Metadata() { grpc_lite_metadata_destroy(metadata_); }
  Metadata(const Metadata&) = delete;
  Metadata& operator=(const Metadata&) = delete;

  Metadata(Metadata&& other) noexcept
      : metadata_(std::exchange(other.metadata_, nullptr)) {}
  Metadata& operator=(Metadata&& other) noexcept {
    if (this != &other) {
      grpc_lite_metadata_destroy(metadata_);
      metadata_ = std::exchange(other.metadata_, nullptr);
    }
    return *this;
  }

  static Error Create(Metadata* out) {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    grpc_lite_metadata* metadata = nullptr;
    const Error error(grpc_lite_metadata_create(&metadata));
    if (!error.ok()) return error;
    *out = Metadata(metadata);
    return {};
  }

  Error Add(const std::string& key, const std::string& value) {
    if (metadata_ == nullptr) return Error(ErrorCode::InvalidState);
    return Error(grpc_lite_metadata_add(
        metadata_, internal::Bytes(key), internal::Bytes(value)));
  }

  std::size_t size() const {
    return metadata_ == nullptr ? 0 : grpc_lite_metadata_count(metadata_);
  }
  bool empty() const { return size() == 0; }

  Error At(std::size_t index, std::string* key, std::string* value) const {
    if (metadata_ == nullptr) return Error(ErrorCode::InvalidState);
    if (key == nullptr || value == nullptr) {
      return Error(ErrorCode::InvalidArgument);
    }
    grpc_lite_metadata_entry_view entry{};
    const Error error(grpc_lite_metadata_at(metadata_, index, &entry));
    if (!error.ok()) return error;
    *key = internal::CopyBytes(entry.key);
    *value = internal::CopyBytes(entry.value);
    return {};
  }

  Error CopyEntries(MetadataEntries* out) const {
    if (out == nullptr) return Error(ErrorCode::InvalidArgument);
    MetadataEntries entries;
    entries.reserve(size());
    for (std::size_t index = 0; index < size(); ++index) {
      std::string key;
      std::string value;
      const Error error = At(index, &key, &value);
      if (!error.ok()) return error;
      entries.emplace_back(std::move(key), std::move(value));
    }
    *out = std::move(entries);
    return {};
  }

 private:
  friend class ClientStream;
  friend class ServerCall;
  explicit Metadata(grpc_lite_metadata* metadata) : metadata_(metadata) {}

  grpc_lite_metadata* metadata_ = nullptr;
};

namespace internal {

inline MetadataEntries CopyMetadataView(const grpc_lite_metadata_view* view) {
  MetadataEntries entries;
  if (view == nullptr) return entries;
  const std::size_t count = grpc_lite_metadata_view_count(view);
  entries.reserve(count);
  for (std::size_t index = 0; index < count; ++index) {
    grpc_lite_metadata_entry_view entry{};
    if (grpc_lite_metadata_view_at(view, index, &entry) != GRPC_LITE_OK) break;
    entries.emplace_back(CopyBytes(entry.key), CopyBytes(entry.value));
  }
  return entries;
}

}  // namespace internal
}  // namespace grpc_lite

#endif
