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
