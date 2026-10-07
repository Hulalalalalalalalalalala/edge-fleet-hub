#!/usr/bin/env python3
"""Regression tests for `envelopefile encrypt`.

Covers the guarantees the README makes about the encryption envelope:

* a successful run exits 0, prints a completion message naming the save
  path (and never the plaintext or key), and leaves the input file and
  the key file byte-for-byte and permission-for-permission untouched;
* the produced envelope follows the published v1 layout exactly: 7-byte
  magic "ENVFILE", version byte 1, algorithm byte 1 (AES-256-GCM), a
  12-byte nonce, a ciphertext as long as the input, and a full 16-byte
  authentication tag — 37 bytes longer than the input in total;
* the envelope authenticates under standard AES-256-GCM with the 21-byte
  header as AAD, and the recovered plaintext is byte-for-byte the input.
  This is verified by decrypting independently of the product (Python's
  `cryptography`, exactly as the README's format section demonstrates),
  never by trusting the exit code or the file's mere existence;
* round-trips cover empty input (a valid 37-byte envelope that opens to
  zero bytes), text with newlines, zero bytes mixed with other non-text
  bytes, inputs of tens of thousands of bytes, and lengths that are not
  a multiple of the 16-byte block size (no dropped tail, no padding);
* authentication actually protects everything: for one and the same
  valid envelope, decrypting with a different 32-byte key, flipping a
  single byte anywhere in the 21-byte header, in the ciphertext, or in
  the tag, or truncating the tag, must all fail authentication — no
  tampered envelope may yield an authenticated plaintext, even when the
  modification keeps the total length unchanged;
* each encryption uses a fresh nonce: two envelopes of the same input
  under the same key differ, and both still open to the same plaintext.

The report printed by this script contains only test names and status —
key bytes and plaintext are never written to stdout/stderr by these
tests.
"""

import argparse
import os
import stat
import subprocess
import sys
import tempfile

from cryptography.hazmat.primitives.ciphers.aead import AESGCM

KEY_SIZE = 32
KEY_MODE = 0o600
ENVELOPE_MODE = 0o600
MAGIC = b"ENVFILE"
VERSION = 1
ALGORITHM_AES_256_GCM = 1
NONCE_SIZE = 12
TAG_SIZE = 16
HEADER_SIZE = len(MAGIC) + 1 + 1 + NONCE_SIZE  # 21
OVERHEAD = HEADER_SIZE + TAG_SIZE  # 37
SUCCESS_MARKER = "envelope saved to"


class Failure(Exception):
    pass


class Context:
    def __init__(self, binary):
        self.binary = binary
        self.failures = []
        self.total = 0

    def check(self, condition, message):
        if not condition:
            raise Failure(message)


def run(ctx, args, cwd=None):
    """Run the production binary and capture exit code/stdout/stderr."""
    proc = subprocess.run(
        [ctx.binary, *args],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        cwd=cwd,
    )
    return proc.returncode, proc.stdout, proc.stderr


def read_bytes(path):
    with open(path, "rb") as handle:
        return handle.read()


def make_key(ctx, dirpath, name="envelope.key"):
    """Generate a real 32-byte key file with the product's own keygen."""
    path = os.path.join(dirpath, name)
    rc, out, err = run(ctx, ["keygen", "--output", path])
    ctx.check(rc == 0, f"keygen fixture failed (rc={rc}): {err!r:.200}")
    key = read_bytes(path)
    ctx.check(len(key) == KEY_SIZE,
              f"keygen fixture produced {len(key)} bytes, expected {KEY_SIZE}")
    return path, key


def make_input(ctx, dirpath, content, name="input.bin", mode=0o640):
    """Create the file to encrypt; return (path, content, mode)."""
    path = os.path.join(dirpath, name)
    with open(path, "wb") as handle:
        handle.write(content)
    os.chmod(path, mode)
    return path, content, mode


def encrypt(ctx, workdir, key_path, input_path, name="output.env"):
    """Run a successful encrypt and return (output_path, rc, out, err)."""
    output_path = os.path.join(workdir, name)
    rc, out, err = run(ctx, ["encrypt", "--key", key_path,
                             "--input", input_path,
                             "--output", output_path])
    return output_path, rc, out, err


def assert_encrypt_success(ctx, rc, out, err, output_path, key, plaintext):
    """Contract of a successful encrypt run: exit 0, a completion message
    naming the save path, nothing on stderr, and no key or plaintext
    material on either stream."""
    ctx.check(rc == 0, f"expected exit 0, got {rc} (stderr: {err!r:.200})")
    ctx.check(SUCCESS_MARKER.encode() in out,
              f"stdout missing the completion message: {out!r:.200}")
    ctx.check(str(output_path).encode() in out,
              "completion message does not contain the save path")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")
    if plaintext:
        ctx.check(plaintext not in out,
                  "plaintext leaked into the completion message")
    ctx.check(key not in out, "key material leaked into stdout")


def parse_envelope(ctx, blob, expected_size):
    """Assert the published v1 layout and return (header, nonce, ct, tag)."""
    ctx.check(len(blob) == expected_size,
              f"envelope is {len(blob)} bytes, expected {expected_size} "
              f"(input size + {OVERHEAD})")
    ctx.check(blob[:7] == MAGIC,
              f"bad magic: {blob[:7]!r}, expected {MAGIC!r}")
    ctx.check(blob[7] == VERSION,
              f"version byte is {blob[7]}, expected {VERSION}")
    ctx.check(blob[8] == ALGORITHM_AES_256_GCM,
              f"algorithm byte is {blob[8]}, expected {ALGORITHM_AES_256_GCM}")
    header = blob[:HEADER_SIZE]
    nonce = blob[9:HEADER_SIZE]
    ciphertext = blob[HEADER_SIZE:len(blob) - TAG_SIZE]
    tag = blob[len(blob) - TAG_SIZE:]
    ctx.check(len(nonce) == NONCE_SIZE, "nonce field is not 12 bytes")
    ctx.check(len(tag) == TAG_SIZE, "tag field is not 16 bytes")
    return header, nonce, ciphertext, tag


def open_envelope(ctx, blob, key, expected_plaintext):
    """Authenticate and decrypt per the published format; the recovered
    bytes must be exactly the original input."""
    header = blob[:HEADER_SIZE]
    nonce = blob[9:HEADER_SIZE]
    ciphertext = blob[HEADER_SIZE:len(blob) - TAG_SIZE]
    tag = blob[len(blob) - TAG_SIZE:]
    try:
        plaintext = AESGCM(key).decrypt(nonce, ciphertext + tag, header)
    except Exception as exc:
        raise Failure(f"valid envelope failed authentication: {exc!r}")
    ctx.check(plaintext == expected_plaintext,
              f"recovered {len(plaintext)} bytes, input was "
              f"{len(expected_plaintext)} bytes (content differs)")
    return plaintext


def assert_rejected_envelope(ctx, blob, key, what):
    """A tampered envelope must NOT authenticate: standard AES-256-GCM
    decryption per the published format has to fail, and no authenticated
    plaintext may come out of it."""
    try:
        AESGCM(key).decrypt(blob[9:HEADER_SIZE],
                            blob[HEADER_SIZE:],
                            blob[:HEADER_SIZE])
    except Exception:
        return
    raise Failure(f"{what}: tampered envelope authenticated successfully "
                  "and produced plaintext")


def flip_byte(blob, offset):
    """Return a copy of blob with the byte at offset changed (same length)."""
    mutated = bytearray(blob)
    mutated[offset] ^= 0x01
    return bytes(mutated)


def assert_roundtrip(ctx, workdir, content, input_name="input.bin"):
    """Full success-path check for one input: exit code and message, exact
    envelope layout, authenticated decryption back to the original bytes,
    and input/key files untouched in content and permissions."""
    key_path, key = make_key(ctx, workdir)
    input_path, content, input_mode = make_input(ctx, workdir, content,
                                                 name=input_name)
    key_mode_before = stat.S_IMODE(os.stat(key_path).st_mode)

    output_path, rc, out, err = encrypt(ctx, workdir, key_path, input_path)
    assert_encrypt_success(ctx, rc, out, err, output_path, key, content)

    ctx.check(os.path.isfile(output_path) and not os.path.islink(output_path),
              "envelope missing or not a regular file")
    blob = read_bytes(output_path)
    _, _, ciphertext, _ = parse_envelope(ctx, blob, len(content) + OVERHEAD)
    ctx.check(len(ciphertext) == len(content),
              f"ciphertext is {len(ciphertext)} bytes, input was "
              f"{len(content)} (bytes dropped or padded)")
    mode = stat.S_IMODE(os.stat(output_path).st_mode)
    ctx.check(mode == ENVELOPE_MODE,
              f"envelope mode is {oct(mode)}, expected {oct(ENVELOPE_MODE)}")

    open_envelope(ctx, blob, key, content)

    # The input and key files are read-only participants: content and
    # permissions must be exactly as before the run.
    ctx.check(read_bytes(input_path) == content, "input file content changed")
    ctx.check(stat.S_IMODE(os.stat(input_path).st_mode) == input_mode,
              "input file permissions changed")
    ctx.check(read_bytes(key_path) == key, "key file content changed")
    ctx.check(stat.S_IMODE(os.stat(key_path).st_mode) == key_mode_before,
              "key file permissions changed")
    return key, blob


# ---------------------------------------------------------------------------
# Individual test cases. Each takes (ctx, workdir) and raises Failure.
# ---------------------------------------------------------------------------

def test_roundtrip_empty_input(ctx, workdir):
    # An empty input is a valid use: the envelope still carries the full
    # 21-byte header and the 16-byte tag (37 bytes total, N=0), and it
    # authenticates down to zero plaintext bytes.
    key, blob = assert_roundtrip(ctx, workdir, b"")
    ctx.check(len(blob) == OVERHEAD,
              f"empty-input envelope is {len(blob)} bytes, expected "
              f"{OVERHEAD} (full header + tag)")


def test_roundtrip_text_with_newlines(ctx, workdir):
    content = (b"first line\nsecond line\r\nthird line\n"
               b"\n\n-- trailing newline follows --\n")
    assert_roundtrip(ctx, workdir, content)


def test_roundtrip_mixed_binary(ctx, workdir):
    # Zero bytes, newline bytes, and other non-text bytes interleaved:
    # every byte value 0..255 several times over, shuffled deterministically.
    content = bytes((i * 131 + 7) % 256 for i in range(2048))
    ctx.check(b"\x00" in content and b"\n" in content,
              "fixture error: binary content lacks zero/newline bytes")
    assert_roundtrip(ctx, workdir, content)


def test_roundtrip_large_input(ctx, workdir):
    # Tens of thousands of bytes: the round-trip must not be a short-text
    # accident. 100,000 bytes of a deterministic non-repeating pattern.
    content = bytes((i * 31 + i // 251) % 256 for i in range(100_000))
    assert_roundtrip(ctx, workdir, content)


def test_roundtrip_unaligned_lengths(ctx, workdir):
    # Lengths that are not a multiple of the 16-byte block size — including
    # just above and below a boundary — must round-trip without a dropped
    # tail or extra padding bytes.
    for size in (1, 15, 16, 17, 31, 33, 255, 1023, 4095, 4097):
        subdir = os.path.join(workdir, f"len-{size}")
        os.mkdir(subdir)
        content = bytes((i * 17 + 3) % 256 for i in range(size))
        assert_roundtrip(ctx, subdir, content)


def test_envelope_layout_and_fresh_nonce(ctx, workdir):
    # The same input encrypted twice under the same key yields two complete,
    # differently-nonced envelopes (fresh random nonce per encryption); both
    # follow the exact layout and both open to the same plaintext.
    content = b"nonce freshness probe\x00\x01\x02" * 8
    key_path, key = make_key(ctx, workdir)
    input_path, content, _ = make_input(ctx, workdir, content)

    first_path, rc, out, err = encrypt(ctx, workdir, key_path, input_path,
                                       name="first.env")
    assert_encrypt_success(ctx, rc, out, err, first_path, key, content)
    second_path, rc, out, err = encrypt(ctx, workdir, key_path, input_path,
                                        name="second.env")
    assert_encrypt_success(ctx, rc, out, err, second_path, key, content)

    first = read_bytes(first_path)
    second = read_bytes(second_path)
    for blob in (first, second):
        parse_envelope(ctx, blob, len(content) + OVERHEAD)
    ctx.check(first[9:HEADER_SIZE] != second[9:HEADER_SIZE],
              "two encryptions reused the same 12-byte nonce")
    ctx.check(first != second,
              "two envelopes of the same input are byte-identical")
    open_envelope(ctx, first, key, content)
    open_envelope(ctx, second, key, content)


def test_wrong_key_fails_authentication(ctx, workdir):
    # The same valid envelope opened with a different 32-byte key must not
    # authenticate. (The two keys are verified to actually differ.)
    content = b"content protected against the wrong key" * 4
    key, blob = assert_roundtrip(ctx, workdir, content)
    _, other_key = make_key(ctx, workdir, name="other.key")
    ctx.check(other_key != key,
              "fixture error: the two generated keys are identical")
    assert_rejected_envelope(ctx, blob, other_key, "wrong key")


def test_header_tamper_fails_authentication(ctx, workdir):
    # Every one of the 21 header bytes participates in the authentication:
    # flipping any single byte of magic/version/algorithm/nonce — the total
    # length unchanged — must break the envelope.
    content = b"header authentication probe" * 4
    key, blob = assert_roundtrip(ctx, workdir, content)
    for offset in range(HEADER_SIZE):
        assert_rejected_envelope(ctx, flip_byte(blob, offset), key,
                                 f"header byte {offset} flipped")


def test_ciphertext_tamper_fails_authentication(ctx, workdir):
    # A single flipped byte anywhere in the ciphertext — first, middle,
    # last — is detected, with the envelope length unchanged.
    content = bytes((i * 7 + 1) % 256 for i in range(512))
    key, blob = assert_roundtrip(ctx, workdir, content)
    last = len(blob) - TAG_SIZE - 1
    for offset in (HEADER_SIZE, HEADER_SIZE + 255, last):
        assert_rejected_envelope(ctx, flip_byte(blob, offset), key,
                                 f"ciphertext byte at offset {offset} flipped")


def test_tag_tamper_fails_authentication(ctx, workdir):
    # Flipping any byte of the 16-byte authentication tag is detected.
    content = b"tag authentication probe" * 4
    key, blob = assert_roundtrip(ctx, workdir, content)
    for offset in range(len(blob) - TAG_SIZE, len(blob)):
        assert_rejected_envelope(ctx, flip_byte(blob, offset), key,
                                 f"tag byte at offset {offset} flipped")


def test_truncated_envelope_fails_authentication(ctx, workdir):
    # Cutting bytes off the end — one tag byte, half the tag, the whole
    # tag, or part of the ciphertext — must not open as a valid envelope,
    # and in particular must not yield an authenticated plaintext.
    content = bytes((i * 13 + 5) % 256 for i in range(256))
    key, blob = assert_roundtrip(ctx, workdir, content)
    for cut, what in ((1, "last tag byte removed"),
                      (8, "half the tag removed"),
                      (TAG_SIZE, "whole tag removed"),
                      (TAG_SIZE + 10, "tag and ciphertext tail removed")):
        assert_rejected_envelope(ctx, blob[:len(blob) - cut], key, what)


def test_success_message_and_file_isolation(ctx, workdir):
    # The completion message names the save path and nothing more: it must
    # not echo the plaintext or the key, and the run must not create or
    # modify anything besides the one new envelope file.
    content = b"distinctive plaintext marker \x00\x01 must stay secret" * 3
    key_path, key = make_key(ctx, workdir)
    input_path, content, _ = make_input(ctx, workdir, content)
    before = sorted(os.listdir(workdir))

    output_path, rc, out, err = encrypt(ctx, workdir, key_path, input_path)
    assert_encrypt_success(ctx, rc, out, err, output_path, key, content)
    ctx.check(key.hex().encode() not in out and key.hex().encode() not in err,
              "key material leaked in hex form")
    ctx.check(b"must stay secret" not in out,
              "plaintext fragment leaked into stdout")

    after = sorted(os.listdir(workdir))
    ctx.check(after == sorted(before + [os.path.basename(output_path)]),
              f"unexpected files appeared or disappeared: {before!r} -> "
              f"{after!r}")


ALL_TESTS = [
    test_roundtrip_empty_input,
    test_roundtrip_text_with_newlines,
    test_roundtrip_mixed_binary,
    test_roundtrip_large_input,
    test_roundtrip_unaligned_lengths,
    test_envelope_layout_and_fresh_nonce,
    test_wrong_key_fails_authentication,
    test_header_tamper_fails_authentication,
    test_ciphertext_tamper_fails_authentication,
    test_tag_tamper_fails_authentication,
    test_truncated_envelope_fails_authentication,
    test_success_message_and_file_isolation,
]


def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True,
                        help="path to the production envelopefile binary")
    parser.add_argument("--work-root", default=None,
                        help="directory under which scratch dirs are created")
    args = parser.parse_args()

    ctx = Context(os.path.abspath(args.binary))

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
