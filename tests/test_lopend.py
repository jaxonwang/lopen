#!/usr/bin/env python3
"""Unit tests for the pure-function layer of lopend.

Run with:  python3 -m unittest  (from the repo root)
"""

import base64
import json
import os
import shlex
import sys
import unittest

# Make the lopend package importable regardless of CWD.
sys.path.insert(0, os.path.join(os.path.dirname(__file__), "..", "lopend"))

import lopend  # noqa: E402


def make_clipboard_string(msg):
    """Build a LOPEN1 clipboard string S from a message dict."""
    compact = json.dumps(msg, separators=(",", ":"))
    b64 = base64.b64encode(compact.encode("utf-8")).decode("ascii")
    return lopend.MAGIC_PREFIX + b64


VALID_MSG = {
    "v": 1,
    "host": "host.example.com",
    "user": "alice",
    "path": "/home/alice/report.txt",
    "name": "report.txt",
    "is_dir": False,
    "size": 1234,
    "mtime": 1700000000,
    "nonce": "0123456789abcdef",
}


class TestParseMessage(unittest.TestCase):
    def test_valid_roundtrip(self):
        s = make_clipboard_string(VALID_MSG)
        parsed = lopend.parse_message(s)
        self.assertEqual(parsed["path"], "/home/alice/report.txt")
        self.assertEqual(parsed["is_dir"], False)
        self.assertEqual(parsed["nonce"], "0123456789abcdef")

    def test_leading_trailing_whitespace_ok(self):
        s = "\n  " + make_clipboard_string(VALID_MSG) + "  \n"
        parsed = lopend.parse_message(s)
        self.assertEqual(parsed["user"], "alice")

    def test_not_a_lopen_message(self):
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message("just some random clipboard text")

    def test_empty(self):
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message("")

    def test_bad_base64(self):
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(lopend.MAGIC_PREFIX + "!!!not-base64!!!")

    def test_bad_json(self):
        b64 = base64.b64encode(b"not json").decode("ascii")
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(lopend.MAGIC_PREFIX + b64)

    def test_wrong_version(self):
        msg = dict(VALID_MSG, v=2)
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_missing_field(self):
        msg = dict(VALID_MSG)
        del msg["path"]
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_is_dir_must_be_bool(self):
        msg = dict(VALID_MSG, is_dir="yes")
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_unicode_path(self):
        msg = dict(VALID_MSG, path="/home/alice/rapport-été.txt",
                   name="rapport-été.txt")
        parsed = lopend.parse_message(make_clipboard_string(msg))
        self.assertEqual(parsed["name"], "rapport-été.txt")

    def test_reject_host_with_leading_dash(self):
        # Guards against ssh/scp option injection via the host field.
        msg = dict(VALID_MSG, host="-oProxyCommand=touch /tmp/pwned")
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_reject_host_with_special_chars(self):
        for bad in ("evil.com;rm -rf", "a@b", "a:b", "a b"):
            msg = dict(VALID_MSG, host=bad)
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(msg))

    def test_reject_user_with_leading_dash(self):
        msg = dict(VALID_MSG, user="-oProxyCommand=x")
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_reject_relative_path(self):
        msg = dict(VALID_MSG, path="relative/file.txt")
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_whitespace_inside_base64_is_tolerated(self):
        s = make_clipboard_string(VALID_MSG)
        body = s[len(lopend.MAGIC_PREFIX):]
        mangled = lopend.MAGIC_PREFIX + body[:8] + "\n " + body[8:]
        parsed = lopend.parse_message(mangled)
        self.assertEqual(parsed["path"], "/home/alice/report.txt")

    def test_host_leading_dash_variants_rejected(self):
        # Explicit coverage for the tightened leading-dash rule (both fields).
        for bad in ("-x", "-oProxyCommand=x", "-", "-abc"):
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(dict(VALID_MSG, host=bad)))
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(dict(VALID_MSG, user=bad)))

    def test_host_user_internal_dash_allowed(self):
        msg = dict(VALID_MSG, host="dev-dsk-1.example.com", user="a-b_c")
        parsed = lopend.parse_message(make_clipboard_string(msg))
        self.assertEqual(parsed["host"], "dev-dsk-1.example.com")

    def test_wait_field_missing_defaults_false(self):
        parsed = lopend.parse_message(make_clipboard_string(VALID_MSG))
        self.assertIn("wait", parsed)
        self.assertFalse(parsed["wait"])

    def test_wait_field_true_preserved(self):
        parsed = lopend.parse_message(make_clipboard_string(dict(VALID_MSG, wait=True)))
        self.assertTrue(parsed["wait"])

    def test_wait_field_non_bool_rejected(self):
        for bad in ("yes", 1, 0, None):
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(dict(VALID_MSG, wait=bad)))


class TestMessageIdAndDedup(unittest.TestCase):
    def test_message_id_depends_on_nonce_and_path(self):
        a = dict(VALID_MSG)
        b = dict(VALID_MSG, nonce="ffffffffffffffff")
        c = dict(VALID_MSG, path="/other/file.txt")
        self.assertNotEqual(lopend.message_id(a), lopend.message_id(b))
        self.assertNotEqual(lopend.message_id(a), lopend.message_id(c))
        self.assertEqual(lopend.message_id(a), lopend.message_id(dict(VALID_MSG)))

    def test_dedup_processes_once(self):
        import tempfile
        with tempfile.TemporaryDirectory() as root:
            d = lopend.Lopend(
                interval=0.5,
                tmp_dir=os.path.join(root, "tmp"),
                ssh_opts=[],
                log_path=os.path.join(root, "log"),
            )

            def fake_run(*a, **k):
                class R:
                    returncode = 0
                    stdout = b""
                    stderr = b""

                return R()

            original_run = lopend.subprocess.run
            lopend.subprocess.run = fake_run
            try:
                s = make_clipboard_string(VALID_MSG)
                first = d.handle_value(s)
                second = d.handle_value(s)
                self.assertTrue(first)
                self.assertFalse(second)  # deduped
            finally:
                lopend.subprocess.run = original_run


class TestBuildScpArgs(unittest.TestCase):
    def test_simple_file(self):
        args = lopend.build_scp_args(VALID_MSG, "/dest/dir")
        self.assertEqual(args[0], "scp")
        self.assertIn("-p", args)
        self.assertNotIn("-r", args)
        self.assertEqual(
            args[-2], "alice@host.example.com:/home/alice/report.txt"
        )
        self.assertEqual(args[-1], "/dest/dir")

    def test_directory_uses_recursive(self):
        msg = dict(VALID_MSG, is_dir=True)
        args = lopend.build_scp_args(msg, "/dest/dir")
        self.assertIn("-r", args)

    def test_path_with_spaces_is_quoted(self):
        msg = dict(
            VALID_MSG,
            path="/home/alice/my report (final).txt",
            name="my report (final).txt",
        )
        args = lopend.build_scp_args(msg, "/dest/dir")
        remote_spec = args[-2]
        # The remote path portion must be shell-quoted for the remote shell.
        self.assertEqual(
            remote_spec,
            "alice@host.example.com:'/home/alice/my report (final).txt'",
        )
        # The whole thing is still ONE argv element (no local shell involved).
        self.assertEqual(len(args), len(lopend.build_scp_args(VALID_MSG, "/dest/dir")))

    def test_path_with_special_chars_is_quoted(self):
        msg = dict(VALID_MSG, path="/tmp/a$b`c;d.txt", name="a$b`c;d.txt")
        args = lopend.build_scp_args(msg, "/dest")
        self.assertEqual(
            args[-2], "alice@host.example.com:'/tmp/a$b`c;d.txt'"
        )

    def test_ssh_opts_included(self):
        args = lopend.build_scp_args(
            VALID_MSG, "/dest", ssh_opts=["-i", "/home/me/.ssh/id"]
        )
        self.assertIn("-i", args)
        self.assertIn("/home/me/.ssh/id", args)

    def test_end_of_options_terminator_before_remote_spec(self):
        args = lopend.build_scp_args(VALID_MSG, "/dest")
        # "--" must appear immediately before the remote spec (which is [-2]).
        self.assertEqual(args[-3], "--")
        self.assertIn("@", args[-2])

    def test_terminator_after_ssh_opts_and_recursive(self):
        msg = dict(VALID_MSG, is_dir=True)
        args = lopend.build_scp_args(msg, "/dest", ssh_opts=["-i", "/k"])
        idx = args.index("--")
        # -r and the ssh opts all come before the terminator.
        self.assertLess(args.index("-r"), idx)
        self.assertLess(args.index("-i"), idx)


class TestBuildSignalArgs(unittest.TestCase):
    # ssh space-JOINS every remote operand into one string that the remote login
    # shell re-parses. So the remote command MUST be a single final argv element.
    # A `bash -c <cmd> _ <status>` split would be re-parsed remotely and break
    # (this is exactly the "mkdir: missing operand" bug). These tests lock in the
    # single-final-element structure so nobody reintroduces the split.

    def test_structure_ends_with_destination_then_single_command(self):
        args = lopend.build_signal_args(VALID_MSG, "ok")
        self.assertEqual(args[0], "ssh")
        self.assertIn("-o", args)
        self.assertIn("BatchMode=yes", args)
        # The last two elements are exactly: destination, then one command str.
        self.assertEqual(args[-2], "alice@host.example.com")
        cmd = args[-1]
        self.assertIsInstance(cmd, str)
        # There must be NO separate bash -c / _ / status elements.
        self.assertNotIn("bash", args)
        self.assertNotIn("_", args)
        # Destination is preceded by the -- end-of-options terminator.
        self.assertEqual(args[-3], "--")

    def test_command_contains_mkdir_nonce_and_atomic_move(self):
        cmd = lopend.build_signal_args(VALID_MSG, "ok")[-1]
        self.assertIn("mkdir -p ~/.lopen/signals", cmd)
        self.assertIn("0123456789abcdef", cmd)  # validated nonce, embedded
        self.assertIn(".part", cmd)             # atomic write
        self.assertIn("mv ~/.lopen/signals/0123456789abcdef.part", cmd)

    def test_status_is_shell_quoted_inline_not_separate_arg(self):
        # A nasty (server-controlled) status must be single-quoted INSIDE the one
        # command string, never a separate argv element.
        status = "error\nboom; rm -rf x"
        args = lopend.build_signal_args(VALID_MSG, status)
        cmd = args[-1]
        # The raw status is NOT its own argv element.
        self.assertNotIn(status, args[:-1])
        # It IS present, shell-quoted (single-quoted) inside the command string.
        self.assertIn(shlex.quote(status), cmd)
        self.assertIn("'error\nboom; rm -rf x'", cmd)
        # And crucially, re-parsing the command string with a shell lexer keeps
        # the dangerous text as ONE token (printf's operand), not commands.
        tokens = shlex.split(cmd, comments=False, posix=True)
        self.assertIn(status, tokens)
        # `rm` never appears as a standalone token (it's inside the quoted arg).
        self.assertNotIn("rm", tokens)

    def test_ok_status_quoted(self):
        cmd = lopend.build_signal_args(VALID_MSG, "ok")[-1]
        # printf %s 'ok' > ...
        self.assertIn("printf %s ok", cmd)

    def test_bad_nonce_rejected(self):
        for bad in ("has space", "a;b", "a/b", "../x", "a-b", "", "a.b"):
            msg = dict(VALID_MSG, nonce=bad)
            with self.assertRaises(lopend.MessageError):
                lopend.build_signal_args(msg, "ok")

    def test_ssh_opts_included_before_terminator(self):
        args = lopend.build_signal_args(VALID_MSG, "ok", ssh_opts=["-i", "/k"])
        self.assertIn("-i", args)
        self.assertLess(args.index("-i"), args.index("--"))


class TestSignalBackBehavior(unittest.TestCase):
    """Daemon signals back only in wait mode, with the right status."""

    def _run_with_fakes(self, msg, scp_ok=True, open_ok=True):
        import tempfile
        signals = []
        with tempfile.TemporaryDirectory() as root:
            d = lopend.Lopend(
                interval=0.5,
                tmp_dir=os.path.join(root, "tmp"),
                ssh_opts=[],
                log_path=os.path.join(root, "log"),
            )

            def fake_run(args, **k):
                class R:
                    returncode = 0
                    stdout = b""
                    stderr = b""
                if args and args[0] == "scp":
                    if not scp_ok:
                        R.returncode = 1
                        R.stderr = b"no such file"
                    else:
                        dest = args[-1]
                        entry = os.path.basename(args[-2].split(":")[-1].strip("'"))
                        open(os.path.join(dest, entry), "w").close()
                elif args and args[0] == "open" and not open_ok:
                    R.returncode = 1
                    R.stderr = b"open failed"
                elif args and args[0] == "ssh":
                    signals.append(args)
                return R()

            orig = lopend.subprocess.run
            lopend.subprocess.run = fake_run
            try:
                d.handle_value(make_clipboard_string(msg))
            finally:
                lopend.subprocess.run = orig
        return signals

    def test_no_signal_when_not_waiting(self):
        signals = self._run_with_fakes(dict(VALID_MSG))  # wait absent -> False
        self.assertEqual(signals, [])

    def test_signal_ok_on_success(self):
        signals = self._run_with_fakes(dict(VALID_MSG, wait=True))
        self.assertEqual(len(signals), 1)
        # The ssh call's final arg is the remote command string; the "ok" status
        # is shell-quoted inside it (printf %s ok > ...).
        cmd = signals[0][-1]
        self.assertIn("printf %s ok", cmd)

    def test_signal_error_on_scp_failure(self):
        signals = self._run_with_fakes(dict(VALID_MSG, wait=True), scp_ok=False)
        self.assertEqual(len(signals), 1)
        cmd = signals[0][-1]
        # error status is quoted inside the command (single-quoted, so it starts
        # with printf %s 'error...).
        self.assertIn("printf %s 'error", cmd)


class TestLocalTargetDerivation(unittest.TestCase):
    """The opened path must come from basename(path), not the forgeable name."""

    def test_open_target_ignores_forged_name(self):
        import tempfile
        with tempfile.TemporaryDirectory() as root:
            d = lopend.Lopend(
                interval=0.5,
                tmp_dir=os.path.join(root, "tmp"),
                ssh_opts=[],
                log_path=os.path.join(root, "log"),
            )
            opened = []

            def fake_run(args, **k):
                class R:
                    returncode = 0
                    stdout = b""
                    stderr = b""
                if args and args[0] == "scp":
                    # Simulate scp writing the file under basename(remote path).
                    dest = args[-1]
                    entry = os.path.basename(args[-2].split(":")[-1].strip("'"))
                    open(os.path.join(dest, entry), "w").close()
                elif args and args[0] == "open":
                    opened.append(args[1])
                return R()

            orig = lopend.subprocess.run
            lopend.subprocess.run = fake_run
            try:
                # name claims a traversal target; path is the real thing.
                msg = dict(
                    VALID_MSG,
                    path="/home/alice/report.txt",
                    name="../../../../Applications/Calculator.app",
                )
                d.handle_value(make_clipboard_string(msg))
            finally:
                lopend.subprocess.run = orig

            self.assertEqual(len(opened), 1)
            # Opened path must end in the real basename, and stay under tmp.
            self.assertTrue(opened[0].endswith("/report.txt"))
            self.assertNotIn("..", opened[0])


class TestBuildOpenArgs(unittest.TestCase):
    def test_open(self):
        self.assertEqual(
            lopend.build_open_args("/tmp/x/report.txt"),
            ["open", "/tmp/x/report.txt"],
        )


class TestLocalDestDir(unittest.TestCase):
    def test_contains_nonce_and_under_root(self):
        import datetime
        now = datetime.datetime(2026, 7, 3, 12, 0, 0)
        d = lopend.local_dest_dir(VALID_MSG, "/root/tmp", now=now)
        self.assertTrue(d.startswith("/root/tmp/20260703-120000-"))
        self.assertTrue(d.endswith("0123456789abcdef"))


if __name__ == "__main__":
    unittest.main()
