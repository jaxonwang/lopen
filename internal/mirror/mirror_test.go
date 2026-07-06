package mirror

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaxonwang/lopen/internal/index"
)

func newMirror(t *testing.T) *Mirror {
	t.Helper()
	base := t.TempDir()
	idx, err := index.Load(filepath.Join(base, "index.json"))
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Join(base, "mirror")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return &Mirror{Root: root, Idx: idx}
}

func TestRelConfinement(t *testing.T) {
	if _, err := Rel("host", "/home/u/f.txt"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/", "/..", "/../../etc/passwd"} {
		if rel, err := Rel("host", p); err == nil {
			abs := filepath.Join("/root", rel)
			if !filepath.HasPrefix(abs, filepath.Join("/root", "host")) {
				t.Errorf("Rel(%q) = %q escapes label dir", p, rel)
			}
		}
	}
	if _, err := Rel("host", "/"); err == nil {
		t.Error("mirroring / should be refused")
	}
}

func TestGCTTLAndGuards(t *testing.T) {
	m := newMirror(t)

	write := func(rel, content string) string {
		abs := m.Abs(rel)
		if err := os.MkdirAll(filepath.Dir(abs), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(abs, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		return abs
	}

	// Entry 1: expired, unmodified → evicted.
	old := write("h/tmp/old.txt", "old")
	fi, _ := os.Stat(old)
	m.Idx.Record("h/tmp/old.txt", false, 3, fi.ModTime())
	backdate(t, m.Idx, "h/tmp/old.txt", -8*24*time.Hour)

	// Entry 2: expired but locally modified → retained.
	mod := write("h/tmp/mod.txt", "orig")
	fi, _ = os.Stat(mod)
	m.Idx.Record("h/tmp/mod.txt", false, 4, fi.ModTime())
	backdate(t, m.Idx, "h/tmp/mod.txt", -8*24*time.Hour)
	time.Sleep(10 * time.Millisecond)
	if err := os.WriteFile(mod, []byte("edited locally"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(time.Hour)
	os.Chtimes(mod, future, future)

	// Entry 3: fresh → kept.
	fresh := write("h/tmp/fresh.txt", "fresh")
	fi, _ = os.Stat(fresh)
	m.Idx.Record("h/tmp/fresh.txt", false, 5, fi.ModTime())

	// Entry 4: pinned label, expired → kept.
	pin := write("pinned/tmp/keep.txt", "keep")
	fi, _ = os.Stat(pin)
	m.Idx.Record("pinned/tmp/keep.txt", false, 4, fi.ModTime())
	backdate(t, m.Idx, "pinned/tmp/keep.txt", -30*24*time.Hour)

	res := m.GC(7*24*time.Hour, 1<<30, map[string]bool{"pinned": true})

	if _, err := os.Stat(old); !os.IsNotExist(err) {
		t.Error("expired entry not evicted")
	}
	if _, err := os.Stat(mod); err != nil {
		t.Error("locally-modified entry was evicted")
	}
	if _, err := os.Stat(fresh); err != nil {
		t.Error("fresh entry was evicted")
	}
	if _, err := os.Stat(pin); err != nil {
		t.Error("pinned entry was evicted")
	}
	if len(res.Removed) != 1 || len(res.Retained) != 1 {
		t.Errorf("res = %+v", res)
	}
}

func TestGCSizeCapLRU(t *testing.T) {
	m := newMirror(t)
	for i, rel := range []string{"h/a", "h/b", "h/c"} {
		abs := m.Abs(rel)
		os.MkdirAll(filepath.Dir(abs), 0o700)
		os.WriteFile(abs, make([]byte, 100), 0o600)
		fi, _ := os.Stat(abs)
		m.Idx.Record(rel, false, 100, fi.ModTime())
		// a is least recently used, c most.
		backdate(t, m.Idx, rel, time.Duration(-(3-i))*time.Hour)
	}
	// Cap of 250 bytes: must evict exactly the LRU entry (a).
	m.GC(30*24*time.Hour, 250, nil)
	if _, err := os.Stat(m.Abs("h/a")); !os.IsNotExist(err) {
		t.Error("LRU entry a not evicted")
	}
	if _, err := os.Stat(m.Abs("h/b")); err != nil {
		t.Error("entry b wrongly evicted")
	}
	if _, err := os.Stat(m.Abs("h/c")); err != nil {
		t.Error("entry c wrongly evicted")
	}
}

// TestFilePromoteDropsAncestorDirEntry: syncing a file inside a slot that a
// directory entry already covers must remove the ancestor dir entry, so the
// two never coexist (which would leave the dir fingerprint stale and let GC
// wipe the fresh file).
func TestFilePromoteDropsAncestorDirEntry(t *testing.T) {
	m := newMirror(t)

	// Record a directory entry at h/a/b.
	dir := m.Abs("h/a/b")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "old.txt"), []byte("old"), 0o600); err != nil {
		t.Fatal(err)
	}
	fi, _ := os.Stat(dir)
	m.Idx.Record("h/a/b", true, 3, fi.ModTime())

	// Now promote a file into h/a/b/child.txt.
	staged, err := m.StagingDir()
	if err != nil {
		t.Fatal(err)
	}
	sp := filepath.Join(staged, "payload")
	if err := os.WriteFile(sp, []byte("child"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := m.Promote(sp, "h/a/b/child.txt", false); err != nil {
		t.Fatal(err)
	}

	if m.Idx.Get("h/a/b") != nil {
		t.Fatal("ancestor directory entry should have been dropped")
	}
	if m.Idx.Get("h/a/b/child.txt") == nil {
		t.Fatal("file entry missing")
	}
}

// backdate rewrites LastUsed for a test entry.
func backdate(t *testing.T, ix *index.Index, rel string, d time.Duration) {
	t.Helper()
	e := ix.Get(rel)
	if e == nil {
		t.Fatalf("no entry %s", rel)
	}
	e.LastUsed = time.Now().Add(d)
	e.LastSync = e.LastUsed
	// Re-record through the public API to persist.
	ix.Entries[rel] = e
}
