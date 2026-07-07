//go:build unix

package daemon

import "path/filepath"

// controlPath returns the ControlMaster socket path for a host. Kept short:
// unix socket paths have a ~104-byte limit on macOS.
func controlPath(ctlDir, label string) string {
	return filepath.Join(ctlDir, label+".ctl")
}

// controlPathOpt formats the ControlPath option value. ssh parses a `-o
// key=value` argument like a config-file line and splits value on whitespace,
// so a path containing a space (the default macOS state dir lives under
// "~/Library/Application Support") must be double-quoted or ssh errors with
// "keyword controlpath extra arguments at end of line". The path cannot
// itself contain a double quote: it is derived from ctlDir + a validated label.
func controlPathOpt(ctlDir, label string) string {
	return `ControlPath="` + controlPath(ctlDir, label) + `"`
}

// masterMuxArgs make the persistent tunnel the ControlMaster.
//
// ControlPersist is deliberately NOT set: with it, ssh backgrounds the master
// and the foreground -N returns immediately (nil error), which the supervisor
// misreads as the tunnel dying — an endless up/down flap while a detached
// master silently holds the forward. Without persist, the foreground -N blocks
// for the life of the connection, and when it dies the master dies with it.
func masterMuxArgs(ctlDir, label string) []string {
	return []string{
		"-o", "ControlMaster=yes",
		"-o", controlPathOpt(ctlDir, label),
	}
}

// clientMuxArgs reuse the tunnel's master if it is up, never promoting
// themselves to master.
func clientMuxArgs(ctlDir, label string) []string {
	return []string{
		"-o", "ControlMaster=no",
		"-o", controlPathOpt(ctlDir, label),
	}
}
