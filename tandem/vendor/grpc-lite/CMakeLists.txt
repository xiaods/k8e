cmake_minimum_required(VERSION 3.20)
project(grpc_lite VERSION 0.4.0 LANGUAGES C)

if(NOT CMAKE_SYSTEM_NAME STREQUAL "Linux")
  message(FATAL_ERROR "grpc-lite currently supports Linux only")
endif()
if(NOT CMAKE_SYSTEM_PROCESSOR MATCHES "^(x86_64|amd64|AMD64)$")
  message(FATAL_ERROR "The grpc-lite transpiled C bundle supports x86_64 only")
endif()

option(GRPC_LITE_ENABLE_TLS "Request TLS (out of scope for the transpiled-C release bundle)" OFF)
option(GRPC_LITE_ENABLE_PROTOBUF "Expose optional official Protobuf C++ integration targets" OFF)
option(GRPC_LITE_NO_NETWORK "Reject dependency sources that could require network access" OFF)
set(GRPC_LITE_TRANSPILED_C_DIR "" CACHE PATH
  "Root of an extracted matching grpc-lite transpiled C bundle")
set(GRPC_LITE_NGHTTP2_URL "" CACHE STRING "Optional nghttp2 source tarball URL")
set(GRPC_LITE_NGHTTP2_URL_HASH "" CACHE STRING "Optional nghttp2 tarball hash (ALGO=value)")
set(GRPC_LITE_CARES_URL "" CACHE STRING "Optional c-ares source tarball URL")
set(GRPC_LITE_CARES_URL_HASH "" CACHE STRING "Optional c-ares tarball hash (ALGO=value)")

if(GRPC_LITE_ENABLE_TLS)
  message(FATAL_ERROR
    "TLS is out of scope for the insecure Linux x86_64 transpiled-C release bundle; configure with GRPC_LITE_ENABLE_TLS=OFF")
endif()

set(_grpc_lite_bundle_help
  "Git checkouts and GitHub automatic 'Source code' archives intentionally omit generated C. "
  "Either (1) download, verify, and extract the matching "
  "grpc-lite-transpiled-c-${PROJECT_VERSION}-linux-x86_64.tar.gz release asset and pass "
  "-DGRPC_LITE_TRANSPILED_C_DIR=<extracted-root>, or (2) install the pinned Zig toolchain, "
  "run 'mise run transpile-c', and explicitly pass that resulting directory.")
if(NOT GRPC_LITE_TRANSPILED_C_DIR)
  message(FATAL_ERROR "GRPC_LITE_TRANSPILED_C_DIR is required. ${_grpc_lite_bundle_help}")
endif()
get_filename_component(_grpc_lite_transpiled_dir
  "${GRPC_LITE_TRANSPILED_C_DIR}" ABSOLUTE BASE_DIR "${CMAKE_CURRENT_SOURCE_DIR}")
set(_grpc_lite_manifest "${_grpc_lite_transpiled_dir}/manifest.json")
if(NOT EXISTS "${_grpc_lite_manifest}")
  message(FATAL_ERROR "The transpiled C bundle manifest is missing: ${_grpc_lite_manifest}. ${_grpc_lite_bundle_help}")
endif()
file(READ "${_grpc_lite_manifest}" _grpc_lite_manifest_json)
string(JSON _grpc_lite_schema ERROR_VARIABLE _grpc_lite_json_error
  GET "${_grpc_lite_manifest_json}" schema_version)
if(NOT _grpc_lite_json_error STREQUAL "NOTFOUND" OR NOT _grpc_lite_schema EQUAL 1)
  message(FATAL_ERROR "The transpiled C bundle manifest must be valid JSON with schema_version 1")
endif()
string(JSON _grpc_lite_manifest_version ERROR_VARIABLE _grpc_lite_json_error
  GET "${_grpc_lite_manifest_json}" project_version)
if(NOT _grpc_lite_json_error STREQUAL "NOTFOUND" OR
    NOT _grpc_lite_manifest_version STREQUAL "${PROJECT_VERSION}")
  message(FATAL_ERROR
    "Transpiled C bundle version mismatch: expected ${PROJECT_VERSION}, got '${_grpc_lite_manifest_version}'")
endif()
string(JSON _grpc_lite_manifest_target ERROR_VARIABLE _grpc_lite_json_error
  GET "${_grpc_lite_manifest_json}" target)
if(NOT _grpc_lite_json_error STREQUAL "NOTFOUND" OR
    NOT _grpc_lite_manifest_target STREQUAL "x86_64-linux-gnu")
  message(FATAL_ERROR
    "Transpiled C bundle target mismatch: expected x86_64-linux-gnu, got '${_grpc_lite_manifest_target}'")
endif()
set(_grpc_lite_runtime_source "${_grpc_lite_transpiled_dir}/src/grpc_lite.c")
set(_grpc_lite_plugin_source "${_grpc_lite_transpiled_dir}/src/protoc-gen-grpc_lite_cpp.c")
set(_grpc_lite_zig_header "${_grpc_lite_transpiled_dir}/include/zig.h")
foreach(_grpc_lite_payload IN ITEMS
    "${_grpc_lite_runtime_source}" "${_grpc_lite_plugin_source}" "${_grpc_lite_zig_header}")
  if(NOT EXISTS "${_grpc_lite_payload}")
    message(FATAL_ERROR
      "Transpiled C bundle payload is missing: ${_grpc_lite_payload}. ${_grpc_lite_bundle_help}")
  endif()
endforeach()

function(_grpc_lite_require_local_archive dependency url hash)
  if(NOT url MATCHES "^file://")
    message(FATAL_ERROR
      "GRPC_LITE_NO_NETWORK=ON requires ${dependency} to be an existing target/local subdirectory "
      "or a file:// archive; configured URL is '${url}'")
  endif()
  string(REGEX REPLACE "^file://" "" _grpc_lite_local_archive "${url}")
  if(NOT EXISTS "${_grpc_lite_local_archive}")
    message(FATAL_ERROR
      "GRPC_LITE_NO_NETWORK=ON local ${dependency} archive does not exist: ${_grpc_lite_local_archive}")
  endif()
  string(LENGTH "${hash}" _grpc_lite_hash_length)
  if(NOT hash MATCHES "^SHA256=[0-9A-Fa-f]+$" OR NOT _grpc_lite_hash_length EQUAL 71)
    message(FATAL_ERROR
      "GRPC_LITE_NO_NETWORK=ON requires a SHA-256 hash for the local ${dependency} archive")
  endif()
endfunction()

if(GRPC_LITE_NO_NETWORK)
  if(GRPC_LITE_NGHTTP2_URL)
    _grpc_lite_require_local_archive(
      "nghttp2" "${GRPC_LITE_NGHTTP2_URL}" "${GRPC_LITE_NGHTTP2_URL_HASH}")
  elseif(NOT TARGET nghttp2 AND
      NOT EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/third_party/nghttp2/CMakeLists.txt")
    _grpc_lite_require_local_archive("nghttp2" "" "")
  endif()
  if(GRPC_LITE_CARES_URL)
    _grpc_lite_require_local_archive(
      "c-ares" "${GRPC_LITE_CARES_URL}" "${GRPC_LITE_CARES_URL_HASH}")
  elseif(NOT TARGET c-ares::cares)
    _grpc_lite_require_local_archive("c-ares" "" "")
  endif()
endif()

find_package(Threads REQUIRED)
include(FetchContent)

function(_grpc_lite_declare_archive name url hash)
  if(hash)
    FetchContent_Declare(${name}
      URL "${url}"
      URL_HASH "${hash}"
      DOWNLOAD_EXTRACT_TIMESTAMP TRUE)
  else()
    FetchContent_Declare(${name}
      URL "${url}"
      DOWNLOAD_EXTRACT_TIMESTAMP TRUE)
  endif()
endfunction()

if(NOT TARGET nghttp2)
  set(ENABLE_LIB_ONLY ON)
  set(BUILD_SHARED_LIBS OFF)
  set(BUILD_STATIC_LIBS ON)
  set(BUILD_TESTING OFF)
  set(ENABLE_DOC OFF)
  if(GRPC_LITE_NGHTTP2_URL)
    _grpc_lite_declare_archive(
      grpc_lite_nghttp2 "${GRPC_LITE_NGHTTP2_URL}" "${GRPC_LITE_NGHTTP2_URL_HASH}")
    FetchContent_MakeAvailable(grpc_lite_nghttp2)
  elseif(EXISTS "${CMAKE_CURRENT_SOURCE_DIR}/third_party/nghttp2/CMakeLists.txt")
    add_subdirectory(third_party/nghttp2 EXCLUDE_FROM_ALL)
  else()
    _grpc_lite_declare_archive(
      grpc_lite_nghttp2
      "https://codeload.github.com/nghttp2/nghttp2/tar.gz/68cb6900fde14c77f0cd7add0e094a862960eb99"
      "SHA256=3f042e5284ad349f837b65dd5be953b34c007a05259119092dbe6ebeaa73d306")
    FetchContent_MakeAvailable(grpc_lite_nghttp2)
  endif()
endif()

if(NOT TARGET c-ares::cares)
  set(CARES_STATIC ON CACHE BOOL "" FORCE)
  set(CARES_SHARED OFF CACHE BOOL "" FORCE)
  set(CARES_INSTALL OFF CACHE BOOL "" FORCE)
  set(CARES_BUILD_TESTS OFF CACHE BOOL "" FORCE)
  set(CARES_BUILD_TOOLS OFF CACHE BOOL "" FORCE)
  if(GRPC_LITE_CARES_URL)
    _grpc_lite_declare_archive(
      grpc_lite_cares "${GRPC_LITE_CARES_URL}" "${GRPC_LITE_CARES_URL_HASH}")
  else()
    _grpc_lite_declare_archive(
      grpc_lite_cares
      "https://github.com/c-ares/c-ares/releases/download/v1.34.8/c-ares-1.34.8.tar.gz"
      "SHA256=c222b6d681096f9444d2c4863d2c1174019e27cacca0a4a5c114d36dd7d7bf78")
  endif()
  FetchContent_MakeAvailable(grpc_lite_cares)
endif()

add_library(grpc_lite_c STATIC "${_grpc_lite_runtime_source}" src/cpucycles_compat.c)
target_compile_features(grpc_lite_c PRIVATE c_std_11)
set_target_properties(grpc_lite_c PROPERTIES C_STANDARD 11 C_STANDARD_REQUIRED TRUE)
target_include_directories(grpc_lite_c
  PUBLIC "${CMAKE_CURRENT_SOURCE_DIR}/include"
  PRIVATE "${_grpc_lite_transpiled_dir}/include")
target_link_libraries(grpc_lite_c
  PUBLIC Threads::Threads ${CMAKE_DL_LIBS} m rt
  PRIVATE nghttp2 c-ares::cares)
add_library(grpc_lite::c ALIAS grpc_lite_c)
add_library(grpc_lite::c_static ALIAS grpc_lite_c)

add_library(grpc_lite_grpcpp INTERFACE)
target_compile_features(grpc_lite_grpcpp INTERFACE cxx_std_17)
target_include_directories(grpc_lite_grpcpp INTERFACE "${CMAKE_CURRENT_SOURCE_DIR}/include")
target_link_libraries(grpc_lite_grpcpp INTERFACE grpc_lite_c)
add_library(grpc_lite::grpcpp ALIAS grpc_lite_grpcpp)

if(GRPC_LITE_ENABLE_PROTOBUF)
  find_package(Protobuf REQUIRED)
  if(NOT TARGET protobuf::libprotobuf)
    message(FATAL_ERROR "protobuf::libprotobuf is missing")
  endif()
  add_library(grpc_lite_grpcpp_protobuf INTERFACE)
  target_link_libraries(grpc_lite_grpcpp_protobuf INTERFACE grpc_lite_grpcpp protobuf::libprotobuf)
  add_library(grpc_lite::grpcpp_protobuf ALIAS grpc_lite_grpcpp_protobuf)

  if(TARGET protobuf::libprotobuf-lite)
    add_library(grpc_lite_grpcpp_protobuf_lite INTERFACE)
    target_link_libraries(grpc_lite_grpcpp_protobuf_lite
      INTERFACE grpc_lite_grpcpp protobuf::libprotobuf-lite)
    add_library(grpc_lite::grpcpp_protobuf_lite ALIAS grpc_lite_grpcpp_protobuf_lite)
  endif()
endif()

add_executable(grpc_lite_protoc_gen EXCLUDE_FROM_ALL "${_grpc_lite_plugin_source}")
target_compile_features(grpc_lite_protoc_gen PRIVATE c_std_11)
set_target_properties(grpc_lite_protoc_gen PROPERTIES C_STANDARD 11 C_STANDARD_REQUIRED TRUE)
target_include_directories(grpc_lite_protoc_gen PRIVATE "${_grpc_lite_transpiled_dir}/include")
target_link_libraries(grpc_lite_protoc_gen PRIVATE Threads::Threads ${CMAKE_DL_LIBS} m rt)
target_link_options(grpc_lite_protoc_gen PRIVATE -nostartfiles)
set_target_properties(grpc_lite_protoc_gen PROPERTIES OUTPUT_NAME protoc-gen-grpc_lite_cpp)
add_executable(grpc_lite::protoc-gen-grpc_lite_cpp ALIAS grpc_lite_protoc_gen)
