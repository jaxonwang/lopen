// Package tarstream packs a directory into a tar stream and safely unpacks
// one. The unpack side treats the archive as hostile input: entry names are
// confined to the destination, only regular files and directories are
// materialized, and symlinks/hardlinks/devices are skipped entirely.
package tarstream

import (
	"archive/tar"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

// Pack writes dir as a tar stream to w. Entry names are relative to dir.
// Symlinks and non-regular files are skipped; the count of skipped entries is
// returned so the caller can surface it.
func Pack(dir string, w io.Writer) (skipped int, err error) {
	tw := tar.NewWriter(w)
	err = filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		if rel == "." {
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		switch {
		case d.Type().IsDir():
			hdr := &tar.Header{Name: rel + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: info.ModTime()}
			return tw.WriteHeader(hdr)
		case d.Type().IsRegular():
			hdr := &tar.Header{Name: rel, Typeflag: tar.TypeReg, Mode: 0o644, Size: info.Size(), ModTime: info.ModTime()}
			if info.Mode()&0o100 != 0 {
				hdr.Mode = 0o755
			}
			if err := tw.WriteHeader(hdr); err != nil {
				return err
			}
			f, err := os.Open(path)
			if err != nil {
				return err
			}
			// The file may grow/shrink between stat and read; copy exactly
			// the declared size so the archive stays well-formed.
			_, err = io.CopyN(tw, f, info.Size())
			f.Close()
			if errors.Is(err, io.EOF) {
				return fmt.Errorf("%s shrank while packing", path)
			}
			return err
		default:
			skipped++
			return nil
		}
	})
	if err != nil {
		return skipped, err
	}
	return skipped, tw.Close()
}

// ErrBudgetExceeded is returned when an archive would expand past the byte
// budget or entry-count cap.
var ErrBudgetExceeded = errors.New("archive exceeds extraction budget")

// Unpack extracts a tar stream into dest (which must exist and be empty or
// absent). maxBytes caps the total expanded output and maxEntries caps the
// number of archive members — both defend against a hostile archive that is
// small on the wire but huge on disk (e.g. a sparse entry whose declared
// size synthesizes gigabytes of zeroes, or millions of empty files
// exhausting inodes). A declared entry size is checked against the remaining
// budget *before* copying, which also rejects the sparse-expansion trick
// because the synthesized output equals the declared size. Returns the
// number of entries skipped for safety reasons.
func Unpack(r io.Reader, dest string, maxBytes int64, maxEntries int) (skipped int, err error) {
	if err := os.MkdirAll(dest, 0o700); err != nil {
		return 0, err
	}
	tr := tar.NewReader(r)
	var remaining = maxBytes
	var entries int
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			return skipped, nil
		}
		if err != nil {
			return skipped, err
		}
		if entries++; entries > maxEntries {
			return skipped, fmt.Errorf("%w: more than %d entries", ErrBudgetExceeded, maxEntries)
		}
		target, err := SafeJoin(dest, hdr.Name)
		if err != nil {
			return skipped, fmt.Errorf("rejecting archive entry %q: %w", hdr.Name, err)
		}
		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, 0o700); err != nil {
				return skipped, err
			}
		case tar.TypeReg:
			if hdr.Size < 0 || hdr.Size > remaining {
				return skipped, fmt.Errorf("%w: entry %q size %d", ErrBudgetExceeded, hdr.Name, hdr.Size)
			}
			if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
				return skipped, err
			}
			mode := os.FileMode(0o600)
			if hdr.Mode&0o100 != 0 {
				mode = 0o700
			}
			// O_EXCL: within a fresh staging dir every path is written once;
			// a duplicate entry name in the archive is suspicious, not a
			// normal condition.
			f, err := os.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
			if err != nil {
				return skipped, err
			}
			// CopyN of exactly the declared size: the budget check above
			// already bounded it, and CopyN never writes more even if the
			// reader misbehaves.
			n, err := io.CopyN(f, tr, hdr.Size)
			if cerr := f.Close(); err == nil {
				err = cerr
			}
			if err != nil && !errors.Is(err, io.EOF) {
				return skipped, err
			}
			remaining -= n
		default:
			// Symlinks, hardlinks, devices, FIFOs: never materialized. A
			// symlink inside the mirror could redirect a later overwrite or
			// an `open` outside the mirror root.
			skipped++
		}
	}
}

// SafeJoin joins name under root, rejecting absolute names and any path that
// escapes root. The result is lexically confined; callers must ensure root
// itself contains no attacker-controlled symlinks (Unpack guarantees this by
// never creating symlinks).
//
// name is always a POSIX path (a remote absolute path or a tar entry name),
// so confinement is checked in slash space. A literal backslash is rejected
// rather than treated as data: on Windows filepath.Clean/Join treat '\' as a
// separator, so an unrejected `..\..\x` would escape root there even though
// the '../' check passes. Backslash in a POSIX filename is legal but vanishing
// -ly rare, and refusing it is the safe default for this confinement boundary.
func SafeJoin(root, name string) (string, error) {
	if name == "" {
		return "", errors.New("empty name")
	}
	if strings.ContainsRune(name, 0) {
		return "", errors.New("NUL in name")
	}
	if strings.ContainsRune(name, '\\') {
		return "", errors.New("backslash in name")
	}
	if path.IsAbs(name) {
		return "", errors.New("absolute path")
	}
	clean := path.Clean(name)
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes root")
	}
	return filepath.Join(root, filepath.FromSlash(clean)), nil
}
