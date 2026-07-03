# lopen

Open a **remote** file or directory on your **local Mac** — straight from an
ssh session — with a single command:

```sh
$ lopen report.pdf      # on the remote host
# ...and report.pdf opens in Preview on your Mac.
```

No file server, no reverse tunnel, no browser. It rides your existing ssh
connection: the remote command writes a magic message to your Mac's clipboard
using an OSC 52 terminal escape, and a tiny local daemon notices it, `scp`s the
file down, and runs macOS `open` on the local copy. By default the remote
command then **blocks** until the local open finishes (a small back-channel over
ssh), so `lopen` behaves like a normal blocking command.

## Quickstart

```sh
# On your Mac, from the repo root:
./install/install-local.sh
lopen setup <ssh-host>          # e.g. lopen setup mydev
lopen doctor                    # confirm everything is ✓

# Then from an ssh session on that host:
lopen report.pdf                # opens on your Mac
```

## Requirements

- **Local:** macOS, with the built-in `python3`, `pbpaste`, `open`, `scp`, `ssh` (all present by default). No third-party packages — the daemon and CLI are Python 3 standard library only.
- **Remote:** any Linux/Unix host with `bash`, `base64`, and `coreutils` (`realpath`/`stat`). No runtime to install — `lopen` is a single self-contained bash script.
- **Connectivity:** passwordless ssh from your Mac to the remote host (the daemon uses `BatchMode=yes` for both the file `scp` and the blocking back-channel).
- **Terminal:** an OSC 52-capable terminal on your Mac (iTerm2, WezTerm, kitty, Alacritty, …). Terminal.app is NOT supported — see [Terminal requirements](#terminal-requirements).

## Architecture

```
  REMOTE (Amazon Linux, over ssh)            LOCAL (macOS)
  ┌───────────────────────────┐              ┌────────────────────────────┐
  │  lopen <path>             │              │  lopend (launchd daemon)    │
  │   • realpath + stat        │  OSC 52      │   • polls pbpaste every .5s │
  │   • build LOPEN1 JSON      │  clipboard   │   • sees "LOPEN1:..."       │
  │     (host/user from config)│  write       │   • parse + dedup by nonce  │
  │   • base64 x2 (see below)  │──────────────┼─▶ • scp <user>@<host>:path  │
  │   • ESC ] 52 ; c ; … BEL   │  (rides the  │        → ~/.lopen/tmp/…     │
  │   • block on signal file   │   ssh TTY)   │   • open <local copy>       │
  │      ~/.lopen/signals/<n> ◀─┼──────────────┼── • ssh back: write signal  │
  └───────────────────────────┘   ssh back    └────────────────────────────┘
```

Protocol version: **LOPEN1** (optional `wait` field enables the back-channel).

## Install

### Local (your Mac) — do this first

```sh
./install/install-local.sh
```

This installs three files under `~/.lopen/` (the `lopend` daemon, the `lopen`
management CLI, and a copy of the remote script), symlinks the CLI to
`~/bin/lopen`, and loads a launchd agent (`com.lopen.lopend`) that runs the
daemon now and on every login. If `~/bin` isn't on your `PATH`, the installer
tells you how to add it.

### Remote (the Linux host you ssh into) — via `lopen setup`

The **primary** way to provision the remote side is from your Mac:

```sh
lopen setup <ssh-host>        # e.g.  lopen setup mydev   (or  user@host)
```

`<ssh-host>` is the **exact** string you ssh with (an `ssh_config` alias or
`user@host`). `setup` scp's the remote `lopen` into `~/bin` on that host,
writes `~/.lopen/config` there with `LOPEN_HOST` set to the host the daemon must
scp back from (solving "what hostname does the remote embed?"), records the host
locally, verifies end-to-end, and makes sure the local daemon is running.

Alternatively, provision the remote script manually with
`./install/install-remote.sh` (run on the remote host) — but then the embedded
host is just `hostname -f`, which may not be scp-reachable from your Mac.

### Uninstall

```sh
lopen uninstall            # or ./install/uninstall-local.sh
```

## Usage

From an ssh session on the remote host:

```sh
lopen path/to/file          # opens the file on your Mac; BLOCKS until it opens
lopen path/to/directory     # scp -r, then opens the folder in Finder
lopen --no-wait path/to/file  # fire and forget; don't wait for the open
lopen --print path/to/file  # print the raw clipboard string (testing); no wait
lopen --help
```

### Blocking behavior and `--no-wait`

By default `lopen` waits for the local daemon to confirm the file opened, then
exits `0` (success), `1` (the local open failed — reason printed), or `2`
(timed out waiting for the daemon). The wait timeout defaults to 45s and is
configurable via `LOPEN_WAIT_TIMEOUT` (env or `~/.lopen/config`). Pass
`--no-wait` to skip the wait entirely (`--print` never waits).

The back-channel works by the daemon ssh-ing back to the remote and atomically
dropping `~/.lopen/signals/<nonce>`; the blocked `lopen` polls for that file.

### Managing the daemon from your Mac

```sh
lopen doctor           # checklist: tools, daemon, hosts reachable, remote install
lopen status           # is the launchd agent loaded?
lopen start|stop|restart
lopen logs -f          # tail ~/.lopen/lopend.log
lopen install          # refresh the daemon + launchd agent (idempotent)
```

`lopen doctor` verifies the local tools, whether the daemon is loaded, the last
log activity, and — for each configured host — reverse ssh reachability plus
whether the remote `lopen` is installed. It exits non-zero if a critical check
fails.

## How the OSC 52 double-encoding works

OSC 52 is the terminal escape that lets a program set the clipboard of the
terminal emulator (which, over ssh, is your Mac). The tricky bit is that the
payload is base64-encoded **twice**:

1. `lopen` builds a compact JSON blob and forms the clipboard string
   `S = "LOPEN1:" + base64(JSON)`. This `S` is what we want on the clipboard.
2. OSC 52 *itself* requires its clipboard argument to be base64. So we base64
   the whole of `S` again to get the escape payload:

   ```
   ESC ] 52 ; c ; base64(S) BEL
   ```

   The terminal base64-decodes that layer and puts exactly `S` on the clipboard.

`lopend` then reverses it: it reads `S`, strips `LOPEN1:`, base64-decodes the
remainder, and parses the JSON.

**tmux/screen passthrough:** multiplexers swallow OSC sequences unless wrapped in
a DCS passthrough. When `$TMUX` is set, `lopen` wraps the sequence as
`ESC P tmux; <inner-with-every-ESC-doubled> ESC \`. GNU screen gets an analogous
DCS wrapper (also with ESC doubled; best-effort, since screen's OSC 52 support
varies by version). This is handled automatically.

## Terminal requirements

- **iTerm2**: enable *Settings → General → Selection → "Applications in terminal
  may access clipboard"*. Works great, including inside tmux.
- **tmux / screen**: handled automatically via DCS passthrough (see above). For
  tmux you may also want `set -g set-clipboard on`.
- **Terminal.app**: does **not** support OSC 52 clipboard *writes*. lopen will
  not work there. Use iTerm2 (or another OSC 52-capable terminal).

## Troubleshooting

- **Nothing opens.** Check the daemon is running: `launchctl list | grep lopen`
  and `tail -f ~/.lopen/lopend.log`. Manually test one clipboard item:
  `python3 ~/.lopen/lopend.py --once`.
- **Clipboard not set.** Your terminal may not support OSC 52 writes (see
  Terminal.app note) or clipboard access is disabled in iTerm2 settings. Verify
  the remote side with `lopen --print <path>` — it should print `LOPEN1:...`.
- **scp fails / hangs.** The daemon uses `BatchMode=yes`, so it needs
  passwordless ssh from your Mac to the remote host. Test:
  `scp user@host:/some/file /tmp/`. Pass extra options via
  `LOPEND_SSH_OPTS` or `--ssh-opts` (e.g. a specific identity or jump host).
- **Wrong file / stale.** Each `lopen` run uses a fresh random nonce, so
  re-running always re-triggers. Fetched copies live under
  `~/.lopen/tmp/<timestamp>-<nonce>/`.

## Configuration

**Daemon (`lopend`, local)** — env vars or CLI flags:

| Env | Flag | Default | Meaning |
|-----|------|---------|---------|
| `LOPEND_INTERVAL` | `--interval` | `0.5` | Clipboard poll interval (s) |
| `LOPEND_TMP_DIR` | `--tmp-dir` | `~/.lopen/tmp` | Where fetched files land |
| `LOPEND_LOG` | `--log-file` | `~/.lopen/lopend.log` | Log file |
| `LOPEND_SSH_OPTS` | `--ssh-opts` | (none) | Extra ssh/scp options |
| — | `--once` | — | Process clipboard once and exit (testing) |

**Remote `lopen`** — `~/.lopen/config` (written by `lopen setup`; `KEY=value`
lines only, never sourced) or env:

| Key | Default | Meaning |
|-----|---------|---------|
| `LOPEN_HOST` | `hostname -f` | Host the daemon scp's back from |
| `LOPEN_USER` | `$USER` | User for the scp-back |
| `LOPEN_WAIT_TIMEOUT` | `45` | Seconds to wait for the local open |

## Security note

lopen turns a clipboard value into an **automatic `scp` + `open`** of an
arbitrary remote path. Anyone who can write `LOPEN1:…` to your Mac's clipboard —
including any remote host you ssh into — can cause your Mac to download and open
a file. Only use lopen with hosts you trust and control. The daemon uses
`BatchMode=yes`, so it can only pull from hosts you already have passwordless ssh
access to, but treat the clipboard as a trust boundary.

Hardening in the daemon: the `host`/`user`/`nonce` fields from the clipboard are
charset-validated before ever becoming `ssh`/`scp` operands (no leading dash, so
no option injection), scp/ssh argvs include a `--` end-of-options terminator, the
local file that gets `open`ed is derived from `basename(path)` (not the forgeable
`name` field), and the back-channel signal writes a fixed remote command with a
strictly alphanumeric nonce and a server-controlled status string.

**Fetched directories may contain symlinks that point outside the temp dir**
(e.g. into your home directory), and macOS `open` follows them. This is inherent
to launching fetched content — you are, by design, opening whatever the remote
path resolves to. Only `lopen` paths you trust.

## Development

Run the unit tests (pure functions in `lopend` and the CLI: message parsing,
dedup, scp/signal arg building, config/hosts helpers):

```sh
python3 -m unittest
```

## License

See [LICENSE](LICENSE).
