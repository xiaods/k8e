#include <grpc_lite/grpc_lite.h>

#include <cassert>
#include <cstdint>
#include <cstddef>
#include <string_view>
#include <type_traits>

static_assert(std::is_same_v<grpc_lite_error, std::int32_t>);
static_assert(std::is_same_v<grpc_lite_feature_bits, std::uint64_t>);
static_assert(sizeof(grpc_lite_bytes_view) == 2 * sizeof(std::size_t));
static_assert(alignof(grpc_lite_bytes_view) == alignof(std::size_t));
static_assert(offsetof(grpc_lite_bytes_view, data) == 0);
static_assert(offsetof(grpc_lite_bytes_view, size) == sizeof(void *));
static_assert(sizeof(grpc_lite_metadata_entry_view) ==
              2 * sizeof(grpc_lite_bytes_view));
static_assert(std::is_standard_layout_v<grpc_lite_logger>);
static_assert(offsetof(grpc_lite_logger, struct_size) == 0);
static_assert(std::is_standard_layout_v<grpc_lite_channel_options>);
static_assert(offsetof(grpc_lite_channel_options, struct_size) == 0);
static_assert(offsetof(grpc_lite_channel_options, allow_initial_offline) ==
              sizeof(std::size_t));
static_assert(offsetof(grpc_lite_channel_options, initial_backoff_ns) ==
              2 * sizeof(std::size_t));
static_assert(offsetof(grpc_lite_channel_options, max_backoff_ns) ==
              offsetof(grpc_lite_channel_options, initial_backoff_ns) + 8);
static_assert(offsetof(grpc_lite_channel_options, multiplier_millis) ==
              offsetof(grpc_lite_channel_options, max_backoff_ns) + 8);
static_assert(offsetof(grpc_lite_channel_options, jitter_percent) ==
              offsetof(grpc_lite_channel_options, multiplier_millis) + 4);
static_assert(offsetof(grpc_lite_channel_options,
                       max_response_header_list_size) ==
              offsetof(grpc_lite_channel_options, logger) + sizeof(void *));

int main() {
  grpc_lite_unary_options options = GRPC_LITE_UNARY_OPTIONS_INIT;
  grpc_lite_channel_options channel_options = GRPC_LITE_CHANNEL_OPTIONS_INIT;
  grpc_lite_logger logger = GRPC_LITE_LOGGER_INIT;
  grpc_lite_client_stream_options stream_options =
      GRPC_LITE_CLIENT_STREAM_OPTIONS_INIT;
  grpc_lite_client_stream_callbacks callbacks =
      GRPC_LITE_CLIENT_STREAM_CALLBACKS_INIT;
  grpc_lite_server_options server_options = GRPC_LITE_SERVER_OPTIONS_INIT;
  grpc_lite_server_method_options method_options =
      GRPC_LITE_SERVER_METHOD_OPTIONS_INIT;
  grpc_lite_server_method_callbacks method_callbacks =
      GRPC_LITE_SERVER_METHOD_CALLBACKS_INIT;
  static_assert(std::is_standard_layout_v<grpc_lite_unary_options>);
  static_assert(std::is_standard_layout_v<grpc_lite_client_stream_options>);
  static_assert(std::is_standard_layout_v<grpc_lite_client_stream_callbacks>);
  static_assert(std::is_standard_layout_v<grpc_lite_server_options>);
  static_assert(std::is_standard_layout_v<grpc_lite_server_method_options>);
  static_assert(std::is_standard_layout_v<grpc_lite_server_method_callbacks>);
  assert(options.struct_size == sizeof(options));
  assert(channel_options.struct_size == sizeof(channel_options));
  assert(logger.struct_size == sizeof(logger));
  assert(stream_options.struct_size == sizeof(stream_options));
  assert(callbacks.struct_size == sizeof(callbacks));
  assert(server_options.struct_size == sizeof(server_options));
  assert(method_options.struct_size == sizeof(method_options));
  assert(method_callbacks.struct_size == sizeof(method_callbacks));

  if (grpc_lite_abi_version() != GRPC_LITE_ABI_VERSION) return 1;
  if (std::string_view(grpc_lite_error_string(GRPC_LITE_OK)) != "ok") return 2;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_MANAGED_CHANNEL) == 0) return 4;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_LOGGING_CALLBACK) == 0) return 6;
  assert(channel_options.initial_backoff_ns == UINT64_C(1000000000));
  assert(channel_options.max_backoff_ns == UINT64_C(120000000000));
  assert(channel_options.max_response_header_list_size == UINT64_C(65536));
  grpc_lite_channel *channel = reinterpret_cast<grpc_lite_channel *>(1);
  const grpc_lite_bytes_view invalid_target = {
      reinterpret_cast<const std::uint8_t *>("invalid"), 7};
  if (grpc_lite_channel_create_managed(
          nullptr, invalid_target, nullptr, &channel) !=
      GRPC_LITE_ERROR_INVALID_ARGUMENT) return 5;
  assert(channel == nullptr);
  grpc_lite_channel_shutdown(nullptr);
  grpc_lite_channel_wait(nullptr);
  grpc_lite_server_call_abort(nullptr);

  grpc_lite_metadata *metadata = nullptr;
  if (grpc_lite_metadata_create(&metadata) != GRPC_LITE_OK) return 3;
  grpc_lite_metadata_destroy(metadata);
  return 0;
}
