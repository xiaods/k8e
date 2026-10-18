#ifndef GRPC_LITE_MBEDTLS_USER_CONFIG_H
#define GRPC_LITE_MBEDTLS_USER_CONFIG_H

#define MBEDTLS_THREADING_C
#define MBEDTLS_THREADING_PTHREAD

/* ECP self-test counters are global and unsynchronized, even in threaded builds.
 * Runtime TLS does not need the standalone mbedTLS self-test entry points. */
#undef MBEDTLS_SELF_TEST

#endif
