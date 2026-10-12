# Vendored gRPC HTTP/2 test server

- Upstream: https://github.com/grpc/grpc
- Commit: `8542e01ff47eb07247ff6cfbd545f3b6f4e9b5d3`
- Source path: `test/http2_test`
- License: Apache-2.0, copied as `LICENSE`

This directory contains only the Python files imported by
`http2_test_server.py`. The Python 3 compatibility changes formerly stored in
`tests/official/http2-test-python3.patch` are applied to the vendored files.

Runtime dependencies are vendored as hash-locked wheels for CPython 3.11 on
Linux x86_64 and aarch64. All packages except `zope.interface` are
platform-independent. The h2 and hpack wheels omit their top-level MIT license
files, so copies from their matching upstream tags are retained under
`licenses/` and copied into the image. Other package notices remain in wheel
metadata. Update source, requirements, wheel hashes, and the local image tag in
`tests/official/run_http2_edge.sh` together.

The original upstream interop image included Go and C/C++ build toolchains that
the Python test server does not use. The local image intentionally contains only
the Python runtime closure needed by the eight grpc-lite edge cases.
