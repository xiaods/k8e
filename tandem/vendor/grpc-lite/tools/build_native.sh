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

c_flags=(-fvisibility=hidden)

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

common_options=(
    -S "$source_dir"
    -B "$build_dir"
    -G Ninja
    "-DCMAKE_BUILD_TYPE=$build_type"
    "-DCMAKE_C_FLAGS=${c_flags[*]}"
    "-DCMAKE_CXX_FLAGS=${c_flags[*]}"
    -DBUILD_SHARED_LIBS=OFF
)

case "$library" in
    nghttp2)
        CC="$cc" CXX="$cxx" cmake "${common_options[@]}" \
            -DENABLE_LIB_ONLY=ON \
            -DENABLE_APP=OFF \
            -DENABLE_EXAMPLES=OFF \
            -DENABLE_HPACK_TOOLS=OFF \
            -DENABLE_DOC=OFF \
            -DBUILD_TESTING=OFF \
            -DBUILD_STATIC_LIBS=ON
        ;;
    cares)
        CC="$cc" CXX="$cxx" cmake "${common_options[@]}" \
            -DCMAKE_INSTALL_LIBDIR=lib \
            -DCARES_STATIC=ON \
            -DCARES_SHARED=OFF \
            -DCARES_INSTALL=OFF \
            -DCARES_BUILD_TESTS=OFF \
            -DCARES_BUILD_CONTAINER_TESTS=OFF \
            -DCARES_BUILD_TOOLS=OFF \
            -DCARES_THREADS=ON
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
        enable_heap_profiler=false
        if [[ "$enable_gperftools" == true && "$enable_tcmalloc" == true ]]; then
            enable_heap_profiler=true
        fi
        CC="$cc" CXX="$cxx" cmake "${common_options[@]}" \
            -DBUILD_TESTING=OFF \
            -DGPERFTOOLS_BUILD_CPU_PROFILER="$enable_gperftools" \
            -DGPERFTOOLS_BUILD_HEAP_PROFILER="$enable_heap_profiler" \
            -DGPERFTOOLS_BUILD_DEBUGALLOC=OFF \
            -Dgperftools_build_minimal=OFF \
            -Dgperftools_build_benchmark=OFF \
            -Dgperftools_enable_libunwind=OFF \
            -Dgperftools_enable_frame_pointers=ON
        if [[ "$enable_heap_profiler" == true ]]; then
            targets=(tcmalloc profiler)
            archives=(libtcmalloc.a libprofiler.a libstacktrace.a liblow_level_alloc.a libcommon.a)
        elif [[ "$enable_tcmalloc" == true ]]; then
            targets=(tcmalloc_minimal)
            archives=(libtcmalloc_minimal.a libcommon.a)
        else
            targets=(profiler)
            archives=(libprofiler.a libstacktrace.a libcommon.a)
        fi
        cmake --build "$build_dir" --target "${targets[@]}"
        archive="$build_dir/libgrpc_lite_gperftools.a"
        rm -f "$archive"
        {
            printf 'CREATE %s\n' "$archive"
            for dependency in "${archives[@]}"; do
                printf 'ADDLIB %s/%s\n' "$build_dir" "$dependency"
            done
            printf 'SAVE\nEND\n'
        } | "$zig_exe" ar -M
        gperftools_define=0
        tcmalloc_define=0
        [[ "$enable_gperftools" == true ]] && gperftools_define=1
        [[ "$enable_tcmalloc" == true ]] && tcmalloc_define=1
        "$zig_exe" cc -target "$target_triple" "${c_flags[@]}" \
            -DGRPC_LITE_ENABLE_GPERFTOOLS="$gperftools_define" \
            -DGRPC_LITE_ENABLE_TCMALLOC="$tcmalloc_define" \
            -c "$force_link_source" \
            -o "$build_dir/gperftools_force_link.o"
        exit 0
        ;;
    *)
        printf 'unsupported native library: %s\n' "$library" >&2
        exit 1
        ;;
esac

cmake --build "$build_dir"
