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
// Only the file descriptor opened for the key file is instrumented; all
// other I/O (stdout, stderr, OpenSSL internals, the log file itself) passes
// through untouched.

#include <cerrno>
#include <cstdarg>
#include <cstdint>
#include <cstdio>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <unistd.h>

extern "C" {

// Real libc entry points provided by the --wrap linker mechanism.
int __real_open(const char* path, int flags, ...);
ssize_t __real_write(int fd, const void* buffer, size_t count);
int __real_fsync(int fd);
int __real_close(int fd);

namespace {

constexpr std::size_t kKeyBytes = 32;

// Descriptor currently known to belong to the key file under test, or -1.
int g_keyFd = -1;

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

void logLine(const char* mode, const char* text) {
    const char* logPath = std::getenv("EF_TEST_WRITE_LOG");
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
    logLine("w", line);
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
    logLine("a", line);
}

void logVerifyFailed(const char* reason) {
    char line[160];
    std::snprintf(line, sizeof(line), "VERIFY-FAILED %s", reason);
    logLine("a", line);
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

}  // extern "C"
