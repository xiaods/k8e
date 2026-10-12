#ifndef GRPCPP_CLIENT_CONTEXT_H
#define GRPCPP_CLIENT_CONTEXT_H

#include <chrono>
#include <map>
#include <memory>
#include <mutex>
#include <string>

namespace grpc {

class Channel;
namespace internal {
class CancellationTarget {
 public:
  virtual ~CancellationTarget() = default;
  virtual void Cancel() noexcept = 0;
};
class BlockingCallState;
}  // namespace internal

class ClientContext {
 public:
  using MetadataMap = std::multimap<std::string, std::string>;

  ClientContext() = default;
  ClientContext(const ClientContext&) = delete;
  ClientContext& operator=(const ClientContext&) = delete;

  void AddMetadata(const std::string& key, const std::string& value) {
    metadata_.emplace(key, value);
  }

  template <typename Clock, typename Duration>
  void set_deadline(std::chrono::time_point<Clock, Duration> deadline) {
    using TimePoint = std::chrono::time_point<Clock, Duration>;
    if (deadline == TimePoint::min()) {
      deadline_extreme_ = -1;
      has_deadline_ = true;
      return;
    }
    if (deadline == TimePoint::max()) {
      deadline_extreme_ = 1;
      has_deadline_ = true;
      return;
    }
    const long double clock_deadline =
        std::chrono::duration<long double>(deadline.time_since_epoch()).count();
    const long double clock_now = std::chrono::duration<long double>(
                                      Clock::now().time_since_epoch()).count();
    const long double system_now = std::chrono::duration<long double>(
                                       std::chrono::system_clock::now().time_since_epoch()).count();
    deadline_seconds_ = system_now + (clock_deadline - clock_now);
    deadline_extreme_ = 0;
    has_deadline_ = true;
  }

  std::chrono::system_clock::time_point deadline() const {
    if (!has_deadline_) return std::chrono::system_clock::time_point::max();
    if (deadline_extreme_ < 0) return std::chrono::system_clock::time_point::min();
    if (deadline_extreme_ > 0) return std::chrono::system_clock::time_point::max();
    const long double minimum = std::chrono::duration<long double>(
                                    std::chrono::system_clock::duration::min()).count();
    const long double maximum = std::chrono::duration<long double>(
                                    std::chrono::system_clock::duration::max()).count();
    if (deadline_seconds_ <= minimum) {
      return std::chrono::system_clock::time_point::min();
    }
    if (deadline_seconds_ >= maximum) {
      return std::chrono::system_clock::time_point::max();
    }
    const auto duration = std::chrono::duration<long double>(deadline_seconds_);
    return std::chrono::system_clock::time_point(
        std::chrono::duration_cast<std::chrono::system_clock::duration>(duration));
  }
  const MetadataMap& GetServerInitialMetadata() const { return initial_metadata_; }
  const MetadataMap& GetServerTrailingMetadata() const { return trailing_metadata_; }

  void TryCancel() noexcept {
    std::shared_ptr<internal::CancellationTarget> target;
    {
      std::lock_guard<std::mutex> lock(mutex_);
      if (terminal_) return;
      cancelled_ = true;
      target = cancellation_target_.lock();
    }
    if (target) target->Cancel();
  }

 private:
  friend class Channel;
  friend class internal::BlockingCallState;

  bool RegisterCancellation(
      const std::shared_ptr<internal::CancellationTarget>& target) {
    std::lock_guard<std::mutex> lock(mutex_);
    if (cancelled_ || terminal_) return false;
    cancellation_target_ = target;
    return true;
  }
  void SetInitialMetadata(MetadataMap initial_metadata) {
    std::lock_guard<std::mutex> lock(mutex_);
    initial_metadata_ = std::move(initial_metadata);
  }
  void CompleteCall(const internal::CancellationTarget* target,
                    MetadataMap initial_metadata,
                    MetadataMap trailing_metadata) {
    std::lock_guard<std::mutex> lock(mutex_);
    const auto active = cancellation_target_.lock();
    if (active && active.get() != target) return;
    initial_metadata_ = std::move(initial_metadata);
    trailing_metadata_ = std::move(trailing_metadata);
    cancellation_target_.reset();
    terminal_ = true;
  }

  MetadataMap metadata_;
  MetadataMap initial_metadata_;
  MetadataMap trailing_metadata_;
  long double deadline_seconds_ = 0;
  int deadline_extreme_ = 0;
  bool has_deadline_ = false;
  mutable std::mutex mutex_;
  std::weak_ptr<internal::CancellationTarget> cancellation_target_;
  bool cancelled_ = false;
  bool terminal_ = false;
};

}  // namespace grpc

#endif
