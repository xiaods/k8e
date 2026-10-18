#include <time.h>

/* Match the libcpucycles function-pointer ABI; int64_t can be long. */
static long long cpucycles_monotonic(void) {
    struct timespec value;
    if (clock_gettime(CLOCK_MONOTONIC, &value) != 0) return -1;
    return (long long)value.tv_sec * 1000000000LL + value.tv_nsec;
}

long long (*cpucycles)(void) = cpucycles_monotonic;

const char *cpucycles_implementation(void) {
    return "default-monotonic";
}
