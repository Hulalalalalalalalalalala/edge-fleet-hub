// Fault-injection shims for the regression-test build of envelopefile.
//
// This file is linked together with src/main.cpp into the separate
// `envelopefile_testable` binary using the linker's --wrap option, so every
// open/write/fsync/close/unlink call made by the product code is routed
// through the __wrap_* functions below. The production binary is completely
// unaffected.
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
//   EF_TEST_FAIL_UNLINK=1   unlink() of the key file fails with EIO after a
//                           save failure, simulating cleanup that cannot
//                           remove the incomplete file the run just created.
//                           The key file is really left on disk. Only the
//                           path created by this run is affected: unlinks of
//                           any other path pass through untouched.
//   EF_TEST_FAIL_OPEN=1     the key file's open(O_CREAT|O_EXCL) fails BEFORE
//                           any file is created — the real syscall is never
//                           reached — simulating a save location that refuses
//                           to accept the new file at creation time. The errno
//                           defaults to EACCES (a parent directory that denies
//                           creation) and can be overridden with the decimal
//                           value in EF_TEST_OPEN_ERRNO. Because the refusal
//                           is injected, the scenario is real even when the
//                           test process runs with effective permission in the
//                           directory (e.g. as root), and nothing is left on
//                           disk, so the product's pre-creation ("not created")
//                           path is exercised rather than its post-creation
//                           cleanup. Only the O_CREAT|O_EXCL key-file open is
//                           matched, so log files and every other open pass
//                           through untouched.
//   EF_TEST_FAIL_FCHMOD=1   fchmod() on the key file fails with EPERM, so the
//                           0600 permission guarantee cannot be established.
//   EF_TEST_FAIL_FSTAT=1    fstat() on the key file fails with EIO even though
//                           fchmod() reported success: the post-set permission
//                           verification cannot read the file's metadata.
//   EF_TEST_FSTAT_MODE=mmm  fstat() succeeds but the permission bits reported
//                           back are replaced with the octal mode mmm (only the
//                           low 12 bits are taken; file type bits are kept),
//                           simulating a filesystem where fchmod() reports
//                           success but the effective mode differs, e.g. 0644
//                           (group/other can still read) or 0400 (owner write
//                           was stripped).
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
//                           nor dropped). Every call is also checked to pass a
//                           pointer INTO the exact buffer RAND_bytes filled
//                           (at the current offset), so the bytes that reach
//                           the file are provably the ones the later
//                           OPENSSL_cleanse wipes — not a copy living
//                           elsewhere. The file receives one line:
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
//                               CLEANSE-STATE writes=<N> keyfd=<open|closed>
//                             The CLEANSED-KEY line is emitted when the
//                             buffer previously handed to RAND_bytes is
//                             wiped via OPENSSL_cleanse, proving the key
//                             (including any partial bytes) does not survive
//                             the operation in memory. It is followed by a
//                             CLEANSE-STATE line capturing how many key bytes
//                             had been written and whether the key-file
//                             descriptor was already closed at that moment,
//                             so the test driver can prove the wipe happened
//                             only after the save was complete (never before,
//                             which would save wiped data).
//
// Read-only opens (the `encrypt --key` key file, and in principle the input
// file) are tracked separately from the O_CREAT|O_EXCL output descriptor.
// When EF_TEST_TARGET names a path opened O_RDONLY (without O_CREAT), that
// descriptor is the read target and its read() calls can be scripted:
//
//   EF_TEST_READ_SCRIPT=t1,t2,...
//                           Each token describes the outcome of one read()
//                           call on the tracked read-only descriptor,
//                           consumed in order; once the list is exhausted
//                           every further call performs the real read
//                           normally. Tokens:
//                             ok    - perform the whole requested read
//                             N     - short read: cap the request at N
//                                     (decimal) bytes, so a 32-byte key has
//                                     to be assembled over several calls
//                             intr  - fail with EINTR, no bytes read (the
//                                     caller must retry the same slice)
//                             eio   - fail irrecoverably with EIO
//
//                           The script spans BOTH phases of the product's key
//                           read: the calls that fill the 32 key bytes and
//                           the final one-byte end-of-file probe, so an
//                           interrupt or EIO can be injected at either point.
//
//   EF_TEST_READ_LOG=path   Record the key read for a continuity proof. The
//                           file receives one line per read() call:
//                             CALL <offset> <requested> <returned> [<errno>]
//                           (the errno field is only present when returned is
//                           -1), followed once, at the moment the 32nd key
//                           byte arrives, by:
//                             VERIFY <64 lowercase hex digits>
//                           VERIFY is a snapshot of the exact 32 bytes the
//                           product accepted into its own key buffer; the
//                           test driver compares it byte for byte against the
//                           key file on disk. Every filling call is checked
//                           to resume at the acknowledged offset with the
//                           matching remaining length, so an interrupted or
//                           short read can neither drop nor duplicate nor
//                           substitute a byte. A violation is recorded as a
//                             VERIFY-FAILED <reason>
//                           line and aborts the process, since it means the
//                           product code violated its own read contract.
//
// Two descriptors are instrumented, each on its own match: the O_EXCL-created
// output file (write-side knobs above) and the O_RDONLY open of EF_TEST_TARGET
// (read-side knobs). All other I/O (the input file when the key is the
// target, stdio, OpenSSL's own reads, the log files themselves) passes
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
#include <sys/stat.h>
#include <unistd.h>

extern "C" {

// Real libc/libcrypto entry points provided by the --wrap linker mechanism.
int __real_open(const char* path, int flags, ...);
int __real_fchmod(int fd, mode_t mode);
int __real_fstat(int fd, struct stat* info);
ssize_t __real_read(int fd, void* buffer, size_t count);
ssize_t __real_write(int fd, const void* buffer, size_t count);
int __real_fsync(int fd);
int __real_close(int fd);
int __real_unlink(const char* path);
int __real_RAND_bytes(unsigned char* buf, int num);
void __real_OPENSSL_cleanse(void* ptr, size_t len);

namespace {

constexpr std::size_t kKeyBytes = 32;

// Descriptor currently known to belong to the key file under test, or -1.
int g_keyFd = -1;

// Path the current key-file descriptor was created at, so unlink() cleanup
// can be faulted for exactly that path. Bounded storage is enough: the test
// driver only uses short scratch paths; a longer path simply is not tracked
// and its unlink passes through untouched.
char g_keyPath[4096] = {};

void setKeyPath(const char* path) {
    g_keyPath[0] = '\0';
    if (path != nullptr) {
        std::strncpy(g_keyPath, path, sizeof(g_keyPath) - 1);
        g_keyPath[sizeof(g_keyPath) - 1] = '\0';
    }
}

// Key buffer most recently handed to RAND_bytes, so the OPENSSL_cleanse
// wrapper can recognise (and log) its wipe.
unsigned char* g_randBuf = nullptr;
int g_randBufSize = 0;

// Continuity bookkeeping for EF_TEST_WRITE_LOG.
unsigned char g_keySnapshot[kKeyBytes];
bool g_haveSnapshot = false;     // the snapshot below is valid
std::size_t g_progress = 0;      // bytes acknowledged so far (pre-call offset)

// Read-side counterpart: the descriptor opened O_RDONLY for EF_TEST_TARGET
// (the encrypt --key file) is tracked independently of the O_EXCL output
// descriptor. g_readProgress counts key bytes acknowledged while the 32-byte
// key is being filled; g_readBufBase is the product's own key buffer, seen on
// the first filling call, so later calls can be proven to resume inside that
// exact buffer (and the accepted 32 bytes snapshotted for EF_TEST_READ_LOG).
int g_readFd = -1;
unsigned char* g_readBufBase = nullptr;
std::size_t g_readProgress = 0;
bool g_readVerified = false;     // the VERIFY line was already emitted

bool isReadFd(int fd) {
    return fd != -1 && fd == g_readFd;
}

void resetReadBookkeeping() {
    g_readBufBase = nullptr;
    g_readProgress = 0;
    g_readVerified = false;
}

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
// slice of the same key. Every call must also pass a pointer into the very
// buffer RAND_bytes filled (at the current offset): the bytes reaching the
// file are then provably the ones OPENSSL_cleanse later wipes, not an
// unwiped copy. Any mismatch is a product-code defect: abort so the
// regression run can never pass on a silently regenerated/garbled key.
void verifyIncomingBuffer(const void* buffer, std::size_t count) {
    if (std::getenv("EF_TEST_WRITE_LOG") == nullptr) {
        return;
    }
    const auto* bytes = static_cast<const unsigned char*>(buffer);
    if (g_randBuf != nullptr && bytes != g_randBuf + g_progress) {
        logVerifyFailed("write buffer is not the buffer filled by RAND_bytes");
        std::abort();
    }
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

// Read-side logging, mirroring the write-side CALL/VERIFY lines so the test
// driver can prove byte continuity across short/interrupted key reads.
void logReadCall(std::size_t offset, std::size_t requested,
                 ssize_t returned, int reportedErrno) {
    if (std::getenv("EF_TEST_READ_LOG") == nullptr) {
        return;
    }
    char line[128];
    if (returned < 0) {
        std::snprintf(line, sizeof(line), "CALL %zu %zu %zd %d",
                      offset, requested, returned, reportedErrno);
    } else {
        std::snprintf(line, sizeof(line), "CALL %zu %zu %zd",
                      offset, requested, returned);
    }
    logLine("EF_TEST_READ_LOG", "a", line);
}

void logReadVerify(const unsigned char* buffer) {
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
    logLine("EF_TEST_READ_LOG", "a", line);
}

void logReadVerifyFailed(const char* reason) {
    char line[160];
    std::snprintf(line, sizeof(line), "VERIFY-FAILED %s", reason);
    logLine("EF_TEST_READ_LOG", "a", line);
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

// Outcome of one scripted read() call. EOF (return 0) is deliberately not
// scriptable here: it is the normal end-of-file indicator the product must
// observe on its final probe, and a zero return while key bytes are still
// expected is the "short file" path exercised with genuinely short files.
enum class ReadOutcome {
    kPass,       // behave as the real read with the given count
    kShort,      // behave as the real read but cap the count
    kEintr,      // report a recoverable interrupt, nothing read
    kEio,        // report an irrecoverable I/O error
};

struct ScriptedReadCall {
    ReadOutcome outcome;
    long limit;  // valid for kShort
};

// Parse the next EF_TEST_READ_SCRIPT token. Returns false once the script is
// exhausted (the caller should then perform the real read normally).
bool nextScriptedReadCall(ScriptedReadCall& call) {
    static char script[512];
    static bool initialized = false;
    static char* saveptr = nullptr;

    if (!initialized) {
        const char* raw = std::getenv("EF_TEST_READ_SCRIPT");
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
        call.outcome = ReadOutcome::kPass;
    } else if (std::strcmp(token, "intr") == 0) {
        call.outcome = ReadOutcome::kEintr;
    } else if (std::strcmp(token, "eio") == 0) {
        call.outcome = ReadOutcome::kEio;
    } else {
        char* end = nullptr;
        long limit = std::strtol(token, &end, 10);
        if (end != token && *end == '\0' && limit > 0) {
            call.outcome = ReadOutcome::kShort;
            call.limit = limit;
        } else {
            // An unknown token is a test-script bug: fail loudly rather than
            // silently behave as a normal read.
            std::fprintf(stderr,
                         "envelopefile_testable: invalid read script token "
                         "'%s'\n", token);
            call.outcome = ReadOutcome::kEio;
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
    // Pre-creation refusal: fail WITHOUT calling the real open(), so the file
    // is provably never created (there is no descriptor and nothing on disk),
    // exercising the product's "open() produced no file" branch rather than
    // its post-creation cleanup. Matching only the O_CREAT|O_EXCL key open
    // keeps every other open (logs, stdio) untouched.
    if ((flags & O_CREAT) != 0 && (flags & O_EXCL) != 0 &&
        path != nullptr && isKeyPath(path) &&
        envFlagSet("EF_TEST_FAIL_OPEN")) {
        int injectedErrno = EACCES;
        const char* raw = std::getenv("EF_TEST_OPEN_ERRNO");
        if (raw != nullptr && raw[0] != '\0') {
            char* end = nullptr;
            const long requested = std::strtol(raw, &end, 10);
            if (end != raw && *end == '\0' && requested > 0) {
                injectedErrno = static_cast<int>(requested);
            }
        }
        errno = injectedErrno;
        return -1;
    }
    int fd = __real_open(path, flags, mode);
    if (fd != -1 && (flags & O_CREAT) != 0 && (flags & O_EXCL) != 0 &&
        isKeyPath(path)) {
        g_keyFd = fd;
        setKeyPath(path);
        resetWriteBookkeeping();
    }
    // Track a read-only open of the test target as the read-instrumented
    // descriptor. This is deliberately the OPPOSITE case from above: the
    // product opens the encrypt --key (and --input) file with O_RDONLY and
    // no O_CREAT, while the envelope output is the O_EXCL creation. The
    // write-side and read-side knobs therefore land on different
    // descriptors and cannot interfere. Only an exact target match is
    // tracked, so when the key is targeted the input file (and vice versa)
    // still passes every read through untouched.
    if (fd != -1 && (flags & O_CREAT) == 0 && (flags & O_WRONLY) == 0 &&
        path != nullptr && isKeyPath(path) &&
        (std::getenv("EF_TEST_READ_SCRIPT") != nullptr ||
         std::getenv("EF_TEST_READ_LOG") != nullptr)) {
        g_readFd = fd;
        resetReadBookkeeping();
    }
    return fd;
}

int __wrap_fchmod(int fd, mode_t mode) {
    if (isKeyFd(fd) && envFlagSet("EF_TEST_FAIL_FCHMOD")) {
        errno = EPERM;
        return -1;
    }
    return __real_fchmod(fd, mode);
}

int __wrap_fstat(int fd, struct stat* info) {
    if (!isKeyFd(fd)) {
        return __real_fstat(fd, info);
    }
    // This is the post-fchmod verification read: fchmod itself already
    // reported success for the key fd when the code reaches fstat.
    if (envFlagSet("EF_TEST_FAIL_FSTAT")) {
        errno = EIO;
        return -1;
    }
    const int result = __real_fstat(fd, info);
    if (result == 0) {
        const char* forced = std::getenv("EF_TEST_FSTAT_MODE");
        if (forced != nullptr && forced[0] != '\0') {
            char* end = nullptr;
            const unsigned long forcedMode = std::strtoul(forced, &end, 8);
            if (end != forced && *end == '\0') {
                // Keep the file-type bits; replace only the permission bits
                // the product code verifies against exactly 0600.
                info->st_mode =
                    (info->st_mode & static_cast<mode_t>(~07777)) |
                    (static_cast<mode_t>(forcedMode) & 07777);
            }
        }
    }
    return result;
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

ssize_t __wrap_read(int fd, void* buffer, size_t count) {
    if (!isReadFd(fd)) {
        return __real_read(fd, buffer, count);
    }

    auto* bytes = static_cast<unsigned char*>(buffer);
    const bool filling = g_readProgress < kKeyBytes;
    const std::size_t phaseOffset =
        filling ? g_readProgress : kKeyBytes;  // EOF-probe phase pins at 32

    // While the 32 key bytes are being filled the product must always read
    // into its own key buffer at the acknowledged offset, with exactly the
    // number of bytes still missing. The end-of-file probe is a one-byte
    // read into a scratch byte and is not part of the key buffer.
    if (std::getenv("EF_TEST_READ_LOG") != nullptr && filling) {
        if (g_readBufBase == nullptr) {
            g_readBufBase = bytes;
        } else if (bytes != g_readBufBase + g_readProgress) {
            logReadVerifyFailed("read buffer is not the product key buffer "
                                "at the resumed offset");
            std::abort();
        }
        if (count != kKeyBytes - g_readProgress) {
            logReadVerifyFailed("read length does not match the key bytes "
                                "still missing");
            std::abort();
        }
    }

    ScriptedReadCall call;
    if (nextScriptedReadCall(call)) {
        switch (call.outcome) {
            case ReadOutcome::kEintr:
                logReadCall(phaseOffset, count, -1, EINTR);
                errno = EINTR;
                return -1;
            case ReadOutcome::kEio:
                logReadCall(phaseOffset, count, -1, EIO);
                errno = EIO;
                return -1;
            case ReadOutcome::kShort:
                if (count > static_cast<size_t>(call.limit)) {
                    count = static_cast<size_t>(call.limit);
                }
                break;
            case ReadOutcome::kPass:
                break;
        }
    }

    ssize_t result = __real_read(fd, buffer, count);
    logReadCall(phaseOffset, count, result, errno);
    if (result > 0 && filling) {
        const std::size_t acknowledged =
            g_readProgress + static_cast<std::size_t>(result);
        // A single read must not cross the 32-byte boundary: the product's
        // fill loop stops requesting once it has 32 bytes, so overshoot
        // would mean reads and the product loop disagree on where the key
        // ends.
        if (acknowledged > kKeyBytes) {
            logReadVerifyFailed("read returned bytes past the 32-byte key");
            std::abort();
        }
        g_readProgress = acknowledged;
        if (acknowledged == kKeyBytes && !g_readVerified &&
            std::getenv("EF_TEST_READ_LOG") != nullptr) {
            // Snapshot exactly the 32 bytes the product accepted, from its
            // own key buffer, so the driver can match them against the key
            // file on disk.
            logReadVerify(g_readBufBase);
            g_readVerified = true;
        }
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
    const bool wasReadFd = isReadFd(fd);
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
    if (wasReadFd) {
        // Disarm on close so the fd number the kernel may immediately reuse
        // for a later open (e.g. the input file) is not mistaken for the
        // instrumented key descriptor.
        g_readFd = -1;
    }
    return result;
}

int __wrap_unlink(const char* path) {
    // Only the cleanup of the file this run created can be faulted, so a
    // simulated failure never blocks removal of (or any operation on) an
    // unrelated path — the product code only ever unlinks the O_EXCL-created
    // key path, and this matches the injection to exactly that name.
    if (path != nullptr && g_keyPath[0] != '\0' &&
        std::strcmp(path, g_keyPath) == 0 &&
        envFlagSet("EF_TEST_FAIL_UNLINK")) {
        errno = EIO;
        return -1;
    }
    return __real_unlink(path);
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
        // Record how far the save had progressed when the wipe happened, so
        // the test driver can prove a successful save was not preceded by
        // the wipe (which would have saved wiped data).
        char state[96];
        std::snprintf(state, sizeof(state),
                      "CLEANSE-STATE writes=%zu keyfd=%s",
                      g_progress, g_keyFd == -1 ? "closed" : "open");
        logKeyLine(state);
        // The buffer is about to be released; do not match a stale address.
        g_randBuf = nullptr;
        g_randBufSize = 0;
    }
    __real_OPENSSL_cleanse(ptr, len);
}

}  // extern "C"
