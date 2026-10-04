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
// Only the file descriptor opened for the key file is instrumented; all
// other I/O (stdout, stderr, OpenSSL internals) passes through untouched.

#include <cerrno>
#include <cstdarg>
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

// Descriptor currently known to belong to the key file under test, or -1.
int g_keyFd = -1;

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
    }
    return fd;
}

ssize_t __wrap_write(int fd, const void* buffer, size_t count) {
    if (isKeyFd(fd)) {
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
    }
    return __real_write(fd, buffer, count);
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
