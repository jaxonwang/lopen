//go:build unix

package daemon

import "syscall"

func umask(m int) int { return syscall.Umask(m) }
