// Package index maintains the sync index: one entry per mirrored path,
// recording when it was last synced and last used, its synced size/mtime,
// and whether GC may evict it. The index is the source of truth for the
// "locally modified" guard — filesystem atime is not reliable on macOS.
package index

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
)

type Entry struct {
	// Rel is the mirror-relative path ("<label>/<abs path on host>").
	Rel      string    `json:"rel"`
	Dir      bool      `json:"dir"`
	Size     int64     `json:"size"`
	SyncMod  time.Time `json:"sync_mod"` // local mtime right after sync
	LastSync time.Time `json:"last_sync"`
	LastUsed time.Time `json:"last_used"`
}

type Index struct {
	mu      sync.Mutex
	path    string
	Entries map[string]*Entry `json:"entries"`
}

func Load(path string) (*Index, error) {
	idx := &Index{path: path, Entries: map[string]*Entry{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return idx, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, idx); err != nil {
		// A corrupt index is recoverable: worst case we lose usage history.
		// Start fresh rather than wedging the daemon.
		idx.Entries = map[string]*Entry{}
	}
	return idx, nil
}

func (ix *Index) save() error {
	b, err := json.MarshalIndent(ix, "", " ")
	if err != nil {
		return err
	}
	tmp := ix.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ix.path)
}

// Record registers a completed sync and marks the entry used now.
func (ix *Index) Record(rel string, dir bool, size int64, syncMod time.Time) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	now := time.Now()
	ix.Entries[rel] = &Entry{Rel: rel, Dir: dir, Size: size, SyncMod: syncMod, LastSync: now, LastUsed: now}
	return ix.save()
}

// Touch marks an entry used now (a re-open served from cache still counts).
func (ix *Index) Touch(rel string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if e, ok := ix.Entries[rel]; ok {
		e.LastUsed = time.Now()
		return ix.save()
	}
	return nil
}

func (ix *Index) Get(rel string) *Entry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	if e, ok := ix.Entries[rel]; ok {
		cp := *e
		return &cp
	}
	return nil
}

// Descendants returns copies of every entry whose Rel is at or below prefix
// (prefix itself, or prefix + "/" + ...). Used so overwriting a directory
// also inspects the files inside it for local modifications.
func (ix *Index) Descendants(prefix string) []Entry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var out []Entry
	for rel, e := range ix.Entries {
		if rel == prefix || strings.HasPrefix(rel, prefix+"/") {
			out = append(out, *e)
		}
	}
	return out
}

func (ix *Index) Delete(rel string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	delete(ix.Entries, rel)
	return ix.save()
}

// DeleteDescendantsOf removes every entry strictly below prefix (not prefix
// itself). Called after a directory is re-synced so stale per-file entries
// from the previous copy don't linger and falsely trip the overwrite guard.
func (ix *Index) DeleteDescendantsOf(prefix string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	changed := false
	for rel := range ix.Entries {
		if strings.HasPrefix(rel, prefix+"/") {
			delete(ix.Entries, rel)
			changed = true
		}
	}
	if changed {
		return ix.save()
	}
	return nil
}

// DeleteAncestorDirsOf removes any directory entry that is an ancestor of
// rel. Called when a file is synced into a slot that lives inside a
// previously-synced directory entry: keeping both would leave the directory's
// fingerprint stale (falsely tripping the guard) and let GC evict the
// directory subtree out from under the fresh file.
func (ix *Index) DeleteAncestorDirsOf(rel string) error {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	changed := false
	for r, e := range ix.Entries {
		if e.Dir && strings.HasPrefix(rel, r+"/") {
			delete(ix.Entries, r)
			changed = true
		}
	}
	if changed {
		return ix.save()
	}
	return nil
}

// LocallyModified reports whether the tree at abs diverged from what we
// synced. For a file: mtime moved past the recorded post-sync mtime, or the
// size changed. For a directory: the fingerprint (total regular-file size +
// latest mtime anywhere in the tree) changed — so an edit to any file inside
// is detected even though it does not bump the parent directory's mtime.
// SyncMod holds the tree's latest mtime for a directory, or the file's own
// post-sync mtime for a file.
//
// This is a best-effort data-safety guard (the same mtime+size heuristic
// rsync uses by default), not a security boundary: the mirror is a one-way
// cache whose source of truth is the remote. It can miss a same-byte-size
// edit made within the granularity slop of the sync, so `--force` is always
// available and the guard errs toward refusing (surfacing to the user) when
// unsure. It is not defended against an adversary, because the party it
// protects is the local user, and the remote cannot control local edit
// timing.
func (e *Entry) LocallyModified(abs string) bool {
	if e.Dir {
		size, latest, err := DirFingerprint(abs)
		if err != nil {
			return false // gone or unreadable: nothing local to lose
		}
		if size != e.Size {
			return true
		}
		return latest.After(e.SyncMod.Add(2 * time.Second))
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return false
	}
	if fi.ModTime().After(e.SyncMod.Add(2 * time.Second)) {
		return true
	}
	return fi.Size() != e.Size
}

// DirFingerprint returns the total size of regular files under root and the
// latest mtime of any entry (files and subdirectories) in the tree.
func DirFingerprint(root string) (size int64, latest time.Time, err error) {
	err = filepath.WalkDir(root, func(_ string, d os.DirEntry, werr error) error {
		if werr != nil {
			return werr
		}
		fi, ierr := d.Info()
		if ierr != nil {
			return ierr
		}
		if fi.ModTime().After(latest) {
			latest = fi.ModTime()
		}
		if d.Type().IsRegular() {
			size += fi.Size()
		}
		return nil
	})
	return size, latest, err
}

// Candidates returns entries sorted least-recently-used first.
func (ix *Index) Candidates() []Entry {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	out := make([]Entry, 0, len(ix.Entries))
	for _, e := range ix.Entries {
		out = append(out, *e)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].LastUsed.Before(out[j].LastUsed) })
	return out
}

// TotalBytes sums recorded sizes.
func (ix *Index) TotalBytes() int64 {
	ix.mu.Lock()
	defer ix.mu.Unlock()
	var t int64
	for _, e := range ix.Entries {
		t += e.Size
	}
	return t
}
