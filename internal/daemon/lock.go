package daemon

import "os"

// instanceLock is a held advisory file lock ensuring only one lopend runs per
// state dir. Without it a second lopend (a `brew services restart` racing the
// old process, launchd relaunching, or a manual `lopend run`) would run
// listenLocal, os.Remove the still-live daemon's request socket out from under
// it, and leave the running daemon holding an orphaned fd that no path points
// to — the tunnel stays up but every open fails with "connection reset".
//
// acquireLock/release are platform-specific: flock on unix, LockFileEx on
// Windows (see lock_unix.go / lock_windows.go).
type instanceLock struct {
	f *os.File
}
