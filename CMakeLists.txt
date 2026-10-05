cmake_minimum_required(VERSION 3.20)
project(envelopefile VERSION 0.1.0 LANGUAGES CXX)

# Secure random bytes for `keygen` come from OpenSSL's libcrypto (RAND_bytes,
# backed by the operating system's CSPRNG). This is a hard product dependency:
# without it keygen has no secure random source, so fail configuration up front
# with a clear message instead of producing a broken binary.
# Install the development package before configuring, e.g. on Debian/Ubuntu:
# apt install libssl-dev; on macOS: brew install openssl
find_package(OpenSSL REQUIRED COMPONENTS Crypto)

add_executable(envelopefile src/main.cpp)
target_compile_features(envelopefile PRIVATE cxx_std_20)
set_target_properties(envelopefile PROPERTIES CXX_EXTENSIONS OFF)
target_link_libraries(envelopefile PRIVATE OpenSSL::Crypto)

# Regression tests for `keygen`: the same product source is linked a second
# time with syscall-wrapping shims (see tests/fault_inject.cpp) so the test
# driver can simulate write/fsync/close failures, partial writes, and secure
# random source failures without touching the production binary.
#
# The shims rely on the GNU-style linker `--wrap` option, which only some
# linkers provide (binutils ld, GNU gold, and lld on ELF targets). Apple's
# system linker (ld64, used by the stock macOS toolchain) has no equivalent
# and rejects the option, so on such a toolchain envelopefile_testable cannot
# be linked at all. Rather than breaking the default build there, probe the
# active linker for real support and provide the regression only when both
# the linker capability and the test driver (Python 3) are present. The
# product never depends on either. Configure output states explicitly when
# the regression is not provided (and why) -- a skipped regression is never
# reported as a pass.
include(CTest)
if(BUILD_TESTING)
    include(CheckLinkerFlag)
    check_linker_flag(CXX "LINKER:--wrap=open" EF_LINKER_SUPPORTS_WRAP)

    # Python 3 runs tests/keygen_regression.py and is needed only for tests;
    # it is deliberately not REQUIRED so a product-only build never forces it.
    find_package(Python3 COMPONENTS Interpreter)

    if(NOT EF_LINKER_SUPPORTS_WRAP)
        message(NOTICE
            "envelopefile: fault-injection regression tests NOT PROVIDED: the "
            "active linker does not support the GNU-style '--wrap' option "
            "needed to build the envelopefile_testable fault-injection binary "
            "(tests/fault_inject.cpp). The envelopefile product is built "
            "normally and --version/keygen are fully usable; this notice is a "
            "skip, not a regression pass. To run the regression, reconfigure "
            "with a linker that supports '--wrap' (GNU ld, GNU gold, or lld); "
            "pass -DBUILD_TESTING=OFF to build the product without this notice.")
    elseif(NOT Python3_Interpreter_FOUND)
        message(NOTICE
            "envelopefile: fault-injection regression tests NOT PROVIDED: the "
            "active linker supports '--wrap', but no Python 3 interpreter was "
            "found -- Python 3 is required only to drive "
            "tests/keygen_regression.py. The envelopefile product is built "
            "normally and --version/keygen are fully usable; this notice is a "
            "skip, not a regression pass. Install Python 3 to run the "
            "regression, or pass -DBUILD_TESTING=OFF to build the product "
            "without this notice.")
    else()
        add_executable(envelopefile_testable src/main.cpp tests/fault_inject.cpp)
        target_compile_features(envelopefile_testable PRIVATE cxx_std_20)
        set_target_properties(envelopefile_testable PROPERTIES CXX_EXTENSIONS OFF)
        target_link_libraries(envelopefile_testable PRIVATE OpenSSL::Crypto)
        target_link_options(envelopefile_testable PRIVATE
            "LINKER:--wrap=open"
            "LINKER:--wrap=fchmod"
            "LINKER:--wrap=fstat"
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

        message(NOTICE
            "envelopefile: fault-injection regression tests enabled "
            "(linker '--wrap' supported; Python ${Python3_VERSION} found). "
            "Run them with: ctest --test-dir <build-dir> --output-on-failure")
    endif()
else()
    message(NOTICE
        "envelopefile: tests disabled by -DBUILD_TESTING=OFF; building the "
        "envelopefile product only.")
endif()
