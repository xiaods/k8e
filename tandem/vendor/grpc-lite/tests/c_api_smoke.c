#include <grpc_lite/grpc_lite.h>

#include <assert.h>
#include <stdint.h>
#include <string.h>

static grpc_lite_bytes_view bytes(const void *data, size_t size) {
  grpc_lite_bytes_view value = {(const uint8_t *)data, size};
  return value;
}

static void on_log(
    void *user_data,
    uint32_t level,
    grpc_lite_bytes_view message) {
  (void)user_data;
  (void)level;
  (void)message;
}

static uint32_t on_message(
    void *user_data,
    grpc_lite_client_stream *stream,
    grpc_lite_bytes_view payload,
    uint32_t compression) {
  (void)user_data;
  (void)stream;
  (void)payload;
  (void)compression;
  return GRPC_LITE_RECEIVE_CONTINUE;
}

static void on_terminal(
    void *user_data,
    grpc_lite_client_stream *stream,
    int32_t status_code,
    grpc_lite_bytes_view status_message,
    const grpc_lite_metadata_view *trailing_metadata) {
  (void)user_data;
  (void)stream;
  (void)status_code;
  (void)status_message;
  (void)trailing_metadata;
}

static uint32_t on_server_message(
    void *user_data,
    grpc_lite_server_stream *stream,
    const grpc_lite_server_context *context,
    grpc_lite_bytes_view payload,
    uint32_t compression) {
  (void)user_data;
  (void)stream;
  (void)context;
  (void)payload;
  (void)compression;
  return GRPC_LITE_RECEIVE_CONTINUE;
}

int main(void) {
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
  callbacks.on_message = on_message;
  callbacks.on_terminal = on_terminal;
  method_callbacks.on_message = on_server_message;
  logger.log = on_log;
  channel_options.logger = &logger;
  server_options.logger = &logger;
  assert(options.struct_size == sizeof(options));
  assert(channel_options.struct_size == sizeof(channel_options));
  assert(logger.struct_size == sizeof(logger));
  assert(stream_options.struct_size == sizeof(stream_options));
  assert(callbacks.struct_size == sizeof(callbacks));
  assert(server_options.struct_size == sizeof(server_options));
  assert(server_options.max_connections == UINT64_C(1024));
  assert(server_options.max_concurrent_streams_per_connection == 100);
  assert(server_options.cleartext_preface_timeout_ns == UINT64_C(10000000000));
  assert(server_options.connection_idle_timeout_ns == UINT64_C(300000000000));
  assert(method_options.struct_size == sizeof(method_options));
  assert(method_callbacks.struct_size == sizeof(method_callbacks));
  assert(options.max_response_size == UINT64_C(4194304));
  assert(channel_options.allow_initial_offline == 0);
  assert(channel_options.initial_backoff_ns == UINT64_C(1000000000));
  assert(channel_options.max_backoff_ns == UINT64_C(120000000000));
  assert(channel_options.multiplier_millis == 1600);
  assert(channel_options.jitter_percent == 20);
  assert(channel_options.max_response_header_list_size == UINT64_C(65536));
  assert(grpc_lite_unary_result_status_code(NULL) == 2);

  grpc_lite_metadata *metadata = NULL;
  grpc_lite_runtime *runtime = NULL;
  grpc_lite_channel *channel = NULL;
  grpc_lite_metadata_entry_view entry;
  const uint8_t binary[] = {0, 1, 2};

  if (grpc_lite_abi_version() != GRPC_LITE_ABI_VERSION) return 1;
  if (grpc_lite_library_version()[0] == '\0') return 2;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_STREAMING) == 0) return 3;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_C_STREAMING) == 0) return 11;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_C_SERVER) == 0) return 12;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_MANAGED_CHANNEL) == 0) return 13;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_LOGGING_CALLBACK) == 0) return 14;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_TLS) != 0) return 15;
  grpc_lite_server_call_abort(NULL);
  if (grpc_lite_metadata_create(&metadata) != GRPC_LITE_OK) return 4;
  if (grpc_lite_metadata_add(
          metadata,
          bytes("trace-bin", sizeof("trace-bin") - 1),
          bytes(binary, sizeof(binary))) != GRPC_LITE_OK) return 5;
  if (grpc_lite_metadata_count(metadata) != 1) return 6;
  if (grpc_lite_metadata_at(metadata, 0, &entry) != GRPC_LITE_OK) return 7;
  if (entry.key.size != sizeof("trace-bin") - 1 ||
      memcmp(entry.key.data, "trace-bin", entry.key.size) != 0) return 8;
  if (entry.value.size != sizeof(binary) ||
      memcmp(entry.value.data, binary, entry.value.size) != 0) return 9;
  grpc_lite_metadata_destroy(metadata);

  if (grpc_lite_runtime_create(&runtime) != GRPC_LITE_OK) return 10;
  grpc_lite_runtime_destroy(runtime);
  channel_options.allow_initial_offline = 1;
  channel_options.initial_backoff_ns = UINT64_C(10000000000);
  channel_options.max_backoff_ns = UINT64_C(10000000000);
  if (grpc_lite_channel_create_managed(
          NULL,
          bytes("127.0.0.1:1", sizeof("127.0.0.1:1") - 1),
          &channel_options,
          &channel) != GRPC_LITE_OK) return 14;
  grpc_lite_channel_shutdown(channel);
  grpc_lite_channel_wait(channel);
  grpc_lite_channel_destroy(channel);
  return 0;
}
