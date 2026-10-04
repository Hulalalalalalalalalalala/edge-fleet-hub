#!/usr/bin/env python3
"""Regression tests for `envelopefile keygen`.

Covers the contract of the key file's creation and failure handling:

* success leaves exactly 32 raw bytes, mode 0600 (even under a loose
  umask), exit code 0, and a completion message naming the save path --
  without the key material ever reaching stdout/stderr or this report;
* any pre-existing target (regular file, empty file, directory, symlink,
  dangling symlink) is refused without modifying it or its permissions;
* write/fsync/close failures after creation are reported on stderr with
  a non-zero exit and the partial key file is cleaned up, while a short
  write that later completes still yields a full 32-byte key.

The report printed by this script never contains key bytes.
"""

import argparse
import os
import stat
import subprocess
import sys
import tempfile

KEY_BYTES = 32
SUCCESS_MARKER = b"generated and saved to"

results = []  # (name, ok, detail); detail must never contain key material


def record(name, ok, detail=""):
    results.append((name, bool(ok), detail))


def run(binary, args, env_extra=None, umask=None):
    """Run the binary, capturing stdout/stderr as bytes."""
    env = dict(os.environ)
    if env_extra:
        env.update(env_extra)

    preexec = None
    if umask is not None:
        def set_umask():
            os.umask(umask)
        preexec = set_umask

    proc = subprocess.run(
        [binary, *args],
        stdout=subprocess.PIPE,
        stderr=subprocess.PIPE,
        env=env,
        preexec_fn=preexec,
    )
    return proc.returncode, proc.stdout, proc.stderr


def mode_of(path):
    return stat.S_IMODE(os.lstat(path).st_mode)


def no_key_leak(key_bytes, stdout, stderr):
    """Key material must not appear in either stream, raw or hex-encoded."""
    if not key_bytes:
        return False
    streams = (stdout, stderr)
    if any(key_bytes in s for s in streams):
        return False
    hex_form = key_bytes.hex().encode()
    return not any(hex_form in s for s in streams)


# ---------------------------------------------------------------- version/usage

def test_version(binary):
    rc, out, err = run(binary, ["--version"])
    ok = rc == 0 and out == b"envelopefile 0.1.0\n" and err == b""
    record("version: --version prints version and exits 0", ok,
           f"rc={rc} stdout={out!r} stderr={err!r}" if not ok else "")


def test_usage(binary):
    rc, out, err = run(binary, [])
    ok = (rc == 2 and out == b"" and b"Usage:" in err
          and b"keygen --output" in err)
    record("usage: bare invocation prints usage to stderr, exits 2", ok,
           f"rc={rc} stdout={out!r} stderr={err!r}" if not ok else "")


# ------------------------------------------------------------------- success

def check_success(binary, out_path, umask=None, label="success"):
    rc, out, err = run(binary, ["keygen", "--output", out_path], umask=umask)
    problems = []
    if rc != 0:
        problems.append(f"rc={rc}")
    if SUCCESS_MARKER not in out:
        problems.append("no completion message on stdout")
    if out_path.encode() not in out:
        problems.append("save path missing from stdout")
    if err != b"":
        problems.append(f"stderr not empty: {err!r}")
    key = b""
    if not os.path.isfile(out_path) or os.path.islink(out_path):
        problems.append("key file missing or not a regular file")
    else:
        key = open(out_path, "rb").read()
        if len(key) != KEY_BYTES:
            problems.append(f"key file is {len(key)} bytes, want {KEY_BYTES}")
        mode = mode_of(out_path)
        if mode != 0o600:
            problems.append(f"key file mode {oct(mode)}, want 0o600")
    if key and not no_key_leak(key, out, err):
        problems.append("key material leaked into stdout/stderr")
    record(f"{label}: 32 raw bytes, mode 0600, rc 0, path on stdout",
           not problems, "; ".join(problems))
    return key


def test_success(binary):
    with tempfile.TemporaryDirectory() as d:
        check_success(binary, os.path.join(d, "envelope.key"))


def test_success_loose_umask(binary):
    with tempfile.TemporaryDirectory() as d:
        check_success(binary, os.path.join(d, "loose.key"), umask=0o000,
                      label="success with umask 0000")


def test_success_path_with_spaces(binary):
    with tempfile.TemporaryDirectory() as d:
        sub = os.path.join(d, "dir with space")
        os.mkdir(sub)
        check_success(binary, os.path.join(sub, "my key file.key"),
                      label="success with spaces in path")


def test_two_runs_each_valid(binary):
    """Each run must yield a complete key; the random values are NOT part
    of the expected result, so the two keys are deliberately not compared."""
    with tempfile.TemporaryDirectory() as d:
        k1 = check_success(binary, os.path.join(d, "first.key"),
                           label="first of two runs")
        k2 = check_success(binary, os.path.join(d, "second.key"),
                           label="second of two runs")
        record("two runs: both keys are exactly 32 bytes (values unconstrained)",
               len(k1) == KEY_BYTES and len(k2) == KEY_BYTES)


# ------------------------------------------------------------------ rejection

def check_rejection(binary, out_path, verify_unchanged, label):
    """Run keygen against an occupied path and confirm refusal without
    any modification. verify_unchanged() performs the target-specific
    post-checks and returns a list of problems."""
    rc, out, err = run(binary, ["keygen", "--output", out_path])
    problems = []
    if rc == 0:
        problems.append("exit code is 0")
    if b"already exists" not in err:
        problems.append(f"stderr lacks refusal reason: {err!r}")
    if SUCCESS_MARKER in out or SUCCESS_MARKER in err:
        problems.append("success message emitted")
    problems.extend(verify_unchanged())
    record(label, not problems, "; ".join(problems))


def test_reject_existing_file(binary):
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "taken.key")
        content = bytes(range(64))
        with open(p, "wb") as f:
            f.write(content)
        os.chmod(p, 0o644)

        def verify():
            probs = []
            if open(p, "rb").read() != content:
                probs.append("existing file bytes changed")
            if mode_of(p) != 0o644:
                probs.append("existing file permissions changed")
            return probs

        check_rejection(binary, p, verify,
                        "reject: existing regular file kept intact")


def test_reject_existing_empty_file(binary):
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "empty.key")
        open(p, "wb").close()
        os.chmod(p, 0o640)

        def verify():
            probs = []
            if os.path.getsize(p) != 0:
                probs.append("zero-length file grew")
            if mode_of(p) != 0o640:
                probs.append("existing file permissions changed")
            return probs

        check_rejection(binary, p, verify,
                        "reject: existing zero-length file kept intact")


def test_reject_existing_directory(binary):
    with tempfile.TemporaryDirectory() as d:
        p = os.path.join(d, "taken.dir")
        os.mkdir(p)
        inner = os.path.join(p, "inner.txt")
        with open(inner, "wb") as f:
            f.write(b"keep me")
        os.chmod(p, 0o755)

        def verify():
            probs = []
            if not os.path.isdir(p):
                probs.append("directory vanished")
            elif os.listdir(p) != ["inner.txt"]:
                probs.append("directory contents changed")
            if os.path.isdir(p) and open(inner, "rb").read() != b"keep me":
                probs.append("file inside directory changed")
            if os.path.isdir(p) and mode_of(p) != 0o755:
                probs.append("directory permissions changed")
            return probs

        check_rejection(binary, p, verify,
                        "reject: existing directory and contents kept intact")


def test_reject_symlink_to_file(binary):
    with tempfile.TemporaryDirectory() as d:
        target = os.path.join(d, "real.key")
        content = b"original target bytes"
        with open(target, "wb") as f:
            f.write(content)
        os.chmod(target, 0o644)
        link = os.path.join(d, "link.key")
        os.symlink(target, link)

        def verify():
            probs = []
            if not os.path.islink(link):
                probs.append("symlink replaced")
            elif os.readlink(link) != target:
                probs.append("symlink retargeted")
            if open(target, "rb").read() != content:
                probs.append("link target bytes changed")
            if mode_of(target) != 0o644:
                probs.append("link target permissions changed")
            return probs

        check_rejection(binary, link, verify,
                        "reject: symlink to existing file, link and target intact")


def test_reject_dangling_symlink(binary):
    with tempfile.TemporaryDirectory() as d:
        missing = os.path.join(d, "missing.key")
        link = os.path.join(d, "dangling.key")
        os.symlink(missing, link)

        def verify():
            probs = []
            if not os.path.islink(link):
                probs.append("dangling symlink replaced")
            elif os.readlink(link) != missing:
                probs.append("dangling symlink retargeted")
            if os.path.exists(missing):
                probs.append("dangling target was created")
            return probs

        check_rejection(binary, link, verify,
                        "reject: dangling symlink, target not created")


# ------------------------------------------------------- post-create failures

def fault_env(fault_inject, out_path, **knobs):
    env = {
        "LD_PRELOAD": fault_inject,
        "FI_TARGET_PATH": out_path,
    }
    env.update({k: str(v) for k, v in knobs.items()})
    return env


def check_fault(binary, fault_inject, label, expect_reason, **knobs):
    """A failure after the file was created: non-zero exit, reason on
    stderr, no success message, partial key removed, neighbours intact."""
    with tempfile.TemporaryDirectory() as d:
        keep = os.path.join(d, "keep.txt")
        with open(keep, "wb") as f:
            f.write(b"do not touch")
        out_path = os.path.join(d, "envelope.key")
        rc, out, err = run(binary, ["keygen", "--output", out_path],
                           env_extra=fault_env(fault_inject, out_path, **knobs))
        problems = []
        if rc == 0:
            problems.append("exit code is 0 despite injected failure")
        if expect_reason not in err:
            problems.append(f"stderr lacks reason {expect_reason!r}: {err!r}")
        if SUCCESS_MARKER in out or SUCCESS_MARKER in err:
            problems.append("success message emitted despite failure")
        if os.path.lexists(out_path):
            problems.append("incomplete key file left behind")
        if open(keep, "rb").read() != b"do not touch":
            problems.append("unrelated file in directory changed")
        record(label, not problems, "; ".join(problems))
        return err


def test_write_failure(binary, fault_inject):
    check_fault(binary, fault_inject,
                "fault: write failure reported, partial key cleaned up",
                b"failed writing", FI_WRITE_FAIL="1")


def test_fsync_failure(binary, fault_inject):
    check_fault(binary, fault_inject,
                "fault: fsync failure reported, partial key cleaned up",
                b"failed syncing", FI_FSYNC_FAIL="1")


def test_close_failure(binary, fault_inject):
    check_fault(binary, fault_inject,
                "fault: close failure reported, partial key cleaned up",
                b"failed closing", FI_CLOSE_FAIL="1")


def test_partial_write_completes(binary, fault_inject):
    """A short first write that the program retries to completion must
    still produce a full, valid 32-byte key."""
    with tempfile.TemporaryDirectory() as d:
        out_path = os.path.join(d, "envelope.key")
        rc, out, err = run(
            binary, ["keygen", "--output", out_path],
            env_extra=fault_env(fault_inject, out_path, FI_WRITE_PARTIAL="10"))
        problems = []
        if rc != 0:
            problems.append(f"rc={rc}")
        if SUCCESS_MARKER not in out:
            problems.append("no completion message")
        if not os.path.isfile(out_path):
            problems.append("key file missing")
        else:
            data = open(out_path, "rb").read()
            if len(data) != KEY_BYTES:
                problems.append(f"key file is {len(data)} bytes, want {KEY_BYTES}")
            if not no_key_leak(data, out, err):
                problems.append("key material leaked")
        record("fault: short write retried to a complete 32-byte key",
               not problems, "; ".join(problems))


def test_error_messages_distinguishable(binary, fault_inject):
    """'Target already exists' and 'write failed after creation' must be
    distinguishable so the user knows whether to pick another path or to
    deal with a write fault."""
    with tempfile.TemporaryDirectory() as d:
        taken = os.path.join(d, "taken.key")
        open(taken, "wb").close()
        _, _, err_exists = run(binary, ["keygen", "--output", taken])

        out_path = os.path.join(d, "envelope.key")
        _, _, err_write = run(
            binary, ["keygen", "--output", out_path],
            env_extra=fault_env(fault_inject, out_path, FI_WRITE_FAIL="1"))

        ok = (b"already exists" in err_exists
              and b"failed writing" in err_write
              and err_exists != err_write)
        record("fault: exists-refusal and write-failure messages differ", ok,
               f"exists={err_exists!r} write={err_write!r}" if not ok else "")


# ---------------------------------------------------------------------- main

def main():
    parser = argparse.ArgumentParser(description=__doc__)
    parser.add_argument("--binary", required=True,
                        help="path to the envelopefile binary")
    parser.add_argument("--fault-inject", required=True,
                        help="path to the fault-injection LD_PRELOAD library")
    args = parser.parse_args()

    binary = os.path.abspath(args.binary)
    fault_inject = os.path.abspath(args.fault_inject)
    if not os.path.isfile(binary):
        sys.exit(f"binary not found: {binary}")
    if not os.path.isfile(fault_inject):
        sys.exit(f"fault-injection library not found: {fault_inject}")

    test_version(binary)
    test_usage(binary)
    test_success(binary)
    test_success_loose_umask(binary)
    test_success_path_with_spaces(binary)
    test_two_runs_each_valid(binary)
    test_reject_existing_file(binary)
    test_reject_existing_empty_file(binary)
    test_reject_existing_directory(binary)
    test_reject_symlink_to_file(binary)
    test_reject_dangling_symlink(binary)
    test_write_failure(binary, fault_inject)
    test_fsync_failure(binary, fault_inject)
    test_close_failure(binary, fault_inject)
    test_partial_write_completes(binary, fault_inject)
    test_error_messages_distinguishable(binary, fault_inject)

    width = max(len(name) for name, _, _ in results)
    failed = 0
    for name, ok, detail in results:
        status = "PASS" if ok else "FAIL"
        if not ok:
            failed += 1
        line = f"[{status}] {name.ljust(width)}"
        if detail:
            line += f"  -- {detail}"
        print(line)
    print(f"\n{len(results) - failed}/{len(results)} tests passed")
    sys.exit(1 if failed else 0)


if __name__ == "__main__":
    main()
