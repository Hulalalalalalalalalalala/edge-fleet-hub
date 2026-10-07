#!/usr/bin/env python3
"""Regression tests for `envelopefile encrypt` envelope-save behaviour.

The round-trip/tamper suite (tests/encrypt_regression.py) proves the
envelope content is correct and authenticates; this suite guards the
save-to-disk behaviour the README promises for `encrypt --output`:

* failures AFTER the new envelope file was created — write() failing
  outright, an irrecoverable error after part of the envelope was already
  written, write() returning zero bytes, fsync() failing, and close()
  failing — exit 1 with an empty stdout and a stderr line naming both the
  failing save stage and the output path; the file this run created is
  removed, and the input, the key, and neighbouring files keep their
  content and permissions. A close() failure is a save failure even when
  every envelope byte was already written and synced: the run still fails
  and the output is cleaned up;
* cleanup failure: when removing the just-created file itself fails, the
  original save failure stays the primary error (exit 1, no completion
  message) and stderr adds a warning naming the output path and the
  removal reason, so the user knows an incomplete file may remain. The
  same injected fault is exercised with and without the removal fault so
  the "cleaned up (path absent)" outcome is directly contrasted with the
  "cleanup failed (incomplete file really left behind)" outcome — a
  non-zero exit is never mistaken for a successful deletion;
* pre-creation refusals: an existing regular file, a symlink, or a
  dangling symlink at the output path is rejected as "path already
  exists" — existing content, permissions, and link relations are
  untouched, a dangling link's target is not created, and the existing
  path is not deleted. Pointing --output at the input or the key file
  follows the same rule and never produces a cleanup warning;
* no-fault control: without an injected save fault the run exits 0, the
  completion message names the save path, and the envelope passes the
  independent AES-256-GCM authentication check (reused from
  tests/encrypt_regression.py, which shares no code with the product).

Post-creation faults are injected through the same --wrap shim build
(tests/fault_inject.cpp) the keygen suite uses, aimed at the envelope
output path via EF_TEST_TARGET; pre-creation refusals are also exercised
against the production binary. Neither the product's messages nor this
report ever contain key or plaintext bytes.
"""

import argparse
import errno
import os
import stat
import subprocess
import sys
import tempfile
import types

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
# Independent pure-Python AES-256-GCM decryptor (self-checked against
# known-answer vectors by the round-trip suite), reused so a leftover or
# successfully saved envelope is verified by code that shares nothing
# with the product's OpenSSL path.
from encrypt_regression import MAGIC, decrypt_envelope

KEY_SIZE = 32
ENVELOPE_MODE = 0o600
ENVELOPE_OVERHEAD = 37  # 21-byte header + 16-byte tag
SUCCESS_MARKER = "file encrypted and envelope saved to"
CLEANUP_WARNING_MARKER = "could not remove partial envelope file"
EXISTS_MARKER = "refusing to write, path already exists"

# Distinctive plaintext marker: proves no plaintext leaks onto the
# product's streams in any scenario.
MARKER = b"-- envelope save regression probe 0xEF5A --\n"

_EIO = os.strerror(errno.EIO)


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


def run(ctx, args, env_extra=None, use_testable=False, cwd=None):
    """Run the binary and capture exit code/stdout/stderr as bytes."""
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)
    binary = ctx.testable_binary if use_testable else ctx.binary
    proc = subprocess.run(
        [binary, *args],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        cwd=cwd,
    )
    return proc.returncode, proc.stdout, proc.stderr


def read_bytes(path):
    with open(path, "rb") as handle:
        return handle.read()


def make_fixture(ctx, workdir):
    """A valid 32-byte key, a readable input, a bystander file, and a
    fresh (not yet existing) output path — the setup every save scenario
    starts from."""
    fix = types.SimpleNamespace()

    fix.key_path = os.path.join(workdir, "envelope.key")
    rc, out, err = run(ctx, ["keygen", "--output", fix.key_path])
    ctx.check(rc == 0, f"fixture keygen failed with exit {rc}: {err!r:.200}")
    fix.key = read_bytes(fix.key_path)
    ctx.check(len(fix.key) == KEY_SIZE,
              f"fixture key is {len(fix.key)} bytes, expected {KEY_SIZE}")
    fix.key_mode = stat.S_IMODE(os.stat(fix.key_path).st_mode)

    # Long enough that a "10,eio" write script fails well past the header,
    # i.e. after part of the envelope was already written.
    fix.plaintext = MARKER * 8 + bytes(range(256)) + b"tail"
    fix.input_path = os.path.join(workdir, "document.bin")
    with open(fix.input_path, "wb") as handle:
        handle.write(fix.plaintext)
    os.chmod(fix.input_path, 0o640)
    fix.input_mode = 0o640

    fix.bystander = os.path.join(workdir, "unrelated.txt")
    fix.bystander_content = b"do not touch"
    with open(fix.bystander, "wb") as handle:
        handle.write(fix.bystander_content)
    os.chmod(fix.bystander, 0o644)
    fix.bystander_mode = 0o644

    fix.output_path = os.path.join(workdir, "document.bin.env")
    ctx.check(not os.path.lexists(fix.output_path),
              "fixture error: output path unexpectedly exists")
    return fix


def encrypt_args(fix, output=None):
    return ["encrypt", "--key", fix.key_path, "--input", fix.input_path,
            "--output", output if output is not None else fix.output_path]


def fault_env(output_path, **overrides):
    """Fault-injection environment aimed at exactly the envelope output
    path, so the key/input opens and every unrelated file pass through."""
    env = {"EF_TEST_TARGET": output_path}
    env.update(overrides)
    return env


def assert_fixture_untouched(ctx, fix):
    """Key, input, and bystander keep content and permissions."""
    ctx.check(read_bytes(fix.key_path) == fix.key, "key file content changed")
    mode = stat.S_IMODE(os.stat(fix.key_path).st_mode)
    ctx.check(mode == fix.key_mode,
              f"key file mode changed to {oct(mode)}")
    ctx.check(read_bytes(fix.input_path) == fix.plaintext,
              "input file content changed")
    mode = stat.S_IMODE(os.stat(fix.input_path).st_mode)
    ctx.check(mode == fix.input_mode,
              f"input file mode changed to {oct(mode)}")
    ctx.check(read_bytes(fix.bystander) == fix.bystander_content,
              "neighbouring file content changed")
    mode = stat.S_IMODE(os.stat(fix.bystander).st_mode)
    ctx.check(mode == fix.bystander_mode,
              f"neighbouring file mode changed to {oct(mode)}")


def assert_no_leak(ctx, fix, out, err):
    ctx.check(fix.key not in out, "key material leaked into stdout")
    ctx.check(fix.key not in err, "key material leaked into stderr")
    ctx.check(MARKER not in out, "plaintext leaked into stdout")
    ctx.check(MARKER not in err, "plaintext leaked into stderr")


def save_error_line(path, stage, reason):
    """The exact stderr line for a post-creation save failure: the fixed
    prefix, the failing stage, the output path, and the reason."""
    return (f"envelopefile: failed {stage} envelope file '{path}': "
            f"{reason}\n").encode()


def cleanup_warning_line(path):
    return (f"envelopefile: warning: {CLEANUP_WARNING_MARKER} "
            f"'{path}': {_EIO}\n").encode()


def assert_save_failed(ctx, rc, out, err, path, stage, reason=_EIO):
    """Contract for a post-creation save failure with successful cleanup:
    exit 1, empty stdout, exactly one stderr line naming the stage and the
    output path, no completion message, no cleanup warning."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on save failure: {out!r:.200}")
    ctx.check(err == save_error_line(path, stage, reason),
              f"stderr must be exactly the {stage}-failure line naming the "
              f"output path: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported despite the failed save")
    ctx.check(CLEANUP_WARNING_MARKER.encode() not in err,
              "cleanup warning appeared although the cleanup succeeded")


def assert_save_failed_with_stuck_cleanup(ctx, rc, out, err, path, stage,
                                          reason=_EIO):
    """Contract when the cleanup unlink() also fails: the original save
    failure stays the primary error (first line), the removal warning
    follows (second line), exit code stays 1, stdout stays empty."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on save failure: {out!r:.200}")
    expected = save_error_line(path, stage, reason) + cleanup_warning_line(path)
    ctx.check(err == expected,
              f"stderr must keep the original {stage} failure first and the "
              f"removal warning second: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported despite the failed save")


def assert_exists_refusal(ctx, rc, out, err, path):
    """Contract for a pre-creation refusal: exit 1, empty stdout, exactly
    the "already exists" line — never a cleanup warning, which belongs to
    files this run created."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on refusal: {out!r:.200}")
    expected = f"envelopefile: {EXISTS_MARKER}: {path}\n".encode()
    ctx.check(err == expected,
              f"refusal must be exactly the already-exists line: {err!r:.200}")
    ctx.check(CLEANUP_WARNING_MARKER.encode() not in err,
              "a pre-creation refusal carried a cleanup warning")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported for a refused run")


# ---------------------------------------------------------------------------
# Post-creation save failures: the new envelope is removed, nothing else
# is touched.
# ---------------------------------------------------------------------------

def test_write_failure_cleans_up(ctx, workdir):
    # write() fails with EIO on the freshly created envelope file.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_WRITE="1"),
                       use_testable=True)
    assert_save_failed(ctx, rc, out, err, fix.output_path, "writing")
    ctx.check(not os.path.lexists(fix.output_path),
              "incomplete envelope left behind after write failure")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_zero_byte_write_fails_as_short_write(ctx, workdir):
    # write() returns 0 while envelope bytes are still pending: a failed
    # write, reported as a short write, not retried forever.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_WRITE_SCRIPT="zero"),
                       use_testable=True)
    assert_save_failed(ctx, rc, out, err, fix.output_path, "writing",
                       reason="short write")
    ctx.check(not os.path.lexists(fix.output_path),
              "incomplete envelope left behind after zero-byte write")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_fsync_failure_cleans_up(ctx, workdir):
    # All envelope bytes written, then fsync() fails: durability could not
    # be confirmed, so the run must not report success.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_FSYNC="1"),
                       use_testable=True)
    assert_save_failed(ctx, rc, out, err, fix.output_path, "syncing")
    ctx.check(not os.path.lexists(fix.output_path),
              "envelope left behind after sync failure")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_close_failure_cleans_up(ctx, workdir):
    # Every envelope byte was written and synced, and only close() reports
    # failure: still a save failure — exit 1, no completion message, and
    # the fully written envelope is removed rather than left behind.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_CLOSE="1"),
                       use_testable=True)
    assert_save_failed(ctx, rc, out, err, fix.output_path, "closing")
    ctx.check(not os.path.lexists(fix.output_path),
              "fully written envelope left behind after close failure")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


# ---------------------------------------------------------------------------
# Cleanup failure: the save fails after creation AND removing the new file
# fails. The original save failure stays the primary error; the warning
# only adds that an incomplete file may remain — and the file really does
# remain, observably distinct from a completed cleanup.
# ---------------------------------------------------------------------------

def test_write_failure_with_failed_cleanup_reports_both(ctx, workdir):
    # The very first write fails and the cleanup unlink fails as well.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_WRITE="1",
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_save_failed_with_stuck_cleanup(ctx, rc, out, err,
                                          fix.output_path, "writing")

    # The cleanup genuinely did not happen: the empty (no byte was ever
    # written), owner-only file this run created is still there.
    ctx.check(os.path.lexists(fix.output_path),
              "new envelope disappeared even though unlink reported failure")
    ctx.check(read_bytes(fix.output_path) == b"",
              "leftover file contains bytes although the first write failed")
    mode = stat.S_IMODE(os.stat(fix.output_path).st_mode)
    ctx.check(mode == ENVELOPE_MODE,
              f"leftover file mode is {oct(mode)}, expected "
              f"{oct(ENVELOPE_MODE)}")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_partial_write_cleanup_outcome_is_observable(ctx, workdir):
    # The same injected fault — 10 envelope bytes written, then an
    # irrecoverable EIO — is run twice: once with a working cleanup (the
    # path must be gone) and once with the removal itself failing (a
    # 10-byte incomplete envelope must really remain). A non-zero exit
    # alone can never tell these apart; the filesystem state can.
    cleaned_dir = os.path.join(workdir, "cleaned")
    os.mkdir(cleaned_dir)
    fix = make_fixture(ctx, cleaned_dir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_WRITE_SCRIPT="10,eio"),
                       use_testable=True)
    assert_save_failed(ctx, rc, out, err, fix.output_path, "writing")
    ctx.check(not os.path.lexists(fix.output_path),
              "incomplete envelope left behind after mid-write error")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)

    stuck_dir = os.path.join(workdir, "stuck")
    os.mkdir(stuck_dir)
    fix = make_fixture(ctx, stuck_dir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_WRITE_SCRIPT="10,eio",
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_save_failed_with_stuck_cleanup(ctx, rc, out, err,
                                          fix.output_path, "writing")

    # The leftover is exactly the partial envelope: its first bytes are
    # the fixed header the product writes before any ciphertext.
    ctx.check(os.path.lexists(fix.output_path),
              "partial envelope removed despite the failing cleanup")
    leftover = read_bytes(fix.output_path)
    ctx.check(len(leftover) == 10,
              f"leftover partial envelope is {len(leftover)} bytes, "
              "expected 10")
    ctx.check(leftover[:9] == MAGIC + b"\x01\x01",
              "leftover does not start with the envelope header the "
              "product writes first")
    mode = stat.S_IMODE(os.stat(fix.output_path).st_mode)
    ctx.check(mode == ENVELOPE_MODE,
              f"leftover file mode is {oct(mode)}, expected "
              f"{oct(ENVELOPE_MODE)}")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_close_failure_with_failed_cleanup_keeps_complete_envelope(
        ctx, workdir):
    # close() fails after the complete envelope was written and synced,
    # and the cleanup unlink fails too: the run is still a save failure
    # (exit 1, no completion message, warning appended) even though the
    # leftover on disk is a *complete, authentic* envelope. Byte-complete
    # output must never be reported as success when closing failed.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_CLOSE="1",
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_save_failed_with_stuck_cleanup(ctx, rc, out, err,
                                          fix.output_path, "closing")

    ctx.check(os.path.lexists(fix.output_path),
              "envelope removed despite the failing cleanup")
    leftover = read_bytes(fix.output_path)
    ctx.check(len(leftover) == len(fix.plaintext) + ENVELOPE_OVERHEAD,
              f"leftover is {len(leftover)} bytes, expected the complete "
              f"envelope of {len(fix.plaintext) + ENVELOPE_OVERHEAD}")
    ctx.check(decrypt_envelope(fix.key, leftover) == fix.plaintext,
              "control: the leftover complete envelope does not "
              "authenticate — the close fault did not do what this test "
              "requires (all bytes written before close failed)")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


# ---------------------------------------------------------------------------
# Pre-creation refusals: an existing path is never modified, unlinked, or
# created through — and the diagnostic is "already exists", never a
# cleanup warning.
# ---------------------------------------------------------------------------

def test_reject_existing_regular_file(ctx, workdir):
    fix = make_fixture(ctx, workdir)
    original = b"pre-existing envelope that must survive\x00\x01"
    with open(fix.output_path, "wb") as handle:
        handle.write(original)
    os.chmod(fix.output_path, 0o640)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_exists_refusal(ctx, rc, out, err, fix.output_path)
    ctx.check(read_bytes(fix.output_path) == original,
              "existing output file content changed")
    mode = stat.S_IMODE(os.stat(fix.output_path).st_mode)
    ctx.check(mode == 0o640, f"existing output file mode changed to {oct(mode)}")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_reject_symlink_to_file(ctx, workdir):
    fix = make_fixture(ctx, workdir)
    target = os.path.join(workdir, "real-target.bin")
    original = b"symlink target content must survive"
    with open(target, "wb") as handle:
        handle.write(original)
    os.symlink(target, fix.output_path)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_exists_refusal(ctx, rc, out, err, fix.output_path)
    ctx.check(os.path.islink(fix.output_path), "symlink was replaced")
    ctx.check(os.readlink(fix.output_path) == target, "symlink target changed")
    ctx.check(read_bytes(target) == original, "symlink target was modified")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_reject_dangling_symlink(ctx, workdir):
    fix = make_fixture(ctx, workdir)
    target = os.path.join(workdir, "nowhere.bin")
    os.symlink(target, fix.output_path)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_exists_refusal(ctx, rc, out, err, fix.output_path)
    ctx.check(os.path.islink(fix.output_path), "dangling symlink was replaced")
    ctx.check(os.readlink(fix.output_path) == target,
              "dangling symlink target changed")
    ctx.check(not os.path.exists(target),
              "dangling symlink target was created")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_output_same_as_input_rejected_as_existing(ctx, workdir):
    # --output pointing at --input: the path exists, so it is refused as
    # "already exists" — the input is never overwritten with an envelope
    # and never deleted.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix, output=fix.input_path))
    assert_exists_refusal(ctx, rc, out, err, fix.input_path)
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_output_same_as_key_rejected_as_existing(ctx, workdir):
    # --output pointing at --key: same rule — the key file is refused as
    # an existing path and keeps its content and permissions.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix, output=fix.key_path))
    assert_exists_refusal(ctx, rc, out, err, fix.key_path)
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_existing_target_refusal_skips_cleanup(ctx, workdir):
    # With the unlink fault armed, an existing output path is still simply
    # refused: no removal is attempted (the existing file survives with
    # content and permissions) and no cleanup warning is reported — this
    # is the not-created class, not a cleanup failure.
    fix = make_fixture(ctx, workdir)
    original = b"pre-existing envelope that must survive\x00\x01"
    with open(fix.output_path, "wb") as handle:
        handle.write(original)
    os.chmod(fix.output_path, 0o640)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_exists_refusal(ctx, rc, out, err, fix.output_path)
    ctx.check(read_bytes(fix.output_path) == original,
              "existing output file content changed")
    mode = stat.S_IMODE(os.stat(fix.output_path).st_mode)
    ctx.check(mode == 0o640, f"existing output file mode changed to {oct(mode)}")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


# ---------------------------------------------------------------------------
# No-fault controls: a normal save still succeeds on the instrumented
# build, and the unlink fault only applies to failed saves.
# ---------------------------------------------------------------------------

def assert_success(ctx, rc, out, err, fix):
    ctx.check(rc == 0, f"encrypt exited {rc} (stderr: {err!r:.200})")
    expected = (f"envelopefile: {SUCCESS_MARKER} '{fix.output_path}'\n").encode()
    ctx.check(out == expected,
              f"unexpected completion message: {out!r:.200}")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")
    ctx.check(os.path.isfile(fix.output_path) and
              not os.path.islink(fix.output_path),
              "envelope file missing or not a regular file")
    mode = stat.S_IMODE(os.stat(fix.output_path).st_mode)
    ctx.check(mode == ENVELOPE_MODE,
              f"envelope file mode is {oct(mode)}, expected "
              f"{oct(ENVELOPE_MODE)}")
    blob = read_bytes(fix.output_path)
    ctx.check(len(blob) == len(fix.plaintext) + ENVELOPE_OVERHEAD,
              f"envelope is {len(blob)} bytes, expected "
              f"{len(fix.plaintext) + ENVELOPE_OVERHEAD}")
    ctx.check(decrypt_envelope(fix.key, blob) == fix.plaintext,
              "freshly saved envelope failed the independent "
              "authentication check")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def test_success_on_testable_build(ctx, workdir):
    # The instrumented build with every fault disarmed behaves exactly
    # like the product: exit 0, completion message naming the save path,
    # and an envelope that passes the independent authentication check.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path),
                       use_testable=True)
    assert_success(ctx, rc, out, err, fix)


def test_success_unaffected_by_unlink_fault(ctx, workdir):
    # The unlink fault only applies to the cleanup of a failed save: a
    # normal successful run never unlinks anything and reports no warning.
    fix = make_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(fix.output_path,
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_success(ctx, rc, out, err, fix)


ALL_TESTS = [
    test_write_failure_cleans_up,
    test_zero_byte_write_fails_as_short_write,
    test_fsync_failure_cleans_up,
    test_close_failure_cleans_up,
    test_write_failure_with_failed_cleanup_reports_both,
    test_partial_write_cleanup_outcome_is_observable,
    test_close_failure_with_failed_cleanup_keeps_complete_envelope,
    test_reject_existing_regular_file,
    test_reject_symlink_to_file,
    test_reject_dangling_symlink,
    test_output_same_as_input_rejected_as_existing,
    test_output_same_as_key_rejected_as_existing,
    test_existing_target_refusal_skips_cleanup,
    test_success_on_testable_build,
    test_success_unaffected_by_unlink_fault,
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
