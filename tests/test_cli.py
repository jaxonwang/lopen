#!/usr/bin/env python3
"""Unit tests for the pure-function layer of the local management CLI (cli/lopen).

Run with:  python3 -m unittest  (from the repo root)
"""

import base64
import importlib.machinery
import importlib.util
import json
import os
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


if __name__ == "__main__":
    unittest.main()
