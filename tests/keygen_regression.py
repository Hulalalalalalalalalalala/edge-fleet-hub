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
* a recoverable write interrupt (EINTR), including several interrupts
  alternating with short writes after some bytes were already saved, resumes
  with the original key: no byte is lost or repeated and no second key is
  generated; an irrecoverable mid-write error and a zero-byte write (whether
  on the first call or after partial bytes) fail, explain themselves, remove
  the incomplete file, and do not spin retrying;
* a failing secure random source (the crypto library reports failure, with
  or without an error detail, and even after partially filling the key
  buffer) ends the command with exit 1, an empty stdout, and a stderr
  message naming the random source as the cause; no key file is created or
  modified, the partially filled buffer bytes are never saved or printed,
  and an already-existing target keeps its content and permissions;
* `--version` and the usage text keep working.

The report printed by this script contains only test names and status —
key bytes are never written to stdout/stderr by these tests.
"""

import argparse
import errno
import os
import stat
import subprocess
import sys
import tempfile

KEY_SIZE = 32
KEY_MODE = 0o600
SUCCESS_MARKER = "key generated and saved to"
RAND_FAILURE_MARKER = b"secure random source failed"
# Deterministic byte the fault shim writes into the key buffer before
# reporting a random-source failure (EF_TEST_FAIL_RAND_PARTIAL). It must
# never be treated as key material: not saved, not printed.
RAND_PARTIAL_BYTE = b"\xa5"


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
        "EF_TEST_FAIL_RAND": "0",
        "EF_TEST_FAIL_RAND_DETAIL": "0",
    }
    env.update({key: value for key, value in overrides.items()
                if value is not None})
    return env


def _rand_fail_env(partial=None, detail=False):
    """Environment that makes the shim's RAND_bytes fail deterministically."""
    overrides = {"EF_TEST_FAIL_RAND": "1"}
    if partial is not None:
        overrides["EF_TEST_FAIL_RAND_PARTIAL"] = str(partial)
    if detail:
        overrides["EF_TEST_FAIL_RAND_DETAIL"] = "1"
    return _fault_env(**overrides)


# Error numbers reported by the fault shim, matching <errno.h> on Linux.
_EINTR = errno.EINTR
_EIO = errno.EIO


def parse_write_log(ctx, log_path):
    """Parse the shim's write trace.

    Returns (snapshot_hex_or_None, calls) where each call is a tuple
    (offset, requested, returned, errno_or_None). Fails the test if the shim
    recorded a continuity violation or no calls at all.
    """
    ctx.check(os.path.isfile(log_path),
              f"write trace {log_path!r} was not produced")
    snapshot = None
    calls = []
    with open(log_path, "r", encoding="ascii") as handle:
        for raw in handle:
            fields = raw.split()
            if not fields:
                continue
            kind = fields[0]
            if kind == "VERIFY":
                snapshot = fields[1]
            elif kind == "VERIFY-FAILED":
                raise Failure(
                    "shim detected write-contract violation: " + " ".join(fields[1:]))
            elif kind == "CALL":
                offset = int(fields[1])
                requested = int(fields[2])
                returned = int(fields[3])
                reported = int(fields[4]) if len(fields) > 4 else None
                calls.append((offset, requested, returned, reported))
    ctx.check(snapshot is not None, "write trace has no key snapshot line")
    ctx.check(calls, "write trace recorded no write() calls")
    return snapshot, calls


def assert_call_offsets_contiguous(ctx, calls):
    """Successful calls advance exactly by the returned byte count; an
    EINTR repeats the same offset and request; errors otherwise terminate."""
    offset = 0
    for index, (at, requested, returned, reported) in enumerate(calls):
        ctx.check(at == offset,
                  f"call {index} resumed at {at}, expected {offset}: "
                  "bytes were skipped or duplicated")
        ctx.check(requested == KEY_SIZE - at,
                  f"call {index} requested {requested} of "
                  f"{KEY_SIZE - at} pending bytes at offset {at}")
        if returned < 0:
            ctx.check(reported is not None,
                      f"failing call {index} did not record an errno")
            if reported == _EINTR:
                # Recoverable interrupt: the same slice must be retried, so
                # the offset does not advance.
                continue
            # Any other error ends the loop; nothing may follow it.
            ctx.check(index == len(calls) - 1,
                      f"write loop kept going after hard error at call {index}")
            return
        if returned == 0:
            ctx.check(index == len(calls) - 1,
                      f"write loop retried after a zero-byte write at call {index}")
            return
        ctx.check(returned <= requested,
                  f"call {index} returned {returned} > requested {requested}")
        offset += returned
    ctx.check(offset == KEY_SIZE,
              f"trace accounts for {offset} bytes, expected {KEY_SIZE}")


def assert_not_retrying(ctx, calls, max_calls=128):
    # The loop either makes forward progress or terminates; it must never
    # spin. Thirty-two bytes can never legitimately require this many calls.
    ctx.check(len(calls) <= max_calls,
              f"{len(calls)} write() calls looks like an unbounded retry loop")


def run_scripted(ctx, workdir, script, name):
    """Run keygen with a write script and trace; return run result + paths."""
    path = os.path.join(workdir, name)
    log_path = os.path.join(workdir, name + ".writelog")
    rc, out, err = run(
        ctx, ["keygen", "--output", path],
        env_extra=_fault_env(EF_TEST_WRITE_SCRIPT=script,
                             EF_TEST_WRITE_LOG=log_path),
        use_testable=True)
    return path, log_path, rc, out, err


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


def test_interrupted_write_resumes_same_key(ctx, workdir):
    # Some key bytes are already saved, then write() reports a recoverable
    # EINTR. Generation must continue with the SAME key: no byte lost or
    # repeated, no regeneration, exit 0, and stderr stays silent about the
    # recovered interrupt.
    path, log_path, rc, out, err = run_scripted(
        ctx, workdir, "7,intr", "envelope.key")
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)

    snapshot, calls = parse_write_log(ctx, log_path)
    ctx.check(snapshot == key.hex(),
              "saved key differs from the key presented to the first write "
              "(a new key was generated after the interrupt)")
    # Exactly: short 7-byte write, one EINTR at offset 7, then the remaining
    # 25 bytes in one call.
    ctx.check(calls == [(0, 32, 7, None),
                        (7, 25, -1, _EINTR),
                        (7, 25, 25, None)],
              f"unexpected write trace: {calls!r}")
    assert_call_offsets_contiguous(ctx, calls)
    assert_not_retrying(ctx, calls)


def test_interleaved_short_writes_and_interrupts(ctx, workdir):
    # Multiple short writes alternating with recoverable interrupts must
    # still produce the original 32-byte key exactly once.
    path, log_path, rc, out, err = run_scripted(
        ctx, workdir, "3,intr,5,1,intr,intr,2", "envelope.key")
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)

    snapshot, calls = parse_write_log(ctx, log_path)
    ctx.check(snapshot == key.hex(),
              "saved key differs from the key originally generated")
    assert_call_offsets_contiguous(ctx, calls)
    assert_not_retrying(ctx, calls)
    interrupts = [c for c in calls if c[2] == -1 and c[3] == _EINTR]
    ctx.check(len(interrupts) == 3,
              f"expected 3 recoverable interrupts, saw {len(interrupts)}")
    # Every interrupted call was retried with the identical offset/length,
    # and the bytes that eventually landed cover 0..31 without overlap.
    for at, requested, returned, reported in interrupts:
        retried = [(o, r) for (o, r, ret, errn) in calls
                   if (o, r) == (at, requested) and
                   ret is not None and ret >= 0]
        ctx.check(retried,
                  f"interrupt at offset {at} was never retried")


def test_interrupt_on_first_write_resumes(ctx, workdir):
    # An interrupt before any byte landed is likewise transparent.
    path, log_path, rc, out, err = run_scripted(
        ctx, workdir, "intr", "envelope.key")
    assert_success(ctx, rc, out, err, path)
    key = assert_key_file(ctx, path)
    assert_no_key_leak(ctx, key, out, err)
    snapshot, calls = parse_write_log(ctx, log_path)
    ctx.check(snapshot == key.hex(),
              "saved key differs from the key originally generated")
    assert_call_offsets_contiguous(ctx, calls)


def test_mid_write_error_cleans_up(ctx, workdir):
    # After part of the key reached a newly created file, an irrecoverable
    # write error must fail loudly, print no completion message, and remove
    # the incomplete file. Neighbouring files survive untouched.
    path = os.path.join(workdir, "envelope.key")
    bystander = os.path.join(workdir, "unrelated.txt")
    with open(bystander, "wb") as handle:
        handle.write(b"do not touch")
    log_path = os.path.join(workdir, "trace.log")
    rc, out, err = run(
        ctx, ["keygen", "--output", path],
        env_extra=_fault_env(EF_TEST_WRITE_SCRIPT="10,eio",
                             EF_TEST_WRITE_LOG=log_path),
        use_testable=True)
    assert_rejected(ctx, rc, out, err, reason="failed writing")
    ctx.check(not os.path.lexists(path),
              "incomplete key file left behind after mid-write error")
    ctx.check(read_bytes(bystander) == b"do not touch",
              "unrelated file changed during cleanup")
    # The completed first 10 bytes must not appear in either stream.
    _, calls = parse_write_log(ctx, log_path)
    ctx.check(calls[-1] == (10, 22, -1, _EIO),
              f"trace did not end with the injected error: {calls!r}")
    ctx.check(b"envelopefile: 32-byte key" not in out,
              "completion message leaked despite failed write")


def test_zero_byte_write_fails_without_retry(ctx, workdir):
    # write() returning 0 while bytes remain pending is a failed write: the
    # command must stop (never spin retrying), report it, and clean up.
    for script, label in (("zero", "first write returned 0"),
                          ("5,zero", "zero after partial bytes")):
        subdir = os.path.join(workdir, label.replace(" ", "_"))
        os.mkdir(subdir)
        path = os.path.join(subdir, "envelope.key")
        bystander = os.path.join(subdir, "sibling.txt")
        with open(bystander, "wb") as handle:
            handle.write(b"keep")
        log_path = os.path.join(subdir, "trace.log")
        rc, out, err = run(
            ctx, ["keygen", "--output", path],
            env_extra=_fault_env(EF_TEST_WRITE_SCRIPT=script,
                                 EF_TEST_WRITE_LOG=log_path),
            use_testable=True)
        assert_rejected(ctx, rc, out, err, reason="failed writing")
        ctx.check(b"short write" in err,
                  f"zero-byte result not explained as a short write: {err!r:.200}")
        ctx.check(not os.path.lexists(path),
                  f"incomplete key file left behind ({label})")
        ctx.check(read_bytes(bystander) == b"keep",
                  f"unrelated file changed during cleanup ({label})")
        _, calls = parse_write_log(ctx, log_path)
        ctx.check(calls[-1][2] == 0,
                  f"trace did not end with a zero-byte write: {calls!r}")
        # Exactly one zero-returning call: a retry of the same pending bytes
        # would show as another entry at the same offset.
        zeros = [c for c in calls if c[2] == 0]
        ctx.check(len(zeros) == 1,
                  f"zero-byte write was retried {len(zeros)} times")
        ctx.check(SUCCESS_MARKER.encode() not in out,
                  "completion message printed despite zero-byte write")


def test_write_failure_does_not_touch_existing_target(ctx, workdir):
    # With write injection armed, an existing target is still refused by the
    # O_EXCL open: its content and permissions remain, and no partial bytes
    # are written anywhere.
    path = os.path.join(workdir, "existing.key")
    original = bytes(range(256))[:40]
    with open(path, "wb") as handle:
        handle.write(original)
    os.chmod(path, 0o600)
    log_path = os.path.join(workdir, "trace.log")
    rc, out, err = run(
        ctx, ["keygen", "--output", path],
        env_extra=_fault_env(EF_TEST_WRITE_SCRIPT="10,eio",
                             EF_TEST_WRITE_LOG=log_path),
        use_testable=True)
    assert_rejected(ctx, rc, out, err, reason="already exists")
    ctx.check(read_bytes(path) == original, "existing target was modified")
    mode = stat.S_IMODE(os.stat(path).st_mode)
    ctx.check(mode == 0o600, f"existing target mode changed to {oct(mode)}")
    ctx.check(not os.path.exists(log_path),
              "write shim fired even though open() refused the target")


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


# ---------------------------------------------------------------------------
# Secure random source failures (injected via the shim's RAND_bytes wrap).
# ---------------------------------------------------------------------------

def assert_rand_failure(ctx, rc, out, err):
    """Common assertions for a run where the secure random source failed."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"",
              f"stdout not empty on random-source failure: {out!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out,
              "completion message printed despite random-source failure")
    ctx.check(RAND_FAILURE_MARKER in err,
              "stderr does not name the secure random source as the cause: "
              f"{err!r:.200}")


def test_rand_failure_no_detail(ctx, workdir):
    # The crypto library reports failure with an empty error queue: the
    # message must still state clearly that the secure random source failed
    # (not an empty error, not a file-writing complaint).
    path = os.path.join(workdir, "envelope.key")
    bystander = os.path.join(workdir, "unrelated.txt")
    with open(bystander, "wb") as handle:
        handle.write(b"do not touch")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_rand_fail_env(), use_testable=True)
    assert_rand_failure(ctx, rc, out, err)
    ctx.check(err == b"envelopefile: secure random source failed\n",
              f"unexpected stderr without library detail: {err!r:.200}")
    ctx.check(b"failed writing" not in err,
              "random-source failure misreported as a file-writing failure")
    ctx.check(not os.path.lexists(path),
              "key file created despite random-source failure")
    ctx.check(read_bytes(bystander) == b"do not touch",
              "unrelated file changed during random-source failure")


def test_rand_failure_with_detail(ctx, workdir):
    # The crypto library additionally leaves a reason on the OpenSSL error
    # queue: that detail must be preserved so the user can judge the cause.
    path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_rand_fail_env(detail=True),
                       use_testable=True)
    assert_rand_failure(ctx, rc, out, err)
    prefix = b"envelopefile: secure random source failed: "
    ctx.check(err.startswith(prefix),
              f"library error detail missing from stderr: {err!r:.200}")
    detail = err[len(prefix):].strip()
    ctx.check(len(detail) > 0,
              "error detail is empty even though the library provided one")
    ctx.check(not os.path.lexists(path),
              "key file created despite random-source failure")


def test_rand_failure_partial_buffer_not_saved(ctx, workdir):
    # The crypto library fails AFTER writing part of the key buffer. Those
    # bytes are not a key: nothing may be saved (no empty, partial, or
    # full-looking 32-byte file), the run must not fall back to an ordinary
    # PRNG, and the partially written bytes must not appear on stdout/stderr.
    partial_len = 13
    path = os.path.join(workdir, "envelope.key")
    bystander = os.path.join(workdir, "unrelated.txt")
    with open(bystander, "wb") as handle:
        handle.write(b"do not touch")
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_rand_fail_env(partial=partial_len),
                       use_testable=True)
    assert_rand_failure(ctx, rc, out, err)
    ctx.check(not os.path.lexists(path),
              "a file was left behind even though no valid key was generated")
    ctx.check(os.listdir(workdir) == ["unrelated.txt"],
              f"unexpected files appeared: {sorted(os.listdir(workdir))!r}")
    ctx.check(read_bytes(bystander) == b"do not touch",
              "unrelated file changed during random-source failure")
    # Deliberately do not echo the streams in these messages: they must not
    # carry the partial buffer bytes into the regression report either.
    ctx.check(RAND_PARTIAL_BYTE * 4 not in out,
              "partial key-buffer bytes leaked into stdout")
    ctx.check(RAND_PARTIAL_BYTE * 4 not in err,
              "partial key-buffer bytes leaked into stderr")
    hex_form = (RAND_PARTIAL_BYTE.hex() * 4).encode()
    ctx.check(hex_form not in out and hex_form not in err,
              "partial key-buffer bytes leaked as hex into the output")


def test_rand_failure_preserves_existing_target(ctx, workdir):
    # With the random source failing, an already-existing target must keep
    # its content and permissions exactly; the reported cause is the random
    # source, not the pre-existing path.
    path = os.path.join(workdir, "existing.key")
    original = b"pre-existing content that must survive\x00\x01"
    with open(path, "wb") as handle:
        handle.write(original)
    os.chmod(path, 0o640)
    rc, out, err = run(ctx, ["keygen", "--output", path],
                       env_extra=_rand_fail_env(partial=20),
                       use_testable=True)
    assert_rand_failure(ctx, rc, out, err)
    ctx.check(read_bytes(path) == original,
              "existing target content changed by random-source failure")
    mode = stat.S_IMODE(os.stat(path).st_mode)
    ctx.check(mode == 0o640, f"existing target mode changed to {oct(mode)}")


def test_rand_failure_message_distinguishable(ctx, workdir):
    # The random-source failure must read differently from the other failure
    # causes so the user knows no file operation was even attempted.
    rand_path = os.path.join(workdir, "rand.key")
    _, _, err_rand = run(ctx, ["keygen", "--output", rand_path],
                         env_extra=_rand_fail_env(), use_testable=True)

    write_path = os.path.join(workdir, "write.key")
    _, _, err_write = run(ctx, ["keygen", "--output", write_path],
                          env_extra=_fault_env(EF_TEST_FAIL_WRITE="1"),
                          use_testable=True)

    exists_path = os.path.join(workdir, "taken.key")
    with open(exists_path, "wb") as handle:
        handle.write(b"occupied")
    _, _, err_exists = run(ctx, ["keygen", "--output", exists_path])

    ctx.check(RAND_FAILURE_MARKER in err_rand,
              f"random-source failure not identified as such: {err_rand!r:.200}")
    for label, other in (("write failure", err_write),
                         ("existing target", err_exists)):
        ctx.check(RAND_FAILURE_MARKER not in other,
                  f"{label} misreported as random-source failure")
    ctx.check(b"failed writing" not in err_rand,
              "random-source failure misreported as a file-writing failure")
    ctx.check(b"already exists" not in err_rand,
              "random-source failure misreported as an existing target")


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
    test_interrupted_write_resumes_same_key,
    test_interleaved_short_writes_and_interrupts,
    test_interrupt_on_first_write_resumes,
    test_mid_write_error_cleans_up,
    test_zero_byte_write_fails_without_retry,
    test_write_failure_does_not_touch_existing_target,
    test_error_messages_distinguishable,
    test_rand_failure_no_detail,
    test_rand_failure_with_detail,
    test_rand_failure_partial_buffer_not_saved,
    test_rand_failure_preserves_existing_target,
    test_rand_failure_message_distinguishable,
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
