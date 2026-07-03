#!/bin/bash
#
# install-local.sh - install the lopen tooling on macOS.
#
# Installs BOTH local files under ~/.lopen/:
#   - lopend.py       the clipboard-polling daemon
#   - lopen           the management CLI (setup/doctor/install/logs/...)
#   - remote-lopen    a copy of the remote script, so `lopen setup` can ship it
# then symlinks the CLI onto your PATH and hands off to `lopen install` to
# generate + (re)load the launchd agent. Idempotent.
#
set -euo pipefail

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
LOPEN_HOME="$HOME/.lopen"

mkdir -p "$LOPEN_HOME" "$HOME/Library/LaunchAgents"

# --- copy the three local artifacts into ~/.lopen -----------------------------
cp "$SRC_DIR/lopend/lopend.py" "$LOPEN_HOME/lopend.py"
chmod +x "$LOPEN_HOME/lopend.py"
echo "Installed daemon      -> $LOPEN_HOME/lopend.py"

cp "$SRC_DIR/cli/lopen" "$LOPEN_HOME/lopen"
chmod +x "$LOPEN_HOME/lopen"
echo "Installed CLI         -> $LOPEN_HOME/lopen"

# The remote script, kept locally so `lopen setup` has something to scp out.
cp "$SRC_DIR/bin/lopen" "$LOPEN_HOME/remote-lopen"
chmod +x "$LOPEN_HOME/remote-lopen"
echo "Installed remote copy -> $LOPEN_HOME/remote-lopen"

# --- symlink the CLI onto PATH (sudo-free: prefer ~/bin) ----------------------
BIN_DIR="$HOME/bin"
mkdir -p "$BIN_DIR"
ln -sf "$LOPEN_HOME/lopen" "$BIN_DIR/lopen"
echo "Linked CLI            -> $BIN_DIR/lopen"

case ":$PATH:" in
*":$BIN_DIR:"*)
	PATH_OK=true
	;;
*)
	PATH_OK=false
	;;
esac

# --- install + load the daemon via the CLI (single source of truth) -----------
"$LOPEN_HOME/lopen" install

echo
if [ "$PATH_OK" != true ]; then
	echo "NOTE: $BIN_DIR is not on your PATH. Add this to your shell rc:"
	echo '  export PATH="$HOME/bin:$PATH"'
	echo "(then run 'lopen doctor'). For now, invoke it as $BIN_DIR/lopen."
	echo
fi
echo "Next: provision a remote host with"
echo "  lopen setup <ssh-host>        # e.g. lopen setup mydev  (or user@host)"
echo
echo "Diagnose anytime with:  lopen doctor"
echo "Tail logs with:         lopen logs -f   (or tail -f $LOPEN_HOME/lopend.log)"
echo "Uninstall with:         lopen uninstall (or $SRC_DIR/install/uninstall-local.sh)"
