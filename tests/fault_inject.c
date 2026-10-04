/*
 * Fault-injection shim for envelopefile keygen regression tests.
 *
 * Loaded via LD_PRELOAD. It watches open() for the path named by
 * FI_TARGET_PATH and remembers the returned descriptor; only that
 * descriptor is faulted, so the program's stdout/stderr and any other
 * I/O behave normally.
 *
 * Environment knobs (all optional):
 *   FI_TARGET_PATH    path of the key file to fault (exact string match)
 *   FI_WRITE_FAIL=1   write() on the target fd fails with EIO
 *   FI_WRITE_PARTIAL=N  first write() on the target fd performs only N
 *                     bytes (a real short write); later writes pass through
 *   FI_FSYNC_FAIL=1   fsync() on the target fd fails with EIO
 *   FI_CLOSE_FAIL=1   close() on the target fd really closes it but
 *                     reports EIO, mimicking a late durability failure
 */
#define _GNU_SOURCE
#include <dlfcn.h>
#include <errno.h>
#include <fcntl.h>
#include <stdarg.h>
#include <stdlib.h>
#include <string.h>
#include <sys/syscall.h>
#include <unistd.h>

static int target_fd = -1;
static int partial_done = 0;
static int write_fail = 0;
static long partial_bytes = 0;
static int fsync_fail = 0;
static int close_fail = 0;
static const char *target_path = NULL;
static int initialized = 0;

static ssize_t (*real_write)(int, const void *, size_t) = NULL;
static int (*real_fsync)(int) = NULL;
static int (*real_close)(int) = NULL;

static void fi_init(void) {
    if (initialized) {
        return;
    }
    initialized = 1;
    real_write = dlsym(RTLD_NEXT, "write");
    real_fsync = dlsym(RTLD_NEXT, "fsync");
    real_close = dlsym(RTLD_NEXT, "close");

    target_path = getenv("FI_TARGET_PATH");
    write_fail = getenv("FI_WRITE_FAIL") != NULL;
    fsync_fail = getenv("FI_FSYNC_FAIL") != NULL;
    close_fail = getenv("FI_CLOSE_FAIL") != NULL;
    const char *partial = getenv("FI_WRITE_PARTIAL");
    if (partial != NULL) {
        partial_bytes = strtol(partial, NULL, 10);
        if (partial_bytes < 0) {
            partial_bytes = 0;
        }
    }
}

static void maybe_track(const char *path, int fd) {
    if (fd >= 0 && target_path != NULL && path != NULL &&
        strcmp(path, target_path) == 0) {
        target_fd = fd;
    }
}

/* open() is variadic; routing through the openat syscall avoids forwarding
 * va_list through dlsym, which is not portable. */
int open(const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = va_arg(ap, mode_t);
        va_end(ap);
    }
    fi_init();
    int fd = (int)syscall(SYS_openat, AT_FDCWD, path, flags, mode);
    maybe_track(path, fd);
    return fd;
}

int open64(const char *path, int flags, ...) {
    mode_t mode = 0;
    if (flags & O_CREAT) {
        va_list ap;
        va_start(ap, flags);
        mode = va_arg(ap, mode_t);
        va_end(ap);
    }
    fi_init();
    int fd = (int)syscall(SYS_openat, AT_FDCWD, path, flags, mode);
    maybe_track(path, fd);
    return fd;
}

ssize_t write(int fd, const void *buf, size_t count) {
    fi_init();
    if (fd == target_fd && target_fd != -1) {
        if (write_fail) {
            errno = EIO;
            return -1;
        }
        if (partial_bytes > 0 && !partial_done) {
            partial_done = 1;
            size_t n = (size_t)partial_bytes < count ? (size_t)partial_bytes
                                                     : count;
            /* Really write the short count so the file offset advances
             * exactly like a genuine short write. */
            return real_write(fd, buf, n);
        }
    }
    return real_write(fd, buf, count);
}

int fsync(int fd) {
    fi_init();
    if (fd == target_fd && target_fd != -1 && fsync_fail) {
        errno = EIO;
        return -1;
    }
    return real_fsync(fd);
}

int close(int fd) {
    fi_init();
    if (fd == target_fd && target_fd != -1) {
        target_fd = -1;
        if (close_fail) {
            /* The descriptor is closed regardless; only the reported
             * status is a failure, matching close(2) semantics. */
            real_close(fd);
            errno = EIO;
            return -1;
        }
    }
    return real_close(fd);
}
