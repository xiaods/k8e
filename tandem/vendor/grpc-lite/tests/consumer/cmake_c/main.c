#include <grpc_lite/grpc_lite.h>

#include <stdint.h>
#include <string.h>

int main(void) {
  grpc_lite_metadata *metadata = NULL;
  grpc_lite_runtime *runtime = NULL;

  if (grpc_lite_abi_version() != GRPC_LITE_ABI_VERSION) return 1;
  if (strcmp(grpc_lite_library_version(), GRPC_LITE_EXPECTED_VERSION) != 0) return 2;
  if ((grpc_lite_features() & GRPC_LITE_FEATURE_C_SERVER) == 0) return 3;

  if (grpc_lite_metadata_create(&metadata) != GRPC_LITE_OK) return 4;
  if (grpc_lite_metadata_count(metadata) != 0) return 5;
  grpc_lite_metadata_destroy(metadata);

  if (grpc_lite_runtime_create(&runtime) != GRPC_LITE_OK) return 6;
  grpc_lite_runtime_destroy(runtime);
  return 0;
}
