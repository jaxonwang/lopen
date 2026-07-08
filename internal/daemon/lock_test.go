package daemon

import (
	"path/filepath"
	"testing"
)

// TestInstanceLockExclusive: a second acquire on the same path fails fast
// instead of blocking or succeeding, so a duplicate daemon exits rather than
// clobbering shared state.
func TestInstanceLockExclusive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lopend.lock")
	l1, err := acquireLock(path)
	if err != nil {
		t.Fatalf("first acquire failed: %v", err)
	}
	defer l1.release()

	if _, err := acquireLock(path); err == nil {
		t.Fatal("second acquire should have failed while the lock is held")
	}

	// After release, the lock is reusable.
	l1.release()
	l2, err := acquireLock(path)
	if err != nil {
		t.Fatalf("acquire after release failed: %v", err)
	}
	l2.release()
}
