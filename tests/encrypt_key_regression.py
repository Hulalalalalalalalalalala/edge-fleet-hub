#!/usr/bin/env python3
"""Regression tests for `envelopefile encrypt --key` key-file reading.

The round-trip/tamper suite (tests/encrypt_regression.py) proves the
envelope content is correct and the save suite
(tests/encrypt_save_regression.py) guards the output pipeline; this suite
guards the other end of `encrypt`: how the key file named by --key is
read. The README promises that the key is exactly 32 raw bytes — never a
passphrase, never text-decoded, never trimmed, never substituted:

* valid keys: a 32-byte file is used verbatim as the raw AES-256-GCM key,
  whether it contains zero bytes, newlines, and non-text bytes or is 32
  bytes of printable text (a trailing newline is key material, not
  decoration). Success exits 0 with the completion message naming the
  output path, and the envelope authenticates under exactly the 32 bytes
  in the file — checked with the independent pure-Python AES-256-GCM
  decryptor shared with the round-trip suite. Success is never judged by
  exit code or file existence alone: a decryption with any different key
  must fail, proving the specified key — not a derived, trimmed, decoded,
  or substitute key — is what was used;
* length errors: an empty file, a file shorter than 32 bytes, and a file
  longer than 32 bytes all exit 1 with an empty stdout and a stderr line
  naming the key path and the length problem. A file whose first 32 bytes
  are a perfectly valid key with one extra byte at the end is rejected
  too — the product must not silently truncate to the first 32 bytes and
  encrypt anyway;
* read failures: a key file that cannot be opened, and an irrecoverable
  read error after part of the key was already read, exit 1 with an empty
  stdout and a stderr line naming the key path and the read failure. A
  read error after all 32 bytes were read but before end-of-file was
  confirmed is likewise a failure — never a success, and never
  misreported as a length mismatch;
* recoverable interruptions: when the key arrives in several short reads
  interleaved with EINTR interrupts — including an interrupt on the
  end-of-file confirmation read — the encryption still succeeds and the
  key bytes are neither lost, duplicated, nor replaced, proven both by
  the read-call log the fault-injecting build records and by the envelope
  authenticating under the file's exact bytes;
* side effects: none of the key failures produces an envelope or any
  other new file (no substitute key is ever generated); a pre-existing
  output path keeps its content and permissions, and the key and input
  files always keep theirs. Neither the product's messages nor this
  report ever contain key or plaintext bytes.

Read faults (short reads, EINTR, EIO) are injected through the --wrap
shim build (tests/fault_inject.cpp), aimed at the key path via
EF_TEST_TARGET; plain length/open errors are also exercised against the
production binary.
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
# known-answer vectors by the round-trip suite), reused so the envelope is
# verified by code that shares nothing with the product's OpenSSL path.
from encrypt_regression import decrypt_envelope

KEY_SIZE = 32
ENVELOPE_MODE = 0o600
ENVELOPE_OVERHEAD = 37  # 21-byte header + 16-byte tag
SUCCESS_MARKER = "file encrypted and envelope saved to"

# Distinctive plaintext marker: proves no plaintext leaks onto the
# product's streams in any scenario.
MARKER = b"-- key file regression probe 0xK3YF --\n"

_EIO = os.strerror(errno.EIO)
_ENOENT = os.strerror(errno.ENOENT)
_EISDIR = os.strerror(errno.EISDIR)


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


def run(ctx, args, env_extra=None, use_testable=False):
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
    )
    return proc.returncode, proc.stdout, proc.stderr


def read_bytes(path):
    with open(path, "rb") as handle:
        return handle.read()


def make_fixture(ctx, workdir, key_bytes):
    """A key file with exactly the given content (None: do not create one),
    a readable input, a bystander file, and a fresh (not yet existing)
    output path — the setup every key scenario starts from."""
    fix = types.SimpleNamespace()
    fix.workdir = workdir

    fix.key = key_bytes
    fix.key_path = os.path.join(workdir, "envelope.key")
    if key_bytes is not None:
        with open(fix.key_path, "wb") as handle:
            handle.write(key_bytes)
        os.chmod(fix.key_path, 0o600)
        fix.key_mode = 0o600
    else:
        fix.key_mode = None

    fix.plaintext = MARKER * 4 + bytes(range(256)) + b"\x00tail\n"
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

    # Fault-injection logs live in their own subdirectory so the top-level
    # directory listing can be compared before/after a failing run.
    fix.log_dir = os.path.join(workdir, "logs")
    os.mkdir(fix.log_dir)
    fix.read_log = os.path.join(fix.log_dir, "read.log")

    fix.output_path = os.path.join(workdir, "document.bin.env")
    ctx.check(not os.path.lexists(fix.output_path),
              "fixture error: output path unexpectedly exists")
    fix.entries = sorted(os.listdir(workdir))
    return fix


def encrypt_args(fix, output=None):
    return ["encrypt", "--key", fix.key_path, "--input", fix.input_path,
            "--output", output if output is not None else fix.output_path]


def key_env(fix, **overrides):
    """Fault-injection environment aimed at exactly the key file, so the
    input read, the envelope save, and every unrelated file pass through."""
    env = {"EF_TEST_TARGET": fix.key_path}
    env.update(overrides)
    return env


def assert_fixture_untouched(ctx, fix):
    """Key (when present), input, and bystander keep content and
    permissions."""
    if fix.key is not None:
        ctx.check(read_bytes(fix.key_path) == fix.key,
                  "key file content changed")
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


def assert_no_new_entries(ctx, fix):
    """A key failure must not leave an envelope, a substitute key, or any
    other new file behind, and must not remove anything either."""
    ctx.check(sorted(os.listdir(fix.workdir)) == fix.entries,
              "directory entries changed although the run failed before "
              "any output could be created")


def assert_no_leak(ctx, fix, out, err):
    # Only keys long enough to be distinctive are meaningful leak probes.
    if fix.key is not None and len(fix.key) >= 8:
        ctx.check(fix.key not in out, "key material leaked into stdout")
        ctx.check(fix.key not in err, "key material leaked into stderr")
    ctx.check(MARKER not in out, "plaintext leaked into stdout")
    ctx.check(MARKER not in err, "plaintext leaked into stderr")


# ---------------------------------------------------------------------------
# Exact product diagnostics for the key-file failure classes.
# ---------------------------------------------------------------------------

def short_key_line(path, size):
    return (f"envelopefile: key file '{path}' is {size} bytes, "
            f"expected exactly 32\n").encode()


def long_key_line(path):
    return (f"envelopefile: key file '{path}' is longer than 32 bytes; "
            f"expected exactly 32 raw key bytes\n").encode()


def cannot_read_line(path, reason):
    return f"envelopefile: cannot read key file '{path}': {reason}\n".encode()


def failed_reading_line(path, reason):
    return (f"envelopefile: failed reading key file '{path}': "
            f"{reason}\n").encode()


def assert_key_failure(ctx, rc, out, err, fix, expected_err,
                       expect_output_absent=True):
    """Contract for every key-file failure: exit 1, empty stdout, exactly
    the expected stderr line naming the key path and the problem, no
    completion message, no envelope or other new file, fixtures untouched,
    nothing leaked. expect_output_absent is False only when the output
    path legitimately existed before the run (its content and permissions
    are then verified by the caller)."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on key failure: {out!r:.200}")
    ctx.check(err == expected_err,
              f"stderr must be exactly the key-failure line naming the key "
              f"path: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported despite the key failure")
    if expect_output_absent:
        ctx.check(not os.path.lexists(fix.output_path),
                  "an envelope was created despite the key failure")
    assert_no_new_entries(ctx, fix)
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


def assert_success(ctx, rc, out, err, fix):
    """Contract for a successful encryption: exit 0, the completion message
    naming the output path, empty stderr, and — decisively — an envelope
    that authenticates under exactly the 32 raw bytes in the key file and
    restores the input byte for byte, while any other key fails. Only that
    proves the specified key itself was used."""
    ctx.check(rc == 0, f"encrypt exited {rc} (stderr: {err!r:.200})")
    expected = (f"envelopefile: {SUCCESS_MARKER} "
                f"'{fix.output_path}'\n").encode()
    ctx.check(out == expected, f"unexpected completion message: {out!r:.200}")
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
              "envelope does not authenticate under the exact 32 raw bytes "
              "of the key file, or restores different content")
    # Any other key must fail authentication: the file's own bytes — not a
    # trimmed, decoded, or substitute key — are what was used.
    altered = fix.key[:-1] + bytes([fix.key[-1] ^ 0x01])
    ctx.check(decrypt_envelope(altered, blob) is None,
              "envelope also authenticates under a different key — the "
              "specified key file cannot be what was used")
    assert_fixture_untouched(ctx, fix)
    assert_no_leak(ctx, fix, out, err)


# ---------------------------------------------------------------------------
# Valid keys: exactly 32 raw bytes, used verbatim.
# ---------------------------------------------------------------------------

def test_binary_key_with_nul_and_nontext_bytes(ctx, workdir):
    # Zero bytes, newlines, carriage returns, and high/non-text bytes are
    # all ordinary key material.
    key = b"\x00\xff\n\r\x80\x1a" + bytes(range(1, 27))
    ctx.check(len(key) == KEY_SIZE, "fixture error: key is not 32 bytes")
    fix = make_fixture(ctx, workdir, key)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_success(ctx, rc, out, err, fix)


def test_printable_text_key_used_raw(ctx, workdir):
    # 32 printable characters — including text that would also be valid
    # hexadecimal — are the raw key itself, never decoded or re-encoded.
    key = b"0123456789abcdef0123456789abcdef"
    fix = make_fixture(ctx, workdir, key)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_success(ctx, rc, out, err, fix)


def test_trailing_newline_key_not_trimmed(ctx, workdir):
    # A key whose 32nd byte is a newline: trimming it would leave 31 bytes
    # and fail, so success proves the file is used byte for byte.
    key = b"A" * 31 + b"\n"
    fix = make_fixture(ctx, workdir, key)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_success(ctx, rc, out, err, fix)


def verify_read_log(ctx, fix):
    """The fault-injecting build's read log must show the key arriving
    byte for byte: CALL offsets never skip or go back, interrupted calls
    deliver nothing, and the CHUNK lines reassemble into exactly the key
    file's bytes."""
    with open(fix.read_log, "r", encoding="ascii") as handle:
        lines = handle.read().splitlines()
    ctx.check(lines, "read log is empty although the key was read")

    progress = 0
    assembled = bytearray()
    saw_eof = False
    for line in lines:
        parts = line.split(" ")
        if parts[0] == "CALL":
            offset, requested = int(parts[1]), int(parts[2])
            returned = int(parts[3])
            ctx.check(offset == progress,
                      f"read call at offset {offset} but {progress} key "
                      "bytes had been delivered — bytes skipped or repeated")
            if returned < 0:
                ctx.check(len(parts) == 5,
                          f"failed read call lacks its errno: {line!r}")
            else:
                ctx.check(returned <= requested,
                          f"read returned more than requested: {line!r}")
                progress += returned
                if returned == 0:
                    saw_eof = True
        elif parts[0] == "CHUNK":
            offset = int(parts[1])
            chunk = bytes.fromhex(parts[2])
            ctx.check(offset == len(assembled),
                      f"chunk at offset {offset} but {len(assembled)} bytes "
                      "assembled — key bytes lost, duplicated, or reordered")
            assembled += chunk
        else:
            raise Failure(f"unexpected read log line: {line!r}")
    ctx.check(saw_eof, "end of file was never confirmed in the read log")
    ctx.check(bytes(assembled) == fix.key,
              "the key bytes the product received differ from the key file")


def test_split_reads_and_interrupts_still_succeed(ctx, workdir):
    # The key arrives in three fragments (5 + 7 + 20 bytes) with
    # recoverable EINTR interrupts before, between, and after them —
    # including on the end-of-file confirmation read. The run must still
    # succeed, with stderr staying empty (a recovered interrupt is not a
    # failure) and the received key matching the file byte for byte.
    key = bytes((i * 37 + 11) % 256 for i in range(KEY_SIZE))
    fix = make_fixture(ctx, workdir, key)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=key_env(
                           fix,
                           EF_TEST_READ_SCRIPT="intr,5,intr,7,20,intr,zero",
                           EF_TEST_READ_LOG=fix.read_log),
                       use_testable=True)
    assert_success(ctx, rc, out, err, fix)
    verify_read_log(ctx, fix)


def test_single_byte_reads_still_succeed(ctx, workdir):
    # The extreme of split delivery: every read() yields a single byte, so
    # the 32-byte key takes 32 read calls to assemble.
    fix = make_fixture(ctx, workdir, bytes(range(KEY_SIZE)))
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=key_env(fix,
                                         EF_TEST_READ_SCRIPT=",".join(
                                             ["1"] * KEY_SIZE),
                                         EF_TEST_READ_LOG=fix.read_log),
                       use_testable=True)
    assert_success(ctx, rc, out, err, fix)
    verify_read_log(ctx, fix)


# ---------------------------------------------------------------------------
# Length errors: not exactly 32 bytes is exit 1, stdout empty, stderr
# naming the key path and the length problem — and no envelope, ever.
# ---------------------------------------------------------------------------

def test_empty_key_rejected(ctx, workdir):
    fix = make_fixture(ctx, workdir, b"")
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, rc, out, err, fix,
                       short_key_line(fix.key_path, 0))


def test_short_key_rejected(ctx, workdir):
    # One byte and 31 bytes are equally wrong; the 31-byte case is the
    # first 31 bytes of an otherwise valid key.
    for size in (1, 31):
        subdir = os.path.join(workdir, f"short{size}")
        os.mkdir(subdir)
        key = bytes(range(1, size + 1))
        fix = make_fixture(ctx, subdir, key)
        rc, out, err = run(ctx, encrypt_args(fix))
        assert_key_failure(ctx, rc, out, err, fix,
                           short_key_line(fix.key_path, size))


def test_long_key_rejected_never_truncated(ctx, workdir):
    # The first 32 bytes are a complete, valid key; only the trailing byte
    # (a newline, the classic text-file artefact) is extra. The product
    # must refuse the file outright — encrypting with the first 32 bytes
    # would silently accept a key the user did not specify.
    valid = bytes(range(KEY_SIZE))
    fix = make_fixture(ctx, workdir, valid + b"\n")
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, rc, out, err, fix, long_key_line(fix.key_path))


def test_much_longer_key_rejected(ctx, workdir):
    fix = make_fixture(ctx, workdir, bytes(range(256)) * 2)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, rc, out, err, fix, long_key_line(fix.key_path))


def test_short_key_detected_across_split_reads(ctx, workdir):
    # A 20-byte key delivered in two fragments is still just 20 bytes: the
    # length error names the real total, not a fragment size.
    fix = make_fixture(ctx, workdir, bytes(range(20)))
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=key_env(fix, EF_TEST_READ_SCRIPT="7,intr"),
                       use_testable=True)
    assert_key_failure(ctx, rc, out, err, fix,
                       short_key_line(fix.key_path, 20))


# ---------------------------------------------------------------------------
# Open and read failures: exit 1, stdout empty, stderr naming the key path
# and the read failure — never a length misdiagnosis, never an envelope.
# ---------------------------------------------------------------------------

def test_missing_key_file(ctx, workdir):
    fix = make_fixture(ctx, workdir, None)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, rc, out, err, fix,
                       cannot_read_line(fix.key_path, _ENOENT))


def test_key_path_is_directory(ctx, workdir):
    # A directory opens read-only fine but cannot be read: a read failure
    # naming the key path, not a length error.
    fix = make_fixture(ctx, workdir, None)
    os.mkdir(fix.key_path)
    fix.entries = sorted(os.listdir(workdir))
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, rc, out, err, fix,
                       failed_reading_line(fix.key_path, _EISDIR))


def test_read_error_after_partial_key(ctx, workdir):
    # 10 of the 32 key bytes were already delivered when the read fails
    # irrecoverably: the partial key must not be used, padded, or reported
    # as a short file.
    fix = make_fixture(ctx, workdir, bytes(range(KEY_SIZE)))
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=key_env(fix, EF_TEST_READ_SCRIPT="10,eio"),
                       use_testable=True)
    assert_key_failure(ctx, rc, out, err, fix,
                       failed_reading_line(fix.key_path, _EIO))


def test_read_error_after_full_key_before_eof(ctx, workdir):
    # All 32 key bytes were read, but the end-of-file confirmation read
    # fails: the key's completeness is unconfirmed, so this is a read
    # failure — not a success, and not a "longer than 32 bytes" report.
    fix = make_fixture(ctx, workdir, bytes(range(KEY_SIZE)))
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=key_env(fix, EF_TEST_READ_SCRIPT="ok,eio"),
                       use_testable=True)
    assert_key_failure(ctx, rc, out, err, fix,
                       failed_reading_line(fix.key_path, _EIO))


def test_existing_output_untouched_by_key_failure(ctx, workdir):
    # With a short key AND a pre-existing output file: the run fails on
    # the key, and the existing output keeps its content and permissions
    # (the key is read before any output handling, so it is never touched).
    fix = make_fixture(ctx, workdir, bytes(range(7)))
    original = b"pre-existing output that must survive\x00\x01"
    with open(fix.output_path, "wb") as handle:
        handle.write(original)
    os.chmod(fix.output_path, 0o640)
    fix.entries = sorted(os.listdir(workdir))
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, rc, out, err, fix,
                       short_key_line(fix.key_path, 7),
                       expect_output_absent=False)
    ctx.check(read_bytes(fix.output_path) == original,
              "existing output file content changed by a key failure")
    mode = stat.S_IMODE(os.stat(fix.output_path).st_mode)
    ctx.check(mode == 0o640,
              f"existing output file mode changed to {oct(mode)}")


# ---------------------------------------------------------------------------
# No-fault control: the instrumented build with the read script disarmed
# behaves exactly like the product.
# ---------------------------------------------------------------------------

def test_success_on_testable_build_without_faults(ctx, workdir):
    fix = make_fixture(ctx, workdir, bytes(range(KEY_SIZE)))
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=key_env(fix,
                                         EF_TEST_READ_LOG=fix.read_log),
                       use_testable=True)
    assert_success(ctx, rc, out, err, fix)
    verify_read_log(ctx, fix)


ALL_TESTS = [
    test_binary_key_with_nul_and_nontext_bytes,
    test_printable_text_key_used_raw,
    test_trailing_newline_key_not_trimmed,
    test_split_reads_and_interrupts_still_succeed,
    test_single_byte_reads_still_succeed,
    test_empty_key_rejected,
    test_short_key_rejected,
    test_long_key_rejected_never_truncated,
    test_much_longer_key_rejected,
    test_short_key_detected_across_split_reads,
    test_missing_key_file,
    test_key_path_is_directory,
    test_read_error_after_partial_key,
    test_read_error_after_full_key_before_eof,
    test_existing_output_untouched_by_key_failure,
    test_success_on_testable_build_without_faults,
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
