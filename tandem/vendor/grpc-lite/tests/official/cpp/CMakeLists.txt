cmake_minimum_required(VERSION 3.20)
project(grpc_lite_cpp_official_interop LANGUAGES CXX)

find_package(grpc_lite 0.4 CONFIG REQUIRED COMPONENTS protobuf)

if(NOT DEFINED GRPC_LITE_INTEROP_PROTO_ROOT)
  message(FATAL_ERROR "GRPC_LITE_INTEROP_PROTO_ROOT is required")
endif()
if(NOT TARGET protobuf::protoc)
  message(FATAL_ERROR "protobuf::protoc is missing")
endif()
if(NOT TARGET grpc_lite::protoc-gen-grpc_lite_cpp)
  message(FATAL_ERROR "grpc-lite C++ protoc plugin target is missing")
endif()

set(proto_root "${GRPC_LITE_INTEROP_PROTO_ROOT}")
set(generated_dir "${CMAKE_CURRENT_BINARY_DIR}/generated")
set(proto_files
  "${proto_root}/grpc/testing/empty.proto"
  "${proto_root}/grpc/testing/messages.proto"
  "${proto_root}/grpc/testing/test.proto")
set(generated_sources
  "${generated_dir}/grpc/testing/empty.pb.cc"
  "${generated_dir}/grpc/testing/messages.pb.cc"
  "${generated_dir}/grpc/testing/test.pb.cc"
  "${generated_dir}/grpc/testing/test.grpc.pb.cc")
set(generated_headers
  "${generated_dir}/grpc/testing/empty.pb.h"
  "${generated_dir}/grpc/testing/messages.pb.h"
  "${generated_dir}/grpc/testing/test.pb.h"
  "${generated_dir}/grpc/testing/test.grpc.pb.h")

add_custom_command(
  OUTPUT ${generated_sources} ${generated_headers}
  COMMAND "${CMAKE_COMMAND}" -E make_directory "${generated_dir}"
  COMMAND "$<TARGET_FILE:protobuf::protoc>"
  ARGS
    "--proto_path=${proto_root}"
    "--cpp_out=${generated_dir}"
    ${proto_files}
  COMMAND "$<TARGET_FILE:protobuf::protoc>"
  ARGS
    "--proto_path=${proto_root}"
    "--plugin=protoc-gen-grpc_lite_cpp=$<TARGET_FILE:grpc_lite::protoc-gen-grpc_lite_cpp>"
    "--grpc_lite_cpp_out=${generated_dir}"
    "${proto_root}/grpc/testing/test.proto"
  DEPENDS
    ${proto_files}
    protobuf::protoc
    grpc_lite::protoc-gen-grpc_lite_cpp
  VERBATIM)

add_library(grpc_lite_cpp_official_proto STATIC
  ${generated_sources}
  ${generated_headers})
target_include_directories(grpc_lite_cpp_official_proto
  PUBLIC "${generated_dir}")
target_link_libraries(grpc_lite_cpp_official_proto
  PUBLIC grpc_lite::grpcpp_protobuf)

target_compile_features(grpc_lite_cpp_official_proto PUBLIC cxx_std_17)

add_executable(grpc-lite-cpp-interop-client interop_client.cc)
target_link_libraries(grpc-lite-cpp-interop-client
  PRIVATE grpc_lite_cpp_official_proto)

add_executable(grpc-lite-cpp-interop-server interop_server.cc)
target_link_libraries(grpc-lite-cpp-interop-server
  PRIVATE grpc_lite_cpp_official_proto)
