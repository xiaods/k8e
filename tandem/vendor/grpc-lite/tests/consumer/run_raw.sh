#!/usr/bin/env bash
set -euo pipefail

project_root=$(cd "$(dirname "${BASH_SOURCE[0]}")/../.." && pwd)
work_dir=$(mktemp -d "${TMPDIR:-/tmp}/grpc-lite-raw-consumer.XXXXXX")
trap 'rm -rf "$work_dir"' EXIT

archive="$work_dir/grpc-lite.tar.gz"
consumer="$work_dir/consumer"
global_cache="$work_dir/global-cache"
mkdir -p "$consumer/src" "$global_cache/p"

prefetch_package() {
    local url=$1
    local expected_hash=$2
    local archive_name=$3
    local dependency_archive="$work_dir/$archive_name"

    curl --fail --location --silent --show-error --output "$dependency_archive" "$url"
    local actual_hash
    actual_hash=$(ZIG_GLOBAL_CACHE_DIR="$global_cache" zig fetch "$dependency_archive")
    if [[ "$actual_hash" != "$expected_hash" ]]; then
        printf 'package hash mismatch for %s: expected %s, got %s\n' \
            "$archive_name" "$expected_hash" "$actual_hash" >&2
        exit 1
    fi
}

prefetch_url_package() {
    local url=$1
    local expected_hash=$2
    local archive_name=$3
    local dependency_archive="$work_dir/$archive_name"
    local package_dir="$global_cache/p/$expected_hash"

    curl --fail --location --silent --show-error --output "$dependency_archive" "$url"
    local actual_hash
    actual_hash=$(ZIG_GLOBAL_CACHE_DIR="$global_cache" zig fetch "$dependency_archive")
    if [[ "$actual_hash" != "$expected_hash" ]]; then
        printf 'package hash mismatch for %s: expected %s, got %s\n' \
            "$archive_name" "$expected_hash" "$actual_hash" >&2
        exit 1
    fi
    mkdir -p "$package_dir"
    tar --extract --gzip --file "$dependency_archive" --strip-components=1 --directory "$package_dir"
    tar --create --gzip --file "$global_cache/p/$expected_hash.tar.gz" --directory "$package_dir" .
}

prefetch_zigfetch_url_package() {
    local url=$1
    local expected_hash=$2
    local archive_name=$3
    local output
    local actual_hash

    output=$(ZIG_GLOBAL_CACHE_DIR="$global_cache" zigfetch "$url" 2>&1)
    actual_hash=${output##*$'\n'}
    if [[ "$actual_hash" != "$expected_hash" ]]; then
        printf 'package hash mismatch for %s: expected %s, got %s\n' \
            "$archive_name" "$expected_hash" "$actual_hash" >&2
        exit 1
    fi
    curl --fail --location --silent --show-error \
        --output "$global_cache/p/$expected_hash.tar.gz" "$url"
}

prefetch_url_package \
    'https://codeload.github.com/mitchellh/libxev/tar.gz/b0650f082458226860ed7ab0fc7c9c73823c8950' \
    'libxev-0.0.0-86vtcxkOFACqPXUTAPuq5i0xpDYWU5G5RfrYQXxlUT26' \
    'libxev.tar.gz'
prefetch_package \
    'https://codeload.github.com/nghttp2/nghttp2/tar.gz/68cb6900fde14c77f0cd7add0e094a862960eb99' \
    'N-V-__8AAPOqVwAHvwAVJJjhhX72DyDtjWw--9WUZf3-uKRX' \
    'nghttp2.tar.gz'
prefetch_package \
    'https://github.com/c-ares/c-ares/releases/download/v1.34.8/c-ares-1.34.8.tar.gz' \
    'N-V-__8AADDhTgDOiesa_sidmxGBzfPdF3OWU2HXS2GNZmVp' \
    'cares.tar.gz'
prefetch_zigfetch_url_package \
    'https://cpucycles.cr.yp.to/libcpucycles-20260625.tar.gz' \
    'N-V-__8AAHSUBAA_Vn8NXM2L9F21QFvrTIxbH9yvxs5cO-lY' \
    'cpucycles.tar.gz'
prefetch_url_package \
    'https://github.com/wyzdwdz/nanozlog/archive/e693c11976d55ba0a5b8deeaaaf9f1c5cc30eba9.tar.gz' \
    'nanozlog-0.1.0-5UtdH535AADW7HUBpfLboKJkdB1IVYaqz7YFQ2dHIcqL' \
    'nanozlog.tar.gz'
prefetch_url_package \
    'https://github.com/rockorager/zeit/archive/b1c1c2fcbc71fd7799a316bbcf0ff88d06d80ccc.tar.gz' \
    'zeit-0.9.0-5I6bk2m9AgBSMH8-L6rYJkwuQAyhXplnfxnvTSGzVHUR' \
    'zeit.tar.gz'
prefetch_url_package \
    'https://codeload.github.com/fanyang89/gperftools/tar.gz/1a01cd2cf8f7000845d343fa8e0bbac70378858b' \
    'N-V-__8AAGKVkABvsDVHhSU8seHKvtJ8Q23b9Y0OMiFVWt-y' \
    'gperftools.tar.gz'

git -C "$project_root" archive --format=tar.gz --output="$archive" HEAD
package_hash=$(ZIG_GLOBAL_CACHE_DIR="$global_cache" zig fetch "$archive")

cp "$project_root/tests/consumer/raw/build.zig" "$consumer/build.zig"
cp "$project_root/tests/consumer/raw/src/main.zig" "$consumer/src/main.zig"
cp "$project_root/tests/consumer/raw/src/gperftools.zig" "$consumer/src/gperftools.zig"
cat >"$consumer/build.zig.zon" <<EOF
.{
    .name = .grpc_lite_raw_consumer,
    .version = "0.0.0",
    .minimum_zig_version = "0.16.0",
    .fingerprint = 0x7b2e24f38b336dd0,
    .dependencies = .{
        .grpc_lite = .{
            .url = "file://$archive",
            .hash = "$package_hash",
        },
    },
    .paths = .{ "build.zig", "build.zig.zon", "src" },
}
EOF

ZIG_GLOBAL_CACHE_DIR="$global_cache" zig build --build-file "$consumer/build.zig" --summary all
ZIG_GLOBAL_CACHE_DIR="$global_cache" zig build --build-file "$consumer/build.zig" \
    -Dtarget=x86_64-linux-gnu -Doptimize=ReleaseSafe \
    -Dsanitize-thread=true -Dsanitize-c=false --summary all
ZIG_GLOBAL_CACHE_DIR="$global_cache" zig build --build-file "$consumer/build.zig" \
    -Doptimize=Debug -Dsanitize-thread=false -Dsanitize-c=true --summary all
mbedtls_path="$consumer/zig-pkg/N-V-__8AAI1FmQLj92go7nR7_J6kwr-7uYyuitngGqydvWkd"
if [[ -e "$mbedtls_path" || -L "$mbedtls_path" ]]; then
    printf '%s\n' 'raw consumer unexpectedly resolved optional mbedTLS' >&2
    exit 1
fi
prefetch_zigfetch_url_package \
    'https://codeload.github.com/Mbed-TLS/mbedtls/tar.gz/refs/tags/mbedtls-3.6.6' \
    'N-V-__8AAI1FmQLj92go7nR7_J6kwr-7uYyuitngGqydvWkd' \
    'mbedtls.tar.gz'
ZIG_GLOBAL_CACHE_DIR="$global_cache" zig build --build-file "$consumer/build.zig" \
    -Dtls=true --summary all
ZIG_GLOBAL_CACHE_DIR="$global_cache" zig build --build-file "$consumer/build.zig" \
    -Doptimize=ReleaseFast -Dgperftools=true --summary all
"$consumer/zig-out/bin/grpc-lite-raw-consumer"

if find "$consumer/zig-pkg" -maxdepth 1 -name 'protobuf-*' -print -quit | grep -q .; then
    printf '%s\n' 'raw -Dprotobuf=false consumer unexpectedly resolved zig-protobuf' >&2
    exit 1
fi
grpc_proto_path="$consumer/zig-pkg/N-V-__8AAMvUAgBnxziu_Kuqr4AuJ00Vke6R0R-Rc1A26DRX"
if [[ -e "$grpc_proto_path" || -L "$grpc_proto_path" ]]; then
    printf '%s\n' 'raw consumer unexpectedly resolved grpc-proto' >&2
    exit 1
fi
