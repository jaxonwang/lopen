# lopen

Open a **remote** file or directory on your **local Mac** — from any ssh
session — with a single command:

```sh
$ lopen report.pdf      # on the remote host
# ...report.pdf opens in Preview on your Mac.
```

`lopen` blocks until the file has actually opened locally, so it behaves like
a normal command (`-n` to fire and forget). It works over plain ssh, jump
hosts, deeply nested/chained ssh into private networks, tmux, **and mosh** —
because the mechanism never touches your interactive terminal session.

## Why it works everywhere (including mosh)

Most "open on my Mac from ssh" tools ride the terminal: they emit a terminal
escape sequence (OSC 52 / iTerm2 OSC 1337 / kitty transfer) that a local
terminal or daemon intercepts. That breaks under **mosh**, which is not a byte
pipe — it runs a server-side terminal emulator and synchronizes *screen
state*, silently dropping escape sequences it doesn't model. It also breaks
under per-session reverse tunnels, because mosh has no port forwarding at all.

`lopen` instead uses a signaling path that is **independent of your shell
session**:

```
┌─ your Mac ─────────────────────────────┐        ┌─ dev host ────────────────┐
│ lopend (launchd agent, always running) │        │                           │
│  • holds a persistent ssh -N -R reverse│  ssh   │  ~/.lopen/lopen.sock       │
│    UNIX-socket tunnel to each host  ────┼────────┼─►  (created by sshd)       │
│  • listens on a per-host local socket  │        │                           │
│  • receives the file bytes inline      │◄───────┼── lopen report.pdf         │
│  • writes ~/lopen/<host>/<path>        │        │    (sends request + bytes) │
│  • runs `open`, replies "opened" ──────┼────────┼─►  (unblocks)              │
└─────────────────────────────────────────┘        └───────────────────────────┘
```

Your Mac dials out and holds the tunnel; the remote never has to reach back to
your laptop (which corp VPN/NAT usually forbids anyway). Because the tunnel is
a separate connection from your mosh/tmux session, mosh on the first hop is
fully supported.

**Transfer is inline over ssh.** `lopen` streams the file's bytes back through
the forwarded socket, so the daemon never needs to `scp`/`rsync` *back* to the
host — it only needs the connection the Mac already opened. That's what makes
chained hops into unreachable private networks work.

## Recursive / chained ssh (private networks, no ProxyJump)

For `Mac → A → B → C` where C is only reachable from B, extend the socket one
hop at a time with the bundled `lssh` wrapper instead of `ssh` for inner hops:

```sh
# on your Mac: lopend already tunnels to A
lssh B      # from A — forwards the socket to B
lssh C      # from B — forwards it to C
lopen file  # on C — relays C→B→A→Mac; opens on your Mac
```

`lssh` is just `ssh` plus `-R ~/.lopen/lopen.sock:~/.lopen/lopen.sock` and a
stale-socket pre-clean. If you forget it on a hop, `lopen` fails fast telling
you which hop to reconnect. (Limitation: a *mid-chain* mosh hop can't carry
the forward — mosh as the **first** hop is fine, which is the common case.)

## Install

```sh
# On your Mac:
make darwin                 # builds dist/lopend-darwin-{arm64,amd64}
cp dist/lopend-darwin-$(uname -m | sed s/x86_64/amd64/) ~/bin/lopend

# Create ~/.config/lopen/config.json (see Configuration), then:
lopend install              # writes + loads the launchd agent, excludes the
                            # mirror from Time Machine, chmods it 0700
```

The remote `lopen` binary and `lssh` are pushed to each host by
`lopen setup <host>` (from your Mac); remote hosts need nothing preinstalled.
Everything is userland — **no admin/root on either side**.

## Configuration

`~/.config/lopen/config.json` on the Mac:

```json
{
  "hosts": [
    { "label": "devbox", "dest": "dev-dsk-me.us-west-2.amazon.com" },
    { "label": "prod-jump", "dest": "me@jump.example.com", "keep": true }
  ],
  "ttl_days": 7,
  "max_mirror_bytes": 2147483648,
  "max_payload_bytes": 524288000
}
```

- `label` — names the host locally; becomes the mirror subdirectory
  `~/lopen/<label>/…` and the per-host socket. A request may only claim the
  label of the tunnel it arrived on, so one host cannot masquerade as another.
- `dest` — the ssh destination (may be an `~/.ssh/config` alias).
- `keep` — pin this host's mirror so GC never evicts it.

Defaults: **7-day TTL, 2 GiB mirror cap, 500 MiB per-file cap.**

## The local mirror `~/lopen/` and retention

Fetched files land at a **stable path** `~/lopen/<label>/<absolute-remote-path>`
so re-opening the same file reuses the slot. The mirror is a **bounded
cache**:

- **TTL:** anything not opened in 7 days is evicted (tracked in an index, not
  filesystem atime).
- **Size cap:** if the mirror exceeds 2 GiB, least-recently-used entries are
  evicted until under the cap.
- GC runs at daemon start and once a day; empty directories are pruned.
- Excluded from Time Machine at install and `chmod 0700`.

### Overwrite semantics (opening the same path with a new version)

Re-opening a path whose content changed on the remote **overwrites the old
local copy in place** — there is no version history; you always get the
current remote version, staged and atomically swapped in.

The one guard: if you **edited the local copy** since it was synced (per the
index — sync is one-way, so that edit exists nowhere else), the overwrite is
**refused** and `lopen` tells you to re-run with `--force`. For a directory,
this guard covers edits to any file *inside* it, not just the directory
itself. This is a best-effort data-safety heuristic (mtime + size, like
rsync's default), not a security mechanism; `--force` always overrides.

## Security model

- **No network listeners** on either side — UNIX sockets only, mode 0600
  inside 0700 directories.
- Every request field is treated as **untrusted** (any host on the ssh chain
  can write to the forwarded socket). The daemon validates the protocol
  version, op, mode, origin label, and path; rejects non-absolute paths,
  control characters, and traversal; confines every write under
  `~/lopen/<label>/`.
- Tar streams for directories never materialize symlinks/hardlinks/devices,
  reject `..`/absolute entry names, and are bounded by a byte budget and
  entry-count cap so a small hostile archive can't fill your disk.
- Commands are invoked by **argv, never a shell**; the only value interpolated
  into a remote shell command (pull mode) is single-quoted. ssh destinations
  are validated and always follow `--`.
- **Trust statement:** you are trusting the chain of hosts you ssh through
  (each can read/inject on the forwarded socket — inherent to forwarding). The
  worst a compromised trusted host can do is make your Mac open a file *it*
  supplied, attributed to *its own* label. It cannot escape the mirror, run
  commands, or impersonate another host.

## Commands

| Command | Where | What |
|---|---|---|
| `lopen <path>` | remote | open a file/dir on your Mac (blocks) |
| `lopen -n <path>` | remote | fire and forget |
| `lopen -reveal <path>` | remote | reveal in Finder instead of opening |
| `lopen -a <App> <path>` | remote | open with a specific app |
| `lssh <ssh args>` | remote | ssh one hop deeper, extending the socket chain |
| `lopend install` | Mac | install/refresh the launchd agent |
| `lopen setup <host>` | Mac | enroll a host (push binary + config) |

## Requirements

- **Mac:** macOS with `ssh`, `open` (built in). No admin. Go only to build.
- **Remote:** any Linux/Unix with `ssh` and (for pull mode) `tar`/`cat`. The
  `lopen` binary is self-contained and pushed by `lopen setup`.
- **Connectivity:** passwordless ssh from your Mac to each enrolled host.

## License

See [LICENSE](LICENSE).
