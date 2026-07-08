//go:build unix

package daemon

import (
	"fmt"
	"os"
	"syscall"
)

// acquireLock takes an exclusive, non-blocking flock on path. It returns an
// error (not a block) if another process already holds it, so a duplicate
// daemon exits fast instead of clobbering shared state. The lock is released
// when the process exits (fd closed by the kernel) or release() is called.
func acquireLock(path string) (*instanceLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		f.Close()
		if err == syscall.EWOULDBLOCK {
			return nil, fmt.Errorf("another lopend is already running (lock held on %s)", path)
		}
		return nil, err
	}
	return &instanceLock{f: f}, nil
}

func (l *instanceLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	_ = l.f.Close()
}
