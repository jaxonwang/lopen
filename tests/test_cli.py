#!/usr/bin/env python3
"""Unit tests for the pure-function layer of the local management CLI (cli/lopen).

Run with:  python3 -m unittest  (from the repo root)
"""

import base64
import importlib.machinery
import importlib.util
import json
import os
import shlex
import tempfile
import unittest

# The CLI is a single-file script named `lopen` (no .py extension), so load it
# by path rather than by import name.
_CLI_PATH = os.path.join(os.path.dirname(__file__), "..", "cli", "lopen")
_spec = importlib.util.spec_from_loader(
    "lopen_cli", importlib.machinery.SourceFileLoader("lopen_cli", _CLI_PATH)
)
cli = importlib.util.module_from_spec(_spec)
_spec.loader.exec_module(cli)


class TestSplitSshHost(unittest.TestCase):
    def test_bare_alias(self):
        self.assertEqual(cli.split_ssh_host("mydev"), (None, "mydev"))

    def test_user_at_host(self):
        self.assertEqual(cli.split_ssh_host("alice@mydev"), ("alice", "mydev"))

    def test_user_at_fqdn(self):
        self.assertEqual(
            cli.split_ssh_host("jxwang@dev-dsk-1.us-west-2.amazon.com"),
            ("jxwang", "dev-dsk-1.us-west-2.amazon.com"),
        )

    def test_whitespace_trimmed(self):
        self.assertEqual(cli.split_ssh_host("  mydev \n"), (None, "mydev"))


class TestBuildRemoteConfig(unittest.TestCase):
    def test_bare_alias_stores_alias_as_host(self):
        text = cli.build_remote_config("mydev")
        self.assertIn("LOPEN_HOST=mydev\n", text)
        self.assertNotIn("LOPEN_USER", text)

    def test_user_at_host_splits(self):
        text = cli.build_remote_config("alice@mydev")
        self.assertIn("LOPEN_HOST=mydev\n", text)
        self.assertIn("LOPEN_USER=alice\n", text)

    def test_wait_timeout_included(self):
        text = cli.build_remote_config("mydev", wait_timeout=90)
        self.assertIn("LOPEN_WAIT_TIMEOUT=90\n", text)

    def test_only_key_value_lines(self):
        # Every non-comment line must be KEY=value (never sourced remotely).
        text = cli.build_remote_config("alice@mydev", wait_timeout=30)
        for line in text.splitlines():
            if line.startswith("#") or not line:
                continue
            self.assertRegex(line, r"^[A-Z_]+=.*$")

    def test_rejects_newline_injection(self):
        # A newline in the ssh-host must not smuggle extra config lines.
        for bad in ("mydev\nLOPEN_HOST=evil", "mydev\r\nX=1", "a\nb"):
            with self.assertRaises(cli.SshHostError):
                cli.build_remote_config(bad)

    def test_jump_emits_lopen_jump(self):
        text = cli.build_remote_config("mydev", jump="A,B")
        self.assertIn("LOPEN_JUMP=A,B\n", text)

    def test_jump_single_host(self):
        text = cli.build_remote_config("alice@mydev", jump="jump.example.com")
        self.assertIn("LOPEN_JUMP=jump.example.com\n", text)

    def test_jump_user_at_host_element(self):
        text = cli.build_remote_config("mydev", jump="bob@A,B")
        self.assertIn("LOPEN_JUMP=bob@A,B\n", text)

    def test_no_jump_when_absent(self):
        text = cli.build_remote_config("mydev")
        self.assertNotIn("LOPEN_JUMP", text)

    def test_jump_rejects_injection(self):
        # Newlines / bad charset in --via must not smuggle extra config lines.
        for bad in ("A\nLOPEN_HOST=evil", "A,-oProxyCommand=x", "A;rm", "A B",
                    "A\r\nX=1", "-A,B"):
            with self.assertRaises(cli.SshHostError):
                cli.build_remote_config("mydev", jump=bad)

    def test_jump_normalizes_whitespace_around_elements(self):
        # Each element is stripped by validate_ssh_host.
        text = cli.build_remote_config("mydev", jump=" A , B ")
        self.assertIn("LOPEN_JUMP=A,B\n", text)


class TestValidateSshHost(unittest.TestCase):
    def test_accepts_alias_and_user_at_host(self):
        self.assertEqual(cli.validate_ssh_host("mydev"), "mydev")
        self.assertEqual(
            cli.validate_ssh_host("alice@dev-dsk-1.us-west-2.amazon.com"),
            "alice@dev-dsk-1.us-west-2.amazon.com",
        )

    def test_strips_surrounding_whitespace(self):
        self.assertEqual(cli.validate_ssh_host("  mydev \n"), "mydev")

    def test_rejects_embedded_newline(self):
        for bad in ("a\nb", "a\r\nb", "mydev\nLOPEN_HOST=evil"):
            with self.assertRaises(cli.SshHostError):
                cli.validate_ssh_host(bad)

    def test_rejects_bad_charset(self):
        for bad in ("a b", "a;b", "a$b", "a`b`", "", "   "):
            with self.assertRaises(cli.SshHostError):
                cli.validate_ssh_host(bad)


class TestValidateJump(unittest.TestCase):
    def test_single_and_chain(self):
        self.assertEqual(cli.validate_jump("A"), "A")
        self.assertEqual(cli.validate_jump("A,B"), "A,B")
        self.assertEqual(cli.validate_jump("user@A,B"), "user@A,B")

    def test_strips_element_whitespace(self):
        self.assertEqual(cli.validate_jump(" A , B "), "A,B")

    def test_rejects_bad_elements(self):
        for bad in ("A,-B", "-A", "A B", "A;rm", "A\nB", "A,,B", ""):
            with self.assertRaises(cli.SshHostError):
                cli.validate_jump(bad)


class TestParseHostsFile(unittest.TestCase):
    def test_basic(self):
        text = "mydev\nalice@other\n"
        self.assertEqual(cli.parse_hosts_file(text), ["mydev", "alice@other"])

    def test_dedup_and_order_preserved(self):
        text = "mydev\nother\nmydev\n"
        self.assertEqual(cli.parse_hosts_file(text), ["mydev", "other"])

    def test_ignores_blank_and_comments(self):
        text = "# a comment\n\nmydev\n   \n# another\nother\n"
        self.assertEqual(cli.parse_hosts_file(text), ["mydev", "other"])

    def test_add_host_dedups(self):
        self.assertEqual(cli.add_host_to_text("mydev\n", "mydev"), "mydev\n")
        self.assertEqual(
            cli.add_host_to_text("mydev\n", "other"), "mydev\nother\n"
        )
        self.assertEqual(cli.add_host_to_text("", "first"), "first\n")


class TestDecodeLopen1(unittest.TestCase):
    def _make(self, msg):
        b64 = base64.b64encode(
            json.dumps(msg, separators=(",", ":")).encode()
        ).decode()
        return "LOPEN1:" + b64

    def test_roundtrip(self):
        msg = {"v": 1, "host": "h", "path": "/x"}
        decoded = cli.decode_lopen1(self._make(msg) + "\n")
        self.assertEqual(decoded["host"], "h")

    def test_rejects_non_lopen(self):
        with self.assertRaises(ValueError):
            cli.decode_lopen1("random text")

    def test_tolerates_internal_whitespace(self):
        s = self._make({"v": 1, "host": "h"})
        body = s[len("LOPEN1:"):]
        mangled = "LOPEN1:" + body[:5] + "\n" + body[5:]
        self.assertEqual(cli.decode_lopen1(mangled)["host"], "h")


class TestFindFileLibexec(unittest.TestCase):
    def test_lopen_libexec_wins(self):
        # A fake lopend.py in $LOPEN_LIBEXEC must be found first.
        with tempfile.TemporaryDirectory() as tmp:
            fake = os.path.join(tmp, "lopend.py")
            with open(fake, "w", encoding="utf-8") as fh:
                fh.write("# fake\n")
            old = os.environ.get("LOPEN_LIBEXEC")
            os.environ["LOPEN_LIBEXEC"] = tmp
            try:
                self.assertEqual(cli.find_lopend(), os.path.abspath(fake))
            finally:
                if old is None:
                    del os.environ["LOPEN_LIBEXEC"]
                else:
                    os.environ["LOPEN_LIBEXEC"] = old


class TestDetectServiceMode(unittest.TestCase):
    def test_brew_when_libexec_env(self):
        self.assertEqual(
            cli.detect_service_mode("/anywhere", {"LOPEN_LIBEXEC": "/x"}), "brew"
        )

    def test_brew_when_script_under_homebrew_prefix(self):
        self.assertEqual(
            cli.detect_service_mode(
                "/opt/homebrew/bin", {"HOMEBREW_PREFIX": "/opt/homebrew"}
            ),
            "brew",
        )

    def test_brew_when_in_cellar(self):
        self.assertEqual(
            cli.detect_service_mode(
                "/opt/homebrew/Cellar/lopen/0.1.0/libexec", {}
            ),
            "brew",
        )

    def test_launchd_otherwise(self):
        self.assertEqual(
            cli.detect_service_mode("/Users/me/.lopen", {}), "launchd"
        )
        # HOMEBREW_PREFIX set but script dir NOT under it -> launchd.
        self.assertEqual(
            cli.detect_service_mode(
                "/Users/me/.lopen", {"HOMEBREW_PREFIX": "/opt/homebrew"}
            ),
            "launchd",
        )


class TestResolveLogFile(unittest.TestCase):
    def test_lopen_log_env_wins(self):
        self.assertEqual(
            cli.resolve_log_file({"LOPEN_LOG": "/tmp/x.log"}, "brew"),
            "/tmp/x.log",
        )
        self.assertEqual(
            cli.resolve_log_file({"LOPEN_LOG": "/tmp/x.log"}, "launchd"),
            "/tmp/x.log",
        )

    def test_brew_uses_homebrew_prefix_var_log(self):
        self.assertEqual(
            cli.resolve_log_file({"HOMEBREW_PREFIX": "/opt/homebrew"}, "brew"),
            "/opt/homebrew/var/log/lopen.log",
        )

    def test_brew_default_prefix(self):
        self.assertEqual(
            cli.resolve_log_file({}, "brew"), "/opt/homebrew/var/log/lopen.log"
        )

    def test_launchd_uses_lopen_home(self):
        self.assertEqual(
            cli.resolve_log_file({}, "launchd"),
            os.path.join(cli.LOPEN_HOME, "lopend.log"),
        )


class TestShqPath(unittest.TestCase):
    def test_tilde_home_preserved(self):
        self.assertEqual(cli._shq_path("~/bin"), "~/bin")

    def test_tilde_home_nested_preserved(self):
        self.assertEqual(cli._shq_path("~/bin/lopen"), "~/bin/lopen")

    def test_tilde_user_preserved(self):
        self.assertEqual(cli._shq_path("~user/bin"), "~user/bin")

    def test_absolute_path_unquoted(self):
        # shlex.quote leaves a metachar-free absolute path unquoted.
        self.assertEqual(cli._shq_path("/abs/path"), "/abs/path")

    def test_tilde_with_space_quotes_body(self):
        # Tilde prefix preserved, but the space in the body must be quoted.
        result = cli._shq_path("~/my dir")
        self.assertTrue(result.startswith("~"))
        self.assertNotEqual(result, "~/my dir")
        self.assertEqual(result, "~" + shlex.quote("/my dir"))

    def test_injection_semicolon_fully_quoted(self):
        # "~;rm -rf x" does not match the safe tilde prefix (";" breaks the
        # charset and there's no following "/"), so it falls to full quoting;
        # the shell sees a literal, harmless string.
        result = cli._shq_path("~;rm -rf x")
        self.assertEqual(result, shlex.quote("~;rm -rf x"))
        # The ";" must be inside quotes (not exposed to the shell), so the
        # result is wrapped in single quotes rather than an unquoted prefix.
        self.assertTrue(result.startswith("'") and result.endswith("'"))

    def test_injection_command_substitution_fully_quoted(self):
        self.assertEqual(
            cli._shq_path("~$(whoami)/x"), shlex.quote("~$(whoami)/x")
        )

    def test_bare_tilde(self):
        # No slash, no rest -> rest-is-None branch, prefix returned as-is.
        self.assertEqual(cli._shq_path("~"), "~")

    def test_tilde_user_no_slash(self):
        # ~user with no path body -> rest-is-None branch.
        self.assertEqual(cli._shq_path("~user"), "~user")

    def test_backtick_body_quoted(self):
        result = cli._shq_path("~/foo`whoami`")
        self.assertTrue(result.startswith("~"))
        self.assertEqual(result, "~" + shlex.quote("/foo`whoami`"))

    def test_single_quote_body_quoted(self):
        result = cli._shq_path("~/'; evil")
        self.assertTrue(result.startswith("~"))
        self.assertEqual(result, "~" + shlex.quote("/'; evil"))

    def test_trailing_newline_falls_through(self):
        # \Z (not $) anchors true end-of-string, so a trailing newline must NOT
        # match the tilde prefix; it falls to full quoting and is preserved.
        self.assertEqual(cli._shq_path("~/x\n"), shlex.quote("~/x\n"))
        self.assertNotEqual(cli._shq_path("~/x\n"), "~/x\n")

    def test_embedded_newline_falls_through(self):
        self.assertEqual(cli._shq_path("~/x\nevil"), shlex.quote("~/x\nevil"))

    def test_space_in_prefix_falls_through(self):
        self.assertEqual(cli._shq_path("~foo bar"), shlex.quote("~foo bar"))


if __name__ == "__main__":
    unittest.main()
