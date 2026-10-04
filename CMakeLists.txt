cmake_minimum_required(VERSION 3.20)
project(envelopefile VERSION 0.1.0 LANGUAGES CXX)

# Secure randomness for key generation is provided by OpenSSL's libcrypto
# (RAND_bytes). Requires OpenSSL >= 1.1.0; 3.x is recommended.
find_package(OpenSSL 3.0 REQUIRED)

add_executable(envelopefile src/main.cpp)
target_compile_features(envelopefile PRIVATE cxx_std_20)
set_target_properties(envelopefile PROPERTIES CXX_EXTENSIONS OFF)
target_link_libraries(envelopefile PRIVATE OpenSSL::Crypto)
