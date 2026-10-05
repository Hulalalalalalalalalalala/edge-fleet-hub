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
// Finer-grained, ordered write scenarios use EF_TEST_WRITE_SCRIPT: a
// comma-separated list of actions; every write() attempt on the key file
// consumes the next action, and once the script is exhausted writes proceed
// normally (i.e. the simulated fault condition has recovered):
//
//   pN  write at most N bytes on this attempt (a "short write")
//   i   report a retryable interruption: -1 / errno=EINTR, nothing written
//   e   report an unrecoverable I/O error: -1 / errno=EIO, nothing written
//   z   report zero bytes written (0), nothing written
//   ok  perform a normal full write
//
// For example "p7,i,p7" writes 7 bytes, then reports EINTR (the caller must
// re-present the same remaining slice), then writes another 7 bytes; later
// attempts are normal again. When EF_TEST_WRITE_SCRIPT is set the legacy
// EF_TEST_FAIL_WRITE / EF_TEST_PARTIAL_WRITE knobs are ignored for write().
//
// Independently of the injected scenario, the shim watches every byte the
// product asks to write and its file offset: bytes re-presented after EINTR
// or a short write must be byte-for-byte identical to the bytes presented at
// that offset before. A product that regenerated key material mid-loop, lost
// already-saved bytes, or duplicated a slice would break this invariant; the
// shim then aborts (with a fixed message that never contains key bytes) so
// the run cannot be mistaken for success.
//
// Only the file descriptor opened for the key file is instrumented; all
// other I/O (stdout, stderr, OpenSSL internals) passes through untouched.

#include <cerrno>
#include <cstdarg>
#include <cstdlib>
#include <cstring>
#include <fcntl.h>
#include <string>
#include <unistd.h>
#include <vector>

extern "C" {

// Real libc entry points provided by the --wrap linker mechanism.
int __real_open(const char* path, int flags, ...);
ssize_t __real_write(int fd, const void* buffer, size_t count);
int __real_fsync(int fd);
int __real_close(int fd);

namespace {

// Descriptor currently known to belong to the key file under test, or -1.
int g_keyFd = -1;

// Write-stream watchdog state (see file header): every byte the product has
// presented at each logical file offset, the offset advanced by successful
// real writes, and whether all overlapping presentations have agreed.
std::vector<unsigned char> g_presented;
std::size_t g_presentedOffset = 0;
std::size_t g_scriptIndex = 0;
bool g_streamConsistent = true;

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

void resetWriteWatchdog() {
    g_presented.clear();
    g_presentedOffset = 0;
    g_scriptIndex = 0;
    g_streamConsistent = true;
}

// Records the bytes the product is presenting for the logical range
// [g_presentedOffset, g_presentedOffset + count) and flags any disagreement
// with bytes previously presented at the same offset.
void recordPresented(const void* buffer, std::size_t count) {
    const auto* bytes = static_cast<const unsigned char*>(buffer);
    for (std::size_t i = 0; i < count; ++i) {
        const std::size_t position = g_presentedOffset + i;
        if (position < g_presented.size()) {
            if (g_presented[position] != bytes[i]) {
                g_streamConsistent = false;
            }
        } else {
            if (position != g_presented.size()) {
                // Hole in the logical stream (an offset was skipped).
                g_streamConsistent = false;
            }
            g_presented.push_back(bytes[i]);
        }
    }
}

struct WriteAction {
    enum Kind {
        kNormal,
        kPartial,
        kInterrupt,
        kIoError,
        kZero,
    } kind = kNormal;
    long limit = 0;
};

// Aborts with a fixed, key-material-free diagnostic; used only when the test
// harness itself was misconfigured or the product violated the single-key
// write-stream invariant.
[[noreturn]] void shimAbort(const char* message) {
    const char prefix[] = "envelopefile_testable: ";
    (void)::write(STDERR_FILENO, prefix, sizeof(prefix) - 1);
    (void)::write(STDERR_FILENO, message, std::strlen(message));
    (void)::write(STDERR_FILENO, "\n", 1);
    std::abort();
}

std::vector<WriteAction> parseWriteScript(const char* raw) {
    std::vector<WriteAction> actions;
    const std::string spec(raw);
    std::size_t start = 0;
    while (true) {
        const std::size_t comma = spec.find(',', start);
        const std::string token =
            spec.substr(start, comma == std::string::npos
                                     ? std::string::npos
                                     : comma - start);
        WriteAction action;
        if (token == "ok") {
            action.kind = WriteAction::kNormal;
        } else if (token == "i") {
            action.kind = WriteAction::kInterrupt;
        } else if (token == "e") {
            action.kind = WriteAction::kIoError;
        } else if (token == "z") {
            action.kind = WriteAction::kZero;
        } else if (token.size() > 1 && token[0] == 'p') {
            const char* text = token.c_str() + 1;
            char* end = nullptr;
            const long limit = std::strtol(text, &end, 10);
            if (*end != '\0' || limit <= 0) {
                shimAbort("invalid EF_TEST_WRITE_SCRIPT token");
            }
            action.kind = WriteAction::kPartial;
            action.limit = limit;
        } else {
            shimAbort("invalid EF_TEST_WRITE_SCRIPT token");
        }
        actions.push_back(action);
        if (comma == std::string::npos) {
            break;
        }
        start = comma + 1;
    }
    return actions;
}

// Applies one scripted write() attempt. Returns true when the caller should
// perform the real write with (possibly shortened) count, false when an
// injected return value has already been placed in *result.
bool applyScript(std::size_t count, std::size_t& effectiveCount,
                 ssize_t& result) {
    static const std::vector<WriteAction> actions =
        parseWriteScript(std::getenv("EF_TEST_WRITE_SCRIPT"));
    const WriteAction action =
        g_scriptIndex < actions.size()
            ? actions[g_scriptIndex++]
            : WriteAction{WriteAction::kNormal, 0};
    switch (action.kind) {
        case WriteAction::kInterrupt:
            errno = EINTR;
            result = -1;
            return false;
        case WriteAction::kIoError:
            errno = EIO;
            result = -1;
            return false;
        case WriteAction::kZero:
            result = 0;
            return false;
        case WriteAction::kPartial:
            if (static_cast<std::size_t>(action.limit) < count) {
                effectiveCount = static_cast<std::size_t>(action.limit);
            }
            return true;
        case WriteAction::kNormal:
            return true;
    }
    shimAbort("unhandled EF_TEST_WRITE_SCRIPT action");
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
        resetWriteWatchdog();
    }
    return fd;
}

ssize_t __wrap_write(int fd, const void* buffer, size_t count) {
    if (!isKeyFd(fd)) {
        return __real_write(fd, buffer, count);
    }

    recordPresented(buffer, count);

    if (std::getenv("EF_TEST_WRITE_SCRIPT") != nullptr) {
        std::size_t effectiveCount = count;
        ssize_t injectedResult = 0;
        if (!applyScript(count, effectiveCount, injectedResult)) {
            return injectedResult;
        }
        const ssize_t written = __real_write(fd, buffer, effectiveCount);
        if (written > 0) {
            g_presentedOffset += static_cast<std::size_t>(written);
        }
        return written;
    }

    // Legacy per-call knobs, kept for the existing regression scenarios.
    if (envFlagSet("EF_TEST_FAIL_WRITE")) {
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
    const ssize_t written = __real_write(fd, buffer, count);
    if (written > 0) {
        g_presentedOffset += static_cast<std::size_t>(written);
    }
    return written;
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
        const bool streamConsistent = g_streamConsistent;
        g_keyFd = -1;
        if (!streamConsistent) {
            // The product presented conflicting bytes at the same offset:
            // it lost, duplicated, or regenerated key material. Fail loudly
            // instead of letting the run report success.
            shimAbort("write stream inconsistent across retries");
        }
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
