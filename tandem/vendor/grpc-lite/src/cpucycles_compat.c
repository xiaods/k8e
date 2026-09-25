#include <stdint.h>
#include <time.h>

static int64_t cpucycles_monotonic(void) {
    struct timespec value;
    if (clock_gettime(CLOCK_MONOTONIC, &value) != 0) return -1;
    return (int64_t)value.tv_sec * 1000000000 + value.tv_nsec;
}

int64_t (*cpucycles)(void) = cpucycles_monotonic;

const char *cpucycles_implementation(void) {
    return "default-monotonic";
}
