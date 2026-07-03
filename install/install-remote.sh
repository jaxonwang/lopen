#!/bin/bash
#
# install-remote.sh - install the `lopen` script on the CURRENT (remote) host.
#
# Copies bin/lopen to ~/bin/lopen and makes it executable. Idempotent.
#
set -euo pipefail

SRC_DIR="$(cd "$(dirname "$0")/.." && pwd)"
SRC="$SRC_DIR/bin/lopen"
DEST_DIR="$HOME/bin"
DEST="$DEST_DIR/lopen"

if [ ! -f "$SRC" ]; then
	echo "error: cannot find $SRC" >&2
	echo "If installing via curl, run:" >&2
	echo "  mkdir -p ~/bin && curl -fsSL <RAW_URL>/bin/lopen -o ~/bin/lopen && chmod +x ~/bin/lopen" >&2
	exit 1
fi

mkdir -p "$DEST_DIR"
cp "$SRC" "$DEST"
chmod +x "$DEST"

echo "Installed lopen -> $DEST"

case ":$PATH:" in
*":$DEST_DIR:"*)
	echo "~/bin is already on your PATH."
	;;
*)
	echo
	echo "NOTE: ~/bin is not on your PATH. Add this to your shell rc file:"
	echo '  export PATH="$HOME/bin:$PATH"'
	;;
esac

echo
echo "Usage: lopen <path>   (from an ssh session, with lopend running on your Mac)"
