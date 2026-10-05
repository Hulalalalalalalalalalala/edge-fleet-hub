#include <openssl/crypto.h>
#include <openssl/err.h>
#include <openssl/rand.h>

#include <cerrno>
#include <cstddef>
#include <cstring>
#include <fcntl.h>
#include <iostream>
#include <string>
#include <string_view>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>

namespace {

constexpr std::size_t kKeyBytes = 32;

constexpr const char* kVersionString = "envelopefile 0.1.0\n";
constexpr const char* kUsage =
    "Usage:\n"
    "  envelopefile --version\n"
    "  envelopefile keygen --output <path>\n";
constexpr const char* kKeygenUsage =
    "Usage: envelopefile keygen --output <path>\n";

// Owns raw key material and wipes it with OPENSSL_cleanse on every exit path.
class SecureBuffer {
public:
    SecureBuffer() = default;
    explicit SecureBuffer(std::size_t size) : size_(size) {
        data_ = new unsigned char[size_];
    }
    ~SecureBuffer() { wipe(); }

    SecureBuffer(const SecureBuffer&) = delete;
    SecureBuffer& operator=(const SecureBuffer&) = delete;
    SecureBuffer(SecureBuffer&& other) noexcept
        : data_(other.data_), size_(other.size_) {
        other.data_ = nullptr;
        other.size_ = 0;
    }
    SecureBuffer& operator=(SecureBuffer&&) = delete;

    unsigned char* data() { return data_; }
    std::size_t size() const { return size_; }

    void wipe() {
        if (data_ != nullptr) {
            OPENSSL_cleanse(data_, size_);
            delete[] data_;
            data_ = nullptr;
            size_ = 0;
        }
    }

private:
    unsigned char* data_ = nullptr;
    std::size_t size_ = 0;
};

std::string opensslErrorString() {
    unsigned long code = ERR_peek_last_error();
    if (code == 0) {
        return {};
    }
    char buffer[256];
    ERR_error_string_n(code, buffer, sizeof(buffer));
    return buffer;
}

// Fills the buffer using OpenSSL's CSPRNG (backed by the operating system's
// secure random source). Never seeded from time, paths, or ordinary PRNGs.
bool generateKey(SecureBuffer& key, std::string& error) {
    if (RAND_bytes(key.data(), static_cast<int>(key.size())) != 1) {
        error = "secure random source failed";
        const std::string detail = opensslErrorString();
        if (!detail.empty()) {
            error += ": ";
            error += detail;
        }
        return false;
    }
    return true;
}

std::string errnoDescription(int errorNumber) {
    const char* description = std::strerror(errorNumber);
    return description != nullptr ? description : "unknown error";
}

// Forces the freshly created key file to exactly 0600: owner read/write, no
// execute bit, no group/other access. The mode given to open() is still
// reduced by the inherited process umask, which can strip owner read (umask
// 0400), owner write (umask 0200), or both (umask 0600); fchmod() is not
// affected by the umask. The result is verified with fstat() so a filesystem
// that silently keeps different permissions is treated as a failure instead
// of being reported as a success.
bool fixKeyFilePermissions(int fd, const std::string& path,
                           std::string& error) {
    if (::fchmod(fd, S_IRUSR | S_IWUSR) != 0) {
        error = "cannot set key file permissions to 0600 on '" + path +
                "': " + errnoDescription(errno);
        return false;
    }
    struct stat info;
    if (::fstat(fd, &info) != 0) {
        error = "cannot verify key file permissions on '" + path +
                "': " + errnoDescription(errno);
        return false;
    }
    if ((info.st_mode & 0777) != (S_IRUSR | S_IWUSR)) {
        error = "cannot guarantee key file permissions 0600 on '" + path +
                "'";
        return false;
    }
    return true;
}

// Creates a brand new file containing exactly keySize raw bytes. Any existing
// path (file, directory, symlink including a dangling one) is rejected. The
// file is created with mode 0600 and its permissions are then forced to
// exactly 0600 (the creation mode alone is still subject to the umask) before
// any key byte is written; at no point is the file accessible to group or
// other users. On failure the partially written file is removed and false is
// returned.
bool writeKeyFile(const std::string& path,
                  const unsigned char* key,
                  std::size_t keySize,
                  std::string& error) {
    int fd = ::open(path.c_str(),
                    O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW,
                    S_IRUSR | S_IWUSR);
    if (fd == -1) {
        const int savedErrno = errno;
        if (savedErrno == EEXIST || savedErrno == ELOOP) {
            error = "refusing to write, path already exists: " + path;
        } else {
            error = "cannot create key file '" + path + "': " +
                    errnoDescription(savedErrno);
        }
        return false;
    }

    bool ok = false;
    // If the permissions cannot be guaranteed, this run must not be reported
    // as a success even though the key was generated and the file was
    // created: skip the write and fall through to the cleanup below.
    if (fixKeyFilePermissions(fd, path, error)) {
        std::size_t totalWritten = 0;
        while (totalWritten < keySize) {
            ssize_t written =
                ::write(fd, key + totalWritten, keySize - totalWritten);
            if (written < 0) {
                if (errno == EINTR) {
                    continue;
                }
                error = "failed writing key file '" + path + "': " +
                        errnoDescription(errno);
                break;
            }
            if (written == 0) {
                error = "failed writing key file '" + path + "': short write";
                break;
            }
            totalWritten += static_cast<std::size_t>(written);
        }

        if (totalWritten == keySize) {
            if (::fsync(fd) != 0) {
                error = "failed syncing key file '" + path + "': " +
                        errnoDescription(errno);
            } else if (::close(fd) != 0) {
                // The descriptor is closed by the kernel even when close
                // fails, but durability could not be confirmed: do not
                // report success.
                fd = -1;
                error = "failed closing key file '" + path + "': " +
                        errnoDescription(errno);
            } else {
                fd = -1;
                ok = true;
            }
        }
    }

    if (!ok) {
        if (fd != -1) {
            ::close(fd);
        }
        // The file was created by this invocation (O_EXCL guaranteed it did
        // not exist before open), so removing it cannot touch prior content.
        if (::unlink(path.c_str()) != 0 && errno != ENOENT) {
            std::cerr << "envelopefile: warning: could not remove partial key "
                         "file '"
                      << path << "': " << errnoDescription(errno) << '\n';
        }
    }
    return ok;
}

int runKeygen(int argc, char* argv[]) {
    std::string outputPath;
    bool haveOutput = false;

    for (int i = 2; i < argc; ++i) {
        std::string_view arg(argv[i]);
        if (arg == "--output") {
            if (haveOutput) {
                std::cerr << "envelopefile: '--output' specified more than "
                             "once\n"
                          << kKeygenUsage;
                return 2;
            }
            if (i + 1 >= argc) {
                std::cerr << "envelopefile: option '--output' requires a "
                             "non-empty path\n"
                          << kKeygenUsage;
                return 2;
            }
            outputPath = argv[++i];
            if (outputPath.empty()) {
                std::cerr << "envelopefile: option '--output' requires a "
                             "non-empty path\n"
                          << kKeygenUsage;
                return 2;
            }
            haveOutput = true;
        } else {
            std::cerr << "envelopefile: unsupported argument: '" << arg
                      << "'\n"
                      << kKeygenUsage;
            return 2;
        }
    }

    if (!haveOutput) {
        std::cerr << "envelopefile: missing required option '--output'\n"
                  << kKeygenUsage;
        return 2;
    }

    SecureBuffer key(kKeyBytes);
    std::string error;
    if (!generateKey(key, error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    if (!writeKeyFile(outputPath, key.data(), key.size(), error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    // key is wiped here by the SecureBuffer destructor; its contents never
    // reach stdout, stderr, or any log.
    std::cout << "envelopefile: 32-byte key generated and saved to '"
              << outputPath << "'\n";
    return 0;
}

}  // namespace

int main(int argc, char* argv[]) {
    if (argc == 2 && std::string_view(argv[1]) == "--version") {
        std::cout << kVersionString;
        return 0;
    }

    if (argc >= 2 && std::string_view(argv[1]) == "keygen") {
        return runKeygen(argc, argv);
    }

    std::cerr << kUsage;
    return 2;
}
