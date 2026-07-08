package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"
)

// errSharingViolation is Win32 ERROR_SHARING_VIOLATION (32): returned by
// CreateFile when the file is already open with an incompatible share mode.
// The stdlib syscall package doesn't export the constant, so define it.
const errSharingViolation = syscall.Errno(32)

// acquireLock opens the lock file with an exclusive share mode (dwShareMode=0),
// so a second lopend's CreateFile fails with a sharing violation — the Windows
// equivalent of the unix flock. Windows releases the handle (and thus the lock)
// when the process exits or release() closes it, so a crash never strands it.
func acquireLock(path string) (*instanceLock, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return nil, err
	}
	h, err := syscall.CreateFile(
		p,
		syscall.GENERIC_READ|syscall.GENERIC_WRITE,
		0, // no sharing: exclusive
		nil,
		syscall.OPEN_ALWAYS,
		syscall.FILE_ATTRIBUTE_NORMAL,
		0,
	)
	if err != nil {
		if errors.Is(err, errSharingViolation) {
			return nil, fmt.Errorf("another lopend is already running (lock held on %s)", path)
		}
		return nil, err
	}
	return &instanceLock{f: os.NewFile(uintptr(h), path)}, nil
}

func (l *instanceLock) release() {
	if l == nil || l.f == nil {
		return
	}
	_ = l.f.Close()
}
