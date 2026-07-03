#!/usr/bin/env python3
"""lopend - local macOS daemon for the lopen tool.

Polls the Mac clipboard (via `pbpaste`). When it sees a magic "LOPEN1:" message
(produced by the remote `lopen` script), it decodes the embedded JSON, scp's the
referenced file/dir from the remote host into a per-invocation temp dir, and runs
macOS `open` on the local copy.

Python 3 standard library only - no third-party dependencies.

The pure-function layer (parse_message, build_scp_args, build_open_args) is
importable and unit-testable without touching the clipboard.
"""

from __future__ import annotations

import argparse
import base64
import binascii
import datetime as _dt
import gzip
import io
import json
import os
import re
import shlex
import subprocess
import sys
import tarfile
import time

MAGIC_PREFIX = "LOPEN1:"
DEFAULT_INTERVAL = 0.5
DEFAULT_HOME = os.path.expanduser("~/.lopen")

# The clipboard is an untrusted trust boundary: anything that can write to it
# (including any host you ssh into) can hand us a LOPEN1 message. We therefore
# constrain the fields that end up as scp/ssh operands. host/user must NOT begin
# with '-' -- scp/ssh treat leading-dash operands as options, so a host like
# "-oProxyCommand=..." would be option injection. The first character must be
# alphanumeric/dot/underscore (no leading dash), and the remainder may also
# include '-'. No '@' or ':' anywhere.
_USER_RE = re.compile(r"^[A-Za-z0-9._][A-Za-z0-9._-]*$")
_HOST_RE = re.compile(r"^[A-Za-z0-9._][A-Za-z0-9._-]*$")

# Nonce is used in a fixed remote shell command (the signal-back write), so it
# must be strictly alphanumeric to be safe to interpolate.
_NONCE_RE = re.compile(r"^[A-Za-z0-9]+$")

# Each element of a jump chain (comma-separated) becomes an ssh/scp `-J` operand.
# Same anti-option-injection rule as host, but a leading `user@` is permitted
# (jump hosts are commonly written as user@host). No leading dash on either the
# user or host component, and no whitespace/CR/LF anywhere.
_JUMP_RE = re.compile(r"^(?:[A-Za-z0-9._][A-Za-z0-9._-]*@)?[A-Za-z0-9._][A-Za-z0-9._-]*$")

# Inline delivery encodings the daemon understands.
_INLINE_ENCODINGS = ("gz", "tgz")


# --------------------------------------------------------------------------- #
# Pure functions (unit-testable, no side effects)
# --------------------------------------------------------------------------- #
class MessageError(ValueError):
    """Raised when a clipboard value is not a valid LOPEN1 message."""


def parse_message(clipboard_value):
    """Parse a clipboard string into a validated LOPEN1 message dict.

    Returns the parsed/validated dict, or raises MessageError if the string is
    not a well-formed LOPEN1 message.

    The clipboard string S has the form:  "LOPEN1:" + base64(compact-JSON)
    """
    if not isinstance(clipboard_value, str):
        raise MessageError("clipboard value is not text")

    value = clipboard_value.strip()
    if not value.startswith(MAGIC_PREFIX):
        raise MessageError("not a LOPEN1 message")

    # Strip ALL whitespace from the base64 body, not just the ends: clipboard
    # managers and terminals occasionally inject line breaks or spaces. We then
    # decode leniently (validate=False) so a mildly-mangled-but-recoverable
    # payload still works; the strict JSON/version/field checks below remain the
    # real gate.
    b64 = "".join(value[len(MAGIC_PREFIX):].split())
    try:
        raw = base64.b64decode(b64)
    except (binascii.Error, ValueError) as exc:
        raise MessageError("invalid base64 payload: %s" % exc)

    try:
        msg = json.loads(raw.decode("utf-8"))
    except (UnicodeDecodeError, json.JSONDecodeError) as exc:
        raise MessageError("invalid JSON payload: %s" % exc)

    if not isinstance(msg, dict):
        raise MessageError("payload is not a JSON object")

    if msg.get("v") != 1:
        raise MessageError("unsupported protocol version: %r" % msg.get("v"))

    # Required string fields.
    for field in ("host", "user", "path", "name", "nonce"):
        if not isinstance(msg.get(field), str) or not msg.get(field):
            raise MessageError("missing or invalid field: %s" % field)

    if not isinstance(msg.get("is_dir"), bool):
        raise MessageError("missing or invalid field: is_dir")

    # Harden the fields that become scp/ssh operands or local filesystem paths.
    if not _USER_RE.fullmatch(msg["user"]):
        raise MessageError("invalid user: %r" % msg["user"])
    if not _HOST_RE.fullmatch(msg["host"]):
        raise MessageError("invalid host: %r" % msg["host"])
    # path must be absolute so we never resolve it against some local cwd.
    if not msg["path"].startswith("/"):
        raise MessageError("path is not absolute: %r" % msg["path"])

    # Optional "wait" field: if present it must be a bool. Missing -> False
    # (legacy non-blocking behaviour). We normalize it onto the dict so callers
    # can always read msg["wait"].
    if "wait" in msg:
        if not isinstance(msg["wait"], bool):
            raise MessageError("invalid wait field: %r" % msg["wait"])
    else:
        msg["wait"] = False

    # Optional inline-delivery fields: "enc" ("gz"|"tgz") + "content" (base64).
    # Both are optional; absent -> scp delivery (legacy behaviour). If either is
    # present we validate the pair loosely here and fully at decode time.
    if "enc" in msg and msg["enc"] is not None:
        if not isinstance(msg["enc"], str) or msg["enc"] not in _INLINE_ENCODINGS:
            raise MessageError("invalid enc field: %r" % msg.get("enc"))
    if "content" in msg and msg["content"] is not None:
        if not isinstance(msg["content"], str):
            raise MessageError("invalid content field: not a string")

    # Optional "jump" field: comma-separated ssh jump hosts for ProxyJump. Each
    # element must pass _JUMP_RE (no option injection, no newlines). Empty
    # elements are rejected. Missing -> no jump.
    if "jump" in msg and msg["jump"] is not None:
        jump = msg["jump"]
        if not isinstance(jump, str):
            raise MessageError("invalid jump field: not a string")
        if jump:
            for part in jump.split(","):
                if not _JUMP_RE.fullmatch(part):
                    raise MessageError("invalid jump host: %r" % part)

    return msg


def message_id(msg):
    """Stable dedup id for a parsed message.

    Uses the nonce (which the remote regenerates every invocation) combined with
    the path, so re-opening the same file always produces a new id.
    """
    return "%s\x00%s" % (msg.get("nonce", ""), msg.get("path", ""))


def delivery_mode(msg):
    """Return "inline" or "scp" for a parsed message (pure).

    Inline delivery is chosen iff the message carries both a non-empty inline
    encoding ("enc") AND non-empty inline "content". Otherwise the file is
    fetched by scp (the legacy path).
    """
    if msg.get("enc") and msg.get("content"):
        return "inline"
    return "scp"


def decode_inline_content(msg):
    """base64-decode the inline "content" field into raw bytes.

    Lenient like parse_message's payload decode: strips all whitespace from the
    base64 body before decoding (clipboard managers / terminals occasionally
    inject line breaks). Raises MessageError on a missing or undecodable field.
    """
    content = msg.get("content")
    if not isinstance(content, str) or not content:
        raise MessageError("no inline content to decode")
    b64 = "".join(content.split())
    try:
        return base64.b64decode(b64)
    except (binascii.Error, ValueError) as exc:
        raise MessageError("invalid inline content base64: %s" % exc)


def _is_within(base, target):
    """True iff `target` (a path) resolves to `base` or a descendant of it.

    Both are normalized (no symlink resolution, purely lexical) with os.path so
    a member like "a/../../etc" that escapes `base` is caught before any write.
    """
    base = os.path.abspath(base)
    target = os.path.abspath(target)
    # commonpath raises on differing drives / mixed abs-rel; treat that as unsafe.
    try:
        return os.path.commonpath([base, target]) == base
    except ValueError:
        return False


def safe_extract_tar(tar, dest):
    """Extract a tarfile.TarFile into `dest`, refusing path-traversal escapes.

    tarfile.extractall is unsafe: a member named "../evil" or "/etc/passwd", or a
    symlink/hardlink, can write outside `dest`. Two classes of escape:
      1. Lexical: a member name that resolves outside dest (e.g. "../x", "/etc").
      2. Symlink-then-write-through: an in-dest symlink extracted first, then a
         later member written *through* it to a real path outside dest. A purely
         lexical name check passes step 2 but the write follows the on-disk link.

    We defend against both:
      * Refuse symlink AND hardlink members outright (an inline tgz of a normal
        tree does not need them; refusing is strictly safer than trying to
        validate link targets, and closes the write-through vector entirely).
      * Skip device/fifo specials.
      * For every file/dir member, verify the REAL (symlink-resolved) parent
        directory is still within the REAL dest before extracting, so we never
        write through any pre-existing link.

    Returns the list of member names actually extracted. Mirrors the repo's
    basename-only caution: we never trust archive-supplied paths.
    """
    dest_abs = os.path.abspath(dest)
    real_dest = os.path.realpath(dest_abs)
    extracted = []
    for member in tar.getmembers():
        # Reject absolute paths and any member whose lexical destination escapes.
        member_path = os.path.join(dest_abs, member.name)
        if os.path.isabs(member.name) or not _is_within(dest_abs, member_path):
            continue
        # Refuse links (both sym and hard): they are the write-through vector.
        if member.issym() or member.islnk():
            continue
        if not (member.isfile() or member.isdir()):
            # Skip block/char devices, fifos, etc.
            continue
        # Ensure the (validated-in-dest) parent exists: tars don't always carry
        # explicit directory members, and tar.extract won't create them. We only
        # create dirs whose lexical path is inside dest (checked above via the
        # member's own path), so makedirs cannot itself escape.
        parent = os.path.dirname(member_path)
        if parent and not os.path.isdir(parent):
            os.makedirs(parent, exist_ok=True)
        # Defense-in-depth against symlink-then-write-through: verify the REAL
        # resolved parent is still under the REAL dest. Since we refuse all link
        # members no in-dest symlink should exist, but this also catches a dest
        # that is itself reached via links and any exotic ordering.
        real_parent = os.path.realpath(parent) if parent else real_dest
        try:
            if os.path.commonpath([real_dest, real_parent]) != real_dest:
                continue
        except ValueError:
            continue
        tar.extract(member, dest_abs)
        extracted.append(member.name)
    return extracted


def build_scp_args(msg, dest_dir, ssh_opts=None):
    """Build the argv list for the scp fetch.

    CRITICAL quoting note:
      scp runs the remote path through a shell on the remote sshd. So a path with
      spaces / special chars must be shell-quoted for that remote shell, then the
      whole "user@host:quoted-remote-path" becomes ONE argv element locally
      (argv passing means the LOCAL side needs no extra quoting - subprocess does
      not invoke a shell). We therefore shlex.quote ONLY the remote path portion.

    Returns a list suitable for subprocess.run(..., shell=False).
    """
    ssh_opts = ssh_opts or []

    user = msg["user"]
    host = msg["host"]
    remote_path = msg["path"]

    # Quote the remote path for the remote shell. e.g. "/a b/c" -> "'/a b/c'".
    quoted_remote = shlex.quote(remote_path)
    remote_spec = "%s@%s:%s" % (user, host, quoted_remote)

    args = ["scp", "-p", "-o", "BatchMode=yes"]
    if msg["is_dir"]:
        args.append("-r")
    # ProxyJump chain (validated in parse_message). Goes in the OPTIONS section,
    # before the `--` terminator, so it is never mistaken for an operand.
    jump = msg.get("jump")
    if jump:
        args.extend(["-J", jump])
    args.extend(ssh_opts)
    # End-of-options terminator: defense in depth so nothing after it can be
    # (mis)read as an scp option, even if validation upstream regressed.
    args.append("--")
    args.append(remote_spec)
    args.append(dest_dir)
    return args


def build_open_args(local_path):
    """Build the argv list for macOS `open`."""
    return ["open", local_path]


def build_signal_args(msg, status, ssh_opts=None):
    """Build the argv for the back-channel signal write to the remote host.

    After the daemon finishes handling a wait-mode message it ssh's back and
    atomically drops ~/.lopen/signals/<nonce> so the blocked remote `lopen`
    unblocks.

    CRITICAL ssh-flattening gotcha: ssh does NOT preserve argv boundaries for
    the remote command. It space-JOINS every operand after the destination into
    a single string and hands that to the remote LOGIN shell to re-parse. So you
    must NOT pass `bash -c <cmd> _ <status>` as separate argv elements - the
    remote shell would re-split them (e.g. `ssh host bash -c mkdir -p ...` sends
    the literal string "bash -c mkdir -p ...", so remotely `-c` grabs only
    `mkdir` and `-p` becomes $0 -> "mkdir: missing operand"). Instead the ENTIRE
    remote command must be ONE final argv element.

    `status` is server-controlled ("ok" or "error\\n<reason>"), never attacker
    text, so we shlex.quote it INLINE into that single command string (shlex
    quoting handles the embedded newline in the error form). The nonce is
    validated strictly alphanumeric and embedded directly.

    Raises MessageError if the nonce is not strictly alphanumeric (it originates
    from the untrusted message and is interpolated into the remote command).
    """
    ssh_opts = ssh_opts or []
    nonce = msg.get("nonce", "")
    if not _NONCE_RE.fullmatch(nonce):
        raise MessageError("refusing to signal: invalid nonce %r" % nonce)

    user = msg["user"]
    host = msg["host"]

    # Single remote command string. nonce is alphanumeric-validated so it is
    # safe to embed directly; status is server-controlled and shell-quoted
    # inline. Written atomically (.part then mv).
    qstatus = shlex.quote(status)
    remote_cmd = (
        "mkdir -p ~/.lopen/signals && "
        "printf %s {st} > ~/.lopen/signals/{n}.part && "
        "mv ~/.lopen/signals/{n}.part ~/.lopen/signals/{n}"
    ).format(st=qstatus, n=nonce)

    args = ["ssh", "-o", "BatchMode=yes", "-o", "ConnectTimeout=10"]
    # ProxyJump chain (validated in parse_message). Goes in the ssh OPTIONS,
    # before the destination and remote command -- NOT in the remote command
    # (which must stay the single final argv element; see docstring).
    jump = msg.get("jump")
    if jump:
        args.extend(["-J", jump])
    args.extend(ssh_opts)
    # The remote command MUST be the single final argv element (see docstring):
    # ssh space-joins remote operands, so any split here would be re-parsed.
    args.extend(["--", "%s@%s" % (user, host), remote_cmd])
    return args


def local_dest_dir(msg, tmp_root, now=None):
    """Compute the per-invocation temp directory path (does not create it)."""
    now = now or _dt.datetime.now()
    stamp = now.strftime("%Y%m%d-%H%M%S")
    # Nonce is hex from the remote; still sanitize to be safe as a dir name.
    nonce = "".join(c for c in msg.get("nonce", "") if c.isalnum())[:16] or "x"
    return os.path.join(tmp_root, "%s-%s" % (stamp, nonce))


# --------------------------------------------------------------------------- #
# Daemon (side-effecting)
# --------------------------------------------------------------------------- #
class Lopend:
    def __init__(self, interval, tmp_dir, ssh_opts, log_path):
        self.interval = interval
        self.tmp_dir = tmp_dir
        self.ssh_opts = ssh_opts
        self.log_path = log_path
        self._last_id = None
        os.makedirs(self.tmp_dir, exist_ok=True)
        os.makedirs(os.path.dirname(self.log_path), exist_ok=True)

    def log(self, level, message):
        line = "%s [%s] %s\n" % (
            _dt.datetime.now().isoformat(timespec="seconds"),
            level,
            message,
        )
        try:
            with open(self.log_path, "a", encoding="utf-8") as fh:
                fh.write(line)
        except OSError:
            pass
        # Also echo to stderr so launchd captures it / interactive runs show it.
        sys.stderr.write(line)
        sys.stderr.flush()

    def read_clipboard(self):
        """Return the current clipboard text, or None on failure."""
        try:
            proc = subprocess.run(
                ["pbpaste"],
                stdout=subprocess.PIPE,
                stderr=subprocess.DEVNULL,
                check=False,
            )
        except OSError as exc:
            self.log("ERROR", "pbpaste failed to launch: %s" % exc)
            return None
        if proc.returncode != 0:
            return None
        return proc.stdout.decode("utf-8", errors="replace")

    def handle_value(self, clipboard_value):
        """Process one clipboard value. Returns True if it triggered a fetch."""
        try:
            msg = parse_message(clipboard_value)
        except MessageError:
            # Not our message (or malformed). Silent - the clipboard has lots of
            # non-lopen content. We only log real processing errors below.
            return False

        mid = message_id(msg)
        if mid == self._last_id:
            return False  # Dedup: same message as last time.
        self._last_id = mid

        self.log(
            "INFO",
            "LOPEN1 %s@%s:%s (is_dir=%s wait=%s delivery=%s)"
            % (msg["user"], msg["host"], msg["path"], msg["is_dir"],
               msg["wait"], delivery_mode(msg)),
        )

        # Fetch + open. On any failure we record a reason so the back-channel
        # signal can report it. status is server-controlled text.
        error_reason = self._fetch_and_open(msg)

        if msg.get("wait"):
            if error_reason is None:
                self._signal_back(msg, "ok")
            else:
                self._signal_back(msg, "error\n%s" % error_reason)
        return True

    def _fetch_and_open(self, msg):
        """Materialize the target locally then `open` it. Returns None on
        success, else a short error reason string (used for the back-channel
        signal + logs). Delivery is inline (content embedded in the message) or
        scp (fetched over the network), per delivery_mode()."""
        dest = local_dest_dir(msg, self.tmp_dir)
        try:
            os.makedirs(dest, exist_ok=True)
        except OSError as exc:
            reason = "cannot create dest dir: %s" % exc
            self.log("ERROR", reason)
            return reason

        if delivery_mode(msg) == "inline":
            local_path, reason = self._materialize_inline(msg, dest)
            if reason is not None:
                return reason
            return self._open_path(local_path)

        return self._fetch_scp_and_open(msg, dest)

    def _materialize_inline(self, msg, dest):
        """Decode the inline content into `dest`. Returns (local_path, None) on
        success or (None, reason) on failure. No scp/ssh; works at any ssh depth.

        gz  -> gunzip to dest/<basename(path)>.
        tgz -> path-traversal-safe extract into dest; open dest/<basename(path)>.
        """
        enc = msg.get("enc")
        entry = os.path.basename(msg["path"].rstrip("/"))
        if entry in ("", "..", "."):
            reason = "refusing inline materialize: unsafe basename %r" % entry
            self.log("ERROR", reason)
            return None, reason
        try:
            raw = decode_inline_content(msg)
        except MessageError as exc:
            reason = "inline decode failed: %s" % exc
            self.log("ERROR", reason)
            return None, reason

        if enc == "gz":
            try:
                data = gzip.decompress(raw)
            except (OSError, EOFError) as exc:
                reason = "inline gunzip failed: %s" % exc
                self.log("ERROR", reason)
                return None, reason
            local_path = os.path.join(dest, entry) if entry else dest
            try:
                with open(local_path, "wb") as fh:
                    fh.write(data)
            except OSError as exc:
                reason = "cannot write inline file: %s" % exc
                self.log("ERROR", reason)
                return None, reason
            self.log("INFO", "inline gz materialized -> %s" % local_path)
            return local_path, None

        if enc == "tgz":
            try:
                with tarfile.open(fileobj=io.BytesIO(raw), mode="r:gz") as tar:
                    names = safe_extract_tar(tar, dest)
            except (tarfile.TarError, OSError, EOFError) as exc:
                reason = "inline tar extract failed: %s" % exc
                self.log("ERROR", reason)
                return None, reason
            self.log("INFO", "inline tgz extracted %d members -> %s"
                     % (len(names), dest))
            local_path = os.path.join(dest, entry) if entry else dest
            if not os.path.exists(local_path):
                local_path = dest
            return local_path, None

        # parse_message validates enc, but guard defensively.
        reason = "unknown inline enc: %r" % enc
        self.log("ERROR", reason)
        return None, reason

    def _open_path(self, local_path):
        """`open` a local path. Returns None on success, else a reason string."""
        open_args = build_open_args(local_path)
        self.log("INFO", "open: %s" % " ".join(shlex.quote(a) for a in open_args))
        try:
            proc = subprocess.run(
                open_args,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
        except OSError as exc:
            reason = "open failed to launch: %s" % exc
            self.log("ERROR", reason)
            return reason
        if proc.returncode != 0:
            detail = proc.stderr.decode("utf-8", "replace").strip()
            reason = "open exited %d: %s" % (proc.returncode, detail)
            self.log("ERROR", reason)
            return reason
        self.log("INFO", "opened %s" % local_path)
        return None

    def _fetch_scp_and_open(self, msg, dest):
        """scp the target into `dest` then `open` it. Returns None on success,
        else a short error reason string."""
        scp_args = build_scp_args(msg, dest, self.ssh_opts)
        self.log("INFO", "scp: %s" % " ".join(shlex.quote(a) for a in scp_args))
        try:
            proc = subprocess.run(
                scp_args,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
        except OSError as exc:
            reason = "scp failed to launch: %s" % exc
            self.log("ERROR", reason)
            return reason
        if proc.returncode != 0:
            detail = proc.stderr.decode("utf-8", "replace").strip()
            reason = "scp exited %d: %s" % (proc.returncode, detail)
            self.log("ERROR", reason)
            return reason

        # scp writes the entry under the basename of the REMOTE PATH (not the
        # separate, forgeable "name" field). Derive the local target from
        # basename(path) so a crafted "name" (e.g. containing "../") can't make
        # us `open` an arbitrary local path. os.path.basename strips any
        # directory components defensively.
        entry = os.path.basename(msg["path"].rstrip("/"))
        local_path = os.path.join(dest, entry) if entry else dest
        if not os.path.exists(local_path):
            # Fallback: open the whole dest dir (something unexpected happened).
            local_path = dest

        return self._open_path(local_path)

    def _signal_back(self, msg, status):
        """ssh back to the remote and drop the signal file. Failures are logged
        but never raised - a failed signal just means the remote will time out."""
        try:
            args = build_signal_args(msg, status, self.ssh_opts)
        except MessageError as exc:
            self.log("ERROR", "not signalling back: %s" % exc)
            return
        first_line = status.splitlines()[0] if status else status
        self.log("INFO", "signal-back status=%s -> %s@%s"
                 % (first_line, msg["user"], msg["host"]))
        try:
            proc = subprocess.run(
                args,
                stdout=subprocess.PIPE,
                stderr=subprocess.PIPE,
                check=False,
            )
        except OSError as exc:
            self.log("ERROR", "signal-back failed to launch: %s" % exc)
            return
        if proc.returncode != 0:
            self.log(
                "ERROR",
                "signal-back ssh exited %d: %s"
                % (proc.returncode, proc.stderr.decode("utf-8", "replace").strip()),
            )

    def run_once(self):
        value = self.read_clipboard()
        if value is None:
            return False
        return self.handle_value(value)

    def run_forever(self):
        self.log("INFO", "lopend started (interval=%.2fs tmp=%s)"
                 % (self.interval, self.tmp_dir))
        while True:
            try:
                self.run_once()
            except Exception as exc:  # never crash the daemon
                self.log("ERROR", "unexpected error: %s" % exc)
            time.sleep(self.interval)


# --------------------------------------------------------------------------- #
# CLI
# --------------------------------------------------------------------------- #
def _env_interval():
    """Read LOPEND_INTERVAL, falling back to DEFAULT_INTERVAL on any bad value.

    Evaluated at argparse-build time, so a non-numeric env var must NOT crash
    the process before the daemon starts.
    """
    raw = os.environ.get("LOPEND_INTERVAL")
    if raw is None:
        return DEFAULT_INTERVAL
    try:
        value = float(raw)
        if value <= 0:
            raise ValueError("must be positive")
        return value
    except (TypeError, ValueError):
        sys.stderr.write(
            "lopend: ignoring invalid LOPEND_INTERVAL=%r; using %s\n"
            % (raw, DEFAULT_INTERVAL)
        )
        return DEFAULT_INTERVAL


def parse_args(argv=None):
    parser = argparse.ArgumentParser(
        prog="lopend",
        description="Local macOS daemon for the lopen tool.",
    )
    parser.add_argument(
        "--interval",
        type=float,
        default=_env_interval(),
        help="Clipboard poll interval in seconds (default: %(default)s).",
    )
    parser.add_argument(
        "--tmp-dir",
        default=os.environ.get("LOPEND_TMP_DIR", os.path.join(DEFAULT_HOME, "tmp")),
        help="Root temp dir for fetched files (default: %(default)s).",
    )
    parser.add_argument(
        "--log-file",
        default=os.environ.get("LOPEND_LOG", os.path.join(DEFAULT_HOME, "lopend.log")),
        help="Log file path (default: %(default)s).",
    )
    parser.add_argument(
        "--ssh-opts",
        default=os.environ.get("LOPEND_SSH_OPTS", ""),
        help="Extra ssh/scp options, space-separated (e.g. '-i ~/.ssh/id_x').",
    )
    parser.add_argument(
        "--once",
        action="store_true",
        help="Process the current clipboard once and exit (for testing).",
    )
    return parser.parse_args(argv)


def main(argv=None):
    args = parse_args(argv)
    # shlex.split handles quoted ssh opts robustly; expand ~ in each token.
    ssh_opts = [os.path.expanduser(t) for t in shlex.split(args.ssh_opts)]

    daemon = Lopend(
        interval=args.interval,
        tmp_dir=os.path.expanduser(args.tmp_dir),
        ssh_opts=ssh_opts,
        log_path=os.path.expanduser(args.log_file),
    )

    if args.once:
        # Exit 0 if we processed a LOPEN1 message this run, 1 otherwise. Handy
        # for the testing workflow that --once is documented for.
        triggered = daemon.run_once()
        return 0 if triggered else 1
    try:
        daemon.run_forever()
    except KeyboardInterrupt:
        daemon.log("INFO", "lopend stopped")
    return 0


if __name__ == "__main__":
    sys.exit(main())
