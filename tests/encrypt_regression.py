#!/usr/bin/env python3
"""Regression tests for `envelopefile encrypt`.

Covers the guarantees the README makes about the encryption envelope:

* round trip: encrypting an existing 32-byte raw key + input file + fresh
  output path produces an envelope that passes AES-256-GCM authentication
  and restores the input byte for byte. Inputs cover zero bytes, newlines
  and non-text bytes mixed together, files of tens of thousands of bytes,
  lengths that are not a multiple of the 16-byte block size, and the empty
  file (which still yields a complete header and tag and restores to zero
  bytes);
* format: the envelope starts with the fixed `ENVFILE` magic, version 1
  and algorithm 1, carries a 12-byte nonce and ends with the full 16-byte
  tag; the total length is exactly 37 bytes more than the input and the
  ciphertext has the same length as the input. Success is never judged by
  exit code or file existence alone — the layout is parsed and the
  authenticated decryption is checked against the original bytes;
* authentication: all 21 header bytes are covered by the tag — flipping
  any single header, ciphertext, or tag byte (which leaves the total
  length unchanged) makes standard authenticated decryption fail, as does
  decrypting with a different 32-byte key or truncating tag bytes off the
  end. A failed authentication never yields a plaintext result;
* command-line behaviour: a normal encryption exits 0 and prints the
  completion message naming the save path; the input and key files keep
  their content and permissions; neither the message nor the envelope
  leaks plaintext or key bytes;
* output saving (run against the fault-injecting test build): once the
  envelope bytes are sealed, a save failure after the new output file was
  created — write, fsync, or close failing, including an unrecoverable
  error after part of the envelope is already on disk — exits 1 with an
  empty stdout and a stderr line naming the failed save stage and the
  output path; the envelope this run created is removed, and the key,
  input, and neighbouring files keep their content and permissions. A
  close failure counts as a save failure even though every envelope byte
  was already written and synced. If removing the incomplete file itself
  fails, the original save failure stays the primary error (exit 1, no
  completion message) and a second stderr line warns with the output path
  and the removal reason while the incomplete file genuinely remains —
  the suite checks both "removed" and "still there" outcomes instead of
  treating any non-zero exit as cleaned up. An output path that already
  exists (regular file, symlink, dangling symlink) or that names the
  input or key file is refused before creation with an "already exists"
  reason — never a cleanup warning — leaving the existing content,
  permissions, and link relations untouched and never creating a dangling
  link's target. With no save fault armed the run exits 0 and the
  envelope authenticates as usual.

Decryption is performed by an independent pure-Python AES-256-GCM
implementation embedded below (no OpenSSL, no third-party packages), so
the envelope is verified exactly the way the README says any independent
implementation can interpret it. The decryptor is itself checked against
known-answer vectors before any product output is trusted.

The report printed by this script contains only test names and status —
key and plaintext bytes are never written to stdout/stderr by these tests.
"""

import argparse
import errno
import hmac
import os
import random
import stat
import subprocess
import sys
import tempfile

KEY_SIZE = 32
NONCE_SIZE = 12
TAG_SIZE = 16
HEADER_SIZE = 21
ENVELOPE_OVERHEAD = HEADER_SIZE + TAG_SIZE  # 37
MAGIC = b"ENVFILE"
VERSION = 1
ALGORITHM_AES_256_GCM = 1
ENVELOPE_MODE = 0o600
SUCCESS_MARKER = "file encrypted and envelope saved to"


# ---------------------------------------------------------------------------
# Independent pure-Python AES-256-GCM decryptor.
#
# This exists so the envelope produced by the product (which uses OpenSSL) is
# checked by code that shares no implementation with it, exactly as the
# README's "封装文件格式" section promises any independent implementation can
# do. Only decryption is needed; GCM decryption uses the AES block function
# in the forward direction only.
# ---------------------------------------------------------------------------

_SBOX = bytes.fromhex(
    "637c777bf26b6fc53001672bfed7ab76"
    "ca82c97dfa5947f0add4a2af9ca472c0"
    "b7fd9326363ff7cc34a5e5f171d83115"
    "04c723c31896059a071280e2eb27b275"
    "09832c1a1b6e5aa0523bd6b329e32f84"
    "53d100ed20fcb15b6acbbe394a4c58cf"
    "d0efaafb434d338545f9027f503c9fa8"
    "51a3408f929d38f5bcb6da2110fff3d2"
    "cd0c13ec5f974417c4a77e3d645d1973"
    "60814fdc222a908846eeb814de5e0bdb"
    "e0323a0a4906245cc2d3ac629195e479"
    "e7c8376d8dd54ea96c56f4ea657aae08"
    "ba78252e1ca6b4c6e8dd741f4bbd8b8a"
    "703eb5664803f60e613557b986c11d9e"
    "e1f8981169d98e949b1e87e9ce5528df"
    "8ca1890dbfe6426841992d0fb054bb16"
)


def _xtime(value):
    """Multiply by x (0x02) in GF(2^8) with the AES reduction polynomial."""
    return ((value << 1) ^ (0x1B if value & 0x80 else 0)) & 0xFF


def _expand_key(key):
    """AES-256 key schedule: 32-byte key -> 15 round keys of 16 bytes."""
    nk, nr = 8, 14
    words = [key[4 * i:4 * i + 4] for i in range(nk)]
    rcon = 1
    while len(words) < 4 * (nr + 1):
        temp = words[-1]
        i = len(words)
        if i % nk == 0:
            temp = bytes((_SBOX[temp[1]], _SBOX[temp[2]],
                          _SBOX[temp[3]], _SBOX[temp[0]]))
            temp = bytes((temp[0] ^ rcon,)) + temp[1:]
            rcon = _xtime(rcon)
        elif i % nk == 4:
            temp = bytes(_SBOX[b] for b in temp)
        prev = words[i - nk]
        words.append(bytes(p ^ t for p, t in zip(prev, temp)))
    flat = b"".join(words)
    return [flat[16 * r:16 * r + 16] for r in range(nr + 1)]


def _shift_rows(state):
    # Column-major state: index = row + 4 * column; row r rotates left by r.
    return bytearray(state[row + 4 * ((col + row) % 4)]
                     for col in range(4) for row in range(4))


def _mix_columns(state):
    for col in range(4):
        i = 4 * col
        a0, a1, a2, a3 = state[i:i + 4]
        t = a0 ^ a1 ^ a2 ^ a3
        state[i] = a0 ^ t ^ _xtime(a0 ^ a1)
        state[i + 1] = a1 ^ t ^ _xtime(a1 ^ a2)
        state[i + 2] = a2 ^ t ^ _xtime(a2 ^ a3)
        state[i + 3] = a3 ^ t ^ _xtime(a3 ^ a0)


def _encrypt_block(round_keys, block):
    state = bytearray(a ^ b for a, b in zip(block, round_keys[0]))
    for rnd in range(1, 14):
        state = bytearray(_SBOX[b] for b in state)
        state = _shift_rows(state)
        _mix_columns(state)
        rk = round_keys[rnd]
        for i in range(16):
            state[i] ^= rk[i]
    state = bytearray(_SBOX[b] for b in state)
    state = _shift_rows(state)
    rk = round_keys[14]
    for i in range(16):
        state[i] ^= rk[i]
    return bytes(state)


def _gf128_mul(x, y):
    """Multiply two GF(2^128) elements (GCM's polynomial, bit-reversed)."""
    z = 0
    v = y
    for i in range(128):
        if (x >> (127 - i)) & 1:
            z ^= v
        v = (v >> 1) ^ (0xE1000000000000000000000000000000 if v & 1 else 0)
    return z


def _ghash(h, aad, ciphertext):
    y = 0
    for data in (aad, ciphertext):
        for i in range(0, len(data), 16):
            block = data[i:i + 16]
            block = block + b"\0" * (16 - len(block))
            y = _gf128_mul(y ^ int.from_bytes(block, "big"), h)
    lengths = (8 * len(aad)).to_bytes(8, "big") + \
        (8 * len(ciphertext)).to_bytes(8, "big")
    return _gf128_mul(y ^ int.from_bytes(lengths, "big"), h)


def _inc32(counter):
    """Increment the rightmost 32 bits of a 128-bit counter, modulo 2^32."""
    return (counter & ~0xFFFFFFFF) | ((counter + 1) & 0xFFFFFFFF)


def _gctr(round_keys, initial_counter, data):
    out = bytearray()
    counter = initial_counter
    for i in range(0, len(data), 16):
        keystream = _encrypt_block(round_keys, counter.to_bytes(16, "big"))
        chunk = data[i:i + 16]
        out += bytes(a ^ b for a, b in zip(chunk, keystream))
        counter = _inc32(counter)
    return bytes(out)


def aes256gcm_decrypt(key, nonce, ciphertext, tag, aad):
    """Standard AES-256-GCM authenticated decryption.

    Returns the plaintext, or None when authentication fails — a failure
    never produces a plaintext result.
    """
    if len(key) != KEY_SIZE or len(nonce) != NONCE_SIZE or len(tag) != TAG_SIZE:
        return None
    round_keys = _expand_key(key)
    h = int.from_bytes(_encrypt_block(round_keys, bytes(16)), "big")
    j0 = (int.from_bytes(nonce, "big") << 32) | 1
    expected_tag = _gctr(round_keys, j0, _ghash(h, aad, ciphertext)
                         .to_bytes(16, "big"))
    if not hmac.compare_digest(expected_tag, tag):
        return None
    return _gctr(round_keys, _inc32(j0), ciphertext)


# Known-answer vectors (AES-256-GCM, 12-byte nonce, 16-byte tag). Vector A
# is the classic all-zero case; B exercises AAD and a plaintext length that
# is not a multiple of 16; C is the empty plaintext with AAD.
_KNOWN_ANSWERS = (
    {
        "key": bytes(32),
        "nonce": bytes(12),
        "aad": b"",
        "plaintext": bytes(16),
        "sealed": bytes.fromhex(
            "cea7403d4d606b6e074ec5d3baf39d18"
            "d0d1c8a799996bf0265b98b5d48ab919"),
    },
    {
        "key": bytes(range(32)),
        "nonce": bytes(range(12)),
        "aad": bytes.fromhex("454e5646494c450101000102030405060708090a0b"),
        "plaintext": b"hello envelope, hello envelope, hello!",
        "sealed": bytes.fromhex(
            "2f67ba77aac5a775fb24fbe4c18c544debb3eb589f5b3a12"
            "4e0289ea6d0c2c926975c290c0e06aae3f984a7bad28f9e5"
            "c0fe0ed5e030"),
    },
    {
        "key": bytes(range(32)),
        "nonce": bytes(range(12)),
        "aad": bytes.fromhex("454e5646494c450101000102030405060708090a0b"),
        "plaintext": b"",
        "sealed": bytes.fromhex("72b0fd4bcd1c7a3f39c06c118ca6c44b"),
    },
)


# ---------------------------------------------------------------------------
# Envelope parsing per the README's public format.
# ---------------------------------------------------------------------------

def parse_envelope(blob):
    """Split an envelope into (header, nonce, ciphertext, tag).

    Returns None when the bytes cannot be a valid version-1 envelope: too
    short to hold header plus tag, wrong magic, or unsupported
    version/algorithm identifiers.
    """
    if len(blob) < ENVELOPE_OVERHEAD:
        return None
    if blob[:7] != MAGIC or blob[7] != VERSION or blob[8] != ALGORITHM_AES_256_GCM:
        return None
    return blob[:HEADER_SIZE], blob[9:21], blob[HEADER_SIZE:-TAG_SIZE], blob[-TAG_SIZE:]


def decrypt_envelope(key, blob):
    """Authenticated decryption of an envelope per the public format.

    The 21-byte header is the additional authenticated data. Returns the
    recovered plaintext, or None when the layout is invalid or the tag does
    not verify — never a partial or unauthenticated plaintext.
    """
    parsed = parse_envelope(blob)
    if parsed is None:
        return None
    header, nonce, ciphertext, tag = parsed
    return aes256gcm_decrypt(key, nonce, ciphertext, tag, header)


# ---------------------------------------------------------------------------
# Test harness.
# ---------------------------------------------------------------------------

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


def make_key(ctx, workdir, name="envelope.key"):
    """Create a real key file with the product's own keygen."""
    path = os.path.join(workdir, name)
    rc, out, err = run(ctx, ["keygen", "--output", path])
    ctx.check(rc == 0, f"fixture keygen failed with exit {rc}: {err!r:.200}")
    key = read_bytes(path)
    ctx.check(len(key) == KEY_SIZE,
              f"fixture key is {len(key)} bytes, expected {KEY_SIZE}")
    return path, key


def assert_envelope_format(ctx, blob, input_size):
    """The envelope matches the published layout for an input of this size."""
    ctx.check(len(blob) == input_size + ENVELOPE_OVERHEAD,
              f"envelope is {len(blob)} bytes, expected {input_size} + "
              f"{ENVELOPE_OVERHEAD} = {input_size + ENVELOPE_OVERHEAD}")
    ctx.check(blob[:7] == MAGIC,
              f"bad magic: {blob[:7]!r}, expected {MAGIC!r}")
    ctx.check(blob[7] == VERSION,
              f"version byte is {blob[7]}, expected {VERSION}")
    ctx.check(blob[8] == ALGORITHM_AES_256_GCM,
              f"algorithm byte is {blob[8]}, expected {ALGORITHM_AES_256_GCM}")
    ciphertext = blob[HEADER_SIZE:-TAG_SIZE]
    ctx.check(len(ciphertext) == input_size,
              f"ciphertext is {len(ciphertext)} bytes, input was {input_size}: "
              "trailing bytes lost or padding added")
    ctx.check(parse_envelope(blob) is not None,
              "envelope does not parse under the published format")


def encrypt_and_verify(ctx, workdir, name, plaintext, key_path, key):
    """Encrypt one file and verify success the strong way: exit 0, the
    completion message, the exact published layout, and an authenticated
    decryption that reproduces the input byte for byte."""
    input_path = os.path.join(workdir, name + ".bin")
    output_path = os.path.join(workdir, name + ".env")
    with open(input_path, "wb") as handle:
        handle.write(plaintext)

    rc, out, err = run(ctx, ["encrypt", "--key", key_path,
                             "--input", input_path, "--output", output_path])
    ctx.check(rc == 0, f"encrypt exited {rc} (stderr: {err!r:.200})")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() in out,
              f"stdout missing completion message: {out!r:.200}")
    ctx.check(str(output_path).encode() in out,
              "completion message does not contain the save path")

    ctx.check(os.path.isfile(output_path) and not os.path.islink(output_path),
              "envelope file missing or not a regular file")
    mode = stat.S_IMODE(os.stat(output_path).st_mode)
    ctx.check(mode == ENVELOPE_MODE,
              f"envelope file mode is {oct(mode)}, expected {oct(ENVELOPE_MODE)}")

    blob = read_bytes(output_path)
    assert_envelope_format(ctx, blob, len(plaintext))
    recovered = decrypt_envelope(key, blob)
    ctx.check(recovered is not None,
              "a freshly produced envelope failed to authenticate")
    ctx.check(recovered == plaintext,
              "recovered content differs from the original input "
              f"({len(plaintext)} bytes)")
    return input_path, output_path, blob


def assert_tamper_rejected(ctx, key, blob, offset, note):
    """Flipping one byte (total length unchanged) must break authentication
    and must not yield any plaintext."""
    tampered = bytearray(blob)
    tampered[offset] ^= 0x01
    ctx.check(decrypt_envelope(key, bytes(tampered)) is None,
              f"tampered envelope ({note}, byte {offset}) still authenticated")


# ---------------------------------------------------------------------------
# Individual test cases. Each takes (ctx, workdir) and raises Failure.
# ---------------------------------------------------------------------------

def test_decryptor_known_answers(ctx, workdir):
    # Everything else in this suite trusts the embedded decryptor, so prove
    # it first: the known-answer vectors must decrypt, and the same vectors
    # with a flipped tag byte, a wrong key, or wrong AAD must NOT.
    for index, vector in enumerate(_KNOWN_ANSWERS):
        sealed = vector["sealed"]
        ciphertext, tag = sealed[:-TAG_SIZE], sealed[-TAG_SIZE:]
        recovered = aes256gcm_decrypt(vector["key"], vector["nonce"],
                                      ciphertext, tag, vector["aad"])
        ctx.check(recovered == vector["plaintext"],
                  f"known-answer vector {index} did not decrypt")

        bad_tag = tag[:-1] + bytes([tag[-1] ^ 0x01])
        ctx.check(aes256gcm_decrypt(vector["key"], vector["nonce"],
                                    ciphertext, bad_tag, vector["aad"]) is None,
                  f"vector {index}: corrupted tag still authenticated")
        wrong_key = bytes([vector["key"][0] ^ 0x01]) + vector["key"][1:]
        ctx.check(aes256gcm_decrypt(wrong_key, vector["nonce"],
                                    ciphertext, tag, vector["aad"]) is None,
                  f"vector {index}: wrong key still authenticated")
        ctx.check(aes256gcm_decrypt(vector["key"], vector["nonce"],
                                    ciphertext, tag, vector["aad"] + b"x")
                  is None,
                  f"vector {index}: extended AAD still authenticated")


def test_roundtrip_text_with_newlines(ctx, workdir):
    key_path, key = make_key(ctx, workdir)
    plaintext = (b"#!/bin/sh\n"
                 b"\n"
                 b"echo first line\r\n"
                 b"echo second line\n"
                 b"utf-8 text: \xc3\xa4\xc2\xb8\xc2\xad\xc3\xa6\xe2\x80\x93\xe2\x80\x93\n"
                 b"trailing empty lines\n\n\n")
    encrypt_and_verify(ctx, workdir, "text", plaintext, key_path, key)


def test_roundtrip_mixed_binary(ctx, workdir):
    # Zero bytes, newlines and non-text bytes interleaved: every byte value
    # plus explicit NUL/CR/LF runs plus deterministic pseudo-random bytes.
    key_path, key = make_key(ctx, workdir)
    plaintext = (bytes(range(256)) * 4
                 + b"\x00\n\x00\r\n\x00" * 100
                 + random.Random(0xEF01).randbytes(777))
    encrypt_and_verify(ctx, workdir, "mixed", plaintext, key_path, key)


def test_roundtrip_empty_input(ctx, workdir):
    # An empty input is valid: the envelope is exactly 37 bytes (full header
    # plus tag, no ciphertext) and authenticates down to zero bytes.
    key_path, key = make_key(ctx, workdir)
    _, output_path, blob = encrypt_and_verify(ctx, workdir, "empty", b"",
                                              key_path, key)
    ctx.check(len(blob) == ENVELOPE_OVERHEAD,
              f"empty-input envelope is {len(blob)} bytes, expected "
              f"{ENVELOPE_OVERHEAD} (header + tag only)")


def test_roundtrip_large_file(ctx, workdir):
    # Tens of thousands of bytes, and a length that is not a multiple of the
    # 16-byte block size: guards against implementations that only handle
    # short texts, drop the tail, or add padding.
    key_path, key = make_key(ctx, workdir)
    size = 50003
    ctx.check(size % 16 != 0, "fixture error: size is block-aligned")
    plaintext = random.Random(0xEF02).randbytes(size)
    encrypt_and_verify(ctx, workdir, "large", plaintext, key_path, key)


def test_roundtrip_unaligned_lengths(ctx, workdir):
    # Boundary lengths around the 16-byte block size, each restoring exactly.
    key_path, key = make_key(ctx, workdir)
    for size in (1, 2, 15, 16, 17, 31, 32, 33, 255, 1023):
        subdir = os.path.join(workdir, f"len-{size}")
        os.mkdir(subdir)
        plaintext = random.Random(size).randbytes(size)
        encrypt_and_verify(ctx, subdir, "data", plaintext, key_path, key)


def test_envelope_layout_and_fresh_nonce(ctx, workdir):
    # The same input encrypted twice with the same key must produce two
    # envelopes that both follow the layout and both restore the input, but
    # carry different nonces (fresh randomness per encryption), hence
    # different ciphertext and tag.
    key_path, key = make_key(ctx, workdir)
    plaintext = b"same input encrypted twice\n" * 8
    _, _, first = encrypt_and_verify(ctx, workdir, "first", plaintext,
                                     key_path, key)
    _, _, second = encrypt_and_verify(ctx, workdir, "second", plaintext,
                                      key_path, key)
    ctx.check(first[9:21] != second[9:21],
              "two encryptions of the same input reused the same nonce")
    ctx.check(first[HEADER_SIZE:-TAG_SIZE] != second[HEADER_SIZE:-TAG_SIZE],
              "identical ciphertext for two encryptions of the same input")
    ctx.check(first[-TAG_SIZE:] != second[-TAG_SIZE:],
              "identical tag for two encryptions of the same input")


def test_header_fully_authenticated(ctx, workdir):
    # Every one of the 21 header bytes participates in authentication:
    # flipping each single byte (magic, version, algorithm, nonce) must be
    # detected — the header cannot be peeled off or swapped in from another
    # envelope.
    key_path, key = make_key(ctx, workdir)
    plaintext = b"header authentication probe\n" * 4
    _, _, blob = encrypt_and_verify(ctx, workdir, "probe", plaintext,
                                    key_path, key)
    ctx.check(decrypt_envelope(key, blob) == plaintext,
              "control: untampered envelope did not authenticate")
    for offset in range(HEADER_SIZE):
        assert_tamper_rejected(ctx, key, blob, offset, "header")


def test_ciphertext_tamper_detected(ctx, workdir):
    key_path, key = make_key(ctx, workdir)
    plaintext = random.Random(0xEF03).randbytes(64)
    _, _, blob = encrypt_and_verify(ctx, workdir, "probe", plaintext,
                                    key_path, key)
    ctx.check(decrypt_envelope(key, blob) == plaintext,
              "control: untampered envelope did not authenticate")
    for offset in (HEADER_SIZE, HEADER_SIZE + 32,
                   HEADER_SIZE + len(plaintext) - 1):
        assert_tamper_rejected(ctx, key, blob, offset, "ciphertext")


def test_tag_tamper_detected(ctx, workdir):
    key_path, key = make_key(ctx, workdir)
    plaintext = b"tag authentication probe\n" * 4
    _, _, blob = encrypt_and_verify(ctx, workdir, "probe", plaintext,
                                    key_path, key)
    ctx.check(decrypt_envelope(key, blob) == plaintext,
              "control: untampered envelope did not authenticate")
    for offset in (len(blob) - TAG_SIZE, len(blob) - 1):
        assert_tamper_rejected(ctx, key, blob, offset, "tag")


def test_wrong_key_detected(ctx, workdir):
    # The same valid envelope under a different 32-byte key must not
    # authenticate, whether the keys differ in the first or the last byte.
    key_path, key = make_key(ctx, workdir)
    plaintext = b"wrong key probe\n" * 8
    _, _, blob = encrypt_and_verify(ctx, workdir, "probe", plaintext,
                                    key_path, key)
    ctx.check(decrypt_envelope(key, blob) == plaintext,
              "control: untampered envelope did not authenticate")
    for position in (0, KEY_SIZE - 1):
        other = bytearray(key)
        other[position] ^= 0x01
        ctx.check(bytes(other) != key, "fixture error: keys are identical")
        ctx.check(decrypt_envelope(bytes(other), blob) is None,
                  f"envelope authenticated under a key differing at byte "
                  f"{position}")


def test_truncated_tag_rejected(ctx, workdir):
    # Cutting bytes off the end of the envelope — including just the last
    # tag byte — must not be restorable as a valid envelope, and must not
    # yield an authenticated plaintext.
    key_path, key = make_key(ctx, workdir)
    plaintext = b"truncation probe payload\n" * 4
    _, _, blob = encrypt_and_verify(ctx, workdir, "probe", plaintext,
                                    key_path, key)
    ctx.check(decrypt_envelope(key, blob) == plaintext,
              "control: untampered envelope did not authenticate")
    for cut in (1, 5, TAG_SIZE, len(blob) - HEADER_SIZE):
        ctx.check(decrypt_envelope(key, blob[:len(blob) - cut]) is None,
                  f"envelope truncated by {cut} byte(s) still authenticated")

    # The empty-input envelope is exactly header + tag; losing its final tag
    # byte drops it below the minimum envelope size.
    _, _, empty_blob = encrypt_and_verify(ctx, workdir, "empty", b"",
                                          key_path, key)
    ctx.check(decrypt_envelope(key, empty_blob[:-1]) is None,
              "empty-input envelope with a truncated tag still authenticated")


def test_completion_message_and_preservation(ctx, workdir):
    # A normal encryption: exit 0, the exact completion line naming the save
    # path, empty stderr; the input and key files keep their content and
    # permissions; no plaintext or key bytes reach the streams or the
    # envelope.
    key_path, key = make_key(ctx, workdir)
    key_mode = stat.S_IMODE(os.stat(key_path).st_mode)

    marker = b"-- confidential payload 0xDEADBEEF --\n"
    plaintext = marker * 100 + bytes(range(256))
    input_path = os.path.join(workdir, "document.bin")
    with open(input_path, "wb") as handle:
        handle.write(plaintext)
    os.chmod(input_path, 0o640)

    output_path = os.path.join(workdir, "document.bin.env")
    rc, out, err = run(ctx, ["encrypt", "--key", key_path,
                             "--input", input_path, "--output", output_path])
    ctx.check(rc == 0, f"encrypt exited {rc} (stderr: {err!r:.200})")
    expected = (f"envelopefile: {SUCCESS_MARKER} '{output_path}'\n").encode()
    ctx.check(out == expected,
              f"unexpected completion message: {out!r:.200}")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")

    # The original files were opened read-only: content and mode unchanged.
    ctx.check(read_bytes(input_path) == plaintext, "input file content changed")
    ctx.check(stat.S_IMODE(os.stat(input_path).st_mode) == 0o640,
              "input file permissions changed")
    ctx.check(read_bytes(key_path) == key, "key file content changed")
    ctx.check(stat.S_IMODE(os.stat(key_path).st_mode) == key_mode,
              "key file permissions changed")

    # No leakage: neither the message nor the envelope contains the key or
    # the (distinctive) plaintext.
    blob = read_bytes(output_path)
    for stream, label in ((out, "stdout"), (err, "stderr"), (blob, "envelope")):
        ctx.check(key not in stream, f"key material leaked into {label}")
        ctx.check(marker not in stream, f"plaintext leaked into {label}")

    # And the envelope itself still verifies end to end.
    assert_envelope_format(ctx, blob, len(plaintext))
    ctx.check(decrypt_envelope(key, blob) == plaintext,
              "envelope did not restore the original input")


# ---------------------------------------------------------------------------
# Output-save behaviour.
#
# The save-failure cases run against the fault-injecting test build: the
# envelope file is the only path opened O_CREAT|O_EXCL, so the shim's
# write/fsync/close/unlink faults apply to exactly the output being saved.
# Every case starts from a valid 32-byte raw key and a readable input file
# and targets a path that does not exist yet.
# ---------------------------------------------------------------------------

CLEANUP_WARNING_MARKER = "could not remove partial envelope file"
REFUSAL_MARKER = "refusing to write, path already exists"
PLAINTEXT_MARKER = b"-- confidential payload 0xDEADBEEF --"


def fault_env(**overrides):
    """Environment for the fault-injecting build; every knob defaults off."""
    env = {
        "EF_TEST_FAIL_WRITE": "0",
        "EF_TEST_FAIL_FSYNC": "0",
        "EF_TEST_FAIL_CLOSE": "0",
    }
    env.update({key: value for key, value in overrides.items()
                if value is not None})
    return env


def make_save_fixture(ctx, workdir):
    """A valid 32-byte key, a readable input file, and a bystander file —
    everything a save run must leave untouched, with content and
    permissions recorded for the post-run comparison."""
    key_path, key = make_key(ctx, workdir)
    plaintext = PLAINTEXT_MARKER + b"\n" + PLAINTEXT_MARKER * 40 + \
        bytes(range(256))
    input_path = os.path.join(workdir, "document.bin")
    with open(input_path, "wb") as handle:
        handle.write(plaintext)
    os.chmod(input_path, 0o640)
    bystander = os.path.join(workdir, "unrelated.txt")
    with open(bystander, "wb") as handle:
        handle.write(b"do not touch")
    os.chmod(bystander, 0o644)
    return {
        "key_path": key_path,
        "key": key,
        "key_mode": stat.S_IMODE(os.stat(key_path).st_mode),
        "input_path": input_path,
        "plaintext": plaintext,
        "input_mode": 0o640,
        "bystander": bystander,
        "bystander_content": b"do not touch",
        "bystander_mode": 0o644,
        "output_path": os.path.join(workdir, "document.bin.env"),
    }


def encrypt_args(fix):
    return ["encrypt", "--key", fix["key_path"],
            "--input", fix["input_path"], "--output", fix["output_path"]]


def assert_fixture_untouched(ctx, fix):
    """Key, input, and neighbouring files keep content and permissions."""
    ctx.check(read_bytes(fix["key_path"]) == fix["key"],
              "key file content changed")
    ctx.check(stat.S_IMODE(os.stat(fix["key_path"]).st_mode) ==
              fix["key_mode"], "key file permissions changed")
    ctx.check(read_bytes(fix["input_path"]) == fix["plaintext"],
              "input file content changed")
    ctx.check(stat.S_IMODE(os.stat(fix["input_path"]).st_mode) ==
              fix["input_mode"], "input file permissions changed")
    ctx.check(read_bytes(fix["bystander"]) == fix["bystander_content"],
              "neighbouring file content changed")
    ctx.check(stat.S_IMODE(os.stat(fix["bystander"]).st_mode) ==
              fix["bystander_mode"], "neighbouring file permissions changed")


def assert_no_secret_leak(ctx, fix, out, err):
    ctx.check(fix["key"] not in out, "key material leaked into stdout")
    ctx.check(fix["key"] not in err, "key material leaked into stderr")
    ctx.check(PLAINTEXT_MARKER not in out, "plaintext leaked into stdout")
    ctx.check(PLAINTEXT_MARKER not in err, "plaintext leaked into stderr")


def assert_save_failed_and_cleaned(ctx, fix, rc, out, err, stage):
    """Contract for a save failure after the output file was created:
    exit 1, empty stdout, exactly one stderr line naming the failed save
    stage and the output path, and the envelope this run created removed
    again (the path must not exist afterwards)."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on save failure: {out!r:.200}")
    expected = (f"envelopefile: failed {stage} envelope file "
                f"'{fix['output_path']}': "
                f"{os.strerror(errno.EIO)}\n").encode()
    ctx.check(err == expected,
              f"stderr must name the failed save stage and the output "
              f"path: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported despite the failed save")
    ctx.check(CLEANUP_WARNING_MARKER.encode() not in err,
              f"cleanup warning without a cleanup failure: {err!r:.200}")
    ctx.check(not os.path.lexists(fix["output_path"]),
              "the envelope this run created was left behind")
    assert_fixture_untouched(ctx, fix)
    assert_no_secret_leak(ctx, fix, out, err)


def test_save_write_failure_cleans_up(ctx, workdir):
    # The very first write of the sealed envelope fails: the run is a
    # failure and the file it created is removed again.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_FAIL_WRITE="1"),
                       use_testable=True)
    assert_save_failed_and_cleaned(ctx, fix, rc, out, err, "writing")


def test_save_partial_write_then_error_cleans_up(ctx, workdir):
    # Part of the envelope is already on disk when an unrecoverable write
    # error hits: still a failure, and the partial envelope is removed.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_WRITE_SCRIPT="10,eio"),
                       use_testable=True)
    assert_save_failed_and_cleaned(ctx, fix, rc, out, err, "writing")


def test_save_fsync_failure_cleans_up(ctx, workdir):
    # All bytes written, then fsync fails: durability is unconfirmed.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_FAIL_FSYNC="1"),
                       use_testable=True)
    assert_save_failed_and_cleaned(ctx, fix, rc, out, err, "syncing")


def test_save_close_failure_cleans_up(ctx, workdir):
    # close() reports failure after every envelope byte was written and
    # synced: durability could not be confirmed, so this is a save
    # failure — no success report, and the fully written envelope is
    # removed anyway.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_FAIL_CLOSE="1"),
                       use_testable=True)
    assert_save_failed_and_cleaned(ctx, fix, rc, out, err, "closing")


def assert_cleanup_failed_run(ctx, fix, rc, out, err, primary_line):
    """The original save failure stays the primary cause; the failed
    removal only adds a warning naming the output path and the removal
    reason, and the file this run created genuinely remains on disk."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty: {out!r:.200}")
    warning = (f"envelopefile: warning: {CLEANUP_WARNING_MARKER} "
               f"'{fix['output_path']}': "
               f"{os.strerror(errno.EIO)}\n").encode()
    ctx.check(err == primary_line + warning,
              f"stderr must keep the original save failure first and the "
              f"removal warning second: {err!r:.200}")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported despite the failed save")
    # The cleanup genuinely did not happen: the path this run created is
    # still there — the exact opposite of the cleaned-up cases, where the
    # path must not exist. A non-zero exit alone proves neither.
    ctx.check(os.path.lexists(fix["output_path"]),
              "new envelope disappeared even though its removal failed")
    assert_fixture_untouched(ctx, fix)
    assert_no_secret_leak(ctx, fix, out, err)


def test_save_failure_with_failed_cleanup_leaves_partial_file(ctx, workdir):
    # Ten envelope bytes reached disk, then an unrecoverable write error,
    # then the cleanup unlink fails too: the partial file remains and the
    # warning says an incomplete file may have been left behind.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_WRITE_SCRIPT="10,eio",
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    primary = (f"envelopefile: failed writing envelope file "
               f"'{fix['output_path']}': "
               f"{os.strerror(errno.EIO)}\n").encode()
    assert_cleanup_failed_run(ctx, fix, rc, out, err, primary)
    leftover = read_bytes(fix["output_path"])
    ctx.check(len(leftover) == 10,
              f"leftover partial envelope is {len(leftover)} bytes, "
              "expected the 10 written before the error")
    mode = stat.S_IMODE(os.stat(fix["output_path"]).st_mode)
    ctx.check(mode == ENVELOPE_MODE,
              f"leftover file mode is {oct(mode)}, expected "
              f"{oct(ENVELOPE_MODE)}")
    ctx.check(leftover not in out and leftover not in err,
              "partial envelope bytes leaked onto stdout/stderr")


def test_close_failure_with_failed_cleanup_leaves_full_file(ctx, workdir):
    # Every envelope byte was written and synced, close() failed, and the
    # removal failed as well: the run is still a save failure (exit 1, no
    # completion message) and the complete-but-unconfirmed file remains.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_FAIL_CLOSE="1",
                                           EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    primary = (f"envelopefile: failed closing envelope file "
               f"'{fix['output_path']}': "
               f"{os.strerror(errno.EIO)}\n").encode()
    assert_cleanup_failed_run(ctx, fix, rc, out, err, primary)
    leftover = read_bytes(fix["output_path"])
    ctx.check(len(leftover) == len(fix["plaintext"]) + ENVELOPE_OVERHEAD,
              f"leftover is {len(leftover)} bytes, expected the complete "
              f"{len(fix['plaintext']) + ENVELOPE_OVERHEAD}-byte envelope")


def test_save_success_unaffected_by_unlink_fault(ctx, workdir):
    # The unlink fault only applies to the cleanup of a failed save: a
    # normal run never unlinks anything, exits 0 with the completion
    # message naming the save path, and its envelope authenticates.
    fix = make_save_fixture(ctx, workdir)
    rc, out, err = run(ctx, encrypt_args(fix),
                       env_extra=fault_env(EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    ctx.check(rc == 0, f"encrypt exited {rc} (stderr: {err!r:.200})")
    ctx.check(err == b"", f"stderr not empty on success: {err!r:.200}")
    expected = (f"envelopefile: {SUCCESS_MARKER} "
                f"'{fix['output_path']}'\n").encode()
    ctx.check(out == expected,
              f"unexpected completion message: {out!r:.200}")
    blob = read_bytes(fix["output_path"])
    assert_envelope_format(ctx, blob, len(fix["plaintext"]))
    ctx.check(decrypt_envelope(fix["key"], blob) == fix["plaintext"],
              "envelope did not restore the original input")
    assert_fixture_untouched(ctx, fix)
    assert_no_secret_leak(ctx, fix, out, err)


# ---------------------------------------------------------------------------
# Pre-creation protection: an output path that already exists — or that
# names the input or key file — is refused before anything is created, and
# the refusal never enters the new-file removal path.
# ---------------------------------------------------------------------------

def assert_existing_path_refused(ctx, fix, rc, out, err, output_path):
    """Contract for refusing an already-existing output path: exit 1,
    empty stdout, exactly the "already exists" line naming the path —
    never a deletion-failure warning."""
    ctx.check(rc == 1, f"expected exit 1, got {rc} (stderr: {err!r:.200})")
    ctx.check(out == b"", f"stdout not empty on refusal: {out!r:.200}")
    expected = (f"envelopefile: {REFUSAL_MARKER}: {output_path}\n").encode()
    ctx.check(err == expected,
              f"refusal must report the existing path, got: {err!r:.200}")
    ctx.check(CLEANUP_WARNING_MARKER.encode() not in err,
              "a pre-creation refusal carried a cleanup warning")
    ctx.check(SUCCESS_MARKER.encode() not in out + err,
              "completion message reported for a refused run")
    assert_no_secret_leak(ctx, fix, out, err)


def test_output_existing_file_refused(ctx, workdir):
    fix = make_save_fixture(ctx, workdir)
    existing_content = b"pre-existing content that must survive\x00\x01"
    with open(fix["output_path"], "wb") as handle:
        handle.write(existing_content)
    os.chmod(fix["output_path"], 0o640)
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_existing_path_refused(ctx, fix, rc, out, err, fix["output_path"])
    ctx.check(read_bytes(fix["output_path"]) == existing_content,
              "existing output file content changed")
    mode = stat.S_IMODE(os.stat(fix["output_path"]).st_mode)
    ctx.check(mode == 0o640, f"existing output file mode changed to "
              f"{oct(mode)}")
    assert_fixture_untouched(ctx, fix)


def test_output_symlink_refused(ctx, workdir):
    fix = make_save_fixture(ctx, workdir)
    target = os.path.join(workdir, "target.bin")
    target_content = b"link target content must survive"
    with open(target, "wb") as handle:
        handle.write(target_content)
    os.chmod(target, 0o600)
    os.symlink(target, fix["output_path"])
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_existing_path_refused(ctx, fix, rc, out, err, fix["output_path"])
    ctx.check(os.path.islink(fix["output_path"]), "symlink was replaced")
    ctx.check(os.readlink(fix["output_path"]) == target,
              "symlink target changed")
    ctx.check(read_bytes(target) == target_content,
              "symlink target was modified")
    mode = stat.S_IMODE(os.stat(target).st_mode)
    ctx.check(mode == 0o600, f"symlink target mode changed to {oct(mode)}")
    assert_fixture_untouched(ctx, fix)


def test_output_dangling_symlink_refused(ctx, workdir):
    fix = make_save_fixture(ctx, workdir)
    target = os.path.join(workdir, "nowhere.bin")
    os.symlink(target, fix["output_path"])
    rc, out, err = run(ctx, encrypt_args(fix))
    assert_existing_path_refused(ctx, fix, rc, out, err, fix["output_path"])
    ctx.check(os.path.islink(fix["output_path"]),
              "dangling symlink was replaced")
    ctx.check(os.readlink(fix["output_path"]) == target,
              "dangling symlink target changed")
    ctx.check(not os.path.exists(target),
              "dangling symlink target was created")
    assert_fixture_untouched(ctx, fix)


def test_output_equal_to_input_refused(ctx, workdir):
    # --output naming the input file itself: the path exists, so the run
    # is refused with "already exists" before anything is written — the
    # input is neither overwritten nor removed. The cleanup fault is armed
    # on the testable build: if the refusal wrongly entered the new-file
    # removal path, the deletion-failure warning would appear here.
    fix = make_save_fixture(ctx, workdir)
    args = ["encrypt", "--key", fix["key_path"],
            "--input", fix["input_path"],
            "--output", fix["input_path"]]
    rc, out, err = run(ctx, args,
                       env_extra=fault_env(EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_existing_path_refused(ctx, fix, rc, out, err, fix["input_path"])
    assert_fixture_untouched(ctx, fix)


def test_output_equal_to_key_refused(ctx, workdir):
    # --output naming the key file itself: same rule, same "already
    # exists" reason, and the key file is left byte-for-byte untouched.
    fix = make_save_fixture(ctx, workdir)
    args = ["encrypt", "--key", fix["key_path"],
            "--input", fix["input_path"],
            "--output", fix["key_path"]]
    rc, out, err = run(ctx, args,
                       env_extra=fault_env(EF_TEST_FAIL_UNLINK="1"),
                       use_testable=True)
    assert_existing_path_refused(ctx, fix, rc, out, err, fix["key_path"])
    assert_fixture_untouched(ctx, fix)


ALL_TESTS = [
    test_decryptor_known_answers,
    test_roundtrip_text_with_newlines,
    test_roundtrip_mixed_binary,
    test_roundtrip_empty_input,
    test_roundtrip_large_file,
    test_roundtrip_unaligned_lengths,
    test_envelope_layout_and_fresh_nonce,
    test_header_fully_authenticated,
    test_ciphertext_tamper_detected,
    test_tag_tamper_detected,
    test_wrong_key_detected,
    test_truncated_tag_rejected,
    test_completion_message_and_preservation,
    test_save_write_failure_cleans_up,
    test_save_partial_write_then_error_cleans_up,
    test_save_fsync_failure_cleans_up,
    test_save_close_failure_cleans_up,
    test_save_failure_with_failed_cleanup_leaves_partial_file,
    test_close_failure_with_failed_cleanup_leaves_full_file,
    test_save_success_unaffected_by_unlink_fault,
    test_output_existing_file_refused,
    test_output_symlink_refused,
    test_output_dangling_symlink_refused,
    test_output_equal_to_input_refused,
    test_output_equal_to_key_refused,
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
