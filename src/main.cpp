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

// Owns a file descriptor opened for the key file and guarantees it is closed
// exactly once: a failure path simply leaves the handle in place and the
// destructor performs the close, while the success path releases the handle
// once durability has been confirmed by an explicit successful close().
class FileDescriptor {
public:
    FileDescriptor() = default;
    explicit FileDescriptor(int fd) : fd_(fd) {}
    ~FileDescriptor() { close(); }

    FileDescriptor(const FileDescriptor&) = delete;
    FileDescriptor& operator=(const FileDescriptor&) = delete;

    int get() const { return fd_; }

    // Close the owned descriptor, if any. The reported status is returned so
    // the caller can treat a failed close as a durability failure; either way
    // ownership is released, matching the kernel guarantee that close() frees
    // the descriptor even when it reports an error.
    int close() {
        if (fd_ == -1) {
            return 0;
        }
        const int result = ::close(fd_);
        fd_ = -1;
        return result;
    }

private:
    int fd_ = -1;
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

// How a single key-file save attempt ended.
enum class KeySaveOutcome {
    // The complete key was written, synced, and closed; the file mode was
    // confirmed as 0600.
    kSuccess,
    // No key file was created by this invocation (the target already existed
    // or creation itself failed). Nothing is ours to clean up: the pre-existing
    // path must never be unlinked.
    kNotCreated,
    // A file created by this invocation could not hold the saved key; it was
    // closed and removed. `error` explains the original failure.
    kCleanedUp,
    // As kCleanedUp, but the removal failed. The original failure in `error`
    // still decides the exit status; `cleanupError` additionally names the
    // path and the reason it may still be on disk.
    kCleanupFailed,
};

// Full result of one save attempt. The save logic itself never writes to
// stdout or stderr; the command line decides how to present these fields.
// Neither field ever carries key material.
struct KeySaveResult {
    KeySaveOutcome outcome = KeySaveOutcome::kNotCreated;
    std::string error;        // primary failure reason
    std::string cleanupError; // removal failure, when outcome == kCleanupFailed

    bool succeeded() const {
        return outcome == KeySaveOutcome::kSuccess;
    }
    // True only when a file this invocation created was definitely removed.
    // A failed save does not imply this: check kCleanupFailed separately.
    bool fileWasRemoved() const {
        return outcome == KeySaveOutcome::kCleanedUp;
    }
};

// Creates a brand new file containing exactly keySize raw bytes. Any existing
// path (file, directory, symlink including a dangling one) is rejected without
// touching it, and such a refusal never enters the new-file cleanup. The file
// is created with mode 0600 and its permissions are then forced to exactly
// 0600 (the creation mode alone is still subject to the umask) before any key
// byte is written; at no point is the file accessible to group or other
// users. If the save cannot complete, the descriptor opened by this call is
// closed and the file this call created is removed; should that removal fail,
// the result distinguishes "cleaned up" from "possibly left behind" without
// masking the original failure.
KeySaveResult saveKeyFile(const std::string& path,
                          const unsigned char* key,
                          std::size_t keySize) {
    KeySaveResult result;

    int opened = ::open(path.c_str(),
                        O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW,
                        S_IRUSR | S_IWUSR);
    if (opened == -1) {
        const int savedErrno = errno;
        // Creation failed, so there is no descriptor to close and no file of
        // ours to remove; in particular an existing target is never unlinked.
        if (savedErrno == EEXIST || savedErrno == ELOOP) {
            result.error = "refusing to write, path already exists: " + path;
        } else {
            result.error = "cannot create key file '" + path + "': " +
                           errnoDescription(savedErrno);
        }
        result.outcome = KeySaveOutcome::kNotCreated;
        return result;
    }

    // From here on the file was created by this invocation. The handle is
    // always closed: explicitly after a confirmed successful save, or by the
    // destructor on every failure path.
    FileDescriptor fd(opened);
    std::string error;
    bool saved = false;

    // If the permissions cannot be guaranteed, this run must not be reported
    // as a success even though the key was generated and the file was
    // created: skip the write entirely and fall through to the cleanup below,
    // before any key byte reaches the file.
    if (fixKeyFilePermissions(fd.get(), path, error)) {
        std::size_t totalWritten = 0;
        while (totalWritten < keySize) {
            ssize_t written =
                ::write(fd.get(), key + totalWritten, keySize - totalWritten);
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

        if (totalWritten == keySize && error.empty()) {
            if (::fsync(fd.get()) != 0) {
                error = "failed syncing key file '" + path + "': " +
                        errnoDescription(errno);
            } else if (fd.close() != 0) {
                // The descriptor is closed by the kernel even when close
                // fails (close() always releases the handle), but durability
                // could not be confirmed: do not report success.
                error = "failed closing key file '" + path + "': " +
                        errnoDescription(errno);
            } else {
                // Handle already released by the successful close(); the
                // destructor has nothing left to do.
                saved = true;
            }
        }
    }

    if (saved) {
        result.outcome = KeySaveOutcome::kSuccess;
        return result;
    }

    // The save failed after the file was created. Close the descriptor opened
    // by this call (a no-op when the failed close() above already released
    // it), then remove only the file this invocation created (O_EXCL
    // guaranteed the path did not exist before open), so prior content and
    // other directory entries cannot be touched.
    result.error = std::move(error);
    (void)fd.close();
    if (::unlink(path.c_str()) != 0 && errno != ENOENT) {
        result.outcome = KeySaveOutcome::kCleanupFailed;
        result.cleanupError =
            "could not remove partial key file '" + path + "': " +
            errnoDescription(errno);
        return result;
    }
    result.outcome = KeySaveOutcome::kCleanedUp;
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

    const KeySaveResult save =
        saveKeyFile(outputPath, key.data(), key.size());
    if (!save.succeeded()) {
        // The original save failure always determines the outcome; the
        // incomplete-file warning, if any, is reported in addition rather
        // than in its place.
        std::cerr << "envelopefile: " << save.error << '\n';
        if (save.outcome == KeySaveOutcome::kCleanupFailed) {
            std::cerr << "envelopefile: warning: " << save.cleanupError
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
