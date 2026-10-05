// Fault-injection shims for the regression-test build of envelopefile.
//
// This file is linked together with src/main.cpp into the separate
// `envelopefile_testable` binary using the linker's --wrap option, so every
// open/write/fsync/close call made by the product code is routed through the
// __wrap_* functions below. The production binary is completely unaffected.
//
// Injection is controlled entirely through environment variables so the test
// driver can select a scenario per process run:
//
//   EF_TEST_TARGET          Only inject faults for this exact path. When
//                           unset, any file created with O_CREAT|O_EXCL is
//                           considered the key file under test.
//   EF_TEST_FAIL_WRITE=1    write() on the key file fails with EIO.
//   EF_TEST_PARTIAL_WRITE=N write() on the key file returns at most N bytes
//                           per call (the caller must loop to complete).
//   EF_TEST_FAIL_FSYNC=1    fsync() on the key file fails with EIO.
//   EF_TEST_FAIL_CLOSE=1    close() on the key file reports EIO (the
//                           descriptor is still really closed, matching the
//                           POSIX guarantee that close() releases the fd).
//
// For deterministic coverage of the write loop itself, a per-call script can
// be supplied:
//
//   EF_TEST_WRITE_SCRIPT=t1,t2,...
//                           Each token describes the outcome of one write()
//                           call on the key file, consumed in order; once the
//                           list is exhausted every further call succeeds
//                           normally. Tokens:
//                             ok    - write the whole requested buffer
//                             N     - short write: at most N (decimal) bytes
//                             intr  - fail with EINTR, no bytes written
//                             eio   - fail irrecoverably with EIO
//                             zero  - return 0 with bytes still pending
//
//   EF_TEST_WRITE_LOG=path  Record what the write loop actually did, so the
//                           test driver can prove byte continuity across
//                           retries. On the first key-file write the original
//                           32-byte key is snapshotted; every subsequent call
//                           is checked to advance from the current offset with
//                           a buffer that is a slice of that same snapshot
//                           (i.e. the key is neither regenerated, duplicated,
//                           nor dropped). The file receives one line:
//                             VERIFY <64 lowercase hex digits>
//                           followed by one line per write() call:
//                             CALL <offset> <requested> <returned> [<errno>]
//                           (the errno field is only present when returned is
//                           -1). A continuity violation is recorded as a
//                             VERIFY-FAILED <reason>
//                           line and aborts the process, since it means the
//                           product code violated its own write contract.
//
// The secure random source can be made to fail deterministically:
//
//   EF_TEST_FAIL_RAND=1       RAND_bytes reports failure (returns 0).
//   EF_TEST_RAND_PARTIAL=N    before failing, N marker bytes (0xA5) are
//                             written into the caller's key buffer,
//                             simulating a CSPRNG that dies mid-fill. The
//                             marker lets the test driver prove the partial
//                             data is neither saved nor leaked.
//   EF_TEST_RAND_ERROR_DETAIL=1
//                             leave a descriptive OpenSSL error on the
//                             thread's error queue so the failure carries
//                             detail text; when unset the queue is cleared
//                             instead, exercising the no-detail message.
//
// Key-buffer lifecycle events are recorded when requested:
//
//   EF_TEST_KEY_LOG=path      one line per event:
//                               RAND <requested> partial=<N> result=<ok|fail>
//                               CLEANSED-KEY <len>
//                             The CLEANSED-KEY line is emitted when the
//                             buffer previously handed to RAND_bytes is
//                             wiped via OPENSSL_cleanse, proving the key
//                             (including any partial bytes) does not survive
//                             the operation in memory.
//
// Only the file descriptor opened for the key file is instrumented; all
// other I/O (stdout, stderr, OpenSSL internals, the log file itself) passes
// through untouched.

#include <openssl/err.h>
#include <openssl/opensslv.h>
#include <openssl/rand.h>

#include <cerrno>
#include <cstdarg>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <unistd.h>

extern "C" {

// Real libc/libcrypto entry points provided by the --wrap linker mechanism.
int __real_open(const char* path, int flags, ...);
ssize_t __real_write(int fd, const void* buffer, size_t count);
int __real_fsync(int fd);
int __real_close(int fd);
int __real_RAND_bytes(unsigned char* buf, int num);
void __real_OPENSSL_cleanse(void* ptr, size_t len);

namespace {

constexpr std::size_t kKeyBytes = 32;

// Descriptor currently known to belong to the key file under test, or -1.
int g_keyFd = -1;

// Key buffer most recently handed to RAND_bytes, so the OPENSSL_cleanse
// wrapper can recognise (and log) its wipe.
unsigned char* g_randBuf = nullptr;
int g_randBufSize = 0;

// Continuity bookkeeping for EF_TEST_WRITE_LOG.
unsigned char g_keySnapshot[kKeyBytes];
bool g_haveSnapshot = false;     // the snapshot below is valid
std::size_t g_progress = 0;      // bytes acknowledged so far (pre-call offset)

bool envFlagSet(const char* name) {
    const char* value = std::getenv(name);
    return value != nullptr && value[0] != '\0' && std::strcmp(value, "0") != 0;
}

bool isKeyPath(const char* path) {
    const char* target = std::getenv("EF_TEST_TARGET");
    return target == nullptr || std::strcmp(path, target) == 0;
}

bool isKeyFd(int fd) {
    return fd != -1 && fd == g_keyFd;
}

// Preserve errno across the bookkeeping I/O so the injected failure code is
// what the product code actually observes.
class ErrnoSaver {
public:
    ErrnoSaver() : saved_(errno) {}
    ~ErrnoSaver() { errno = saved_; }

private:
    int saved_;
};

void logLine(const char* pathEnv, const char* mode, const char* text) {
    const char* logPath = std::getenv(pathEnv);
    if (logPath == nullptr || logPath[0] == '\0') {
        return;
    }
    ErrnoSaver saver;
    std::FILE* file = std::fopen(logPath, mode);
    if (file == nullptr) {
        return;
    }
    std::fputs(text, file);
    std::fputc('\n', file);
    std::fclose(file);
}

void logKeyLine(const char* text) {
    logLine("EF_TEST_KEY_LOG", "a", text);
}

void logVerifyHeader(const unsigned char* buffer) {
    constexpr std::size_t kPrefix = 7;  // strlen("VERIFY ")
    char line[kPrefix + 2 * kKeyBytes + 1];
    char* out = line;
    std::strcpy(out, "VERIFY ");
    out += 7;
    static const char hex[] = "0123456789abcdef";
    for (std::size_t i = 0; i < kKeyBytes; ++i) {
        *out++ = hex[buffer[i] >> 4];
        *out++ = hex[buffer[i] & 0x0f];
    }
    *out = '\0';
    logLine("EF_TEST_WRITE_LOG", "w", line);
}

void logCall(std::size_t offset, std::size_t requested,
             ssize_t returned, int reportedErrno) {
    char line[128];
    if (returned < 0) {
        std::snprintf(line, sizeof(line), "CALL %zu %zu %zd %d",
                      offset, requested, returned, reportedErrno);
    } else {
        std::snprintf(line, sizeof(line), "CALL %zu %zu %zd",
                      offset, requested, returned);
    }
    logLine("EF_TEST_WRITE_LOG", "a", line);
}

void logVerifyFailed(const char* reason) {
    char line[160];
    std::snprintf(line, sizeof(line), "VERIFY-FAILED %s", reason);
    logLine("EF_TEST_WRITE_LOG", "a", line);
}

// On the first key-file write, snapshot the key; afterwards confirm that the
// caller resumes exactly at the acknowledged offset and presents the matching
// slice of the same key. Any mismatch is a product-code defect: abort so the
// regression run can never pass on a silently regenerated/garbled key.
void verifyIncomingBuffer(const void* buffer, std::size_t count) {
    if (std::getenv("EF_TEST_WRITE_LOG") == nullptr) {
        return;
    }
    const auto* bytes = static_cast<const unsigned char*>(buffer);
    if (!g_haveSnapshot) {
        if (count != kKeyBytes) {
            logVerifyFailed("first write did not carry the whole 32-byte key");
            std::abort();
        }
        std::memcpy(g_keySnapshot, bytes, kKeyBytes);
        g_haveSnapshot = true;
        logVerifyHeader(g_keySnapshot);
        return;
    }
    if (g_progress > kKeyBytes) {
        logVerifyFailed("offset past end of key");
        std::abort();
    }
    const std::size_t remaining = kKeyBytes - g_progress;
    if (count != remaining) {
        logVerifyFailed("write length does not match the bytes still pending");
        std::abort();
    }
    if (std::memcmp(g_keySnapshot + g_progress, bytes, remaining) != 0) {
        logVerifyFailed("buffer is not the same key (key was regenerated?)");
        std::abort();
    }
}

void resetWriteBookkeeping() {
    g_haveSnapshot = false;
    g_progress = 0;
}

// Outcome of one scripted write() call.
enum class WriteOutcome {
    kPass,       // behave as the real syscall with the given count
    kShort,      // behave as the real syscall but cap the count
    kEintr,      // report a recoverable interrupt, nothing written
    kEio,        // report an irrecoverable I/O error
    kZero,       // report zero bytes with data still pending
};

struct ScriptedCall {
    WriteOutcome outcome;
    long limit;  // valid for kShort
};

// Parse the next EF_TEST_WRITE_SCRIPT token. Returns false once the script is
// exhausted (the caller should then behave normally).
bool nextScriptedCall(ScriptedCall& call) {
    static char script[512];
    static bool initialized = false;
    static char* saveptr = nullptr;

    if (!initialized) {
        const char* raw = std::getenv("EF_TEST_WRITE_SCRIPT");
        if (raw == nullptr) {
            script[0] = '\0';
        } else {
            std::strncpy(script, raw, sizeof(script) - 1);
            script[sizeof(script) - 1] = '\0';
        }
        saveptr = script;
        initialized = true;
    }

    char* token = ::strtok_r(nullptr, ",", &saveptr);
    if (token == nullptr) {
        return false;
    }
    if (std::strcmp(token, "ok") == 0) {
        call.outcome = WriteOutcome::kPass;
    } else if (std::strcmp(token, "intr") == 0) {
        call.outcome = WriteOutcome::kEintr;
    } else if (std::strcmp(token, "eio") == 0) {
        call.outcome = WriteOutcome::kEio;
    } else if (std::strcmp(token, "zero") == 0) {
        call.outcome = WriteOutcome::kZero;
    } else {
        char* end = nullptr;
        long limit = std::strtol(token, &end, 10);
        if (end != token && *end == '\0' && limit > 0) {
            call.outcome = WriteOutcome::kShort;
            call.limit = limit;
        } else {
            // An unknown token is a test-script bug: fail loudly rather than
            // silently behave as a normal write.
            std::fprintf(stderr,
                         "envelopefile_testable: invalid write script token "
                         "'%s'\n", token);
            call.outcome = WriteOutcome::kEio;
        }
    }
    return true;
}

}  // namespace

int __wrap_open(const char* path, int flags, ...) {
    mode_t mode = 0;
    if ((flags & O_CREAT) != 0) {
        va_list args;
        va_start(args, flags);
        mode = static_cast<mode_t>(va_arg(args, int));
        va_end(args);
    }
    int fd = __real_open(path, flags, mode);
    if (fd != -1 && (flags & O_CREAT) != 0 && (flags & O_EXCL) != 0 &&
        isKeyPath(path)) {
        g_keyFd = fd;
        resetWriteBookkeeping();
    }
    return fd;
}

ssize_t __wrap_write(int fd, const void* buffer, size_t count) {
    if (!isKeyFd(fd)) {
        return __real_write(fd, buffer, count);
    }

    verifyIncomingBuffer(buffer, count);

    const size_t requested = count;  // caller's original length for the log
    ScriptedCall call;
    const bool scripted = nextScriptedCall(call);
    if (scripted) {
        switch (call.outcome) {
            case WriteOutcome::kEintr:
                logCall(g_progress, requested, -1, EINTR);
                errno = EINTR;
                return -1;
            case WriteOutcome::kEio:
                logCall(g_progress, requested, -1, EIO);
                errno = EIO;
                return -1;
            case WriteOutcome::kZero:
                logCall(g_progress, requested, 0, 0);
                errno = 0;
                return 0;
            case WriteOutcome::kShort:
                if (count > static_cast<size_t>(call.limit)) {
                    count = static_cast<size_t>(call.limit);
                }
                break;
            case WriteOutcome::kPass:
                break;
        }
    } else {
        // Legacy single-knob injection, kept for the existing regressions.
        if (envFlagSet("EF_TEST_FAIL_WRITE")) {
            logCall(g_progress, requested, -1, EIO);
            errno = EIO;
            return -1;
        }
        const char* chunk = std::getenv("EF_TEST_PARTIAL_WRITE");
        if (chunk != nullptr) {
            const long limit = std::atol(chunk);
            if (limit > 0 && count > static_cast<size_t>(limit)) {
                count = static_cast<size_t>(limit);
            }
        }
    }

    ssize_t result = __real_write(fd, buffer, count);
    logCall(g_progress, requested, result, errno);
    if (result > 0) {
        g_progress += static_cast<std::size_t>(result);
    }
    return result;
}

int __wrap_fsync(int fd) {
    if (isKeyFd(fd) && envFlagSet("EF_TEST_FAIL_FSYNC")) {
        errno = EIO;
        return -1;
    }
    return __real_fsync(fd);
}

int __wrap_close(int fd) {
    const bool wasKeyFd = isKeyFd(fd);
    const int result = __real_close(fd);
    if (wasKeyFd) {
        g_keyFd = -1;
        if (envFlagSet("EF_TEST_FAIL_CLOSE")) {
            // The descriptor is already closed for real; only the reported
            // status is faked, mirroring a genuine close(2) failure.
            errno = EIO;
            return -1;
        }
    }
    return result;
}

int __wrap_RAND_bytes(unsigned char* buf, int num) {
    if (!envFlagSet("EF_TEST_FAIL_RAND")) {
        const int result = __real_RAND_bytes(buf, num);
        if (result == 1) {
            g_randBuf = buf;
            g_randBufSize = num;
            char line[96];
            std::snprintf(line, sizeof(line),
                          "RAND %d partial=0 result=ok", num);
            logKeyLine(line);
        }
        return result;
    }

    g_randBuf = buf;
    g_randBufSize = num;

    // Simulate a CSPRNG that wrote some bytes before dying: the caller must
    // not mistake the partial fill for a valid key.
    long partial = 0;
    const char* raw = std::getenv("EF_TEST_RAND_PARTIAL");
    if (raw != nullptr) {
        partial = std::atol(raw);
        if (partial < 0) {
            partial = 0;
        }
        if (partial > num) {
            partial = num;
        }
    }
    for (long i = 0; i < partial; ++i) {
        buf[i] = 0xA5;
    }

    // Start from an empty error queue, then optionally leave a descriptive
    // OpenSSL error so the "detail" and "no detail" messages are both
    // exercised deterministically.
    ERR_clear_error();
    if (envFlagSet("EF_TEST_RAND_ERROR_DETAIL")) {
#if OPENSSL_VERSION_NUMBER >= 0x30000000L
        ERR_raise(ERR_LIB_RAND, 0x7f);
#else
        ERR_put_error(ERR_LIB_RAND, 0, 0x7f, __FILE__, __LINE__);
#endif
    }

    char line[96];
    std::snprintf(line, sizeof(line), "RAND %d partial=%ld result=fail",
                  num, partial);
    logKeyLine(line);
    return 0;
}

void __wrap_OPENSSL_cleanse(void* ptr, size_t len) {
    if (ptr != nullptr && ptr == g_randBuf) {
        char line[96];
        std::snprintf(line, sizeof(line), "CLEANSED-KEY %zu", len);
        logKeyLine(line);
        // The buffer is about to be released; do not match a stale address.
        g_randBuf = nullptr;
        g_randBufSize = 0;
    }
    __real_OPENSSL_cleanse(ptr, len);
}

}  // extern "C"
