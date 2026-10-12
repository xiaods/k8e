cmake_minimum_required(VERSION 3.20)
project(grpc_lite_cpp_examples LANGUAGES CXX)

find_package(grpc_lite 0.4 CONFIG REQUIRED COMPONENTS protobuf)

if(NOT TARGET protobuf::protoc)
  message(FATAL_ERROR "protobuf::protoc is missing")
endif()
if(NOT TARGET protobuf::libprotobuf)
  message(FATAL_ERROR "protobuf::libprotobuf is missing")
endif()
if(NOT TARGET grpc_lite::protoc-gen-grpc_lite_cpp)
  message(FATAL_ERROR "grpc-lite C++ protoc plugin target is missing")
endif()

set(proto_file "${CMAKE_CURRENT_SOURCE_DIR}/echo.proto")
set(generated_dir "${CMAKE_CURRENT_BINARY_DIR}/generated")
set(generated_sources
  "${generated_dir}/echo.pb.cc"
  "${generated_dir}/echo.grpc.pb.cc")
set(generated_headers
  "${generated_dir}/echo.pb.h"
  "${generated_dir}/echo.grpc.pb.h")

add_custom_command(
  OUTPUT ${generated_sources} ${generated_headers}
  COMMAND "${CMAKE_COMMAND}" -E make_directory "${generated_dir}"
  COMMAND "$<TARGET_FILE:protobuf::protoc>"
  ARGS
    "--proto_path=${CMAKE_CURRENT_SOURCE_DIR}"
    "--cpp_out=${generated_dir}"
    "--plugin=protoc-gen-grpc_lite_cpp=$<TARGET_FILE:grpc_lite::protoc-gen-grpc_lite_cpp>"
    "--grpc_lite_cpp_out=${generated_dir}"
    "${proto_file}"
  DEPENDS
    "${proto_file}"
    protobuf::protoc
    grpc_lite::protoc-gen-grpc_lite_cpp
  VERBATIM)

add_library(grpc_lite_cpp_example_proto STATIC
  ${generated_sources}
  ${generated_headers})
target_include_directories(grpc_lite_cpp_example_proto PUBLIC "${generated_dir}")
target_link_libraries(grpc_lite_cpp_example_proto
  PUBLIC
    grpc_lite::grpcpp_protobuf)

foreach(example IN ITEMS basic_echo metadata_and_large errors_and_deadline)
  add_executable("grpc_lite_cpp_${example}" "${example}.cc")
  target_link_libraries("grpc_lite_cpp_${example}"
    PRIVATE grpc_lite_cpp_example_proto)
endforeach()
