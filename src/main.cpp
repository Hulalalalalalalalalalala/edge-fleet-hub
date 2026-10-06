#include <openssl/crypto.h>
#include <openssl/err.h>
#include <openssl/evp.h>
#include <openssl/rand.h>

#include <algorithm>
#include <array>
#include <cerrno>
#include <cstddef>
#include <cstring>
#include <fcntl.h>
#include <iostream>
#include <memory>
#include <string>
#include <string_view>
#include <sys/stat.h>
#include <sys/types.h>
#include <unistd.h>
#include <vector>

namespace {

constexpr std::size_t kKeyBytes = 32;

// Envelope format version 1 (see README.md for the full specification):
//
//   offset  size  field
//   0       8     magic: ASCII "ENVELOPE"
//   8       1     format version: 0x01
//   9       1     algorithm identifier: 0x01 = AES-256-GCM
//   10      12    nonce (fresh random bytes per encryption)
//   22      N     ciphertext (N = plaintext size in bytes)
//   22+N    16    GCM authentication tag
//
// All fixed-width integer fields are single bytes, so there is no byte-order
// encoding to consider. The 22-byte header is passed to GCM as additional
// authenticated data (AAD): every byte needed to interpret the envelope —
// the magic, the version, the algorithm, and the nonce — is covered by the
// authentication tag, and the content never depends on the file name or its
// extension.
constexpr std::array<unsigned char, 8> kEnvelopeMagic = {
    'E', 'N', 'V', 'E', 'L', 'O', 'P', 'E'};
constexpr unsigned char kEnvelopeVersion = 0x01;
constexpr unsigned char kAlgorithmAes256Gcm = 0x01;
constexpr std::size_t kNonceBytes = 12;
constexpr std::size_t kTagBytes = 16;
constexpr std::size_t kHeaderBytes =
    kEnvelopeMagic.size() + 2 + kNonceBytes;  // 22

constexpr const char* kVersionString = "envelopefile 0.1.0\n";
constexpr const char* kUsage =
    "Usage:\n"
    "  envelopefile --version\n"
    "  envelopefile keygen --output <path>\n"
    "  envelopefile encrypt --key <key-path> --input <path> --output <path>\n";
constexpr const char* kKeygenUsage =
    "Usage: envelopefile keygen --output <path>\n";
constexpr const char* kEncryptUsage =
    "Usage: envelopefile encrypt --key <key-path> --input <path> "
    "--output <path>\n";

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
    const unsigned char* data() const { return data_; }
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
bool generateRandom(SecureBuffer& buffer, std::string& error) {
    if (RAND_bytes(buffer.data(), static_cast<int>(buffer.size())) != 1) {
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
enum class SaveStatus {
    kSaved,          // all bytes written, fsynced, and closed
    kNotCreated,     // open() produced no file, so nothing had to be cleaned
    kCleanedUp,      // this run created the file, failed, and removed it
    kCleanupFailed,  // this run created the file, failed, and removal failed
};

// Complete outcome of one save attempt: whether it succeeded, why it failed
// when it did, and whether the file this run created was cleaned up. The
// save logic itself writes nothing to stdout or stderr; the command line
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

// Owns the descriptor opened for a file for the duration of one operation
// and closes it exactly once. Closing is centralized here rather than spread
// across the failure branches: a branch only records its reason, and the
// descriptor is still released on every path out of scope.
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

    // Best-effort close for failure paths: the operation has already failed,
    // and close(2) releases the descriptor even when it reports an error, so
    // the result is intentionally discarded here. Idempotent via disarming.
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

// Creates a brand new file containing exactly size raw bytes, described by
// `what` ("key file", "envelope file") in diagnostics. Any existing path
// (file, directory, symlink including a dangling one) is rejected. When
// enforceOwnerOnly is set (key files), the file is created with mode 0600 and
// its permissions are forced to exactly 0600 (the creation mode alone is
// still subject to the umask) before any byte is written; otherwise the file
// gets the ordinary 0666 & ~umask permissions of a new data file.
//
// The outcome is reported entirely through SaveResult: an open() failure
// creates nothing (kNotCreated); a failure after creation closes the
// descriptor via the FdGuard and removes this run's file, reporting either
// kCleanedUp or, if the removal itself failed, kCleanupFailed with the
// removal reason in removalWarning so the caller can warn that an incomplete
// file may remain. This function never writes to stdout or stderr.
SaveResult writeNewFile(const std::string& path,
                        const unsigned char* data,
                        std::size_t size,
                        const char* what,
                        bool enforceOwnerOnly) {
    SaveResult result;

    const mode_t createMode =
        enforceOwnerOnly
            ? (S_IRUSR | S_IWUSR)
            : (S_IRUSR | S_IWUSR | S_IRGRP | S_IWGRP | S_IROTH | S_IWOTH);
    int fd = ::open(path.c_str(),
                    O_WRONLY | O_CREAT | O_EXCL | O_NOFOLLOW,
                    createMode);
    if (fd == -1) {
        const int savedErrno = errno;
        // Pre-creation failure: nothing was created, so this must not enter
        // the "remove the new file" handling and an existing target's
        // content, permissions, and links are never touched.
        result.status = SaveStatus::kNotCreated;
        if (savedErrno == EEXIST || savedErrno == ELOOP) {
            result.error = "refusing to write, path already exists: " + path;
        } else {
            result.error = std::string("cannot create ") + what + " '" + path +
                           "': " + errnoDescription(savedErrno);
        }
        return result;
    }

    FdGuard guard(fd);
    result.status = SaveStatus::kCleanedUp;  // assumed until the save completes
    std::string error;

    // For key files: if the permissions cannot be guaranteed, this run must
    // not be reported as a success even though the file was created: skip the
    // write and fall through to the cleanup below, before any byte reaches
    // the file.
    if (!enforceOwnerOnly || fixKeyFilePermissions(fd, path, error)) {
        std::size_t totalWritten = 0;
        while (totalWritten < size) {
            ssize_t written =
                ::write(fd, data + totalWritten, size - totalWritten);
            if (written < 0) {
                if (errno == EINTR) {
                    continue;
                }
                error = std::string("failed writing ") + what + " '" + path +
                        "': " + errnoDescription(errno);
                break;
            }
            if (written == 0) {
                error = std::string("failed writing ") + what + " '" + path +
                        "': short write";
                break;
            }
            totalWritten += static_cast<std::size_t>(written);
        }

        if (totalWritten == size) {
            if (::fsync(fd) != 0) {
                error = std::string("failed syncing ") + what + " '" + path +
                        "': " + errnoDescription(errno);
            } else if (guard.finish() != 0) {
                // The descriptor is closed by the kernel even when close
                // fails, but durability could not be confirmed: do not
                // report success.
                error = std::string("failed closing ") + what + " '" + path +
                        "': " + errnoDescription(errno);
            } else {
                // The complete content is written, synced, and closed.
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
        result.removalWarning = std::string("could not remove partial ") +
                                what + " '" + path +
                                "': " + errnoDescription(errno);
    }
    result.error = std::move(error);
    return result;
}

// Reads an entire file as raw bytes: no text interpretation, no newline or
// NUL handling, an empty file yields an empty vector. `what` names the file
// kind ("key file", "input file") in diagnostics.
bool readWholeFile(const std::string& path, const char* what,
                   std::vector<unsigned char>& contents, std::string& error) {
    int fd = ::open(path.c_str(), O_RDONLY);
    if (fd == -1) {
        error = std::string("cannot read ") + what + " '" + path +
                "': " + errnoDescription(errno);
        return false;
    }
    FdGuard guard(fd);
    contents.clear();
    unsigned char chunk[65536];
    for (;;) {
        const ssize_t got = ::read(fd, chunk, sizeof(chunk));
        if (got < 0) {
            if (errno == EINTR) {
                continue;
            }
            error = std::string("cannot read ") + what + " '" + path +
                    "': " + errnoDescription(errno);
            return false;
        }
        if (got == 0) {
            return true;
        }
        contents.insert(contents.end(), chunk, chunk + got);
    }
}

// Loads the raw 32-byte key exactly as stored: the file is binary data, not
// text, so nothing is decoded, trimmed, or generated as a substitute. Any
// other length — shorter or longer — is rejected. Bytes read from a
// wrong-length file may still be key material, so they are wiped before
// returning; the accepted key lives in the caller's SecureBuffer, which the
// caller wipes on every exit path.
bool loadKey(const std::string& path, SecureBuffer& key, std::string& error) {
    std::vector<unsigned char> raw;
    if (!readWholeFile(path, "key file", raw, error)) {
        return false;
    }
    if (raw.size() != kKeyBytes) {
        error = "key file '" + path +
                "' must contain exactly 32 bytes, but contains " +
                std::to_string(raw.size());
        if (!raw.empty()) {
            OPENSSL_cleanse(raw.data(), raw.size());
        }
        return false;
    }
    std::memcpy(key.data(), raw.data(), kKeyBytes);
    OPENSSL_cleanse(raw.data(), raw.size());
    return true;
}

bool encryptionFailed(std::string& error) {
    error = "encryption failed";
    const std::string detail = opensslErrorString();
    if (!detail.empty()) {
        error += ": ";
        error += detail;
    }
    return false;
}

// AES-256-GCM via OpenSSL EVP: the 22-byte envelope header is authenticated
// as AAD, the plaintext is encrypted as raw bytes, and the full 16-byte tag
// is produced for the envelope.
bool encryptAes256Gcm(const SecureBuffer& key,
                      const unsigned char* nonce,
                      const unsigned char* aad,
                      std::size_t aadSize,
                      const unsigned char* plaintext,
                      std::size_t plaintextSize,
                      std::vector<unsigned char>& ciphertext,
                      unsigned char* tag,
                      std::string& error) {
    const std::unique_ptr<EVP_CIPHER_CTX, decltype(&EVP_CIPHER_CTX_free)> ctx(
        EVP_CIPHER_CTX_new(), &EVP_CIPHER_CTX_free);
    if (ctx == nullptr) {
        return encryptionFailed(error);
    }
    if (EVP_EncryptInit_ex(ctx.get(), EVP_aes_256_gcm(), nullptr, nullptr,
                           nullptr) != 1) {
        return encryptionFailed(error);
    }
    if (EVP_CIPHER_CTX_ctrl(ctx.get(), EVP_CTRL_GCM_SET_IVLEN,
                            static_cast<int>(kNonceBytes), nullptr) != 1) {
        return encryptionFailed(error);
    }
    if (EVP_EncryptInit_ex(ctx.get(), nullptr, nullptr, key.data(), nonce) !=
        1) {
        return encryptionFailed(error);
    }
    int outLength = 0;
    if (EVP_EncryptUpdate(ctx.get(), nullptr, &outLength, aad,
                          static_cast<int>(aadSize)) != 1) {
        return encryptionFailed(error);
    }
    ciphertext.resize(plaintextSize + kTagBytes);
    // Feed the plaintext in bounded chunks so a very large input cannot
    // overflow EVP's int length parameter. GCM is a stream construction, so
    // each call produces exactly as many bytes as it consumes.
    std::size_t offset = 0;
    while (offset < plaintextSize) {
        const std::size_t chunk =
            std::min<std::size_t>(plaintextSize - offset, 1u << 30);
        if (EVP_EncryptUpdate(ctx.get(), ciphertext.data() + offset,
                              &outLength, plaintext + offset,
                              static_cast<int>(chunk)) != 1) {
            return encryptionFailed(error);
        }
        offset += chunk;
    }
    int finalLength = 0;
    if (EVP_EncryptFinal_ex(ctx.get(), ciphertext.data() + offset,
                            &finalLength) != 1) {
        return encryptionFailed(error);
    }
    ciphertext.resize(offset + static_cast<std::size_t>(finalLength));
    if (EVP_CIPHER_CTX_ctrl(ctx.get(), EVP_CTRL_GCM_GET_TAG,
                            static_cast<int>(kTagBytes), tag) != 1) {
        return encryptionFailed(error);
    }
    return true;
}

// Parses one --option <path> pair for the encrypt command. Returns 0 to keep
// parsing, or the exit code (2) after reporting the usage error.
int parsePathOption(const char* name, int& index, int argc, char* argv[],
                    std::string& destination, bool& alreadyHave) {
    if (alreadyHave) {
        std::cerr << "envelopefile: '" << name << "' specified more than "
                     "once\n"
                  << kEncryptUsage;
        return 2;
    }
    if (index + 1 >= argc || argv[index + 1][0] == '\0') {
        std::cerr << "envelopefile: option '" << name << "' requires a "
                     "non-empty path\n"
                  << kEncryptUsage;
        return 2;
    }
    destination = argv[++index];
    alreadyHave = true;
    return 0;
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
    if (!generateRandom(key, error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    const SaveResult save =
        writeNewFile(outputPath, key.data(), key.size(), "key file", true);
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

int runEncrypt(int argc, char* argv[]) {
    std::string keyPath;
    std::string inputPath;
    std::string outputPath;
    bool haveKey = false;
    bool haveInput = false;
    bool haveOutput = false;

    // The whole argument list is validated before anything is read or
    // encrypted: any usage error rejects the run with exit 2 and no file is
    // created, modified, or even opened. Options may appear in any order,
    // and spaces inside a non-empty path are part of the file name.
    for (int i = 2; i < argc; ++i) {
        std::string_view arg(argv[i]);
        int parsed = 0;
        if (arg == "--key") {
            parsed = parsePathOption("--key", i, argc, argv, keyPath, haveKey);
        } else if (arg == "--input") {
            parsed =
                parsePathOption("--input", i, argc, argv, inputPath, haveInput);
        } else if (arg == "--output") {
            parsed = parsePathOption("--output", i, argc, argv, outputPath,
                                     haveOutput);
        } else {
            std::cerr << "envelopefile: unsupported argument: '" << arg
                      << "'\n"
                      << kEncryptUsage;
            return 2;
        }
        if (parsed != 0) {
            return parsed;
        }
    }

    if (!haveKey) {
        std::cerr << "envelopefile: missing required option '--key'\n"
                  << kEncryptUsage;
        return 2;
    }
    if (!haveInput) {
        std::cerr << "envelopefile: missing required option '--input'\n"
                  << kEncryptUsage;
        return 2;
    }
    if (!haveOutput) {
        std::cerr << "envelopefile: missing required option '--output'\n"
                  << kEncryptUsage;
        return 2;
    }

    // The key buffer is wiped by the SecureBuffer destructor on every path
    // out of this function, success or failure.
    SecureBuffer key(kKeyBytes);
    std::string error;
    if (!loadKey(keyPath, key, error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    std::vector<unsigned char> plaintext;
    if (!readWholeFile(inputPath, "input file", plaintext, error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    // A fresh nonce from the secure random source for every encryption;
    // reusing a nonce with the same AES-GCM key would break confidentiality,
    // so it is never derived from the file contents or a counter.
    SecureBuffer nonce(kNonceBytes);
    if (!generateRandom(nonce, error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    std::array<unsigned char, kHeaderBytes> header{};
    std::memcpy(header.data(), kEnvelopeMagic.data(), kEnvelopeMagic.size());
    header[kEnvelopeMagic.size()] = kEnvelopeVersion;
    header[kEnvelopeMagic.size() + 1] = kAlgorithmAes256Gcm;
    std::memcpy(header.data() + kEnvelopeMagic.size() + 2, nonce.data(),
                kNonceBytes);

    std::vector<unsigned char> ciphertext;
    std::array<unsigned char, kTagBytes> tag{};
    if (!encryptAes256Gcm(key, nonce.data(), header.data(), header.size(),
                          plaintext.data(), plaintext.size(), ciphertext,
                          tag.data(), error)) {
        std::cerr << "envelopefile: " << error << '\n';
        return 1;
    }

    std::vector<unsigned char> envelope;
    envelope.reserve(kHeaderBytes + ciphertext.size() + kTagBytes);
    envelope.insert(envelope.end(), header.begin(), header.end());
    envelope.insert(envelope.end(), ciphertext.begin(), ciphertext.end());
    envelope.insert(envelope.end(), tag.begin(), tag.end());

    // The output path is created exclusively: any existing path — including
    // the input or the key file itself, a directory, or a dangling symlink —
    // is rejected and never modified.
    const SaveResult save = writeNewFile(outputPath, envelope.data(),
                                         envelope.size(), "envelope file",
                                         false);
    if (!save.saved()) {
        std::cerr << "envelopefile: " << save.error << '\n';
        if (save.status == SaveStatus::kCleanupFailed) {
            std::cerr << "envelopefile: warning: " << save.removalWarning
                      << '\n';
        }
        return 1;
    }

    std::cout << "envelopefile: encrypted envelope saved to '" << outputPath
              << "'\n";
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
