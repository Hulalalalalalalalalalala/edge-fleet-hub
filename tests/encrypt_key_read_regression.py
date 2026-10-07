#!/usr/bin/env python3
"""Regression tests for how `envelopefile encrypt` reads the --key file.

The round-trip suite (tests/encrypt_regression.py) guards the envelope and
the save suite (tests/encrypt_save_regression.py) guards the output file.
This suite guards the one file named by `--key`: only a file whose raw
content is EXACTLY 32 bytes may be used as the AES-256-GCM key, and the
read must neither accept a bad file nor fail a valid key because of a
temporary interrupt. The command line, the version-1 envelope, and the
save rules are exercised exactly as shipped — the key bytes are never
treated as a passphrase or as text to decode/trim.

Success is never judged from the exit code or the mere presence of the
output: the produced envelope is parsed and authenticated with the
independent pure-Python AES-256-GCM decryptor from
tests/encrypt_regression.py using the exact 32 bytes found in the key
file, and must restore the input byte for byte; the same envelope must
fail under any other key. Coverage:

* verbatim raw keys — a 32-byte binary key containing NUL, CR/LF and
  non-text bytes, a 32-byte printable text key (including one that looks
  like hex and one ending in a newline), are used exactly as the bytes
  are: no trimming, no newline stripping, no hex/text decoding, no
  substitution;
* length refusals — empty (0), short (1..31) and long (33..) key files
  exit 1 with empty stdout and a stderr line naming the key path and the
  length problem. The decisive case is a valid 32-byte key followed by
  exactly ONE extra byte: it must be rejected, never truncated to the
  legal 32-byte prefix and encrypted;
* open/read failures — a missing key file cannot be opened; a key path
  that is a directory opens but its first read fails irrecoverably;
  EIO injected after some (or zero, or all 32) key bytes is a read
  failure. Each exits 1 with empty stdout and a stderr line naming the
  key path and the read reason. EIO on the end-of-file confirmation
  read — after 32 bytes are in hand but EOF is not yet confirmed — is
  reported as a READ failure and never as success or as a length
  mismatch;
* recoverable interrupts and short reads — when the 32 bytes arrive over
  several reads, or reads report EINTR (including on the EOF probe) and
  then complete, encryption still succeeds: the accepted bytes are
  logged by the fault build and proven to partition offsets 0..31 with
  no gap, overlap or replacement, to equal the on-disk key byte for
  byte, and to authenticate the envelope;
* side effects — none of the key failures creates an envelope or a
  substitute key: a previously absent output stays absent, a
  pre-existing output keeps its content and permissions, and the input
  and key files keep theirs (a user-supplied key not created by keygen
  keeps its non-0600 mode). No key or plaintext byte reaches stdout,
  stderr, or this report.

Faults are injected through the same --wrap shim build
(tests/fault_inject.cpp) the other suites use, aimed at the key path via
EF_TEST_TARGET, with per-call read() scripting (EF_TEST_READ_SCRIPT:
ok / N-byte short read / intr=EINTR / eio) and a continuity log
(EF_TEST_READ_LOG). Length/open refusals also run against the production
binary.
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
# known-answer vectors by the round-trip suite): the envelope is verified
# with code that shares nothing with the product's OpenSSL path.
from encrypt_regression import decrypt_envelope

KEY_SIZE = 32
ENVELOPE_MODE = 0o600
ENVELOPE_OVERHEAD = 37
SUCCESS_MARKER = "file encrypted and envelope saved to"

# Distinctive plaintext marker; must never appear on the product streams.
MARKER = b"-- encrypt key-read regression probe 0xEF6E --\n"

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
    """Run a binary and capture exit code/stdout/stderr as bytes."""
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


def write_file(path, data, mode=None):
    with open(path, "wb") as handle:
        handle.write(data)
    if mode is not None:
        os.chmod(path, mode)


def mode_of(path):
    return stat.S_IMODE(os.stat(path).st_mode)


def make_plaintext():
    # Long enough and non-block-aligned so a key mix-up cannot be hidden by
    # a short input; includes every byte value plus repeated marker text.
    return MARKER * 8 + bytes(range(256)) + b"tail"


def make_fixture(ctx, workdir, key, key_mode=None, input_mode=0o640):
    """Write a key file and input file with explicit contents/modes and a
    fresh (absent) output path. key_mode None leaves the process umask
    default in place; callers that care set it explicitly."""
    fix = types.SimpleNamespace()
    fix.key_path = os.path.join(workdir, "envelope.key")
    write_file(fix.key_path, key, mode=key_mode)
    fix.key = key
    fix.key_mode = mode_of(fix.key_path)

    fix.plaintext = make_plaintext()
    fix.input_path = os.path.join(workdir, "document.bin")
    write_file(fix.input_path, fix.plaintext, mode=input_mode)
    fix.input_mode = input_mode

    fix.output_path = os.path.join(workdir, "document.bin.env")
    ctx.check(not os.path.lexists(fix.output_path),
              "fixture error: output path unexpectedly exists")
    fix.workdir = workdir
    return fix


def encrypt_args(fix, output=None, key=None):
    return ["encrypt",
            "--key", key if key is not None else fix.key_path,
            "--input", fix.input_path,
            "--output", output if output is not None else fix.output_path]


def key_fault_env(fix, log_name="read.log", **overrides):
    """Fault environment aimed at exactly the key path, so the input and
    output opens pass through untouched."""
    env = {"EF_TEST_TARGET": fix.key_path,
           "EF_TEST_READ_LOG": os.path.join(fix.workdir, log_name)}
    env.update(overrides)
    return env


def success_line(output_path):
    return (f"envelopefile: {SUCCESS_MARKER} '{output_path}'\n").encode()


def cannot_open_line(path, reason):
    return f"envelopefile: cannot read key file '{path}': {reason}\n".encode()


def read_failure_line(path, reason):
    return f"envelopefile: failed reading key file '{path}': {reason}\n".encode()


def short_line(path, length):
    return (f"envelopefile: key file '{path}' is {length} bytes, expected "
            f"exactly {KEY_SIZE}\n").encode()


LONG_LINE_SUFFIX = (
    f"is longer than {KEY_SIZE} bytes; expected exactly 32 raw key "
    "bytes\n").encode()


def long_line(path):
    return (f"envelopefile: key file '{path}' is longer than {KEY_SIZE} "
            "bytes; expected exactly 32 raw key bytes\n").encode()


def assert_no_secret_leak(ctx, key, out, err):
    # Diagnostic and completion strings carry paths and reasons only; the
    # raw key and the distinctive plaintext must never be present. An
    # empty key has no bytes to leak (b"" is a substring of everything),
    # so its containment is not asserted.
    if key:
        ctx.check(key not in out, "key material leaked into stdout")
        ctx.check(key not in err, "key material leaked into stderr")
    ctx.check(MARKER not in out, "plaintext leaked into stdout")
    ctx.check(MARKER not in err, "plaintext leaked into stderr")


def assert_input_and_key_untouched(ctx, fix):
    ctx.check(read_bytes(fix.key_path) == fix.key,
              "key file content changed")
    ctx.check(mode_of(fix.key_path) == fix.key_mode,
              f"key file mode changed to {oct(mode_of(fix.key_path))}, "
              f"expected {oct(fix.key_mode)}")
    ctx.check(read_bytes(fix.input_path) == fix.plaintext,
              "input file content changed")
    ctx.check(mode_of(fix.input_path) == fix.input_mode,
              f"input file mode changed to {oct(mode_of(fix.input_path))}")


def assert_output_absent(ctx, fix):
    ctx.check(not os.path.lexists(fix.output_path),
              "a key-reading failure created or left an envelope at the "
              "output path")


def assert_encrypt_used_key(ctx, fix, rc, out, err):
    """The strong success contract: exit 0, the exact completion line, and
    an envelope that authenticates ONLY under the exact 32 bytes of the
    key file and restores the input byte for byte. File existence or the
    exit code alone is never accepted as proof."""
    ctx.check(rc == 0, f"encrypt exited {rc} (stderr: {err!r:.200})")
    ctx.check(out == success_line(fix.output_path),
              f"unexpected stdout on success: {out!r:.200}")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")

    ctx.check(os.path.isfile(fix.output_path) and
              not os.path.islink(fix.output_path),
              "envelope file missing or not a regular file")
    ctx.check(mode_of(fix.output_path) == ENVELOPE_MODE,
              f"envelope mode is {oct(mode_of(fix.output_path))}, expected "
              f"{oct(ENVELOPE_MODE)}")

    blob = read_bytes(fix.output_path)
    ctx.check(len(blob) == len(fix.plaintext) + ENVELOPE_OVERHEAD,
              f"envelope is {len(blob)} bytes, expected "
              f"{len(fix.plaintext) + ENVELOPE_OVERHEAD}")
    recovered = decrypt_envelope(fix.key, blob)
    ctx.check(recovered is not None,
              "envelope did not authenticate under the key file's exact 32 "
              "raw bytes — the product did not use the specified key")
    ctx.check(recovered == fix.plaintext,
              "authenticated plaintext differs from the original input")

    # The envelope must be bound to THIS key: changing the first or last
    # key byte must break authentication.
    for position in (0, KEY_SIZE - 1):
        other = bytearray(fix.key)
        other[position] ^= 0x01
        ctx.check(bytes(other) != fix.key, "fixture error: keys identical")
        ctx.check(decrypt_envelope(bytes(other), blob) is None,
                  f"envelope authenticated under a key differing at byte "
                  f"{position}")

    assert_no_secret_leak(ctx, fix.key, out, err)
    assert_input_and_key_untouched(ctx, fix)


def assert_key_failure(ctx, fix, rc, out, err, expected_err):
    """Common contract for every --key reading failure: exit 1, empty
    stdout, exactly the expected stderr line (which must name the key
    path), no envelope, and untouched inputs."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on key failure: {out!r:.200}")
    ctx.check(err == expected_err,
              f"stderr must be exactly the key-failure line naming the key "
              f"path: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported despite the key failure")
    assert_output_absent(ctx, fix)
    assert_input_and_key_untouched(ctx, fix)
    assert_no_secret_leak(ctx, fix.key, out, err)


# ---------------------------------------------------------------------------
# Read-log continuity proof (testable build).
# ---------------------------------------------------------------------------

def parse_read_log(path):
    """Parse EF_TEST_READ_LOG into (calls, verify_hex).

    calls is a list of (offset, requested, returned, errno_or_None);
    verify_hex is the hex string of the 32 bytes the product accepted, or
    None if the fill never completed.
    """
    calls = []
    verify_hex = None
    with open(path, "r", encoding="ascii") as handle:
        for line in handle:
            line = line.strip()
            if line.startswith("VERIFY-FAILED"):
                raise Failure(f"shim reported a key-read contract "
                              f"violation: {line}")
            if line.startswith("VERIFY "):
                verify_hex = line.split()[1]
            elif line.startswith("CALL "):
                fields = line.split()
                offset, requested, returned = (int(fields[1]),
                                               int(fields[2]),
                                               int(fields[3]))
                err = int(fields[4]) if len(fields) > 4 else None
                calls.append((offset, requested, returned, err))
    return calls, verify_hex


def assert_read_log_success(ctx, log_path, key, min_fill_calls=1,
                            expect_probe_interrupts=False):
    """Full continuity check for a successful instrumented run."""
    calls, verify_hex = parse_read_log(log_path)
    ctx.check(verify_hex is not None,
              "no VERIFY line: the product never assembled 32 key bytes")
    ctx.check(verify_hex == key.hex(),
              "the 32 bytes accepted into the product's key buffer differ "
              "from the key file on disk (a byte was replaced or decoded)")

    covered = [False] * KEY_SIZE
    fill_calls = 0
    saw_interrupt = False
    for offset, _requested, returned, err in calls:
        if offset < KEY_SIZE:
            fill_calls += 1
            if returned < 0:
                ctx.check(err == errno.EINTR,
                          f"unexpected errno {err} at fill offset {offset}")
                saw_interrupt = True
                continue
            for index in range(offset, offset + returned):
                ctx.check(0 <= index < KEY_SIZE,
                          "fill read crossed the 32-byte boundary")
                ctx.check(not covered[index],
                          f"key byte {index} delivered more than once")
                covered[index] = True

    ctx.check(fill_calls >= min_fill_calls,
              f"expected at least {min_fill_calls} filling calls, saw "
              f"{fill_calls}")
    ctx.check(all(covered),
              "filling reads do not cover every key offset 0..31")

    # Phase 2 is the one-byte EOF probe at pinned offset 32; the last call
    # must be a real EOF (returned 0), optionally after EINTR retries.
    probe = [c for c in calls if c[0] == KEY_SIZE]
    ctx.check(probe, "no end-of-file confirmation read was logged")
    ctx.check(probe[-1][1] == 1 and probe[-1][2] == 0,
              "the key read did not end with a one-byte EOF confirmation")
    probe_interrupts = [c for c in probe if c[2] == -1]
    if expect_probe_interrupts:
        ctx.check(probe_interrupts,
                  "expected an interrupted EOF probe but none was logged")
        ctx.check(all(c[3] == errno.EINTR for c in probe_interrupts),
                  "EOF probe reported an error other than EINTR")
    ctx.check(all(c[2] != -1 or c[3] == errno.EINTR for c in probe),
              "EOF probe reported an irrecoverable error in a success case")
    return saw_interrupt


def assert_read_log_failure(ctx, log_path, after_bytes):
    """For an injected EIO: the log must show the irrecoverable read at the
    expected offset with EIO, and no successful EOF confirmation."""
    calls, _verify = parse_read_log(log_path)
    eio_calls = [c for c in calls if c[2] == -1 and c[3] == errno.EIO]
    ctx.check(eio_calls, "no EIO read was logged although one was injected")
    for offset, _req, _ret, err in eio_calls:
        ctx.check(offset == after_bytes,
                  f"EIO was injected/read at offset {offset}, expected "
                  f"{after_bytes}")
    ctx.check(not any(c[0] == KEY_SIZE and c[2] == 0 for c in calls),
              "EOF was confirmed despite the injected irrecoverable error")


def chunk_script(sizes, probe=None):
    """Build a read script that delivers the 32 key bytes via the given
    chunk sizes, then handles the EOF probe."""
    ctx_check = sum(sizes)
    assert ctx_check == KEY_SIZE, f"chunk sizes sum to {ctx_check}, not 32"
    tokens = [str(size) for size in sizes]
    tokens += list(probe or ["ok"])
    return ",".join(tokens)


# ---------------------------------------------------------------------------
# Success: valid keys are used verbatim.
# ---------------------------------------------------------------------------

def test_binary_key_verbatim_nul_newline_nontext(ctx, workdir):
    # 32 raw bytes spanning NUL, LF, CR and bytes outside the text range —
    # plus all low control values. Nothing here may be converted, stripped
    # or decoded.
    key = b"\x00\x0a\x0d\xff\xfe\x80\x01\x02" + bytes(range(24))
    ctx.check(len(key) == KEY_SIZE, "fixture error: key is not 32 bytes")
    fix = make_fixture(ctx, workdir, key, key_mode=0o600)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_encrypt_used_key(ctx, fix, rc, out, err)


def test_printable_text_key_is_raw_not_decoded(ctx, workdir):
    # 32 printable ASCII characters that LOOK like hex: a decoder would
    # turn them into 16 bytes, a trimmer might alter them. They must be
    # the raw key exactly as written.
    cases = [
        b"0123456789abcdef0123456789abcdef",
        b"the quick brown fox jumps over12",  # exactly 32 letters/spaces/digits
        b"a" * (KEY_SIZE - 1) + b"\n",       # 31 letters + trailing newline
    ]
    for index, key in enumerate(cases):
        ctx.check(len(key) == KEY_SIZE, f"fixture error: case {index}")
        subdir = os.path.join(workdir, f"text-{index}")
        os.mkdir(subdir)
        fix = make_fixture(ctx, subdir, key, key_mode=0o644)
        rc, out, err = run(ctx, encrypt_args(fix))
        assert_encrypt_used_key(ctx, fix, rc, out, err)
        # A user-supplied key that was not created by keygen keeps its
        # wider permissions: the product opens it read-only and never
        # chmods an input path.
        ctx.check(mode_of(fix.key_path) == 0o644,
                  "a readable key file's permissions were modified")


def test_nonzero_umask_does_not_change_key_use(ctx, workdir):
    # Key bytes and key file mode are independent: a 0644 key under a
    # permissive umask still seals exactly with its file bytes.
    key = bytes((i * 7 + 3) & 0xFF for i in range(KEY_SIZE))
    fix = make_fixture(ctx, workdir, key, key_mode=0o644)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_encrypt_used_key(ctx, fix, rc, out, err)


# ---------------------------------------------------------------------------
# Length refusals.
# ---------------------------------------------------------------------------

def test_empty_key_file_rejected(ctx, workdir):
    fix = make_fixture(ctx, workdir, b"", key_mode=0o600)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_key_failure(ctx, fix, rc, out, err, short_line(fix.key_path, 0))
    # The length diagnostic states the problem category and the path; it
    # must not masquerade as an open error.
    ctx.check(b"expected exactly 32" in err,
              "empty-key error does not state the expected length")
    ctx.check(str(fix.key_path).encode() in err,
              "empty-key error does not name the key path")


def test_short_key_files_rejected(ctx, workdir):
    # Sizes across the range, each ending one or more bytes early.
    for length in (1, 2, 15, 16, 17, 31):
        subdir = os.path.join(workdir, f"short-{length}")
        os.mkdir(subdir)
        key = bytes(range(length))
        fix = make_fixture(ctx, subdir, key, key_mode=0o600)
        rc, out, err = run(ctx, encrypt_args(fix))
        assert_key_failure(ctx, fix, rc, out, err,
                           short_line(fix.key_path, length))


def test_long_key_files_rejected(ctx, workdir):
    for length in (34, 48, 256):
        subdir = os.path.join(workdir, f"long-{length}")
        os.mkdir(subdir)
        fix = make_fixture(ctx, subdir, bytes(range(length)), key_mode=0o600)
        rc, out, err = run(ctx, encrypt_args(fix))
        assert_key_failure(ctx, fix, rc, out, err, long_line(fix.key_path))


def test_thirty_three_byte_key_not_truncated(ctx, workdir):
    # The decisive boundary: a fully valid 32-byte key followed by exactly
    # ONE extra byte. The product must reject the file, not seal with its
    # legal 32-byte prefix. Proven by contrasting the same prefix alone.
    prefix = bytes((i * 13 + 5) & 0xFF for i in range(KEY_SIZE))
    extra = b"\x5a"

    control_dir = os.path.join(workdir, "control")
    os.mkdir(control_dir)
    good = make_fixture(ctx, control_dir, prefix, key_mode=0o600)
    rc, out, err = run(ctx, encrypt_args(good))
    assert_encrypt_used_key(ctx, good, rc, out, err)

    reject_dir = os.path.join(workdir, "reject")
    os.mkdir(reject_dir)
    bad = make_fixture(ctx, reject_dir, prefix + extra, key_mode=0o600)
    rc, out, err = run(ctx, encrypt_args(bad))
    assert_key_failure(ctx, bad, rc, out, err, long_line(bad.key_path))
    # It is a "too long" refusal, never a "wrong length (32)" or read error.
    ctx.check(LONG_LINE_SUFFIX in err,
              "33-byte key was not reported as too long")
    ctx.check(b"failed reading" not in err and b"cannot read" not in err,
              "33-byte key was misreported as an I/O failure")
    # And no envelope sealed under the prefix exists anywhere in the
    # rejecting run's directory.
    leftovers = [name for name in os.listdir(reject_dir)
                 if name.endswith(".env")]
    ctx.check(not leftovers,
              f"rejecting the 33-byte key left an envelope: {leftovers}")


# ---------------------------------------------------------------------------
# Open and irrecoverable read failures.
# ---------------------------------------------------------------------------

def test_missing_key_file_cannot_open(ctx, workdir):
    fix = make_fixture(ctx, workdir, bytes(KEY_SIZE), key_mode=0o600)
    missing = os.path.join(workdir, "does-not-exist.key")
    ctx.check(not os.path.lexists(missing),
              "fixture error: missing key path exists")
    before = set(os.listdir(workdir))
    rc, out, err = run(ctx, encrypt_args(fix, key=missing))
    # The key itself is irrelevant here (it is never read); only check the
    # contract pieces that do not compare a fixture key to the streams.
    ctx.check(rc == 1, f"expected exit 1, got {rc} ({err!r:.200})")
    ctx.check(out == b"", f"stdout not empty: {out!r:.200}")
    ctx.check(err == cannot_open_line(missing, _ENOENT),
              f"unexpected open-failure line: {err!r:.200}")
    assert_output_absent(ctx, fix)
    ctx.check(read_bytes(fix.input_path) == fix.plaintext,
              "input file changed while reporting a missing key")
    ctx.check(set(os.listdir(workdir)) == before,
              "reporting a missing key created or removed a file")
    ctx.check(MARKER not in out + err, "plaintext leaked onto the streams")


def test_directory_key_path_read_error(ctx, workdir):
    # Opening a directory O_RDONLY succeeds, but the first read fails
    # irrecoverably (EISDIR) with zero key bytes delivered — including
    # when the suite runs as root, where DAC-based permission denials are
    # bypassed. Must be reported as a read failure, not as "0 bytes".
    fix = make_fixture(ctx, workdir, bytes(KEY_SIZE), key_mode=0o600)
    directory = os.path.join(workdir, "a-directory")
    os.mkdir(directory)
    rc, out, err = run(ctx, encrypt_args(fix, key=directory))
    ctx.check(rc == 1, f"expected exit 1, got {rc} ({err!r:.200})")
    ctx.check(out == b"", f"stdout not empty: {out!r:.200}")
    ctx.check(err == read_failure_line(directory, _EISDIR),
              f"directory key must produce the EISDIR read line: "
              f"{err!r:.200}")
    assert_output_absent(ctx, fix)
    ctx.check(os.path.isdir(directory), "key directory was modified")


def test_unreadable_key_file_permission_denied(ctx, workdir):
    # A key inside a directory that denies traversal cannot be opened.
    # Root bypasses POSIX permissions; in that case this DAC scenario does
    # not apply (the missing-file and EIO cases cover open/read failure
    # deterministically regardless of uid), so it is reported as skipped.
    if os.geteuid() == 0:
        print("SKIP test_unreadable_key_file_permission_denied: running as "
              "root, DAC denials do not apply")
        return
    fix = make_fixture(ctx, workdir, bytes(KEY_SIZE), key_mode=0o600)
    sealed_dir = os.path.join(workdir, "no-access")
    os.mkdir(sealed_dir)
    hidden = os.path.join(sealed_dir, "secret.key")
    write_file(hidden, fix.key, mode=0o600)
    os.chmod(sealed_dir, 0)
    try:
        rc, out, err = run(ctx, encrypt_args(fix, key=hidden))
    finally:
        os.chmod(sealed_dir, stat.S_IRWXU)
    eacces = os.strerror(errno.EACCES)
    ctx.check(rc == 1, f"expected exit 1, got {rc} ({err!r:.200})")
    ctx.check(out == b"", f"stdout not empty: {out!r:.200}")
    ctx.check(err == cannot_open_line(hidden, eacces),
              f"unexpected permission line: {err!r:.200}")
    assert_output_absent(ctx, fix)


def test_ioerror_after_partial_key_bytes(ctx, workdir):
    # EIO after some key bytes were already read: reject with a READ
    # failure (naming path and reason), never a length mismatch and never
    # success — at several offsets, including all-but-one.
    for index, (script_size, after) in enumerate(
            ((1, 1), (10, 10), (31, 31))):
        subdir = os.path.join(workdir, f"eio-{after}")
        os.mkdir(subdir)
        key = bytes((i * 11 + 1) & 0xFF for i in range(KEY_SIZE))
        fix = make_fixture(ctx, subdir, key, key_mode=0o600)
        env = key_fault_env(fix, log_name=f"read-{after}.log",
                            EF_TEST_READ_SCRIPT=f"{script_size},eio")
        rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                           use_testable=True)
        assert_key_failure(ctx, fix, rc, out, err,
                           read_failure_line(fix.key_path, _EIO))
        ctx.check(b"bytes, expected" not in err,
                  "an EIO after partial bytes was misreported as a length "
                  "mismatch")
        assert_read_log_failure(ctx, os.path.join(subdir,
                                                  f"read-{after}.log"), after)


def test_ioerror_on_first_read(ctx, workdir):
    # Irrecoverable error before any key byte is delivered.
    fix = make_fixture(ctx, workdir, bytes(KEY_SIZE), key_mode=0o600)
    env = key_fault_env(fix, EF_TEST_READ_SCRIPT="eio")
    rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                       use_testable=True)
    assert_key_failure(ctx, fix, rc, out, err,
                       read_failure_line(fix.key_path, _EIO))
    assert_read_log_failure(ctx, os.path.join(workdir, "read.log"), 0)


def test_ioerror_on_eof_probe_not_success_not_length(ctx, workdir):
    # 32 bytes are in hand but EOF is not confirmed; the confirmation read
    # fails irrecoverably. This must be a READ failure: not success (no
    # envelope), and not a length error.
    key = bytes((i * 17 + 9) & 0xFF for i in range(KEY_SIZE))
    fix = make_fixture(ctx, workdir, key, key_mode=0o600)
    env = key_fault_env(fix, EF_TEST_READ_SCRIPT="ok,eio")
    rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                       use_testable=True)
    assert_key_failure(ctx, fix, rc, out, err,
                       read_failure_line(fix.key_path, _EIO))
    ctx.check(b"longer than" not in err and b"bytes, expected" not in err,
              "an EOF-probe error was misreported as a length mismatch")
    assert_read_log_failure(ctx, os.path.join(workdir, "read.log"), KEY_SIZE)


# ---------------------------------------------------------------------------
# Recoverable interrupts and short reads: the valid key still seals.
# ---------------------------------------------------------------------------

def test_chunked_reads_use_exact_bytes(ctx, workdir):
    # The 32 key bytes arrive in many shapes; every accepted byte is
    # logged and must equal the on-disk key, and the envelope authenticates
    # only under it.
    plans = [
        [1] * 32,
        [16, 16],
        [7, 7, 7, 7, 4],
        [31, 1],
        [1, 31],
        [3, 5, 8, 13, 3],
    ]
    for index, sizes in enumerate(plans):
        subdir = os.path.join(workdir, f"chunks-{index}")
        os.mkdir(subdir)
        key = bytes((i * 19 + index) & 0xFF for i in range(KEY_SIZE))
        fix = make_fixture(ctx, subdir, key, key_mode=0o600)
        log_name = "read.log"
        env = key_fault_env(fix, log_name=log_name,
                            EF_TEST_READ_SCRIPT=chunk_script(sizes))
        rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                           use_testable=True)
        assert_encrypt_used_key(ctx, fix, rc, out, err)
        assert_read_log_success(ctx, os.path.join(subdir, log_name), key,
                                min_fill_calls=len(sizes))


def test_recoverable_interrupts_then_complete(ctx, workdir):
    # EINTR before any byte, between reads, and repeated: the same slice is
    # retried, so no byte is lost, duplicated or replaced.
    scripts = [
        "intr,ok",
        "intr,intr,ok",
        "10,intr,22,ok",
        "15,intr,intr,17,ok",
        "3,intr,5,7,intr,17,ok",
        "31,intr,1,ok",
    ]
    for index, script in enumerate(scripts):
        subdir = os.path.join(workdir, f"intr-{index}")
        os.mkdir(subdir)
        key = bytes((i * 23 + index * 3) & 0xFF for i in range(KEY_SIZE))
        fix = make_fixture(ctx, subdir, key, key_mode=0o600)
        log_name = "read.log"
        env = key_fault_env(fix, log_name=log_name,
                            EF_TEST_READ_SCRIPT=script)
        rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                           use_testable=True)
        assert_encrypt_used_key(ctx, fix, rc, out, err)
        saw_interrupt = assert_read_log_success(
            ctx, os.path.join(subdir, log_name), key, min_fill_calls=2)
        ctx.check(saw_interrupt,
                  f"script {script} was meant to inject an EINTR during the "
                  "fill but none was logged")


def test_interrupted_eof_probe_continues(ctx, workdir):
    # The end-of-file confirmation read itself is interrupted once or
    # repeatedly, possibly right after a chunked fill: the probe is
    # retried and the valid key still seals.
    scripts = [
        "ok,intr,ok",
        "ok,intr,intr,ok",
        chunk_script([16, 16], probe=["intr", "ok"]),
    ]
    for index, script in enumerate(scripts):
        subdir = os.path.join(workdir, f"eofintr-{index}")
        os.mkdir(subdir)
        key = bytes((i * 29 + index) & 0xFF for i in range(KEY_SIZE))
        fix = make_fixture(ctx, subdir, key, key_mode=0o600)
        log_name = "read.log"
        env = key_fault_env(fix, log_name=log_name,
                            EF_TEST_READ_SCRIPT=script)
        rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                           use_testable=True)
        assert_encrypt_used_key(ctx, fix, rc, out, err)
        # The first two scripts fill the key in one normal call and only
        # interrupt the EOF probe; the last splits the fill too. In all
        # cases the 32 bytes still cover offsets 0..31 exactly once.
        assert_read_log_success(ctx, os.path.join(subdir, log_name), key,
                                min_fill_calls=1,
                                expect_probe_interrupts=True)


def test_testable_build_control_two_phase_read(ctx, workdir):
    # Without a script the instrumented build behaves like the product and
    # its log documents the fixed two-phase key read: one 32-byte fill and
    # one one-byte EOF probe.
    key = bytes((i * 31 + 1) & 0xFF for i in range(KEY_SIZE))
    fix = make_fixture(ctx, workdir, key, key_mode=0o600)
    env = key_fault_env(fix)  # log only, no script
    rc, out, err = run(ctx, encrypt_args(fix), env_extra=env,
                       use_testable=True)
    assert_encrypt_used_key(ctx, fix, rc, out, err)
    calls, verify_hex = parse_read_log(os.path.join(workdir, "read.log"))
    ctx.check(verify_hex == key.hex(),
              "control VERIFY does not match the on-disk key")
    phases = [(off, req, ret) for off, req, ret, _err in calls]
    ctx.check(phases == [(0, KEY_SIZE, KEY_SIZE), (KEY_SIZE, 1, 0)],
              f"normal key read is not one fill plus one EOF probe: "
              f"{phases}")


# ---------------------------------------------------------------------------
# Side effects: failures create nothing; existing outputs are preserved.
# ---------------------------------------------------------------------------

def test_failures_preserve_existing_output(ctx, workdir):
    # A pre-existing output must keep its exact content and permissions
    # across every class of key failure; the product reads and rejects the
    # key before it ever opens the output.
    sentinel = b"pre-existing envelope contents must survive\x00\xff"
    key33 = bytes(range(KEY_SIZE)) + b"\x5a"
    scenarios = []

    # Length failure (33-byte key), production binary.
    d = os.path.join(workdir, "long"); os.mkdir(d)
    fix = make_fixture(ctx, d, key33, key_mode=0o600)
    write_file(fix.output_path, sentinel, mode=0o640)
    scenarios.append((fix, encrypt_args(fix), None, False))

    # Missing key, production binary.
    d = os.path.join(workdir, "missing"); os.mkdir(d)
    fix = make_fixture(ctx, d, bytes(KEY_SIZE), key_mode=0o600)
    write_file(fix.output_path, sentinel, mode=0o640)
    scenarios.append((fix, encrypt_args(
        fix, key=os.path.join(d, "no-such.key")), None, False))

    # Injected EIO after partial bytes, testable build.
    d = os.path.join(workdir, "eio"); os.mkdir(d)
    key = bytes(range(KEY_SIZE))
    fix = make_fixture(ctx, d, key, key_mode=0o600)
    write_file(fix.output_path, sentinel, mode=0o640)
    env = key_fault_env(fix, EF_TEST_READ_SCRIPT="12,eio")
    scenarios.append((fix, encrypt_args(fix), env, True))

    # Injected EIO on the EOF probe, testable build.
    d = os.path.join(workdir, "probe-eio"); os.mkdir(d)
    fix = make_fixture(ctx, d, key, key_mode=0o600)
    write_file(fix.output_path, sentinel, mode=0o640)
    env = key_fault_env(fix, log_name="read2.log",
                        EF_TEST_READ_SCRIPT="ok,eio")
    scenarios.append((fix, encrypt_args(fix), env, True))

    for fix, args, env, testable in scenarios:
        rc, out, err = run(ctx, args, env_extra=env, use_testable=testable)
        ctx.check(rc == 1 and out == b"",
                  f"key failure scenario did not fail cleanly: rc={rc}")
        ctx.check(read_bytes(fix.output_path) == sentinel,
                  "existing output content changed after a key failure")
        ctx.check(mode_of(fix.output_path) == 0o640,
                  "existing output permissions changed after a key failure")
        ctx.check(not os.path.islink(fix.output_path),
                  "existing output path was replaced with a symlink")
        assert_input_and_key_untouched(ctx, fix)


def test_no_substitute_key_or_stray_files(ctx, workdir):
    # Failures must not generate a substitute key or any other file: the
    # working directory gains nothing that was not a known fixture/log.
    fix = make_fixture(ctx, workdir, bytes(range(KEY_SIZE + 1)),
                       key_mode=0o600)
    before = set(os.listdir(workdir))
    rc, out, err = run(ctx, encrypt_args(fix))
    ctx.check(rc == 1, f"expected rejection, got {rc}")
    after = set(os.listdir(workdir))
    ctx.check(after == before,
              f"a rejected key run changed the directory: "
              f"added {sorted(after - before)}, removed "
              f"{sorted(before - after)}")
    assert_output_absent(ctx, fix)


ALL_TESTS = [
    test_binary_key_verbatim_nul_newline_nontext,
    test_printable_text_key_is_raw_not_decoded,
    test_nonzero_umask_does_not_change_key_use,
    test_empty_key_file_rejected,
    test_short_key_files_rejected,
    test_long_key_files_rejected,
    test_thirty_three_byte_key_not_truncated,
    test_missing_key_file_cannot_open,
    test_directory_key_path_read_error,
    test_unreadable_key_file_permission_denied,
    test_ioerror_after_partial_key_bytes,
    test_ioerror_on_first_read,
    test_ioerror_on_eof_probe_not_success_not_length,
    test_chunked_reads_use_exact_bytes,
    test_recoverable_interrupts_then_complete,
    test_interrupted_eof_probe_continues,
    test_testable_build_control_two_phase_read,
    test_failures_preserve_existing_output,
    test_no_substitute_key_or_stray_files,
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
