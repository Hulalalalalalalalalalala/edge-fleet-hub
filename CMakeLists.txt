cmake_minimum_required(VERSION 3.20)
project(envelopefile VERSION 0.1.0 LANGUAGES C CXX)

# Secure random bytes for `keygen` come from OpenSSL's libcrypto (RAND_bytes,
# backed by the operating system's CSPRNG). Install the development package
# before configuring, e.g. on Debian/Ubuntu: apt install libssl-dev
find_package(OpenSSL REQUIRED COMPONENTS Crypto)

add_executable(envelopefile src/main.cpp)
target_compile_features(envelopefile PRIVATE cxx_std_20)
set_target_properties(envelopefile PROPERTIES CXX_EXTENSIONS OFF)
target_link_libraries(envelopefile PRIVATE OpenSSL::Crypto)

# Regression tests for `keygen`: a Python driver plus an LD_PRELOAD shim
# that injects write/fsync/close faults into the key file's descriptor.
include(CTest)
if(BUILD_TESTING)
    find_package(Python3 REQUIRED COMPONENTS Interpreter)

    add_library(keygen_fault_inject SHARED tests/fault_inject.c)
    target_link_libraries(keygen_fault_inject PRIVATE ${CMAKE_DL_LIBS})

    add_test(
        NAME keygen_regression
        COMMAND ${Python3_EXECUTABLE}
            ${CMAKE_CURRENT_SOURCE_DIR}/tests/run_keygen_regression.py
            --binary $<TARGET_FILE:envelopefile>
            --fault-inject $<TARGET_FILE:keygen_fault_inject>)
endif()
