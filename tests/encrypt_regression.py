#!/usr/bin/env python3
"""Regression tests for `envelopefile encrypt`.

Covers the guarantees of the file-encryption subcommand:

* success encrypts the input as raw bytes with AES-256-GCM under the exact
  32-byte key read from the key file, saves a binary envelope, exits 0, and
  prints a completion message naming the save path; the envelope carries the
  fixed magic, version 1, the algorithm identifier, and a fresh 12-byte
  nonce, and the whole 22-byte header is authenticated as AAD — every case
  that decrypts does so independently through the Python `cryptography`
  package, never through envelopefile itself;
* an empty input still produces a valid envelope with the complete header
  and the full 16-byte tag; binary content (NUL bytes, CR/LF, every byte
  value) round-trips unchanged; two encryptions of the same input with the
  same key use different nonces and produce different ciphertext;
* the output path is created exclusively: an existing regular file,
  directory, symlink, or dangling symlink is rejected with exit 1 and left
  untouched, and naming the input or the key file as the output is rejected
  the same way; the key file and the input file keep their content and
  permissions on every outcome;
* an unreadable key file, a key file whose length is not exactly 32 bytes,
  an unreadable input, a failing secure random source, and a failing output
  save all exit 1 with the corresponding reason on stderr, no completion
  message, and no plaintext or key material on either stream; a failure
  after the output was created removes the incomplete envelope, and if that
  removal itself fails the original cause is kept and a warning naming the
  path is added; the in-memory key is wiped (OPENSSL_cleanse) on success
  and on failure alike;
* argument validation is all-or-nothing: a missing --key/--input/--output,
  an empty path, a duplicated option, or an unknown argument rejects the
  whole run with exit 2 before any file is read or written; options may
  appear in any order and spaces inside a non-empty path are part of the
  file name.

The report printed by this script contains only test names and status —
key and plaintext bytes are never written to stdout/stderr by these tests.
"""

import argparse
import os
import stat
import sys
import tempfile

sys.path.insert(0, os.path.dirname(os.path.abspath(__file__)))
import keygen_regression as kg  # noqa: E402  shared harness helpers

try:
    from cryptography.hazmat.primitives.ciphers.aead import AESGCM
except ImportError:  # decryption cross-checks are skipped, structure still is
    AESGCM = None

KEY_SIZE = 32
NONCE_SIZE = 12
TAG_SIZE = 16
HEADER_SIZE = 22  # magic(8) + version(1) + algorithm(1) + nonce(12)
MAGIC = b"ENVELOPE"
VERSION = 1
ALGORITHM_AES_256_GCM = 1

SUCCESS_MARKER = b"encrypted envelope saved to"
ENCRYPT_USAGE = (b"Usage: envelopefile encrypt --key <key-path> "
                 b"--input <path> --output <path>")

# Phrases belonging to other failure classes; a usage error must never be
# reported as one of these.
_NON_USAGE_PHRASES = (
    b"already exists",
    b"cannot read",
    b"exactly 32 bytes",
    b"secure random source failed",
    b"cannot create",
    b"failed writing",
    b"failed syncing",
    b"failed closing",
    b"encryption failed",
)


Failure = kg.Failure


def make_key(ctx, dirpath, name="key.bin"):
    """Generate a real key with the product's own keygen; return its path."""
    path = os.path.join(dirpath, name)
    rc, out, err = kg.run(ctx, ["keygen", "--output", path])
    ctx.check(rc == 0, f"fixture keygen failed: rc={rc} {err!r:.200}")
    return path


def make_input(ctx, dirpath, content, name="plain.bin", mode=0o644):
    path = os.path.join(dirpath, name)
    with open(path, "wb") as handle:
        handle.write(content)
    os.chmod(path, mode)
    return path


def assert_encrypt_success(ctx, rc, out, err, output_path):
    ctx.check(rc == 0, f"expected exit 0, got {rc} (stderr: {err!r:.200})")
    ctx.check(SUCCESS_MARKER in out,
              f"stdout missing completion message: {out!r:.200}")
    ctx.check(str(output_path).encode() in out,
              "completion message does not contain the save path")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")


def parse_envelope(ctx, path):
    """Validate the fixed envelope structure; return (header, nonce, body)."""
    blob = kg.read_bytes(path)
    ctx.check(len(blob) >= HEADER_SIZE + TAG_SIZE,
              f"envelope is {len(blob)} bytes, smaller than header+tag")
    ctx.check(blob[:8] == MAGIC,
              f"bad magic: {blob[:8]!r}")
    ctx.check(blob[8] == VERSION,
              f"bad version byte: {blob[8]}")
    ctx.check(blob[9] == ALGORITHM_AES_256_GCM,
              f"bad algorithm byte: {blob[9]}")
    return blob[:HEADER_SIZE], blob[10:22], blob[HEADER_SIZE:]


def decrypt_envelope(ctx, key, path):
    """Independently decrypt the envelope with `cryptography` (never with
    envelopefile itself). Returns the plaintext, or None when the
    cryptography package is unavailable."""
    header, nonce, body = parse_envelope(ctx, path)
    if AESGCM is None:
        return None
    return AESGCM(key).decrypt(nonce, body, header)


def assert_file_state(ctx, path, content, mode):
    ctx.check(kg.read_bytes(path) == content,
              f"{path!r} content changed")
    actual = stat.S_IMODE(os.stat(path).st_mode)
    ctx.check(actual == mode,
              f"{path!r} mode changed to {oct(actual)}")


def assert_encrypt_failure(ctx, rc, out, err, reason=None):
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stdout: {out!r:.200})")
    ctx.check(SUCCESS_MARKER not in out,
              f"completion message printed for failed run: {out!r:.200}")
    ctx.check(len(err) > 0, "stderr empty: failure reason not reported")
    if reason is not None:
        ctx.check(reason.encode() in err,
                  f"stderr does not mention {reason!r}: {err!r:.200}")


def assert_usage_error(ctx, rc, out, err, reason):
    ctx.check(rc == 2, f"expected exit 2, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on usage error: {out!r:.200}")
    ctx.check(reason.encode() in err,
              f"stderr does not name the argument problem {reason!r}: "
              f"{err!r:.200}")
    ctx.check(ENCRYPT_USAGE in err,
              f"stderr does not show the encrypt usage: {err!r:.200}")
    ctx.check(SUCCESS_MARKER not in out + err,
              "usage error carried the completion message")
    for phrase in _NON_USAGE_PHRASES:
        ctx.check(phrase not in err,
                  f"usage error misreported as {phrase.decode()!r}: "
                  f"{err!r:.200}")


# ---------------------------------------------------------------------------
# Success paths and the envelope format.
# ---------------------------------------------------------------------------

def test_success_binary_roundtrip(ctx, workdir):
    # Every byte value, NUL bytes, and CR/LF pairs pass through unchanged:
    # the input is encrypted as raw bytes with no text transformation.
    key_path = make_key(ctx, workdir)
    plaintext = bytes(range(256)) * 3 + b"\x00\r\n\x00end"
    input_path = make_input(ctx, workdir, plaintext, mode=0o640)
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_success(ctx, rc, out, err, output_path)

    blob = kg.read_bytes(output_path)
    ctx.check(len(blob) == HEADER_SIZE + len(plaintext) + TAG_SIZE,
              f"envelope is {len(blob)} bytes, expected "
              f"{HEADER_SIZE + len(plaintext) + TAG_SIZE}")
    key = kg.read_bytes(key_path)
    decrypted = decrypt_envelope(ctx, key, output_path)
    if decrypted is not None:
        ctx.check(decrypted == plaintext,
                  "decrypted content differs from the original input")

    # Key and input files are untouched in content and permissions.
    assert_file_state(ctx, key_path, key, 0o600)
    assert_file_state(ctx, input_path, plaintext, 0o640)
    # Neither key nor plaintext appears on either stream.
    ctx.check(key not in out + err, "key material leaked onto a stream")
    ctx.check(plaintext not in out + err,
              "plaintext leaked onto a stream")


def test_success_empty_input(ctx, workdir):
    # An empty file still yields a valid envelope: complete header and the
    # full 16-byte tag, decrypting to zero bytes.
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"", name="empty.bin")
    output_path = os.path.join(workdir, "empty.env")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_success(ctx, rc, out, err, output_path)
    blob = kg.read_bytes(output_path)
    ctx.check(len(blob) == HEADER_SIZE + TAG_SIZE,
              f"empty-input envelope is {len(blob)} bytes, expected "
              f"{HEADER_SIZE + TAG_SIZE}")
    decrypted = decrypt_envelope(ctx, kg.read_bytes(key_path), output_path)
    if decrypted is not None:
        ctx.check(decrypted == b"", "empty input did not decrypt to empty")


def test_success_nonce_is_fresh_per_run(ctx, workdir):
    # The same key and input encrypted twice must not reuse the nonce (and
    # therefore must not produce the same ciphertext or tag).
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"same content both times")
    first = os.path.join(workdir, "first.env")
    second = os.path.join(workdir, "second.env")
    for output_path in (first, second):
        rc, out, err = kg.run(
            ctx, ["encrypt", "--key", key_path, "--input", input_path,
                  "--output", output_path])
        assert_encrypt_success(ctx, rc, out, err, output_path)
    _, nonce_a, body_a = parse_envelope(ctx, first)
    _, nonce_b, body_b = parse_envelope(ctx, second)
    ctx.check(nonce_a != nonce_b, "the nonce was reused across encryptions")
    ctx.check(body_a != body_b,
              "identical ciphertext+tag across runs (nonce reuse?)")


def test_success_header_is_authenticated(ctx, workdir):
    # Every header byte needed to interpret the envelope is AAD: tampering
    # with the version byte, the algorithm byte, or the tag must make
    # authentication fail. (Skipped without the cryptography package.)
    if AESGCM is None:
        return
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"authenticate me")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_success(ctx, rc, out, err, output_path)
    key = kg.read_bytes(key_path)
    blob = kg.read_bytes(output_path)
    for label, offset in (("version", 8), ("algorithm", 9),
                          ("tag", len(blob) - 1)):
        tampered = bytearray(blob)
        tampered[offset] ^= 0x01
        rejected = False
        try:
            AESGCM(key).decrypt(bytes(tampered[10:22]),
                                bytes(tampered[HEADER_SIZE:]),
                                bytes(tampered[:HEADER_SIZE]))
        except Exception:
            rejected = True
        ctx.check(rejected,
                  f"tampered {label} byte was not rejected by authentication")


def test_success_options_in_any_order(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"order does not matter")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--output", output_path, "--input", input_path,
              "--key", key_path])
    assert_encrypt_success(ctx, rc, out, err, output_path)
    decrypted = decrypt_envelope(ctx, kg.read_bytes(key_path), output_path)
    if decrypted is not None:
        ctx.check(decrypted == b"order does not matter",
                  "decrypted content differs from the original input")


def test_success_paths_with_spaces(ctx, workdir):
    # Spaces inside a non-empty path are part of the file name, for all
    # three options.
    subdir = os.path.join(workdir, "my files")
    os.mkdir(subdir)
    key_path = make_key(ctx, subdir, name="my key.key")
    input_path = make_input(ctx, subdir, b"spaces in names",
                            name="my input.bin")
    output_path = os.path.join(subdir, "my envelope.env")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_success(ctx, rc, out, err, output_path)
    ctx.check(os.listdir(subdir) == sorted(["my key.key", "my input.bin",
                                            "my envelope.env"]),
              f"path with spaces was trimmed or split: "
              f"{os.listdir(subdir)!r}")
    decrypted = decrypt_envelope(ctx, kg.read_bytes(key_path), output_path)
    if decrypted is not None:
        ctx.check(decrypted == b"spaces in names",
                  "decrypted content differs from the original input")


# ---------------------------------------------------------------------------
# Output path exclusivity.
# ---------------------------------------------------------------------------

def _assert_output_rejected(ctx, workdir, output_path, check_untouched):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"some content")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_failure(ctx, rc, out, err, reason="already exists")
    check_untouched()


def test_reject_output_existing_file(ctx, workdir):
    output = os.path.join(workdir, "taken.env")
    original = b"pre-existing content that must survive\x00\x01"
    with open(output, "wb") as handle:
        handle.write(original)
    os.chmod(output, 0o640)
    _assert_output_rejected(
        ctx, workdir, output,
        lambda: assert_file_state(ctx, output, original, 0o640))


def test_reject_output_existing_directory(ctx, workdir):
    output = os.path.join(workdir, "a-directory")
    os.mkdir(output)
    inner = os.path.join(output, "inner.txt")
    with open(inner, "wb") as handle:
        handle.write(b"keep me")
    def check():
        ctx.check(os.path.isdir(output), "existing directory disappeared")
        ctx.check(kg.read_bytes(inner) == b"keep me",
                  "directory contents changed")
    _assert_output_rejected(ctx, workdir, output, check)


def test_reject_output_symlink(ctx, workdir):
    target = os.path.join(workdir, "target.bin")
    original = b"target content must survive"
    with open(target, "wb") as handle:
        handle.write(original)
    link = os.path.join(workdir, "link.env")
    os.symlink(target, link)
    def check():
        ctx.check(os.path.islink(link), "symlink was replaced")
        ctx.check(os.readlink(link) == target, "symlink target changed")
        ctx.check(kg.read_bytes(target) == original,
                  "symlink target was modified")
    _assert_output_rejected(ctx, workdir, link, check)


def test_reject_output_dangling_symlink(ctx, workdir):
    target = os.path.join(workdir, "nowhere.env")
    link = os.path.join(workdir, "dangling.env")
    os.symlink(target, link)
    def check():
        ctx.check(os.path.islink(link), "dangling symlink was replaced")
        ctx.check(os.readlink(link) == target,
                  "dangling symlink target changed")
        ctx.check(not os.path.exists(target),
                  "dangling symlink target was created")
    _assert_output_rejected(ctx, workdir, link, check)


def test_reject_output_same_as_input(ctx, workdir):
    key_path = make_key(ctx, workdir)
    plaintext = b"input that must not become its own envelope"
    input_path = make_input(ctx, workdir, plaintext, mode=0o640)
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", input_path])
    assert_encrypt_failure(ctx, rc, out, err, reason="already exists")
    assert_file_state(ctx, input_path, plaintext, 0o640)


def test_reject_output_same_as_key(ctx, workdir):
    key_path = make_key(ctx, workdir)
    key = kg.read_bytes(key_path)
    input_path = make_input(ctx, workdir, b"some content")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", key_path])
    assert_encrypt_failure(ctx, rc, out, err, reason="already exists")
    assert_file_state(ctx, key_path, key, 0o600)


# ---------------------------------------------------------------------------
# Key and input read failures.
# ---------------------------------------------------------------------------

def test_key_file_missing(ctx, workdir):
    key_path = os.path.join(workdir, "absent.key")
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_failure(ctx, rc, out, err, reason="cannot read key file")
    ctx.check(str(key_path).encode() in err,
              f"stderr does not name the key path: {err!r:.200}")
    ctx.check(not os.path.lexists(output_path),
              "output created despite the unreadable key")


def test_key_file_wrong_length(ctx, workdir):
    input_path = make_input(ctx, workdir, b"some content")
    for size in (0, 1, 31, 33, 64):
        subdir = os.path.join(workdir, f"len-{size}")
        os.mkdir(subdir)
        key_path = os.path.join(subdir, "key.bin")
        with open(key_path, "wb") as handle:
            handle.write(b"\x5a" * size)
        output_path = os.path.join(subdir, "envelope.bin")
        rc, out, err = kg.run(
            ctx, ["encrypt", "--key", key_path, "--input", input_path,
                  "--output", output_path])
        assert_encrypt_failure(ctx, rc, out, err,
                               reason="must contain exactly 32 bytes")
        ctx.check(str(key_path).encode() in err,
                  f"stderr does not name the key path: {err!r:.200}")
        ctx.check(not os.path.lexists(output_path),
                  f"output created for a {size}-byte key")
        # The wrong-length content is not echoed to either stream.
        if size >= 2:
            ctx.check(b"\x5a" * size not in out + err,
                      "key file content leaked onto a stream")


def test_key_file_is_directory(ctx, workdir):
    key_path = os.path.join(workdir, "key-dir")
    os.mkdir(key_path)
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_failure(ctx, rc, out, err)
    ctx.check(not os.path.lexists(output_path),
              "output created despite the unreadable key")


def test_input_file_missing(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = os.path.join(workdir, "absent.bin")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_failure(ctx, rc, out, err,
                           reason="cannot read input file")
    ctx.check(str(input_path).encode() in err,
              f"stderr does not name the input path: {err!r:.200}")
    ctx.check(not os.path.lexists(output_path),
              "output created despite the unreadable input")


def test_input_file_is_directory(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = os.path.join(workdir, "input-dir")
    os.mkdir(input_path)
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path])
    assert_encrypt_failure(ctx, rc, out, err,
                           reason="cannot read input file")
    ctx.check(not os.path.lexists(output_path),
              "output created despite the unreadable input")


# ---------------------------------------------------------------------------
# Secure random source and output save failures (fault injection).
# ---------------------------------------------------------------------------

def test_rand_failure_creates_nothing(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path],
        env_extra=kg._fault_env(EF_TEST_FAIL_RAND="1"),
        use_testable=True)
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty: {out!r:.200}")
    ctx.check(kg.RAND_FAILURE_MARKER.encode() in err,
              f"stderr does not name the random-source failure: {err!r:.200}")
    ctx.check(not os.path.lexists(output_path),
              "output created despite the random-source failure")


def test_write_failure_cleans_up(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"content that never lands")
    output_path = os.path.join(workdir, "envelope.bin")
    bystander, content, mode = kg.make_bystander(ctx, workdir)
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path],
        env_extra=kg._fault_env(EF_TEST_FAIL_WRITE="1"),
        use_testable=True)
    assert_encrypt_failure(ctx, rc, out, err, reason="failed writing")
    ctx.check(not os.path.lexists(output_path),
              "incomplete envelope left behind after write failure")
    kg.assert_bystander_untouched(ctx, bystander, content, mode)


def test_write_failure_with_failed_cleanup_reports_both(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"content that never lands")
    output_path = os.path.join(workdir, "envelope.bin")
    bystander, content, mode = kg.make_bystander(ctx, workdir)
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path],
        env_extra=kg._fault_env(EF_TEST_FAIL_WRITE="1",
                                EF_TEST_FAIL_UNLINK="1"),
        use_testable=True)
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(SUCCESS_MARKER not in out,
              "completion message printed despite failed save")
    lines = err.splitlines()
    ctx.check(len(lines) == 2,
              f"expected the save failure and the removal warning, got: "
              f"{err!r:.200}")
    ctx.check(b"failed writing envelope file" in lines[0] and
              str(output_path).encode() in lines[0],
              f"first stderr line is not the write failure: {err!r:.200}")
    ctx.check(b"could not remove partial envelope file" in lines[1] and
              str(output_path).encode() in lines[1],
              f"second stderr line is not the removal warning: {err!r:.200}")
    ctx.check(os.path.lexists(output_path),
              "incomplete envelope removed even though unlink reported "
              "failure")
    kg.assert_bystander_untouched(ctx, bystander, content, mode)


def test_save_failure_does_not_leak_plaintext(ctx, workdir):
    # A failed save must not print plaintext or key material on any stream.
    key_path = make_key(ctx, workdir)
    plaintext = b"secret content that must not leak\x00\x01"
    input_path = make_input(ctx, workdir, plaintext)
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path],
        env_extra=kg._fault_env(EF_TEST_FAIL_WRITE="1"),
        use_testable=True)
    assert_encrypt_failure(ctx, rc, out, err, reason="failed writing")
    key = kg.read_bytes(key_path)
    ctx.check(key not in out + err, "key material leaked onto a stream")
    ctx.check(plaintext not in out + err, "plaintext leaked onto a stream")


# ---------------------------------------------------------------------------
# In-memory key wiping (OPENSSL_cleanse), on success and on failure.
# ---------------------------------------------------------------------------

def _cleanse_events(ctx, log_path):
    ctx.check(os.path.isfile(log_path),
              f"cleanse log {log_path!r} was not produced")
    with open(log_path, "r", encoding="ascii") as handle:
        return [line.strip() for line in handle if line.strip()]


def test_key_wiped_on_success(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"wipe me afterwards")
    output_path = os.path.join(workdir, "envelope.bin")
    cleanse_log = os.path.join(workdir, "cleanse.log")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path],
        env_extra=kg._fault_env(EF_TEST_CLEANSE_LOG=cleanse_log),
        use_testable=True)
    assert_encrypt_success(ctx, rc, out, err, output_path)
    events = _cleanse_events(ctx, cleanse_log)
    ctx.check(f"CLEANSE {KEY_SIZE}" in events,
              f"the 32-byte key buffer was not wiped: {events!r}")


def test_key_wiped_on_failure(ctx, workdir):
    # The output path already exists, so the run fails after the key was
    # read: the key must still be wiped before exit.
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "taken.env")
    with open(output_path, "wb") as handle:
        handle.write(b"occupied")
    cleanse_log = os.path.join(workdir, "cleanse.log")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", key_path, "--input", input_path,
              "--output", output_path],
        env_extra=kg._fault_env(EF_TEST_CLEANSE_LOG=cleanse_log),
        use_testable=True)
    assert_encrypt_failure(ctx, rc, out, err, reason="already exists")
    events = _cleanse_events(ctx, cleanse_log)
    ctx.check(f"CLEANSE {KEY_SIZE}" in events,
              f"the 32-byte key buffer was not wiped on failure: {events!r}")


# ---------------------------------------------------------------------------
# Argument validation (exit 2, before anything is read or written).
# ---------------------------------------------------------------------------

def test_usage_missing_options(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    cases = (
        (["encrypt", "--input", input_path, "--output", output_path],
         "missing required option '--key'"),
        (["encrypt", "--key", key_path, "--output", output_path],
         "missing required option '--input'"),
        (["encrypt", "--key", key_path, "--input", input_path],
         "missing required option '--output'"),
        (["encrypt"], "missing required option '--key'"),
    )
    for args, reason in cases:
        rc, out, err = kg.run(ctx, args)
        assert_usage_error(ctx, rc, out, err, reason)
        ctx.check(not os.path.lexists(output_path),
                  f"output created despite the usage error: {args!r}")


def test_usage_option_without_value(ctx, workdir):
    for option in ("--key", "--input", "--output"):
        rc, out, err = kg.run(ctx, ["encrypt", option])
        assert_usage_error(ctx, rc, out, err, "requires a non-empty path")


def test_usage_empty_path(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    cases = (
        ["encrypt", "--key", "", "--input", input_path,
         "--output", output_path],
        ["encrypt", "--key", key_path, "--input", "",
         "--output", output_path],
        ["encrypt", "--key", key_path, "--input", input_path,
         "--output", ""],
    )
    for args in cases:
        rc, out, err = kg.run(ctx, args)
        assert_usage_error(ctx, rc, out, err, "requires a non-empty path")
        ctx.check(not os.path.lexists(output_path),
                  f"output created despite the usage error: {args!r}")


def test_usage_duplicate_options(ctx, workdir):
    key_path = make_key(ctx, workdir)
    other_key = make_key(ctx, workdir, name="other.key")
    input_path = make_input(ctx, workdir, b"some content")
    other_input = make_input(ctx, workdir, b"other", name="other.bin")
    output_path = os.path.join(workdir, "envelope.bin")
    other_output = os.path.join(workdir, "other.env")
    cases = (
        ["encrypt", "--key", key_path, "--key", other_key,
         "--input", input_path, "--output", output_path],
        ["encrypt", "--key", key_path, "--input", input_path,
         "--input", other_input, "--output", output_path],
        ["encrypt", "--key", key_path, "--input", input_path,
         "--output", output_path, "--output", other_output],
    )
    for args in cases:
        rc, out, err = kg.run(ctx, args)
        assert_usage_error(ctx, rc, out, err, "more than once")
        ctx.check(not os.path.lexists(output_path),
                  f"output created despite the duplicate option: {args!r}")
        ctx.check(not os.path.lexists(other_output),
                  f"second output created despite the usage error: {args!r}")


def test_usage_unsupported_argument(ctx, workdir):
    key_path = make_key(ctx, workdir)
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    for args in (
            ["encrypt", "--frobnicate", "--key", key_path,
             "--input", input_path, "--output", output_path],
            ["encrypt", "--key", key_path, "--input", input_path,
             "--output", output_path, "extra"]):
        rc, out, err = kg.run(ctx, args)
        assert_usage_error(ctx, rc, out, err, "unsupported argument")
        ctx.check(not os.path.lexists(output_path),
                  f"output created despite the unsupported argument: {args!r}")


def test_usage_error_precedes_file_errors(ctx, workdir):
    # The key path does not exist, but the run also has a usage error: the
    # usage error is reported (exit 2), not the key read failure (exit 1),
    # and nothing is created.
    missing_key = os.path.join(workdir, "absent.key")
    input_path = make_input(ctx, workdir, b"some content")
    output_path = os.path.join(workdir, "envelope.bin")
    rc, out, err = kg.run(
        ctx, ["encrypt", "--key", missing_key, "--input", input_path,
              "--output", output_path, "extra"])
    assert_usage_error(ctx, rc, out, err, "unsupported argument")
    ctx.check(not os.path.lexists(output_path),
              "output created despite the usage error")


ALL_TESTS = [
    test_success_binary_roundtrip,
    test_success_empty_input,
    test_success_nonce_is_fresh_per_run,
    test_success_header_is_authenticated,
    test_success_options_in_any_order,
    test_success_paths_with_spaces,
    test_reject_output_existing_file,
    test_reject_output_existing_directory,
    test_reject_output_symlink,
    test_reject_output_dangling_symlink,
    test_reject_output_same_as_input,
    test_reject_output_same_as_key,
    test_key_file_missing,
    test_key_file_wrong_length,
    test_key_file_is_directory,
    test_input_file_missing,
    test_input_file_is_directory,
    test_rand_failure_creates_nothing,
    test_write_failure_cleans_up,
    test_write_failure_with_failed_cleanup_reports_both,
    test_save_failure_does_not_leak_plaintext,
    test_key_wiped_on_success,
    test_key_wiped_on_failure,
    test_usage_missing_options,
    test_usage_option_without_value,
    test_usage_empty_path,
    test_usage_duplicate_options,
    test_usage_unsupported_argument,
    test_usage_error_precedes_file_errors,
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

    ctx = kg.Context(os.path.abspath(args.binary),
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
