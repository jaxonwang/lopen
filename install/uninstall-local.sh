#!/bin/bash
#
# uninstall-local.sh - stop and remove the lopend launchd job.
#
# Leaves ~/.lopen/ (logs, tmp) in place so you don't lose anything. Idempotent.
#
set -euo pipefail

LABEL="com.lopen.lopend"
PLIST="$HOME/Library/LaunchAgents/$LABEL.plist"

if [ -f "$PLIST" ]; then
	launchctl unload "$PLIST" 2>/dev/null || true
	rm -f "$PLIST"
	echo "Unloaded and removed $PLIST"
else
	echo "No plist at $PLIST (already uninstalled)."
fi

echo
echo "Note: ~/.lopen/ (logs + temp files) was left in place. Remove it manually"
echo "if you want a full cleanup:  rm -rf ~/.lopen"
