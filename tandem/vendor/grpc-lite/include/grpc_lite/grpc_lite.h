#ifndef GRPC_LITE_GRPC_LITE_H
#define GRPC_LITE_GRPC_LITE_H

#include <stddef.h>
#include <stdint.h>

#ifdef __cplusplus
extern "C" {
#endif

#define GRPC_LITE_ABI_MAJOR 1u
#define GRPC_LITE_ABI_MINOR 6u
#define GRPC_LITE_ABI_VERSION \
  ((GRPC_LITE_ABI_MAJOR << 16) | GRPC_LITE_ABI_MINOR)

typedef int32_t grpc_lite_error;

enum {
  GRPC_LITE_OK = 0,
  GRPC_LITE_ERROR_INVALID_ARGUMENT = 1,
  GRPC_LITE_ERROR_INVALID_STATE = 2,
  GRPC_LITE_ERROR_OUT_OF_MEMORY = 3,
  GRPC_LITE_ERROR_UNSUPPORTED = 4,
  GRPC_LITE_ERROR_UNAVAILABLE = 5,
  GRPC_LITE_ERROR_OUT_OF_RANGE = 6,
  GRPC_LITE_ERROR_CLOSED = 7,
  GRPC_LITE_ERROR_WOULD_BLOCK = 8,
  GRPC_LITE_ERROR_INTERNAL = 255,
};

typedef uint64_t grpc_lite_feature_bits;

#define GRPC_LITE_FEATURE_RAW_UNARY (UINT64_C(1) << 0)
#define GRPC_LITE_FEATURE_STREAMING (UINT64_C(1) << 1)
#define GRPC_LITE_FEATURE_GZIP (UINT64_C(1) << 2)
#define GRPC_LITE_FEATURE_DNS (UINT64_C(1) << 3)
/* Reserved for source compatibility; the C API currently exposes insecure transport. */
#define GRPC_LITE_FEATURE_TLS (UINT64_C(1) << 4)
#define GRPC_LITE_FEATURE_GRACEFUL_SERVER_DRAIN (UINT64_C(1) << 5)
#define GRPC_LITE_FEATURE_C_STREAMING (UINT64_C(1) << 6)
#define GRPC_LITE_FEATURE_C_SERVER (UINT64_C(1) << 7)
#define GRPC_LITE_FEATURE_MANAGED_CHANNEL (UINT64_C(1) << 8)
#define GRPC_LITE_FEATURE_LOGGING_CALLBACK (UINT64_C(1) << 9)

typedef struct grpc_lite_runtime grpc_lite_runtime;
typedef struct grpc_lite_metadata grpc_lite_metadata;
typedef struct grpc_lite_channel grpc_lite_channel;
typedef struct grpc_lite_unary_result grpc_lite_unary_result;
typedef struct grpc_lite_metadata_view grpc_lite_metadata_view;
typedef struct grpc_lite_client_stream grpc_lite_client_stream;
typedef struct grpc_lite_server grpc_lite_server;
typedef struct grpc_lite_server_stream grpc_lite_server_stream;
typedef struct grpc_lite_server_call grpc_lite_server_call;
typedef struct grpc_lite_server_context grpc_lite_server_context;

typedef struct grpc_lite_bytes_view {
  const uint8_t *data;
  size_t size;
} grpc_lite_bytes_view;

enum {
  GRPC_LITE_LOG_DEBUG = 0,
  GRPC_LITE_LOG_INFO = 1,
  GRPC_LITE_LOG_WARN = 2,
  GRPC_LITE_LOG_ERROR = 3,
};

typedef struct grpc_lite_logger {
  size_t struct_size;
  void *user_data;
  void (*log)(
      void *user_data,
      uint32_t level,
      grpc_lite_bytes_view message);
} grpc_lite_logger;

#define GRPC_LITE_LOGGER_INIT \
  { sizeof(grpc_lite_logger), NULL, NULL }

/*
 * Log callbacks run synchronously on API caller or transport threads and may run
 * concurrently. They should return promptly and must not call back into the originating
 * handle. The message is borrowed for the call.
 * Logger storage is borrowed during handle creation; user_data must outlive the handle.
 */

typedef struct grpc_lite_metadata_entry_view {
  grpc_lite_bytes_view key;
  grpc_lite_bytes_view value;
} grpc_lite_metadata_entry_view;

uint32_t grpc_lite_abi_version(void);
/* Returned strings are immutable library-owned storage and must not be freed. */
const char *grpc_lite_library_version(void);
/* Reports capabilities usable through this C API, not all compiled transport features. */
grpc_lite_feature_bits grpc_lite_features(void);
const char *grpc_lite_error_string(grpc_lite_error error_code);

/*
 * Runtime initialization must happen before the application creates threads.
 * Only one Runtime may be active. Destroy it after all dependent handles.
 * Every non-NULL owning handle must be destroyed exactly once.
 */
grpc_lite_error grpc_lite_runtime_create(grpc_lite_runtime **out_runtime);
void grpc_lite_runtime_destroy(grpc_lite_runtime *runtime);

/* Metadata handles require external synchronization when shared across threads. */
grpc_lite_error grpc_lite_metadata_create(grpc_lite_metadata **out_metadata);
void grpc_lite_metadata_destroy(grpc_lite_metadata *metadata);
/* Key and value bytes are copied before this function returns. */
grpc_lite_error grpc_lite_metadata_add(
    grpc_lite_metadata *metadata,
    grpc_lite_bytes_view key,
    grpc_lite_bytes_view value);
size_t grpc_lite_metadata_count(const grpc_lite_metadata *metadata);
/* Returned views are immutable and remain valid until metadata is destroyed. */
grpc_lite_error grpc_lite_metadata_at(
    const grpc_lite_metadata *metadata,
    size_t index,
    grpc_lite_metadata_entry_view *out_entry);

enum {
  GRPC_LITE_COMPRESSION_IDENTITY = 0,
  GRPC_LITE_COMPRESSION_GZIP = 1,
};

typedef struct grpc_lite_unary_options {
  size_t struct_size;
  const grpc_lite_metadata *metadata;
  uint32_t has_timeout;
  uint32_t request_compression;
  uint64_t timeout_ns;
  uint64_t max_response_size;
} grpc_lite_unary_options;

#define GRPC_LITE_UNARY_OPTIONS_INIT \
  { sizeof(grpc_lite_unary_options), NULL, 0, GRPC_LITE_COMPRESSION_IDENTITY, \
    0, UINT64_C(4194304) }

typedef struct grpc_lite_channel_options {
  size_t struct_size;
  uint32_t allow_initial_offline;
  uint64_t initial_backoff_ns;
  uint64_t max_backoff_ns;
  uint32_t multiplier_millis;
  uint32_t jitter_percent;
  const grpc_lite_logger *logger;
  /* Finite cumulative response metadata limit; valid range is 1..UINT32_MAX. */
  uint64_t max_response_header_list_size;
} grpc_lite_channel_options;

#define GRPC_LITE_CHANNEL_OPTIONS_INIT \
  { sizeof(grpc_lite_channel_options), 0, UINT64_C(1000000000), \
    UINT64_C(120000000000), 1600, 20, NULL, UINT64_C(65536) }

/* Creates a connected insecure channel. Hostnames require a Runtime. */
grpc_lite_error grpc_lite_channel_create(
    grpc_lite_runtime *runtime,
    grpc_lite_bytes_view target,
    grpc_lite_channel **out_channel);
/*
 * Creates an insecure channel that reconnects after connection loss.
 * NULL options select GRPC_LITE_CHANNEL_OPTIONS_INIT defaults. With
 * allow_initial_offline set, creation may succeed before the first connection.
 * Hostnames require a Runtime. The target and options are borrowed for this call.
 */
grpc_lite_error grpc_lite_channel_create_managed(
    grpc_lite_runtime *runtime,
    grpc_lite_bytes_view target,
    const grpc_lite_channel_options *options,
    grpc_lite_channel **out_channel);
/*
 * Starts shutdown and promptly completes active calls. Thread-safe with calls.
 * grpc_lite_channel_wait blocks until shutdown completes and must be called only
 * after shutdown. Neither function releases the handle; NULL is a no-op.
 */
void grpc_lite_channel_shutdown(grpc_lite_channel *channel);
void grpc_lite_channel_wait(grpc_lite_channel *channel);
/*
 * Implicitly shuts down and waits, then releases the handle. Requires exclusive
 * access after all concurrent calls have returned. NULL is a no-op.
 */
void grpc_lite_channel_destroy(grpc_lite_channel *channel);

/* Blocks until the RPC completes. RPC failures are stored in out_result. */
grpc_lite_error grpc_lite_channel_call_unary(
    grpc_lite_channel *channel,
    grpc_lite_bytes_view full_method_path,
    grpc_lite_bytes_view request,
    const grpc_lite_unary_options *options,
    grpc_lite_unary_result **out_result);
void grpc_lite_unary_result_destroy(grpc_lite_unary_result *result);
int32_t grpc_lite_unary_result_status_code(const grpc_lite_unary_result *result);
grpc_lite_bytes_view grpc_lite_unary_result_status_message(
    const grpc_lite_unary_result *result);
grpc_lite_bytes_view grpc_lite_unary_result_payload(
    const grpc_lite_unary_result *result);
uint32_t grpc_lite_unary_result_response_compression(
    const grpc_lite_unary_result *result);
/* trailing == 0 selects initial metadata; any other value selects trailing. */
size_t grpc_lite_unary_result_metadata_count(
    const grpc_lite_unary_result *result, uint32_t trailing);
grpc_lite_error grpc_lite_unary_result_metadata_at(
    const grpc_lite_unary_result *result,
    uint32_t trailing,
    size_t index,
    grpc_lite_metadata_entry_view *out_entry);

/* Metadata views are borrowed and remain valid only during their callback. */
size_t grpc_lite_metadata_view_count(const grpc_lite_metadata_view *metadata);
grpc_lite_error grpc_lite_metadata_view_at(
    const grpc_lite_metadata_view *metadata,
    size_t index,
    grpc_lite_metadata_entry_view *out_entry);

enum {
  GRPC_LITE_RECEIVE_CONTINUE = 0,
  GRPC_LITE_RECEIVE_PAUSE = 1,
};

typedef struct grpc_lite_client_stream_options {
  size_t struct_size;
  const grpc_lite_metadata *metadata;
  uint32_t has_timeout;
  uint32_t send_compression;
  uint64_t timeout_ns;
  uint64_t max_message_size;
  uint64_t max_inbound_buffer_size;
  uint64_t max_outbound_buffer_size;
} grpc_lite_client_stream_options;

#define GRPC_LITE_CLIENT_STREAM_OPTIONS_INIT \
  { sizeof(grpc_lite_client_stream_options), NULL, 0, \
    GRPC_LITE_COMPRESSION_IDENTITY, 0, UINT64_C(4194304), \
    UINT64_C(8388608), UINT64_C(8388608) }

typedef struct grpc_lite_client_stream_callbacks {
  size_t struct_size;
  void *user_data;
  void (*on_headers)(
      void *user_data,
      grpc_lite_client_stream *stream,
      const grpc_lite_metadata_view *headers);
  uint32_t (*on_message)(
      void *user_data,
      grpc_lite_client_stream *stream,
      grpc_lite_bytes_view payload,
      uint32_t compression);
  void (*on_remote_end)(
      void *user_data,
      grpc_lite_client_stream *stream);
  void (*on_writable)(
      void *user_data,
      grpc_lite_client_stream *stream);
  void (*on_terminal)(
      void *user_data,
      grpc_lite_client_stream *stream,
      int32_t status_code,
      grpc_lite_bytes_view status_message,
      const grpc_lite_metadata_view *trailing_metadata);
} grpc_lite_client_stream_callbacks;

/* on_message and on_terminal are required. Callback data is borrowed. */
#define GRPC_LITE_CLIENT_STREAM_CALLBACKS_INIT \
  { sizeof(grpc_lite_client_stream_callbacks), NULL, NULL, NULL, NULL, NULL, NULL }

grpc_lite_error grpc_lite_channel_open_stream(
    grpc_lite_channel *channel,
    grpc_lite_bytes_view full_method_path,
    const grpc_lite_client_stream_options *options,
    const grpc_lite_client_stream_callbacks *callbacks,
    grpc_lite_client_stream **out_stream);
/* Commands are thread-safe while the owning handle remains alive. */
/* GRPC_LITE_ERROR_WOULD_BLOCK is retryable. */
grpc_lite_error grpc_lite_client_stream_send(
    grpc_lite_client_stream *stream,
    grpc_lite_bytes_view payload,
    uint32_t compression);
grpc_lite_error grpc_lite_client_stream_close_send(
    grpc_lite_client_stream *stream);
void grpc_lite_client_stream_cancel(grpc_lite_client_stream *stream);
grpc_lite_error grpc_lite_client_stream_resume_receive(
    grpc_lite_client_stream *stream);
/* Destroy exactly once, outside callbacks and synchronized with all commands. */
void grpc_lite_client_stream_destroy(grpc_lite_client_stream *stream);

typedef struct grpc_lite_server_options {
  size_t struct_size;
  grpc_lite_bytes_view host;
  uint32_t port;
  uint32_t reactor_count;
  uint64_t max_message_size;
  uint64_t max_inbound_buffer_size;
  uint64_t max_outbound_buffer_size;
  const grpc_lite_logger *logger;
  /* Aggregate across every reactor owned by this server. */
  uint64_t max_connections;
  /* Absolute cleartext deadline through the initial non-ACK SETTINGS. */
  uint64_t cleartext_preface_timeout_ns;
  /* Maximum continuous interval with no active inbound RPC streams. */
  uint64_t connection_idle_timeout_ns;
  /* Simultaneous inbound HTTP/2 request streams on one connection. */
  uint32_t max_concurrent_streams_per_connection;
} grpc_lite_server_options;

/* Resource limits and timeouts are finite and must all be nonzero. */
#define GRPC_LITE_SERVER_OPTIONS_INIT \
  { sizeof(grpc_lite_server_options), \
    { (const uint8_t *)"127.0.0.1", sizeof("127.0.0.1") - 1 }, 0, 1, \
    UINT64_C(4194304), UINT64_C(8388608), UINT64_C(8388608), NULL, \
    UINT64_C(1024), UINT64_C(10000000000), UINT64_C(300000000000), 100 }

typedef struct grpc_lite_server_method_options {
  size_t struct_size;
  uint32_t receive_initially_paused;
  uint32_t explicit_initial_metadata;
} grpc_lite_server_method_options;

#define GRPC_LITE_SERVER_METHOD_OPTIONS_INIT \
  { sizeof(grpc_lite_server_method_options), 0, 1 }

enum {
  GRPC_LITE_SERVER_TERMINAL_COMPLETED = 0,
  GRPC_LITE_SERVER_TERMINAL_PEER_CANCELLED = 1,
  GRPC_LITE_SERVER_TERMINAL_DEADLINE_EXCEEDED = 2,
  GRPC_LITE_SERVER_TERMINAL_SERVER_SHUTDOWN = 3,
  GRPC_LITE_SERVER_TERMINAL_TRANSPORT_ERROR = 4,
  GRPC_LITE_SERVER_TERMINAL_LOCAL_ERROR = 5,
};

typedef struct grpc_lite_server_method_callbacks {
  size_t struct_size;
  void *user_data;
  void (*on_start)(
      void *user_data,
      grpc_lite_server_stream *stream,
      const grpc_lite_server_context *context);
  uint32_t (*on_message)(
      void *user_data,
      grpc_lite_server_stream *stream,
      const grpc_lite_server_context *context,
      grpc_lite_bytes_view payload,
      uint32_t compression);
  void (*on_remote_end)(
      void *user_data,
      grpc_lite_server_stream *stream,
      const grpc_lite_server_context *context);
  void (*on_writable)(
      void *user_data,
      grpc_lite_server_stream *stream,
      const grpc_lite_server_context *context);
  void (*on_cancel)(
      void *user_data,
      grpc_lite_server_stream *stream,
      const grpc_lite_server_context *context);
  void (*on_terminal)(
      void *user_data,
      size_t call_id,
      uint32_t reason);
} grpc_lite_server_method_callbacks;

#define GRPC_LITE_SERVER_METHOD_CALLBACKS_INIT \
  { sizeof(grpc_lite_server_method_callbacks), NULL, NULL, NULL, NULL, NULL, NULL, NULL }

/* on_message is required. Callbacks run on reactor threads and must not block. */
/* Different calls may invoke the same user_data concurrently. */
/* Server configuration, start, wait, and destroy require external serialization. */
/* wait and destroy must not be called from a server callback. */
grpc_lite_error grpc_lite_server_create(
    const grpc_lite_server_options *options,
    grpc_lite_server **out_server);
grpc_lite_error grpc_lite_server_register_stream(
    grpc_lite_server *server,
    grpc_lite_bytes_view full_method_path,
    const grpc_lite_server_method_options *options,
    const grpc_lite_server_method_callbacks *callbacks);
grpc_lite_error grpc_lite_server_start(grpc_lite_server *server);
grpc_lite_error grpc_lite_server_port(
    const grpc_lite_server *server,
    uint32_t *out_port);
void grpc_lite_server_shutdown(grpc_lite_server *server);
void grpc_lite_server_shutdown_gracefully(
    grpc_lite_server *server,
    uint64_t timeout_ns);
void grpc_lite_server_wait(grpc_lite_server *server);
/* All retained calls must be destroyed before their server. */
void grpc_lite_server_destroy(grpc_lite_server *server);

/* Server stream and context pointers are borrowed for their callback only. */
size_t grpc_lite_server_stream_id(const grpc_lite_server_stream *stream);
grpc_lite_error grpc_lite_server_stream_retain(
    const grpc_lite_server_stream *stream,
    grpc_lite_server_call **out_call);

/* Retained server-call commands copy all input before returning. */
/* Commands are thread-safe while the handle remains alive. */
/* Destroy must be synchronized with all commands using that handle. */
grpc_lite_error grpc_lite_server_call_clone(
    const grpc_lite_server_call *call,
    grpc_lite_server_call **out_call);
void grpc_lite_server_call_destroy(grpc_lite_server_call *call);
size_t grpc_lite_server_call_id(const grpc_lite_server_call *call);
uint32_t grpc_lite_server_call_is_cancelled(const grpc_lite_server_call *call);
uint32_t grpc_lite_server_call_is_terminal(const grpc_lite_server_call *call);
/* Allocation-free emergency request. Sends RST_STREAM(INTERNAL_ERROR), or closes */
/* the connection if reset submission fails. Inactive calls are unchanged. */
void grpc_lite_server_call_abort(grpc_lite_server_call *call);
grpc_lite_error grpc_lite_server_call_send_initial_metadata(
    grpc_lite_server_call *call,
    const grpc_lite_metadata *metadata,
    uint32_t compression);
grpc_lite_error grpc_lite_server_call_send(
    grpc_lite_server_call *call,
    grpc_lite_bytes_view payload,
    uint32_t compression);
grpc_lite_error grpc_lite_server_call_finish(
    grpc_lite_server_call *call,
    uint32_t status_code,
    grpc_lite_bytes_view status_message,
    const grpc_lite_metadata *trailing_metadata);
/*
 * May be called during on_message before a returned PAUSE has been committed.
 * Repeated resume requests for the same pause are idempotent.
 */
grpc_lite_error grpc_lite_server_call_resume_receive(
    grpc_lite_server_call *call);

const grpc_lite_metadata_view *grpc_lite_server_context_request_metadata(
    const grpc_lite_server_context *context);
uint32_t grpc_lite_server_context_request_compression(
    const grpc_lite_server_context *context);
uint32_t grpc_lite_server_context_has_deadline(
    const grpc_lite_server_context *context);
uint64_t grpc_lite_server_context_remaining_time_ns(
    const grpc_lite_server_context *context);

#ifdef __cplusplus
}
#endif

#endif
