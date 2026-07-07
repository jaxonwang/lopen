//go:build unix

package mirror

import (
	"os/exec"
	"strings"
)

// fileInUse best-effort checks whether any process has the path open.
// Deleting an open file on unix does not crash the reader (the inode
// persists), so a false negative here is cosmetic, not a correctness issue.
func fileInUse(abs string) bool {
	out, err := exec.Command("lsof", "-t", "--", abs).Output()
	return err == nil && len(strings.TrimSpace(string(out))) > 0
}
