
## Platform support

Tandem builds only on Linux. Its native C dependencies (nghttp2, c-ares,
mbedtls, cpucycles) are cross-compiled with `tools/build_native.sh`, which
invokes CMake + Ninja with a `zig cc -target <triple>` toolchain; the CMake
probe injects macOS-host-only flags (`-arch`, `-isysroot`) that zig cc rejects
for non-host targets. CI therefore builds and tests Tandem exclusively on
Linux runners (`zig build persistence-probe test -Dtarget=x86_64-linux-musl`).

On macOS hosts use `make` (which builds `sandboxd` and the Go binaries only)
and rely on CI for Tandem verification.
