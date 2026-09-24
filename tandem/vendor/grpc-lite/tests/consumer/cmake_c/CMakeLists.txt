cmake_minimum_required(VERSION 3.20)
project(grpc_lite_cmake_c_consumer LANGUAGES C CXX)

foreach(required IN ITEMS
    GRPC_LITE_SOURCE_DIR GRPC_LITE_TRANSPILED_C_DIR GRPC_LITE_EXPECTED_VERSION
    GRPC_LITE_NGHTTP2_URL GRPC_LITE_NGHTTP2_URL_HASH
    GRPC_LITE_CARES_URL GRPC_LITE_CARES_URL_HASH)
  if(NOT ${required})
    message(FATAL_ERROR "${required} is required")
  endif()
endforeach()

find_package(Protobuf REQUIRED)
set(GRPC_LITE_ENABLE_PROTOBUF ON CACHE BOOL "" FORCE)
include(FetchContent)
FetchContent_Declare(grpc_lite SOURCE_DIR "${GRPC_LITE_SOURCE_DIR}")
FetchContent_MakeAvailable(grpc_lite)

add_executable(c_consumer_shared main.c)
target_compile_features(c_consumer_shared PRIVATE c_std_11)
target_compile_definitions(c_consumer_shared PRIVATE
  GRPC_LITE_EXPECTED_VERSION="${GRPC_LITE_EXPECTED_VERSION}")
target_link_libraries(c_consumer_shared PRIVATE grpc_lite::c)

add_executable(c_consumer_static main.c)
target_compile_features(c_consumer_static PRIVATE c_std_11)
target_compile_definitions(c_consumer_static PRIVATE
  GRPC_LITE_EXPECTED_VERSION="${GRPC_LITE_EXPECTED_VERSION}")
target_link_libraries(c_consumer_static PRIVATE grpc_lite::c_static)

set(generated_dir "${CMAKE_CURRENT_BINARY_DIR}/generated")
set(generated_proto_sources
  "${generated_dir}/cpp_codegen.pb.cc"
  "${generated_dir}/cpp_codegen.grpc.pb.cc")
set(generated_proto_headers
  "${generated_dir}/cpp_codegen.pb.h"
  "${generated_dir}/cpp_codegen.grpc.pb.h")
add_custom_command(
  OUTPUT ${generated_proto_sources} ${generated_proto_headers}
  COMMAND "${CMAKE_COMMAND}" -E make_directory "${generated_dir}"
  COMMAND "${Protobuf_PROTOC_EXECUTABLE}"
    -I "${GRPC_LITE_SOURCE_DIR}/proto"
    --cpp_out=${generated_dir}
    --plugin=protoc-gen-grpc_lite_cpp=$<TARGET_FILE:grpc_lite_protoc_gen>
    --grpc_lite_cpp_out=${generated_dir}
    "${GRPC_LITE_SOURCE_DIR}/proto/cpp_codegen.proto"
  COMMAND test -s "${generated_dir}/cpp_codegen.grpc.pb.h"
  COMMAND test -s "${generated_dir}/cpp_codegen.grpc.pb.cc"
  DEPENDS grpc_lite_protoc_gen "${GRPC_LITE_SOURCE_DIR}/proto/cpp_codegen.proto"
  VERBATIM)
add_executable(grpcpp_generated_from_bundle
  "${GRPC_LITE_SOURCE_DIR}/tests/grpcpp_generated_test.cc"
  ${generated_proto_sources})
target_include_directories(grpcpp_generated_from_bundle PRIVATE "${generated_dir}")
target_link_libraries(grpcpp_generated_from_bundle PRIVATE grpc_lite::grpcpp_protobuf)

if(NOT TARGET grpc_lite_protoc_gen)
  message(FATAL_ERROR "The transpiled C bundle protoc plugin target is missing")
endif()

enable_testing()
add_test(NAME grpc_lite_c_shared COMMAND c_consumer_shared)
add_test(NAME grpc_lite_c_static COMMAND c_consumer_static)
add_test(NAME grpc_lite_bundle_plugin_generated_cpp COMMAND grpcpp_generated_from_bundle)
