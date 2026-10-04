#include <array>
#include <cerrno>
#include <cstddef>
#include <cstring>
#include <iostream>
#include <string>
#include <string_view>
#include <vector>

#include <fcntl.h>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

#include <openssl/err.h>
#include <openssl/rand.h>

namespace {

inline constexpr std::string_view kVersionString = "envelopefile 0.1.0";
inline constexpr std::size_t kKeySize = 32;  // 256-bit key, raw bytes
inline constexpr int kUsageExitCode = 2;

// Fixed-width 32-byte key buffer. Its contents are cleansed from memory on
// every exit path (including stack unwinding) via the destructor. No copy of
// the key material is ever printed, logged, or placed in an error message.
class KeyBuffer {
public:
    KeyBuffer() = default;
    ~KeyBuffer() { OPENSSL_cleanse(data.data(), data.size()); }

    KeyBuffer(const KeyBuffer&) = delete;
    KeyBuffer& operator=(const KeyBuffer&) = delete;
    KeyBuffer(KeyBuffer&&) = delete;
    KeyBuffer& operator=(KeyBuffer&&) = delete;

    std::array<unsigned char, kKeySize> data{};
};

// Drains the OpenSSL error queue into a human-readable string.
std::string cryptoError() {
    unsigned long code = ERR_peek_error();
    if (code == 0) {
        return "secure random source unavailable";
    }
    char buf[256];
    ERR_error_string_n(code, buf, sizeof(buf));
    return std::string(buf);
}

// Writes exactly len bytes, retrying on EINTR and short writes.
bool writeAll(int fd, const unsigned char* buf, std::size_t len) {
    std::size_t off = 0;
    while (off < len) {
        ssize_t n = ::write(fd, buf + off, len - off);
        if (n > 0) {
            off += static_cast<std::size_t>(n);
            continue;
        }
        if (n == 0) {
            return false;  // cannot make progress
        }
        if (errno == EINTR) {
            continue;
        }
        return false;
    }
    return true;
}

void printKeygenUsage() {
    std::cerr << "Usage: envelopefile keygen --output <path>\n";
}

// Handles `envelopefile keygen ...`. Never emits key material.
int runKeygen(const std::vector<std::string>& args) {
    bool outputSeen = false;
    std::string outputPath;

    for (std::size_t i = 0; i < args.size(); ++i) {
        if (args[i] == "--output") {
            if (outputSeen) {
                std::cerr << "Error: --output specified more than once.\n";
                return kUsageExitCode;
            }
            if (i + 1 >= args.size()) {
                std::cerr << "Error: --output requires a path argument.\n";
                printKeygenUsage();
                return kUsageExitCode;
            }
            outputPath = args[++i];
            outputSeen = true;
        } else {
            std::cerr << "Error: unsupported argument '" << args[i] << "'.\n";
            printKeygenUsage();
            return kUsageExitCode;
        }
    }

    if (!outputSeen) {
        std::cerr << "Error: --output <path> is required.\n";
        printKeygenUsage();
        return kUsageExitCode;
    }
    if (outputPath.empty()) {
        std::cerr << "Error: --output path must not be empty.\n";
        printKeygenUsage();
        return kUsageExitCode;
    }

    // 1. Generate the key first. If the CSPRNG is unavailable, fail before
    //    touching the filesystem. RAND_bytes draws from the operating
    //    system's secure random source, never from time, paths, or a
    //    general-purpose PRNG.
    KeyBuffer key;
    if (RAND_bytes(key.data.data(), static_cast<int>(kKeySize)) != 1) {
        std::cerr << "Error: failed to generate secure key material ("
                  << cryptoError() << ").\n";
        return 1;
    }

    // 2. Atomically create the target. O_EXCL rejects an existing file,
    //    directory, or symlink (including a dangling one); O_NOFOLLOW is a
    //    second guard against symlinks. The file is born with mode 0600
    //    (owner read/write only), so it is never briefly world-readable.
    int fd = ::open(outputPath.c_str(),
                    O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW | O_CLOEXEC,
                    S_IRUSR | S_IWUSR);  // 0600
    if (fd < 0) {
        if (errno == EEXIST) {
            std::cerr << "Error: path already exists, refusing to overwrite: "
                      << outputPath << "\n";
        } else if (errno == ENOENT) {
            std::cerr << "Error: cannot save to " << outputPath
                      << " (parent directory does not exist).\n";
        } else if (errno == ENOTDIR) {
            std::cerr << "Error: cannot save to " << outputPath
                      << " (a path component is not a directory).\n";
        } else if (errno == EACCES || errno == EROFS || errno == EPERM) {
            std::cerr << "Error: target location is not writable: "
                      << outputPath << " (" << std::strerror(errno) << ").\n";
        } else {
            std::cerr << "Error: failed to create " << outputPath << ": "
                      << std::strerror(errno) << "\n";
        }
        return 1;
    }

    // 3. Write all 32 bytes, flush to disk, and check close() so a short
    //    write, flush failure, or close-time error cannot be reported as
    //    success. errno is captured after each failing call for the message.
    bool ok = writeAll(fd, key.data.data(), kKeySize);
    int writeErrno = errno;
    if (ok && ::fsync(fd) != 0) {
        ok = false;
        writeErrno = errno;
    }
    if (ok && ::close(fd) != 0) {
        ok = false;
        writeErrno = errno;
        fd = -1;
    }
    if (ok) {
        std::cout << "Key generated and saved to: " << outputPath << "\n";
        return 0;
    }

    if (fd >= 0) {
        ::close(fd);  // best effort; already failed
    }
    // Never leave a partial/invalid key file behind, and never touch any
    // pre-existing content (O_EXCL guaranteed the path was ours).
    ::unlink(outputPath.c_str());
    std::cerr << "Error: failed to save key file " << outputPath << ": "
              << std::strerror(writeErrno) << "\n";
    return 1;
}

}  // namespace

int main(int argc, char* argv[]) {
    if (argc == 2 && std::string_view(argv[1]) == "--version") {
        std::cout << kVersionString << "\n";
        return 0;
    }
    if (argc >= 2 && std::string_view(argv[1]) == "keygen") {
        std::vector<std::string> args;
        args.reserve(static_cast<std::size_t>(argc - 2));
        for (int i = 2; i < argc; ++i) {
            args.emplace_back(argv[i]);
        }
        return runKeygen(args);
    }

    std::cerr << "Usage: envelopefile --version\n"
                 "       envelopefile keygen --output <path>\n";
    return kUsageExitCode;
}
