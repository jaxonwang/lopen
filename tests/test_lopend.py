#!/usr/bin/env python3
"""Unit tests for the pure-function layer of lopend.

Run with:  python3 -m unittest  (from the repo root)
"""

import base64
import gzip
import io
import json
import os
import shlex
import sys
import tarfile
import tempfile
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

    def test_trailing_newline_rejected_in_user_host(self):
        # fullmatch (vs match) ensures trailing newlines don't pass validation.
        # (nonce is only validated in build_signal_args, not here)
        for field, value in [("host", "validhost\n"), ("user", "validuser\n")]:
            msg = dict(VALID_MSG, **{field: value})
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(msg))

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
        # fullmatch ensures trailing newlines are also rejected.
        for bad in ("has space", "a;b", "a/b", "../x", "a-b", "", "a.b", "abc\n"):
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


class TestInlineAndJumpParsing(unittest.TestCase):
    """parse_message accepts the new optional enc/content/jump fields and
    rejects malformed variants."""

    def _gz(self, data):
        return base64.b64encode(gzip.compress(data)).decode("ascii")

    def test_enc_gz_and_content_accepted(self):
        msg = dict(VALID_MSG, enc="gz", content=self._gz(b"hello"))
        parsed = lopend.parse_message(make_clipboard_string(msg))
        self.assertEqual(parsed["enc"], "gz")
        self.assertTrue(parsed["content"])

    def test_enc_tgz_accepted(self):
        msg = dict(VALID_MSG, is_dir=True, enc="tgz", content="Zm9v")
        parsed = lopend.parse_message(make_clipboard_string(msg))
        self.assertEqual(parsed["enc"], "tgz")

    def test_bad_enc_rejected(self):
        for bad in ("zip", "gzip", "", "xz"):
            msg = dict(VALID_MSG, enc=bad, content="Zm9v")
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(msg))

    def test_non_string_content_rejected(self):
        msg = dict(VALID_MSG, enc="gz", content=12345)
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))

    def test_jump_single_and_chain_accepted(self):
        for good in ("A", "A,B", "user@A,B", "a-b.example.com,c_d"):
            msg = dict(VALID_MSG, jump=good)
            parsed = lopend.parse_message(make_clipboard_string(msg))
            self.assertEqual(parsed["jump"], good)

    def test_jump_leading_dash_rejected(self):
        for bad in ("-oProxyCommand=x", "A,-B", "-A"):
            msg = dict(VALID_MSG, jump=bad)
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(msg))

    def test_jump_bad_chars_rejected(self):
        for bad in ("A B", "A;rm", "A:B", "A,,B"):
            msg = dict(VALID_MSG, jump=bad)
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(msg))

    def test_jump_newline_rejected(self):
        # A newline would let a jump host smuggle option injection / bad tokens.
        # fullmatch ensures trailing newlines are also rejected (re.match $ quirk).
        for bad in ("A\nB", "A\r\nB", "A\n", "B\n"):
            msg = dict(VALID_MSG, jump=bad)
            with self.assertRaises(lopend.MessageError):
                lopend.parse_message(make_clipboard_string(msg))

    def test_jump_non_string_rejected(self):
        msg = dict(VALID_MSG, jump=["A", "B"])
        with self.assertRaises(lopend.MessageError):
            lopend.parse_message(make_clipboard_string(msg))


class TestDeliveryMode(unittest.TestCase):
    def test_scp_when_no_enc(self):
        self.assertEqual(lopend.delivery_mode(dict(VALID_MSG)), "scp")

    def test_inline_when_enc_and_content(self):
        msg = dict(VALID_MSG, enc="gz", content="Zm9v")
        self.assertEqual(lopend.delivery_mode(msg), "inline")

    def test_scp_when_enc_but_empty_content(self):
        msg = dict(VALID_MSG, enc="gz", content="")
        self.assertEqual(lopend.delivery_mode(msg), "scp")

    def test_scp_when_content_but_no_enc(self):
        msg = dict(VALID_MSG, content="Zm9v")
        self.assertEqual(lopend.delivery_mode(msg), "scp")


class TestDecodeInlineContent(unittest.TestCase):
    def test_gzip_roundtrip(self):
        original = b"the quick brown fox\n" * 100
        content = base64.b64encode(gzip.compress(original)).decode("ascii")
        msg = dict(VALID_MSG, enc="gz", content=content)
        parsed = lopend.parse_message(make_clipboard_string(msg))
        raw = lopend.decode_inline_content(parsed)
        self.assertEqual(gzip.decompress(raw), original)

    def test_tolerates_internal_whitespace(self):
        content = base64.b64encode(b"hello world").decode("ascii")
        mangled = content[:4] + "\n " + content[4:]
        msg = dict(VALID_MSG, enc="gz", content=mangled)
        raw = lopend.decode_inline_content(msg)
        self.assertEqual(raw, b"hello world")

    def test_missing_content_raises(self):
        with self.assertRaises(lopend.MessageError):
            lopend.decode_inline_content(dict(VALID_MSG))

    def test_bad_base64_raises(self):
        msg = dict(VALID_MSG, enc="gz", content="!!!not base64!!!")
        with self.assertRaises(lopend.MessageError):
            lopend.decode_inline_content(msg)


class TestBuildScpArgsJump(unittest.TestCase):
    def test_jump_inserted_in_options_before_terminator(self):
        msg = dict(VALID_MSG, jump="A,B")
        args = lopend.build_scp_args(msg, "/dest")
        idx_j = args.index("-J")
        # -J is immediately followed by the chain string.
        self.assertEqual(args[idx_j + 1], "A,B")
        # -J and its value come BEFORE the -- terminator (i.e. in options).
        self.assertLess(idx_j, args.index("--"))
        self.assertLess(args.index("A,B"), args.index("--"))

    def test_no_jump_when_absent(self):
        args = lopend.build_scp_args(dict(VALID_MSG), "/dest")
        self.assertNotIn("-J", args)

    def test_dash_j_not_after_terminator(self):
        msg = dict(VALID_MSG, jump="A,B")
        args = lopend.build_scp_args(msg, "/dest")
        term = args.index("--")
        self.assertNotIn("-J", args[term + 1:])


class TestBuildSignalArgsJump(unittest.TestCase):
    def test_jump_inserted_in_ssh_options_before_terminator(self):
        msg = dict(VALID_MSG, jump="A,B")
        args = lopend.build_signal_args(msg, "ok")
        idx_j = args.index("-J")
        self.assertEqual(args[idx_j + 1], "A,B")
        self.assertLess(idx_j, args.index("--"))
        # The remote command is still the single final element (not -J-tainted).
        self.assertNotIn("-J", args[-1])

    def test_no_jump_when_absent(self):
        args = lopend.build_signal_args(dict(VALID_MSG), "ok")
        self.assertNotIn("-J", args)

    def test_dash_j_not_after_terminator(self):
        msg = dict(VALID_MSG, jump="A,B")
        args = lopend.build_signal_args(msg, "ok")
        term = args.index("--")
        self.assertNotIn("-J", args[term + 1:])


class TestSafeExtractTar(unittest.TestCase):
    """The tgz extractor must refuse path-traversal escapes."""

    def _extract(self, build_tar, check):
        """Build an in-memory tgz, safe-extract into a temp `dest`, then invoke
        `check(dest, names)` while the temp dir still exists."""
        with tempfile.TemporaryDirectory() as root:
            dest = os.path.join(root, "dest")
            os.makedirs(dest)
            buf = io.BytesIO()
            with tarfile.open(fileobj=buf, mode="w:gz") as tar:
                build_tar(tar)
            buf.seek(0)
            with tarfile.open(fileobj=buf, mode="r:gz") as tar:
                names = lopend.safe_extract_tar(tar, dest)
            check(dest, names)

    def _add_bytes(self, tar, name, data):
        info = tarfile.TarInfo(name=name)
        info.size = len(data)
        tar.addfile(info, io.BytesIO(data))

    def test_dotdot_member_is_refused(self):
        def build(tar):
            self._add_bytes(tar, "../evil", b"pwned")
            self._add_bytes(tar, "good/ok.txt", b"fine")

        def check(dest, names):
            # The traversal file must NOT exist outside dest.
            self.assertFalse(
                os.path.exists(os.path.join(os.path.dirname(dest), "evil")))
            # The safe member was extracted.
            self.assertTrue(os.path.exists(os.path.join(dest, "good", "ok.txt")))
            self.assertIn("good/ok.txt", names)
            self.assertNotIn("../evil", names)

        self._extract(build, check)

    def test_absolute_path_member_is_refused(self):
        def build(tar):
            self._add_bytes(tar, "/tmp/lopen-evil-test", b"pwned")

        def check(dest, names):
            self.assertFalse(os.path.exists("/tmp/lopen-evil-test"))
            self.assertEqual(names, [])

        self._extract(build, check)

    def test_symlink_escaping_dest_is_refused(self):
        def build(tar):
            info = tarfile.TarInfo(name="link")
            info.type = tarfile.SYMTYPE
            info.linkname = "../../etc/passwd"
            tar.addfile(info)

        def check(dest, names):
            self.assertFalse(os.path.lexists(os.path.join(dest, "link")))
            self.assertNotIn("link", names)

        self._extract(build, check)

    def test_normal_members_extracted(self):
        def build(tar):
            self._add_bytes(tar, "top/a.txt", b"a")
            self._add_bytes(tar, "top/sub/b.txt", b"b")

        def check(dest, names):
            self.assertTrue(os.path.exists(os.path.join(dest, "top", "a.txt")))
            self.assertTrue(
                os.path.exists(os.path.join(dest, "top", "sub", "b.txt")))

        self._extract(build, check)

    def test_hardlink_member_is_refused(self):
        def build(tar):
            self._add_bytes(tar, "real.txt", b"x")
            info = tarfile.TarInfo(name="hard")
            info.type = tarfile.LNKTYPE
            info.linkname = "real.txt"
            tar.addfile(info)

        def check(dest, names):
            # The regular file is fine; the hardlink member is refused.
            self.assertTrue(os.path.exists(os.path.join(dest, "real.txt")))
            self.assertFalse(os.path.lexists(os.path.join(dest, "hard")))
            self.assertNotIn("hard", names)

        self._extract(build, check)

    def test_symlink_then_write_through_is_refused(self):
        # The classic escape: an in-dest symlink to a sibling dir, then a file
        # written "through" it. Both the symlink AND the write-through target
        # must be refused so nothing lands outside dest.
        def build(tar):
            # symlink "link" -> "../outside" (lexically inside once joined? no:
            # this escapes, so it's refused lexically too). Use a link that stays
            # lexically inside dest but points at a real escaping location.
            info = tarfile.TarInfo(name="link")
            info.type = tarfile.SYMTYPE
            info.linkname = "sub"  # points at a sibling dir inside dest
            tar.addfile(info)
            # Now a file written through the (refused) link.
            self._add_bytes(tar, "link/pwned.txt", b"pwned")

        def check(dest, names):
            # The symlink member itself is refused (never in the extracted set).
            self.assertNotIn("link", names)
            link_path = os.path.join(dest, "link")
            # If "link" exists at all it must be a plain directory (created as the
            # parent of the write-through member), NEVER a symlink.
            if os.path.lexists(link_path):
                self.assertFalse(os.path.islink(link_path))
            # Crucially: every extracted member's real path stays inside dest.
            real_dest = os.path.realpath(dest)
            for name in names:
                p = os.path.realpath(os.path.join(dest, name))
                self.assertEqual(os.path.commonpath([real_dest, p]), real_dest)

        self._extract(build, check)


class TestInlineFetchAndOpen(unittest.TestCase):
    """End-to-end (minus `open`) inline materialization inside the daemon."""

    def _run(self, msg, check):
        """Run inline delivery and invoke `check(reason, opened)` while the temp
        dir still exists (TemporaryDirectory is torn down on block exit)."""
        opened = []
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
                if args and args[0] == "open":
                    opened.append(args[1])
                elif args and args[0] == "scp":
                    R.returncode = 1
                    R.stderr = b"scp should not be called for inline"
                return R()

            orig = lopend.subprocess.run
            lopend.subprocess.run = fake_run
            try:
                reason = d._fetch_and_open(lopend.parse_message(
                    make_clipboard_string(msg)))
            finally:
                lopend.subprocess.run = orig
            check(reason, opened)

    def test_inline_gz_materializes_and_opens_file(self):
        original = b"inline file contents\n"
        content = base64.b64encode(gzip.compress(original)).decode("ascii")
        msg = dict(VALID_MSG, path="/home/alice/report.txt",
                   name="report.txt", enc="gz", content=content)

        def check(reason, opened):
            self.assertIsNone(reason)
            self.assertEqual(len(opened), 1)
            self.assertTrue(opened[0].endswith("/report.txt"))
            with open(opened[0], "rb") as fh:
                self.assertEqual(fh.read(), original)

        self._run(msg, check)

    def test_inline_tgz_materializes_and_opens_dir(self):
        buf = io.BytesIO()
        with tarfile.open(fileobj=buf, mode="w:gz") as tar:
            for name in ("mydir/", "mydir/inner.txt"):
                data = b"x" if not name.endswith("/") else b""
                info = tarfile.TarInfo(name=name)
                if name.endswith("/"):
                    info.type = tarfile.DIRTYPE
                    info.mode = 0o755
                else:
                    info.size = len(data)
                    info.mode = 0o644
                tar.addfile(info, io.BytesIO(data) if data else None)
        content = base64.b64encode(buf.getvalue()).decode("ascii")
        msg = dict(VALID_MSG, is_dir=True, path="/home/alice/mydir",
                   name="mydir", size=None, enc="tgz", content=content)

        def check(reason, opened):
            self.assertIsNone(reason)
            self.assertEqual(len(opened), 1)
            self.assertTrue(opened[0].endswith("/mydir"))
            self.assertTrue(os.path.exists(os.path.join(opened[0], "inner.txt")))

        self._run(msg, check)

    def test_inline_refuses_unsafe_basename(self):
        # Refuse "", "..", "." basenames to prevent directory escape attempts.
        for path, name in [("/..", ".."), ("/", "ROOT"), ("/.", ".")]:
            original = b"malicious content\n"
            content = base64.b64encode(gzip.compress(original)).decode("ascii")
            # name field must be non-empty to pass parse_message, but the basename
            # check in _materialize_inline uses os.path.basename(path.rstrip("/"))
            msg = dict(VALID_MSG, path=path, name=name or "x",
                       enc="gz", content=content)

            def check(reason, opened):
                self.assertIsNotNone(reason)
                self.assertIn("unsafe basename", reason)
                self.assertEqual(len(opened), 0)

            self._run(msg, check)


if __name__ == "__main__":
    unittest.main()
