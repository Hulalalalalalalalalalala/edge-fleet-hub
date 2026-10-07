#include <openssl/crypto.h>
#include <openssl/err.h>
#include <openssl/evp.h>
#include <openssl/rand.h>

#include <cerrno>
#include <cstddef>
#include <cstdint>
#include <cstring>
#include <fcntl.h>
#include <iostream>
#include <string>
#include <string_view>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#include <vector>

namespace {

constexpr std::size_t kKeyBytes = 32;
constexpr std::size_t kNonceBytes = 12;
constexpr std::size_t kTagBytes = 16;

constexpr const char* kVersionString = "envelopefile 0.1.0\n";
constexpr const char* kUsage =
    "Usage:\n"
    "  envelopefile --version\n"
    "  envelopefile keygen --output <path>\n"
    "  envelopefile encrypt --key <path> --input <path> --output <path>\n";
constexpr const char* kKeygenUsage =
    "Usage: envelopefile keygen --output <path>\n";
constexpr const char* kEncryptUsage =
    "Usage: envelopefile encrypt --key <path> --input <path> --output <path>\n";

// Binary envelope identification. The format is interpreted from these bytes,
// never from the file name/extension.
//
//   magic     "ENVFILE1"  7 bytes, fixed identification
//   version   uint8      1 byte,  format version (1)
//   algorithm uint8      1 byte,  algorithm identifier (1 = AES-256-GCM)
//   nonce     12 bytes            fresh per encryption
//   ciphertext N bytes            raw input bytes encrypted (may be empty)
//   tag       16 bytes            GCM authentication tag
//
// Everything from the first magic byte through the last ciphertext byte is
// the authenticated data, so the header used to interpret the envelope cannot
// be tampered with without detection. Multi-byte integer fields are single
// bytes here, so there is no endianness to decode.
constexpr unsigned char kEnvelopeMagic[7] = {
    'E', 'N', 'V', 'F', 'I', 'L', 'E'};
constexpr std::size_t kEnvelopeMagicBytes = 7;
constexpr std::size_t kHeaderBytes =
    kEnvelopeMagicBytes + 1 + 1 + kNonceBytes;  // 21 bytes
constexpr unsigned char kEnvelopeVersion = 1;
constexpr unsigned char kAlgAes256Gcm = 1;

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
// other status is reported to the user with an explanatory message. Shared by
// keygen's key file and encrypt's envelope file: apart from their permission
// policy their save pipeline is the same.
enum class SaveStatus {
    kSaved,          // all bytes written, fsynced, and closed; mode 0600
    kNotCreated,     // open() produced no file, so nothing had to be cleaned
    kCleanedUp,      // this run created the file, failed, and removed it
    kCleanupFailed,  // this run created the file, failed, and removal failed
};

// Complete outcome of one output save attempt: whether it succeeded, why it
// failed when it did, and whether the file this run created was cleaned up.
// The save logic itself writes nothing to stdout or stderr; the command line
// reports error, and additionally reports removalWarning only when cleanup
// failed, so a failed removal can never mask the original save failure and a
// caller can tell "removed" apart from "could not remove" instead of inferring
// deletion from failure alone. Neither string ever contains key or plaintext
// material.
struct SaveResult {
    SaveStatus status = SaveStatus::kNotCreated;
    std::string error;           // why the save did not succeed; empty on success
    std::string removalWarning;  // populated only for kCleanupFailed

    bool saved() const { return status == SaveStatus::kSaved; }
};

// Owns the descriptor opened for the newly created output file for the
// duration of one save attempt and closes it exactly once. Closing is
// centralized here rather than spread across the failure branches: a branch
// only records its reason, and the descriptor is still released on every
// path out of scope.
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

// Forces the freshly created file to exactly 0600: owner read/write, no
// execute bit, no group/other access. The mode given to open() is still
// reduced by the inherited process umask, which can strip owner read (umask
// 0400), owner write (umask 0200), or both (umask 0600); fchmod() is not
// affected by the umask.
//
// What happens next is the one permission difference the two outputs keep:
//
//   key files   - fchmod() is followed by an fstat() verification, and both
//                 must succeed with exactly 0600 before any key byte is
//                 written. A filesystem that silently keeps different
//                 permissions is a failure, not a success.
//   envelopes   - fchmod() alone must succeed; group/other never gain access
//                 at creation and the result is not re-verified.
//
// The distinction is the verifyPermissions parameter so the save pipeline
// stays shared.
bool forceOwnerOnlyPermissions(int fd, const std::string& path,
                               bool verifyPermissions,
                               const char* fileKind, std::string& error) {
    if (::fchmod(fd, S_IRUSR | S_IWUSR) != 0) {
        error = std::string("cannot set ") + fileKind +
                " file permissions to 0600 on '" + path + "': " +
                errnoDescription(errno);
        return false;
    }
    if (!verifyPermissions) {
        return true;
    }
    struct stat info;
    if (::fstat(fd, &info) != 0) {
        error = std::string("cannot verify ") + fileKind +
                " file permissions on '" + path + "': " +
                errnoDescription(errno);
        return false;
    }
    if ((info.st_mode & 0777) != (S_IRUSR | S_IWUSR)) {
        error = std::string("cannot guarantee ") + fileKind +
                " file permissions 0600 on '" + path + "'";
        return false;
    }
    return true;
}

// Saves one freshly produced output blob to a brand new file. This is the
// single save pipeline shared by keygen (the 32 raw key bytes) and encrypt
// (the sealed envelope bytes); the only business difference between the two
// is the permission policy, selected by verifyPermissions (see above). The
// fileKind strings ("key"/"envelope") only build the existing diagnostic
// messages, and never contain file contents.
//
// Any existing path (file, empty file, directory, symlink including a
// dangling one) is rejected through O_CREAT|O_EXCL|O_NOFOLLOW before the
// target or its link is touched; the inherited umask cannot widen or narrow
// the mode away from 0600 because fchmod() sets it explicitly.
//
// The outcome is reported entirely through SaveResult: an open() failure
// creates nothing (kNotCreated); a failure after creation closes the
// descriptor via the FdGuard and removes this run's file, reporting either
// kCleanedUp or, if the removal itself failed, kCleanupFailed with the
// removal reason in removalWarning so the caller can warn that an incomplete
// file may remain. This function never writes to stdout or stderr.
SaveResult writeOutputFile(const std::string& path,
                           const unsigned char* bytes,
                           std::size_t byteCount, const char* fileKind,
                           bool verifyPermissions) {
    SaveResult result;

    int fd = ::open(path.c_str(),
                    O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW,
                    S_IRUSR | S_IWUSR);
    if (fd == -1) {
        const int savedErrno = errno;
        // Pre-creation failure: nothing was created, so this must not enter
        // the "remove the new file" handling and an existing target's
        // content, permissions, and links are never touched.
        result.status = SaveStatus::kNotCreated;
        if (savedErrno == EEXIST || savedErrno == ELOOP) {
            result.error = "refusing to write, path already exists: " + path;
        } else {
            result.error = std::string("cannot create ") + fileKind +
                           " file '" + path + "': " +
                           errnoDescription(savedErrno);
        }
        return result;
    }

    FdGuard guard(fd);
    result.status = SaveStatus::kCleanedUp;  // assumed until the save completes
    std::string error;

    // If the permissions cannot be guaranteed, this run must not be reported
    // as a success even though the output was produced and the file was
    // created: skip the write and fall through to the cleanup below. For key
    // files this is strictly before the first key byte reaches the file.
    if (forceOwnerOnlyPermissions(fd, path, verifyPermissions, fileKind,
                                  error)) {
        std::size_t totalWritten = 0;
        while (totalWritten < byteCount) {
            ssize_t written =
                ::write(fd, bytes + totalWritten, byteCount - totalWritten);
            if (written < 0) {
                if (errno == EINTR) {
                    // Recoverable interrupt with no byte acknowledged: retry
                    // the same slice, so the content is neither lost nor
                    // duplicated.
                    continue;
                }
                error = std::string("failed writing ") + fileKind +
                        " file '" + path + "': " + errnoDescription(errno);
                break;
            }
            if (written == 0) {
                // Bytes were still pending but nothing was written: a save
                // failure, never an endless retry.
                error = std::string("failed writing ") + fileKind +
                        " file '" + path + "': short write";
                break;
            }
            totalWritten += static_cast<std::size_t>(written);
        }

        if (totalWritten == byteCount) {
            if (::fsync(fd) != 0) {
                error = std::string("failed syncing ") + fileKind +
                        " file '" + path + "': " + errnoDescription(errno);
            } else if (guard.finish() != 0) {
                // The descriptor is closed by the kernel even when close
                // fails, but durability could not be confirmed: do not
                // report success.
                error = std::string("failed closing ") + fileKind +
                        " file '" + path + "': " + errnoDescription(errno);
            } else {
                // The complete original output is written, synced, and
                // closed; key files verified 0600 before the first byte.
                result.status = SaveStatus::kSaved;
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
        result.status = SaveStatus::kCleanupFailed;
        result.removalWarning =
            std::string("could not remove partial ") + fileKind +
            " file '" + path + "': " + errnoDescription(errno);
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

    // Key files get the strict policy: fchmod() plus an fstat() check that
    // the mode is exactly 0600, all before the first key byte is written.
    const SaveResult save = writeOutputFile(outputPath, key.data(), key.size(),
                                            "key", /*verifyPermissions=*/true);
    if (!save.saved()) {
        // The save logic reports outcomes as data; the command line owns all
        // user-facing messages. The original failure is always the primary
        // cause; a failed cleanup is reported only as an additional warning,
        // never as a replacement, so the exit code stays 1 and the user can
        // tell that an incomplete key file may remain.
        std::cerr << "envelopefile: " << save.error << '\n';
        if (save.status == SaveStatus::kCleanupFailed) {
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

// ---------------------------------------------------------------------------
// encrypt
// ---------------------------------------------------------------------------

// Reads an entire file as raw bytes. The bytes are returned without any
// newline/NUL conversion; an empty file yields an empty vector. The path is
// never followed through a terminal symlink for the *output*, but key and
// input paths are opened normally (a symlink key/input is a legitimate file).
bool readAllBytes(const std::string& path, std::vector<unsigned char>& out,
                  std::string& error, const char* what) {
    int fd = ::open(path.c_str(), O_RDONLY);
    if (fd == -1) {
        error = std::string("cannot read ") + what + " '" + path + "': " +
                errnoDescription(errno);
        return false;
    }
    FdGuard guard(fd);

    out.clear();
    unsigned char chunk[4096];
    for (;;) {
        ssize_t got = ::read(fd, chunk, sizeof(chunk));
        if (got < 0) {
            if (errno == EINTR) {
                continue;
            }
            error = std::string("failed reading ") + what + " '" + path +
                    "': " + errnoDescription(errno);
            return false;
        }
        if (got == 0) {
            break;  // end of file
        }
        out.insert(out.end(), chunk, chunk + static_cast<std::size_t>(got));
    }
    return true;
}

// Reads exactly kKeyBytes (32) raw bytes from the key file: no trailing data,
// no text decoding, no substitute key. A short or long file is an error.
bool readKeyFile(const std::string& path, SecureBuffer& key,
                 std::string& error) {
    int fd = ::open(path.c_str(), O_RDONLY);
    if (fd == -1) {
        error = "cannot read key file '" + path + "': " +
                errnoDescription(errno);
        return false;
    }
    FdGuard guard(fd);

    std::size_t total = 0;
    while (total < kKeyBytes) {
        ssize_t got = ::read(fd, key.data() + total, kKeyBytes - total);
        if (got < 0) {
            if (errno == EINTR) {
                continue;
            }
            error = "failed reading key file '" + path + "': " +
                    errnoDescription(errno);
            return false;
        }
        if (got == 0) {
            error = "key file '" + path + "' is " +
                    std::to_string(total) + " bytes, expected exactly " +
                    std::to_string(kKeyBytes);
            return false;
        }
        total += static_cast<std::size_t>(got);
    }

    // The file must contain exactly the key, nothing after it.
    unsigned char extra;
    ssize_t got;
    do {
        got = ::read(fd, &extra, 1);
    } while (got < 0 && errno == EINTR);
    if (got < 0) {
        error = "failed reading key file '" + path + "': " +
                errnoDescription(errno);
        return false;
    }
    if (got > 0) {
        error = "key file '" + path + "' is longer than " +
                std::to_string(kKeyBytes) +
                " bytes; expected exactly 32 raw key bytes";
        return false;
    }
    return true;
}

// Seals plaintext with AES-256-GCM and builds the complete envelope. The
// header (magic/version/algorithm/nonce) is supplied as authenticated data so
// the bytes used to interpret the envelope are covered by the tag. On success
// envelope holds header || ciphertext || tag; on failure it returns false and
// sets error, without ever generating a substitute key.
bool sealEnvelope(const unsigned char* key,
                  const std::vector<unsigned char>& plaintext,
                  std::vector<unsigned char>& envelope,
                  std::string& error) {
    unsigned char nonce[kNonceBytes];
    if (RAND_bytes(nonce, static_cast<int>(kNonceBytes)) != 1) {
        error = "secure random source failed";
        const std::string detail = opensslErrorString();
        if (!detail.empty()) {
            error += ": ";
            error += detail;
        }
        OPENSSL_cleanse(nonce, sizeof(nonce));
        return false;
    }

    EVP_CIPHER_CTX* ctx = EVP_CIPHER_CTX_new();
    if (ctx == nullptr) {
        error = "cannot initialize AES-256-GCM";
        OPENSSL_cleanse(nonce, sizeof(nonce));
        return false;
    }

    int outLen = 0;
    const std::size_t bodySize = plaintext.size();
    envelope.resize(kHeaderBytes + bodySize + kTagBytes);
    unsigned char* body = envelope.data() + kHeaderBytes;
    unsigned char* tag = body + bodySize;

    auto fail = [&](const std::string& message) {
        error = message;
        EVP_CIPHER_CTX_free(ctx);
        OPENSSL_cleanse(nonce, sizeof(nonce));
        // Do not leave partial ciphertext in the caller's buffer.
        if (!envelope.empty()) {
            OPENSSL_cleanse(envelope.data(), envelope.size());
        }
        envelope.clear();
    };

    if (EVP_EncryptInit_ex(ctx, EVP_aes_256_gcm(), nullptr, nullptr,
                           nullptr) != 1) {
        fail("AES-256-GCM initialization failed");
        return false;
    }
    if (EVP_CIPHER_CTX_ctrl(ctx, EVP_CTRL_GCM_SET_IVLEN,
                            static_cast<int>(kNonceBytes), nullptr) != 1) {
        fail("cannot set GCM nonce length");
        return false;
    }
    if (EVP_EncryptInit_ex(ctx, nullptr, nullptr, key, nonce) != 1) {
        fail("cannot set GCM key and nonce");
        return false;
    }

    // Build the header first and authenticate it before any plaintext byte,
    // so magic/version/algorithm/nonce are all bound to the tag. A null output
    // marks these bytes as additional authenticated data; OpenSSL reports the
    // AAD length back, so use a dedicated counter for ciphertext below rather
    // than reusing this one (it would otherwise spoil the empty-input check).
    unsigned char* header = envelope.data();
    std::memcpy(header, kEnvelopeMagic, kEnvelopeMagicBytes);
    header[kEnvelopeMagicBytes] = kEnvelopeVersion;
    header[kEnvelopeMagicBytes + 1] = kAlgAes256Gcm;
    std::memcpy(header + kEnvelopeMagicBytes + 2, nonce, kNonceBytes);

    if (EVP_EncryptUpdate(ctx, nullptr, &outLen, header,
                          static_cast<int>(kHeaderBytes)) != 1) {
        fail("cannot authenticate envelope header");
        return false;
    }

    int ciphertextLen = 0;
    if (!plaintext.empty()) {
        if (EVP_EncryptUpdate(ctx, body, &ciphertextLen, plaintext.data(),
                              static_cast<int>(plaintext.size())) != 1) {
            fail("encryption failed");
            return false;
        }
    }
    int finalLen = 0;
    if (EVP_EncryptFinal_ex(ctx, body + ciphertextLen, &finalLen) != 1) {
        fail("encryption finalization failed");
        return false;
    }
    if (static_cast<std::size_t>(ciphertextLen) +
            static_cast<std::size_t>(finalLen) !=
        bodySize) {
        fail("ciphertext length mismatch");
        return false;
    }
    if (EVP_CIPHER_CTX_ctrl(ctx, EVP_CTRL_GCM_GET_TAG,
                            static_cast<int>(kTagBytes), tag) != 1) {
        fail("cannot obtain authentication tag");
        return false;
    }

    EVP_CIPHER_CTX_free(ctx);
    OPENSSL_cleanse(nonce, sizeof(nonce));
    return true;
}

int runEncrypt(int argc, char* argv[]) {
    std::string keyPath;
    std::string inputPath;
    std::string outputPath;
    bool haveKey = false;
    bool haveInput = false;
    bool haveOutput = false;

    // All-or-nothing parse: validate the whole argument list and reject the
    // entire operation (exit 2) before any file is read or created.
    for (int i = 2; i < argc; ++i) {
        std::string_view arg(argv[i]);
        std::string_view option;
        std::string* target = nullptr;
        bool* seen = nullptr;

        if (arg == "--key") {
            option = "--key";
            target = &keyPath;
            seen = &haveKey;
        } else if (arg == "--input") {
            option = "--input";
            target = &inputPath;
            seen = &haveInput;
        } else if (arg == "--output") {
            option = "--output";
            target = &outputPath;
            seen = &haveOutput;
        } else {
            std::cerr << "envelopefile: unsupported argument: '" << arg
                      << "'\n"
                      << kEncryptUsage;
            return 2;
        }

        if (*seen) {
            std::cerr << "envelopefile: '" << option
                      << "' specified more than once\n"
                      << kEncryptUsage;
            return 2;
        }
        if (i + 1 >= argc) {
            std::cerr << "envelopefile: option '" << option
                      << "' requires a non-empty path\n"
                      << kEncryptUsage;
            return 2;
        }
        *target = argv[++i];
        if (target->empty()) {
            std::cerr << "envelopefile: option '" << option
                      << "' requires a non-empty path\n"
                      << kEncryptUsage;
            return 2;
        }
        *seen = true;
    }

    if (!haveKey || !haveInput || !haveOutput) {
        if (!haveKey) {
            std::cerr << "envelopefile: missing required option '--key'\n"
                      << kEncryptUsage;
        } else if (!haveInput) {
            std::cerr << "envelopefile: missing required option '--input'\n"
                      << kEncryptUsage;
        } else {
            std::cerr << "envelopefile: missing required option '--output'\n"
                      << kEncryptUsage;
        }
        return 2;
    }

    // Read the exact 32-byte raw key. This buffer is wiped on every exit
    // path (success or failure), so the key never survives the operation in
    // memory and never reaches stdout/stderr.
    SecureBuffer key(kKeyBytes);
    {
        std::string error;
        if (!readKeyFile(keyPath, key, error)) {
            std::cerr << "envelopefile: " << error << '\n';
            return 1;
        }
    }

    // Read the input as raw bytes (no conversion; empty input is allowed).
    std::vector<unsigned char> plaintext;
    {
        std::string error;
        if (!readAllBytes(inputPath, plaintext, error, "input file")) {
            std::cerr << "envelopefile: " << error << '\n';
            return 1;
        }
    }

    // Seal with a fresh random nonce and an authenticated header.
    std::vector<unsigned char> envelope;
    {
        std::string error;
        if (!sealEnvelope(key.data(), plaintext, envelope, error)) {
            std::cerr << "envelopefile: " << error << '\n';
            return 1;
        }
    }

    // Save to a brand new path with the shared pipeline; any existing path,
    // including the input or key path itself, is rejected before it is
    // touched. The envelope keeps the lighter permission policy: fchmod()
    // must succeed, but the key file's fstat() verification is not performed.
    const SaveResult save = writeOutputFile(
        outputPath, envelope.data(), envelope.size(), "envelope",
        /*verifyPermissions=*/false);
    if (!save.saved()) {
        std::cerr << "envelopefile: " << save.error << '\n';
        if (save.status == SaveStatus::kCleanupFailed) {
            std::cerr << "envelopefile: warning: " << save.removalWarning
                      << '\n';
        }
        return 1;
    }

    // The original input and key files are never opened for writing, so
    // their contents and permissions are unchanged. The key is wiped by the
    // SecureBuffer destructor as this scope exits.
    std::cout << "envelopefile: file encrypted and envelope saved to '"
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

    if (argc >= 2 && std::string_view(argv[1]) == "encrypt") {
        return runEncrypt(argc, argv);
    }

    std::cerr << kUsage;
    return 2;
}
