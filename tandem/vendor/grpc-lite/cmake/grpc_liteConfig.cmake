get_filename_component(_grpc_lite_prefix "${CMAKE_CURRENT_LIST_DIR}/../../.." ABSOLUTE)
get_filename_component(_grpc_lite_libdir "${CMAKE_CURRENT_LIST_DIR}/../.." ABSOLUTE)

include(CMakeFindDependencyMacro)
find_dependency(Threads)

set(_grpc_lite_supported_components protobuf)
foreach(_grpc_lite_component IN LISTS grpc_lite_FIND_COMPONENTS)
  if(NOT _grpc_lite_component IN_LIST _grpc_lite_supported_components)
    set(grpc_lite_${_grpc_lite_component}_FOUND FALSE)
    if(grpc_lite_FIND_REQUIRED_${_grpc_lite_component})
      set(grpc_lite_FOUND FALSE)
      set(grpc_lite_NOT_FOUND_MESSAGE
        "Unsupported grpc_lite component: ${_grpc_lite_component}")
      return()
    endif()
  endif()
endforeach()

if(NOT TARGET grpc_lite::c)
  add_library(grpc_lite::c SHARED IMPORTED)
  set_target_properties(grpc_lite::c PROPERTIES
    IMPORTED_LOCATION "${_grpc_lite_libdir}/libgrpc_lite.so"
    INTERFACE_INCLUDE_DIRECTORIES "${_grpc_lite_prefix}/include")
endif()

if(NOT TARGET grpc_lite::c_static)
  add_library(grpc_lite::c_static STATIC IMPORTED)
  set_target_properties(grpc_lite::c_static PROPERTIES
    IMPORTED_LOCATION "${_grpc_lite_libdir}/libgrpc_lite.a"
    INTERFACE_INCLUDE_DIRECTORIES "${_grpc_lite_prefix}/include"
    INTERFACE_LINK_LIBRARIES "Threads::Threads;${CMAKE_DL_LIBS};m;rt")
endif()

if(NOT TARGET grpc_lite::grpcpp)
  add_library(grpc_lite::grpcpp INTERFACE IMPORTED)
  set_target_properties(grpc_lite::grpcpp PROPERTIES
    INTERFACE_COMPILE_FEATURES cxx_std_17
    INTERFACE_INCLUDE_DIRECTORIES "${_grpc_lite_prefix}/include"
    INTERFACE_LINK_LIBRARIES grpc_lite::c)
endif()

if(NOT TARGET grpc_lite::protoc-gen-grpc_lite_cpp)
  add_executable(grpc_lite::protoc-gen-grpc_lite_cpp IMPORTED)
  set_target_properties(grpc_lite::protoc-gen-grpc_lite_cpp PROPERTIES
    IMPORTED_LOCATION "${_grpc_lite_prefix}/bin/protoc-gen-grpc_lite_cpp")
endif()

if("protobuf" IN_LIST grpc_lite_FIND_COMPONENTS)
  find_package(Protobuf QUIET)
  if(TARGET protobuf::libprotobuf)
    set(grpc_lite_protobuf_FOUND TRUE)
    if(NOT TARGET grpc_lite::grpcpp_protobuf)
      add_library(grpc_lite::grpcpp_protobuf INTERFACE IMPORTED)
      set_target_properties(grpc_lite::grpcpp_protobuf PROPERTIES
        INTERFACE_LINK_LIBRARIES "grpc_lite::grpcpp;protobuf::libprotobuf")
    endif()
    if(TARGET protobuf::libprotobuf-lite AND
       NOT TARGET grpc_lite::grpcpp_protobuf_lite)
      add_library(grpc_lite::grpcpp_protobuf_lite INTERFACE IMPORTED)
      set_target_properties(grpc_lite::grpcpp_protobuf_lite PROPERTIES
        INTERFACE_LINK_LIBRARIES
          "grpc_lite::grpcpp;protobuf::libprotobuf-lite")
    endif()
  else()
    set(grpc_lite_protobuf_FOUND FALSE)
    if(grpc_lite_FIND_REQUIRED_protobuf)
      set(grpc_lite_FOUND FALSE)
      set(grpc_lite_NOT_FOUND_MESSAGE
        "grpc_lite protobuf component requires protobuf::libprotobuf")
    endif()
  endif()
endif()

unset(_grpc_lite_supported_components)
unset(_grpc_lite_component)
unset(_grpc_lite_prefix)
unset(_grpc_lite_libdir)
