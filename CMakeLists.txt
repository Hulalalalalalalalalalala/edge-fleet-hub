cmake_minimum_required(VERSION 3.20)
project(envelopefile VERSION 0.1.0 LANGUAGES CXX)

# Secure random bytes for `keygen` come from OpenSSL's libcrypto (RAND_bytes,
# backed by the operating system's CSPRNG). This is a product dependency:
# install the development package before configuring, e.g. on Debian/Ubuntu:
# apt install libssl-dev
find_package(OpenSSL REQUIRED COMPONENTS Crypto)

add_executable(envelopefile src/main.cpp)
target_compile_features(envelopefile PRIVATE cxx_std_20)
set_target_properties(envelopefile PROPERTIES CXX_EXTENSIONS OFF)
target_link_libraries(envelopefile PRIVATE OpenSSL::Crypto)

# Regression tests: the `encrypt` round-trip/tamper suite runs against the
# production binary, and for `keygen` the same product source is linked a
# second time with syscall-wrapping shims (see tests/fault_inject.cpp) so the
# test driver can simulate write/fsync/close failures, partial writes, and
# secure random source failures without touching the production binary.
#
# The shims rely on the linker's --wrap option, which not every platform's
# default linker provides (notably the system linker shipped with macOS/Xcode,
# ld64, does not). Probe the actual toolchain instead of assuming the option
# works: where it is supported the tests are built by default as before; where
# it is not, configuration and the product build still succeed and the
# fault-injection suite is simply omitted with an explicit explanation.
include(CTest)
if(BUILD_TESTING)
    include(CheckLinkerFlag)

    set(_fault_inject_wrap_options
        "LINKER:--wrap=open,--wrap=fchmod,--wrap=fstat,--wrap=write,--wrap=fsync,--wrap=close,--wrap=unlink,--wrap=RAND_bytes,--wrap=OPENSSL_cleanse")
    check_linker_flag(CXX "${_fault_inject_wrap_options}"
                      EF_LINKER_SUPPORTS_WRAP)

    if(EF_LINKER_SUPPORTS_WRAP)
        # Python 3 only drives the regression suite; it is not needed unless
        # the fault-injecting test binary is actually being built.
        find_package(Python3 REQUIRED COMPONENTS Interpreter)

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
            "LINKER:--wrap=unlink"
            "LINKER:--wrap=RAND_bytes"
            "LINKER:--wrap=OPENSSL_cleanse")

        add_test(
            NAME keygen_regression
            COMMAND ${Python3_EXECUTABLE}
                ${CMAKE_CURRENT_SOURCE_DIR}/tests/keygen_regression.py
                --binary $<TARGET_FILE:envelopefile>
                --testable-binary $<TARGET_FILE:envelopefile_testable>
                --work-root ${CMAKE_CURRENT_BINARY_DIR}/test-tmp)

        # Round-trip and tamper-detection regression for `encrypt`: driven
        # against the production binary only (no fault injection needed).
        add_test(
            NAME encrypt_regression
            COMMAND ${Python3_EXECUTABLE}
                ${CMAKE_CURRENT_SOURCE_DIR}/tests/encrypt_regression.py
                --binary $<TARGET_FILE:envelopefile>
                --work-root ${CMAKE_CURRENT_BINARY_DIR}/test-tmp)
        file(MAKE_DIRECTORY ${CMAKE_CURRENT_BINARY_DIR}/test-tmp)

        message(STATUS
            "envelopefile: keygen fault-injection and encrypt round-trip "
            "regression tests enabled "
            "(linker supports --wrap, Python ${Python3_VERSION} found)")
    else()
        message(NOTICE
            "envelopefile: the regression tests (keygen fault-injection and "
            "encrypt round-trip) are NOT built on this toolchain. Building "
            "them requires a C++ linker "
            "that supports the --wrap option (such as GNU ld or LLVM lld), "
            "but the linker selected by this configuration does not accept "
            "those options (the macOS/Xcode system linker 'ld64' does not). "
            "The production 'envelopefile' binary is still built and fully "
            "usable: --version, 'keygen' (32-byte key files with the 0600 "
            "permission guarantee) and 'encrypt' are unaffected, and no "
            "test-only "
            "dependency such as Python 3 is required. This notice means the "
            "regression suite is UNAVAILABLE here, not that it ran or passed. "
            "To build and run it, reconfigure with a --wrap-capable linker "
            "and Python 3 installed, or configure with -DBUILD_TESTING=OFF to "
            "build only the product without this notice.")
    endif()
endif()
