#!/usr/bin/env bash
set -euo pipefail

library=$1
source_dir=$2
build_dir=$3
build_type=$4
cc=$5
cxx=$6
zig_exe=$7
target_triple=$8
force_link_source=$9
mbedtls_config_header=${10}
sanitize_thread=${11}
sanitize_c=${12}
enable_gperftools=${13}
enable_tcmalloc=${14}

c_flags=(-fvisibility=hidden -include limits.h)

mkdir -p "$build_dir"

if [[ "$sanitize_thread" == true && "$sanitize_c" == true ]]; then
    printf 'ThreadSanitizer and UndefinedBehaviorSanitizer are mutually exclusive\n' >&2
    exit 1
fi

if [[ "$sanitize_thread" == true ]]; then
    c_flags+=(-fsanitize=thread)
fi
if [[ "$sanitize_c" == true ]]; then
    c_flags+=(-fsanitize=undefined -fno-sanitize-recover=undefined)
else
    c_flags+=(-fno-sanitize=undefined)
fi
if [[ "$sanitize_thread" == true || "$sanitize_c" == true ]]; then
    c_flags+=(-fno-omit-frame-pointer)
fi

if [[ "$sanitize_thread" == true || "$sanitize_c" == true ]]; then
    c_flags+=(-g)
fi

build_zig_static() {
    local library=$1
    local include_dir=$2
    local source_glob=$3
    local archive="$build_dir/lib/lib${library}.a"
    local object_dir="$build_dir/objects"
    mkdir -p "$object_dir" "$(dirname "$archive")"
    case "$library" in
        nghttp2)
            cat >"$build_dir/config.h" <<'EOF'
#define HAVE_ARPA_INET_H 1
#define HAVE_NETINET_IN_H 1
#define HAVE_NETDB_H 1
#define HAVE_SYS_SOCKET_H 1
#define HAVE_SYS_UIO_H 1
#define HAVE_SYS_IOCTL_H 1
#define HAVE_ERRNO_H 1
#define HAVE_UNISTD_H 1
#define HAVE_TIME_H 1
#define HAVE_CLOCK_GETTIME_MONOTONIC 1
#define HAVE_FCNTL_H 1
#define HAVE_FCNTL_O_NONBLOCK 1
#define HAVE_WRITEV 1
#define RECVFROM_TYPE_ARG3 size_t
#define SEND_TYPE_ARG1 int
#define SEND_TYPE_ARG2 const void *
#define SEND_TYPE_ARG3 size_t
#define SEND_TYPE_ARG4 int
#define HAVE_PF_INET6 1
#define HAVE_AF_INET6 1
#define HAVE_STRUCT_SOCKADDR_IN6 1
#define HAVE_STRUCT_ADDRINFO 1
EOF
            mkdir -p "$build_dir/include/nghttp2"
            sed -e 's/@PACKAGE_VERSION@/1.61.0/g' \
                -e 's/@PACKAGE_VERSION_NUM@/0x013d00/g' \
                "$source_dir/lib/includes/nghttp2/nghttp2ver.h.in" \
                >"$build_dir/include/nghttp2/nghttp2ver.h"
            ;;
        cares)
            cat >"$build_dir/ares_config.h" <<'EOF'
#define HAVE_ARPA_INET_H 1
#define HAVE_NETINET_IN_H 1
#define HAVE_NETDB_H 1
#define HAVE_SYS_SOCKET_H 1
#define HAVE_SYS_SELECT_H 1
#define HAVE_SYS_TIME_H 1
#define HAVE_SYS_UIO_H 1
#define HAVE_SYS_IOCTL_H 1
#define HAVE_ERRNO_H 1
#define HAVE_UNISTD_H 1
#define HAVE_TIME_H 1
#define HAVE_CLOCK_GETTIME_MONOTONIC 1
#define HAVE_FCNTL_H 1
#define HAVE_FCNTL_O_NONBLOCK 1
#define HAVE_WRITEV 1
#define RECVFROM_TYPE_ARG3 size_t
#define SEND_TYPE_ARG1 int
#define SEND_TYPE_ARG2 const void *
#define SEND_TYPE_ARG3 size_t
#define SEND_TYPE_ARG4 int
#define HAVE_STRUCT_TIMEVAL 1
#define HAVE_IPV6 1
#define HAVE_PF_INET6 1
#define HAVE_AF_INET6 1
#define HAVE_STRUCT_SOCKADDR_IN6 1
#define HAVE_STRUCT_ADDRINFO 1
EOF
            ;;
    esac

    sources=()
    while IFS= read -r source; do
        sources+=("$source")
    done < <(find "$source_dir/$source_glob" -type f -name '*.c' | sort)
    if [[ ${#sources[@]} -eq 0 ]]; then
        printf 'no C sources found for %s\n' "$library" >&2
        exit 1
    fi

    objects=()
    for source in "${sources[@]}"; do
        object="$object_dir/$(basename "${source%.c}").o"
        "$zig_exe" cc -target "$target_triple" "${c_flags[@]}" \
            -I"$build_dir" -I"$build_dir/include" -I"$include_dir" -I"$source_dir" -I"$source_dir/src" -I"$source_dir/src/lib" -I"$source_dir/src/lib/include" \
            -D_GNU_SOURCE -DHAVE_CONFIG_H=0 -c "$source" -o "$object"
        objects+=("$object")
    done
    "$zig_exe" ar rcs "$archive" "${objects[@]}"
}

case "$library" in
    nghttp2)
        build_zig_static nghttp2 "$source_dir/lib/includes" lib
        mkdir -p "$build_dir/lib/includes"
        cp -a "$source_dir/lib/includes/." "$build_dir/lib/includes/"
        exit 0
        ;;
    cares)
        build_zig_static cares "$source_dir/include" src/lib
        mkdir -p "$build_dir/include"
        cp -a "$source_dir/include/." "$build_dir/include/"
        exit 0
        ;;
    mbedtls)
        source_copy="$build_dir/source"
        mkdir -p "$source_copy"
        cp -a "$source_dir/." "$source_copy"
        touch \
            "$source_copy/library/error.c" \
            "$source_copy/library/version_features.c" \
            "$source_copy/library/ssl_debug_helpers_generated.c" \
            "$source_copy/library/psa_crypto_driver_wrappers.h" \
            "$source_copy/library/psa_crypto_driver_wrappers_no_static.c"
        case "$build_type" in
            Debug) make_cflags=(-O0 -g) ;;
            RelWithDebInfo) make_cflags=(-O2 -g) ;;
            *) make_cflags=(-O2) ;;
        esac
        mbedtls_config_dir=$(dirname "$mbedtls_config_header")
        make -C "$source_copy/library" -j "$(getconf _NPROCESSORS_ONLN)" \
            -o error.c \
            -o version_features.c \
            -o ssl_debug_helpers_generated.c \
            -o psa_crypto_driver_wrappers.h \
            -o psa_crypto_driver_wrappers_no_static.c \
            CC="$cc" \
            AR="$zig_exe ar" \
            AR_DASH= \
            CFLAGS="${make_cflags[*]} ${c_flags[*]} -I$mbedtls_config_dir -DMBEDTLS_USER_CONFIG_FILE=\\\"mbedtls_user_config.h\\\""
        archive="$build_dir/libmbedtls_combined.a"
        {
            printf 'CREATE %s\n' "$archive"
            printf 'ADDLIB %s/library/libmbedtls.a\n' "$source_copy"
            printf 'ADDLIB %s/library/libmbedx509.a\n' "$source_copy"
            printf 'ADDLIB %s/library/libmbedcrypto.a\n' "$source_copy"
            printf 'SAVE\nEND\n'
        } | "$zig_exe" ar -M
        exit 0
        ;;
    gperftools)
        printf 'gperftools is not supported by the Zig-only native builder\n' >&2
        exit 1
        ;;
    *)
        printf 'unsupported native library: %s\n' "$library" >&2
        exit 1
        ;;
esac
