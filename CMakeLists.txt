cmake_minimum_required(VERSION 3.20)
project(envelopefile VERSION 0.1.0 LANGUAGES CXX)

# Secure random bytes for `keygen` come from OpenSSL's libcrypto (RAND_bytes,
# backed by the operating system's CSPRNG). Install the development package
# before configuring, e.g. on Debian/Ubuntu: apt install libssl-dev
find_package(OpenSSL REQUIRED COMPONENTS Crypto)

add_executable(envelopefile src/main.cpp)
target_compile_features(envelopefile PRIVATE cxx_std_20)
set_target_properties(envelopefile PROPERTIES CXX_EXTENSIONS OFF)
target_link_libraries(envelopefile PRIVATE OpenSSL::Crypto)

# Regression tests for `keygen`: the same product source is linked a second
# time with syscall-wrapping shims (see tests/fault_inject.cpp) so the test
# driver can simulate write/fsync/close failures, partial writes, and secure
# random source failures without touching the production binary.
include(CTest)
if(BUILD_TESTING)
    find_package(Python3 REQUIRED COMPONENTS Interpreter)

    add_executable(envelopefile_testable src/main.cpp tests/fault_inject.cpp)
    target_compile_features(envelopefile_testable PRIVATE cxx_std_20)
    set_target_properties(envelopefile_testable PROPERTIES CXX_EXTENSIONS OFF)
    target_link_libraries(envelopefile_testable PRIVATE OpenSSL::Crypto)
    target_link_options(envelopefile_testable PRIVATE
        "LINKER:--wrap=open"
        "LINKER:--wrap=fchmod"
        "LINKER:--wrap=write"
        "LINKER:--wrap=fsync"
        "LINKER:--wrap=close"
        "LINKER:--wrap=RAND_bytes"
        "LINKER:--wrap=OPENSSL_cleanse")

    add_test(
        NAME keygen_regression
        COMMAND ${Python3_EXECUTABLE}
            ${CMAKE_CURRENT_SOURCE_DIR}/tests/keygen_regression.py
            --binary $<TARGET_FILE:envelopefile>
            --testable-binary $<TARGET_FILE:envelopefile_testable>
            --work-root ${CMAKE_CURRENT_BINARY_DIR}/test-tmp)
    file(MAKE_DIRECTORY ${CMAKE_CURRENT_BINARY_DIR}/test-tmp)
endif()
