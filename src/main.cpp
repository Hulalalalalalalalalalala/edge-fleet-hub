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

// Why a save attempt ended the way it did. kSaved is the only success; every
// other status is reported to the user with an explanatory message.
enum class KeySaveStatus {
    kSaved,          // all key bytes written, fsynced, and closed; mode 0600
    kNotCreated,     // open() produced no file, so nothing had to be cleaned
    kCleanedUp,      // this run created the file, failed, and removed it
    kCleanupFailed,  // this run created the file, failed, and removal failed
};

// Complete outcome of one keygen save attempt: whether it succeeded, why it
// failed when it did, and whether the file this run created was cleaned up.
// The save logic itself writes nothing to stdout or stderr; the command line
// reports error, and additionally reports removalWarning only when cleanup
// failed, so a failed removal can never mask the original save failure and a
// caller can tell "removed" apart from "could not remove" instead of inferring
// deletion from failure alone. Neither string ever contains key material.
struct KeySaveResult {
    KeySaveStatus status = KeySaveStatus::kNotCreated;
    std::string error;           // why the save did not succeed; empty on success
    std::string removalWarning;  // populated only for kCleanupFailed

    bool saved() const { return status == KeySaveStatus::kSaved; }
};

// Owns the descriptor opened for the newly created key file for the duration
// of one save attempt and closes it exactly once. Closing is centralized here
// rather than spread across the failure branches: a branch only records its
// reason, and the descriptor is still released on every path out of scope.
class FdGuard {
public:
    FdGuard() = default;
    explicit FdGuard(int fd) : fd_(fd) {}
    ~FdGuard() { close(); }

    FdGuard(const FdGuard&) = delete;
    FdGuard& operator=(const FdGuard&) = delete;
    FdGuard(FdGuard&&) = delete;
    FdGuard& operator=(FdGuard&&) = delete;

    // Closes and disarms the guard, reporting close(2)'s status. Used after
    // fsync(), where a reported failure means durability could not be
    // confirmed and success must not be reported. Must be called while armed.
    int finish() {
        const int fd = fd_;
        fd_ = -1;
        return ::close(fd);
    }

    // Best-effort close for failure paths: the save has already failed, and
    // close(2) releases the descriptor even when it reports an error, so the
    // result is intentionally discarded here. Idempotent via disarming.
    void close() {
        if (fd_ != -1) {
            const int fd = fd_;
            fd_ = -1;
            ::close(fd);
        }
    }

private:
    int fd_ = -1;
};

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
// other users.
//
// The outcome is reported entirely through KeySaveResult: an open() failure
// creates nothing (kNotCreated); a failure after creation closes the
// descriptor via the FdGuard and removes this run's file, reporting either
// kCleanedUp or, if the removal itself failed, kCleanupFailed with the
// removal reason in removalWarning so the caller can warn that an incomplete
// key file may remain. This function never writes to stdout or stderr.
KeySaveResult writeKeyFile(const std::string& path,
                           const unsigned char* key,
                           std::size_t keySize) {
    KeySaveResult result;

    int fd = ::open(path.c_str(),
                    O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW,
                    S_IRUSR | S_IWUSR);
    if (fd == -1) {
        const int savedErrno = errno;
        // Pre-creation failure: nothing was created, so this must not enter
        // the "remove the new file" handling and an existing target's
        // content, permissions, and links are never touched.
        result.status = KeySaveStatus::kNotCreated;
        if (savedErrno == EEXIST || savedErrno == ELOOP) {
            result.error = "refusing to write, path already exists: " + path;
        } else {
            result.error = "cannot create key file '" + path + "': " +
                           errnoDescription(savedErrno);
        }
        return result;
    }

    FdGuard guard(fd);
    result.status = KeySaveStatus::kCleanedUp;  // assumed until the save completes
    std::string error;

    // If the permissions cannot be guaranteed, this run must not be reported
    // as a success even though the key was generated and the file was
    // created: skip the write and fall through to the cleanup below, before
    // any key byte reaches the file.
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
            } else if (guard.finish() != 0) {
                // The descriptor is closed by the kernel even when close
                // fails, but durability could not be confirmed: do not
                // report success.
                error = "failed closing key file '" + path + "': " +
                        errnoDescription(errno);
            } else {
                // The complete original key is written, synced, and closed;
                // 0600 was verified before the first byte.
                result.status = KeySaveStatus::kSaved;
                result.error.clear();
                return result;
            }
        }
    }

    // Failure after creation. The descriptor opened by this run is closed
    // exactly once (guard.finish() above already disarmed it on the
    // close-failure path); removing the path cannot touch prior content
    // because O_EXCL guaranteed the file did not exist before open().
    guard.close();
    if (::unlink(path.c_str()) != 0 && errno != ENOENT) {
        // The original save failure remains the reported cause; this only
        // adds that the incomplete file could not be removed.
        result.status = KeySaveStatus::kCleanupFailed;
        result.removalWarning =
            "could not remove partial key file '" + path + "': " +
            errnoDescription(errno);
    }
    result.error = std::move(error);
    return result;
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

    const KeySaveResult save = writeKeyFile(outputPath, key.data(), key.size());
    if (!save.saved()) {
        // The save logic reports outcomes as data; the command line owns all
        // user-facing messages. The original failure is always the primary
        // cause; a failed cleanup is reported only as an additional warning,
        // never as a replacement, so the exit code stays 1 and the user can
        // tell that an incomplete key file may remain.
        std::cerr << "envelopefile: " << save.error << '\n';
        if (save.status == KeySaveStatus::kCleanupFailed) {
            std::cerr << "envelopefile: warning: " << save.removalWarning
                      << '\n';
        }
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
