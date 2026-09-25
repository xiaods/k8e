#include <stddef.h>

#if GRPC_LITE_ENABLE_GPERFTOOLS
extern int ProfilerStart(const char *path);
#endif
#if GRPC_LITE_ENABLE_GPERFTOOLS && GRPC_LITE_ENABLE_TCMALLOC
extern void HeapProfilerStart(const char *prefix);
#endif
#if GRPC_LITE_ENABLE_TCMALLOC
extern void *tc_malloc(size_t size);

char *strdup(const char *source) {
    size_t length = 0;
    while (source[length] != '\0') length++;

    char *copy = tc_malloc(length + 1);
    if (copy == NULL) return NULL;
    for (size_t i = 0; i <= length; i++) copy[i] = source[i];
    return copy;
}
#endif

__attribute__((used, retain)) static const void *grpc_lite_gperftools_symbols[] = {
#if GRPC_LITE_ENABLE_GPERFTOOLS
    (const void *)&ProfilerStart,
#endif
#if GRPC_LITE_ENABLE_GPERFTOOLS && GRPC_LITE_ENABLE_TCMALLOC
    (const void *)&HeapProfilerStart,
#endif
#if GRPC_LITE_ENABLE_TCMALLOC
    (const void *)&tc_malloc,
#endif
};
