// Package mirror manages the local mirror tree (~/lopen/<label>/<abs path>)
// and its garbage collection.
//
// Overwrite semantics: re-opening a path that already exists in the mirror
// replaces the old copy atomically (staged fetch + rename). There is no
// version history. The one guard: if the local copy was modified locally
// since the last sync (per the index — v1 sync is one-way, so such edits
// exist nowhere else), the overwrite is refused unless the request sets
// force. The same guard protects entries from GC.
package mirror

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/jaxonwang/lopen/internal/index"
	"github.com/jaxonwang/lopen/internal/tarstream"
)

type Mirror struct {
	Root string
	Idx  *index.Index
}

var ErrLocallyModified = errors.New("local copy has unsaved modifications (re-run with --force to overwrite)")

// Rel computes the mirror-relative key for a request path, confined under
// the label directory. remotePath must be absolute and pre-validated by the
// protocol layer; SafeJoin re-checks confinement as defense in depth.
// Keys are always slash-separated (the remote is POSIX and the index's
// prefix logic assumes '/'), regardless of the local OS.
func Rel(label, remotePath string) (string, error) {
	p, err := tarstream.SafeJoin(label, strings.TrimPrefix(remotePath, "/"))
	if err != nil {
		return "", err
	}
	if p == label { // remotePath was "/" or cleaned away entirely
		return "", errors.New("refusing to mirror filesystem root")
	}
	return filepath.ToSlash(p), nil
}

func (m *Mirror) Abs(rel string) string { return filepath.Join(m.Root, filepath.FromSlash(rel)) }

// CheckOverwrite enforces the locally-modified guard before a sync replaces
// an existing mirror slot. Promote replaces the entire subtree rooted at
// rel, so the guard must consider not just rel itself but every indexed
// entry at or below it: opening a directory must not silently blow away a
// file the user edited inside a previously-synced copy (whose edit would not
// bump the parent directory's mtime).
func (m *Mirror) CheckOverwrite(rel string, force bool) error {
	if force {
		return nil
	}
	for _, e := range m.Idx.Descendants(rel) {
		if e.LocallyModified(m.Abs(e.Rel)) {
			return ErrLocallyModified
		}
	}
	return nil
}

func (m *Mirror) stagingBase() string { return filepath.Join(m.Root, ".staging") }

// StagingDir returns a fresh private staging directory beside the mirror (so
// the final rename is same-filesystem and atomic).
func (m *Mirror) StagingDir() (string, error) {
	if err := os.MkdirAll(m.stagingBase(), 0o700); err != nil {
		return "", err
	}
	return os.MkdirTemp(m.stagingBase(), "req-")
}

// sweepStaging removes staging subdirectories older than maxAge — leftovers
// from a crash mid-request or mid-promote (both the request staging dir and
// the moved-aside old copy live here). Not driven by the index, so it catches
// exactly the orphans the index never learned about.
func (m *Mirror) sweepStaging(maxAge time.Duration) {
	entries, err := os.ReadDir(m.stagingBase())
	if err != nil {
		return
	}
	cutoff := time.Now().Add(-maxAge)
	for _, e := range entries {
		fi, err := e.Info()
		if err != nil {
			continue
		}
		if fi.ModTime().Before(cutoff) {
			_ = os.RemoveAll(filepath.Join(m.stagingBase(), e.Name()))
		}
	}
}

// Promote atomically moves staged (a file or directory) to the mirror slot
// rel, replacing whatever was there, and records the sync in the index.
func (m *Mirror) Promote(staged, rel string, dir bool) error {
	dst := m.Abs(rel)
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	// Replace: move the old copy aside first so rename cannot fail on a
	// non-empty directory, then remove it. os.Rename over an existing
	// directory does not work on POSIX. The aside copy goes *inside*
	// .staging (which GC sweeps) rather than beside dst, so a crash between
	// the two renames can't strand an orphan next to live mirror data.
	var old string
	if _, err := os.Lstat(dst); err == nil {
		asideDir, err := os.MkdirTemp(m.stagingBase(), "old-")
		if err != nil {
			return err
		}
		old = filepath.Join(asideDir, "payload")
		if err := os.Rename(dst, old); err != nil {
			os.RemoveAll(asideDir)
			return err
		}
	}
	if err := os.Rename(staged, dst); err != nil {
		if old != "" {
			_ = os.Rename(old, dst) // best-effort rollback
		}
		return err
	}
	if old != "" {
		_ = os.RemoveAll(filepath.Dir(old))
	}

	size := int64(0)
	syncMod := time.Now()
	if dir {
		// Record the tree fingerprint: total size and the latest mtime
		// anywhere under dst, so a later edit to any child is detected.
		size, syncMod, _ = index.DirFingerprint(dst)
	} else if fi, err := os.Stat(dst); err == nil {
		size = fi.Size()
		syncMod = fi.ModTime()
	}
	if dir {
		// The old subtree (and its per-file index entries) is gone; drop
		// stale descendant records so they don't outlive the files.
		_ = m.Idx.DeleteDescendantsOf(rel)
	} else {
		// A file synced inside a previously-recorded directory entry: drop
		// the ancestor dir entry so the two never overlap (stale fingerprint
		// / GC wiping the fresh file).
		_ = m.Idx.DeleteAncestorDirsOf(rel)
	}
	return m.Idx.Record(rel, dir, size, syncMod)
}

// GC applies retention: entries unused for longer than ttl are removed; if
// the mirror still exceeds maxBytes, least-recently-used entries are evicted
// until under the cap. Locally-modified entries and pinned labels are never
// evicted; files currently open (best-effort lsof) are skipped.
type GCResult struct {
	Removed  []string
	Retained []string // locally modified, kept
	Bytes    int64    // bytes freed
}

func (m *Mirror) GC(ttl time.Duration, maxBytes int64, pinned map[string]bool) GCResult {
	var res GCResult
	now := time.Now()
	total := m.Idx.TotalBytes()

	// Reap staging leftovers from crashed requests/promotes. 1h is well past
	// the per-request timeout, so anything older is definitely orphaned.
	m.sweepStaging(time.Hour)

	for _, e := range m.Idx.Candidates() {
		abs := m.Abs(e.Rel)
		expired := now.Sub(e.LastUsed) > ttl
		overCap := total > maxBytes
		if !expired && !overCap {
			continue
		}
		label := strings.SplitN(e.Rel, "/", 2)[0]
		if pinned[label] {
			continue
		}
		if _, err := os.Lstat(abs); err != nil {
			// Already gone from disk; drop the index entry.
			_ = m.Idx.Delete(e.Rel)
			continue
		}
		if e.LocallyModified(abs) {
			res.Retained = append(res.Retained, e.Rel)
			continue
		}
		if fileInUse(abs) {
			continue
		}
		if err := os.RemoveAll(abs); err != nil {
			continue
		}
		_ = m.Idx.Delete(e.Rel)
		res.Removed = append(res.Removed, e.Rel)
		res.Bytes += e.Size
		total -= e.Size
	}
	m.pruneEmptyDirs()
	return res
}

func (m *Mirror) pruneEmptyDirs() {
	// Repeatedly remove empty directories bottom-up (skip the root and
	// .staging). Two passes are enough for typical depths; loop to fixpoint.
	for {
		removed := false
		filepath.WalkDir(m.Root, func(p string, d os.DirEntry, err error) error {
			if err != nil || !d.IsDir() || p == m.Root {
				return nil
			}
			if filepath.Base(p) == ".staging" {
				return filepath.SkipDir
			}
			entries, err := os.ReadDir(p)
			if err == nil && len(entries) == 0 {
				if os.Remove(p) == nil {
					removed = true
				}
				return filepath.SkipDir
			}
			return nil
		})
		if !removed {
			break
		}
	}
}
