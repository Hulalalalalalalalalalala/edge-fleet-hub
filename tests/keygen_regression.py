#!/usr/bin/env python3
"""Regression tests for `envelopefile keygen`.

Covers the guarantees around the user-chosen key file location:

* success leaves exactly 32 raw bytes with owner-only permissions, exit 0,
  and a completion message that names the path without leaking key material;
* an already-existing target (regular file, empty file, directory, symlink,
  dangling symlink) is rejected with a non-zero status, an explanation on
  stderr, and the pre-existing target left byte-for-byte untouched;
* failures after the new file was created (write/fsync/close) never report
  success, return non-zero, clean up the incomplete key file, and leave
  neighbouring files alone; a partially-written file that is completed by
  later writes still yields a full success;
* `--version` and the usage text keep working.

The report printed by this script contains only test names and status —
key bytes are never written to stdout/stderr by these tests.
"""

import argparse
import os
import stat
import subprocess
import sys
import tempfile

KEY_SIZE = 32
KEY_MODE = 0o600
SUCCESS_MARKER = "key generated and saved to"


class Failure(Exception):
    pass


class Context:
    def __init__(self, binary, testable_binary):
        self.binary = binary
        self.testable_binary = testable_binary
        self.failures = []
        self.total = 0

    def check(self, condition, message):
        if not condition:
            raise Failure(message)


def run(ctx, args, env_extra=None, use_testable=False, umask=None):
    """Run the binary and capture exit code/stdout/stderr as bytes."""
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    kwargs = {}
    if umask is not None:
        def set_umask():
            os.umask(umask)
        kwargs["preexec_fn"] = set_umask
    binary = ctx.testable_binary if use_testable else ctx.binary
    proc = subprocess.run(
        [binary, *args],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        **kwargs,
    )
    return proc.returncode, proc.stdout, proc.stderr


def read_bytes(path):
    with open(path, "rb") as handle:
        return handle.read()


def assert_success(ctx, rc, out, err, path):
    """Common assertions for a successful keygen run."""
    ctx.check(rc == 0, f"expected exit 0, got {rc} (stderr: {err!r:.200})")
    ctx.check(SUCCESS_MARKER.encode() in out,
              f"stdout missing completion message: {out!r:.200}")
    ctx.check(str(path).encode() in out,
              "completion message does not contain the save path")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")


def assert_key_file(ctx, path):
    """The produced file is exactly 32 raw bytes, readable only by owner."""
    ctx.check(os.path.isfile(path) and not os.path.islink(path),
              "key file missing or not a regular file")
    data = read_bytes(path)
    ctx.check(len(data) == KEY_SIZE,
              f"key file is {len(data)} bytes, expected {KEY_SIZE}")
    mode = stat.S_IMODE(os.stat(path).st_mode)
    ctx.check(mode == KEY_MODE,
              f"key file mode is {oct(mode)}, expected {oct(KEY_MODE)}")
    return data


def assert_no_key_leak(ctx, key, out, err):
    ctx.check(key not in out, "key material leaked into stdout")
    ctx.check(key not in err, "key material leaked into stderr")


def assert_rejected(ctx, rc, out, err, reason=None):
    """Common assertions for a refused keygen run."""
    ctx.check(rc != 0, f"expected non-zero exit, got 0 (stdout: {out!r:.200})")
    ctx.check(SUCCESS_MARKER.encode() not in out,
              f"success message printed for rejected run: {out!r:.200}")
    ctx.check(len(err) > 0, "stderr empty: rejection reason not reported")
    if reason is not None:
        ctx.check(reason.encode() in err,
                  f"stderr does not mention {reason!r}: {err!r:.200}")


# ---------------------------------------------------------------------------
# Individual test cases. Each takes (ctx, workdir) and raises Failure.
# ---------------------------------------------------------------------------

def test_version(ctx, workdir):
    rc, out, err = run(ctx, ["--version"])
    ctx.check(rc == 0, f"--version exited {rc}")
    ctx.check(out == b"envelopefile 0.1.0\n", f"unexpected version: {out!r}")
    ctx.check(err == b"", f"--version wrote to stderr: {err!r}")


def test_usage_no_args(ctx, workdir):
    rc, out, err = run(ctx, [])
    ctx.check(rc == 2, f"bare invocation exited {rc}, expected 2")
    ctx.check(b"Usage:" in err, "usage text missing from stderr")


def test_success_new_file(ctx, workdir):
    path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", path])
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)


def test_success_permissive_umask(ctx, workdir):
    # Even when the user's umask would create world-accessible files, the
    # key must not be exposed to other users.
    path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", path], umask=0o000)
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)


def test_success_path_with_spaces(ctx, workdir):
    subdir = os.path.join(workdir, "my keys")
    os.mkdir(subdir)
    path = os.path.join(subdir, "envelope key v1.key")
    rc, out, err = run(ctx, ["keygen", "--output", path])
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)


def test_success_repeatable(ctx, workdir):
    # Two sequential runs each produce a complete key; the actual random
    # values are not part of the expectation (and equality is not forbidden).
    for name in ("first.key", "second.key"):
        path = os.path.join(workdir, name)
        rc, out, err = run(ctx, ["keygen", "--output", path])
        assert_success(ctx, rc, out, err, path)
        assert_key_file(ctx, path)


def test_reject_existing_file(ctx, workdir):
    path = os.path.join(workdir, "existing.key")
    original = b"pre-existing content that must survive\x00\x01"
    with open(path, "wb") as handle:
        handle.write(original)
    os.chmod(path, 0o640)
    rc, out, err = run(ctx, ["keygen", "--output", path])
    assert_rejected(ctx, rc, out, err, reason="already exists")
    ctx.check(read_bytes(path) == original, "existing file content changed")
    mode = stat.S_IMODE(os.stat(path).st_mode)
    ctx.check(mode == 0o640, f"existing file mode changed to {oct(mode)}")


def test_reject_empty_existing_file(ctx, workdir):
    path = os.path.join(workdir, "empty.key")
    with open(path, "wb"):
        pass
    os.chmod(path, 0o644)
    rc, out, err = run(ctx, ["keygen", "--output", path])
    assert_rejected(ctx, rc, out, err, reason="already exists")
    ctx.check(read_bytes(path) == b"", "empty existing file was modified")
    mode = stat.S_IMODE(os.stat(path).st_mode)
    ctx.check(mode == 0o644, f"empty file mode changed to {oct(mode)}")


def test_reject_existing_directory(ctx, workdir):
    path = os.path.join(workdir, "a-directory")
    os.mkdir(path)
    inner = os.path.join(path, "inner.txt")
    with open(inner, "wb") as handle:
        handle.write(b"keep me")
    rc, out, err = run(ctx, ["keygen", "--output", path])
    assert_rejected(ctx, rc, out, err)
    ctx.check(os.path.isdir(path), "existing directory disappeared")
    ctx.check(read_bytes(inner) == b"keep me", "directory contents changed")


def test_reject_symlink_to_file(ctx, workdir):
    target = os.path.join(workdir, "target.key")
    original = b"target content must survive"
    with open(target, "wb") as handle:
        handle.write(original)
    link = os.path.join(workdir, "link.key")
    os.symlink(target, link)
    rc, out, err = run(ctx, ["keygen", "--output", link])
    assert_rejected(ctx, rc, out, err, reason="already exists")
    ctx.check(os.path.islink(link), "symlink was replaced")
    ctx.check(os.readlink(link) == target, "symlink target changed")
    ctx.check(read_bytes(target) == original, "symlink target was modified")


def test_reject_dangling_symlink(ctx, workdir):
    target = os.path.join(workdir, "nowhere.key")
    link = os.path.join(workdir, "dangling.key")
    os.symlink(target, link)
    rc, out, err = run(ctx, ["keygen", "--output", link])
    assert_rejected(ctx, rc, out, err, reason="already exists")
    ctx.check(os.path.islink(link), "dangling symlink was replaced")
    ctx.check(os.readlink(link) == target, "dangling symlink target changed")
    ctx.check(not os.path.exists(target),
              "dangling symlink target was created")


def _fault_env(**overrides):
    env = {
        "EF_TEST_FAIL_WRITE": "0",
        "EF_TEST_FAIL_FSYNC": "0",
        "EF_TEST_FAIL_CLOSE": "0",
    }
    env.update({key: value for key, value in overrides.items()
                if value is not None})
    return env


def test_write_failure_cleans_up(ctx, workdir):
    path = os.path.join(workdir, "envelope.key")
    bystander = os.path.join(workdir, "unrelated.txt")
    with open(bystander, "wb") as handle:
        handle.write(b"do not touch")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_fault_env(EF_TEST_FAIL_WRITE="1"),
                       use_testable=True)
    assert_rejected(ctx, rc, out, err, reason="failed writing")
    ctx.check(not os.path.lexists(path),
              "incomplete key file left behind after write failure")
    ctx.check(read_bytes(bystander) == b"do not touch",
              "unrelated file changed during cleanup")


def test_fsync_failure_cleans_up(ctx, workdir):
    path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_fault_env(EF_TEST_FAIL_FSYNC="1"),
                       use_testable=True)
    assert_rejected(ctx, rc, out, err, reason="failed syncing")
    ctx.check(not os.path.lexists(path),
              "incomplete key file left behind after sync failure")


def test_close_failure_cleans_up(ctx, workdir):
    path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_fault_env(EF_TEST_FAIL_CLOSE="1"),
                       use_testable=True)
    assert_rejected(ctx, rc, out, err, reason="failed closing")
    ctx.check(not os.path.lexists(path),
              "key file left behind after close failure")


def test_partial_write_completes(ctx, workdir):
    # A write() that returns only some bytes is not an error: the remaining
    # bytes are written by later calls and the result is a full success.
    path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_fault_env(EF_TEST_PARTIAL_WRITE="7"),
                       use_testable=True)
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)


def test_error_messages_distinguishable(ctx, workdir):
    # "Target already exists" and "write failed after creation" must read
    # differently so the user can tell whether to pick another path or to
    # investigate an I/O problem.
    existing = os.path.join(workdir, "taken.key")
    with open(existing, "wb") as handle:
        handle.write(b"occupied")
    _, _, err_exists = run(ctx, ["keygen", "--output", existing])

    fresh = os.path.join(workdir, "fresh.key")
    _, _, err_write = run(ctx, ["keygen", "--output", fresh],
                          env_extra=_fault_env(EF_TEST_FAIL_WRITE="1"),
                          use_testable=True)
    ctx.check(b"already exists" in err_exists,
              f"exists-case stderr unexpected: {err_exists!r:.200}")
    ctx.check(b"failed writing" in err_write,
              f"write-failure stderr unexpected: {err_write!r:.200}")
    ctx.check(b"already exists" not in err_write,
              "write failure misreported as existing target")
    ctx.check(b"failed writing" not in err_exists,
              "existing target misreported as write failure")


ALL_TESTS = [
    test_version,
    test_usage_no_args,
    test_success_new_file,
    test_success_permissive_umask,
    test_success_path_with_spaces,
    test_success_repeatable,
    test_reject_existing_file,
    test_reject_empty_existing_file,
    test_reject_existing_directory,
    test_reject_symlink_to_file,
    test_reject_dangling_symlink,
    test_write_failure_cleans_up,
    test_fsync_failure_cleans_up,
    test_close_failure_cleans_up,
    test_partial_write_completes,
    test_error_messages_distinguishable,
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True,
                        help="path to the production envelopefile binary")
    parser.add_argument("--testable-binary", required=True,
                        help="path to the fault-injecting test build")
    parser.add_argument("--work-root", default=None,
                        help="directory under which scratch dirs are created")
    args = parser.parse_args()

    ctx = Context(os.path.abspath(args.binary),
                  os.path.abspath(args.testable_binary))

    for test in ALL_TESTS:
        ctx.total += 1
        workdir = tempfile.mkdtemp(prefix=f"ef-{test.__name__}-",
                                   dir=args.work_root)
        try:
            test(ctx, workdir)
        except Failure as exc:
            ctx.failures.append((test.__name__, str(exc)))
            print(f"FAIL {test.__name__}: {exc}")
        except Exception as exc:  # unexpected harness error
            ctx.failures.append((test.__name__, f"harness error: {exc}"))
            print(f"ERROR {test.__name__}: {exc}")
        else:
            print(f"PASS {test.__name__}")

    passed = ctx.total - len(ctx.failures)
    print(f"\n{passed}/{ctx.total} tests passed")
    return 1 if ctx.failures else 0


if __name__ == "__main__":
    sys.exit(main())
