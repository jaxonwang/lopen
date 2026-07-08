//go:build unix

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"testing"

	"github.com/jaxonwang/lopen/internal/config"
)

// cfgFor builds a config whose SocketDir is a short, existing directory (macOS
// unix socket paths have a ~104-byte limit, so t.TempDir() is too long).
func cfgFor(t *testing.T) *config.Config {
	t.Helper()
	dir := t.TempDir()
	c := &config.Config{StateDir: dir}
	if err := os.MkdirAll(c.SocketDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	return c
}

// TestListenLocalRefusesLiveSocket: listenLocal must not unlink a socket that a
// process is actively listening on — the exact failure that orphaned the
// daemon's fd and broke every open with "connection reset".
func TestListenLocalRefusesLiveSocket(t *testing.T) {
	cfg := cfgFor(t)

	ln, _, err := listenLocal(cfg, "devbox")
	if err != nil {
		t.Fatalf("first listen failed (socket path too long?): %v", err)
	}
	defer ln.Close()

	// A second bind on the live socket must refuse, and must NOT remove it.
	if _, _, err := listenLocal(cfg, "devbox"); err == nil {
		t.Fatal("listenLocal should refuse a socket with a live listener")
	}
	// The original listener still works.
	go func() {
		if c, e := ln.Accept(); e == nil {
			c.Close()
		}
	}()
	sock := filepath.Join(cfg.SocketDir(), "devbox.sock")
	c, err := net.Dial("unix", sock)
	if err != nil {
		t.Fatalf("original listener was broken by the second listenLocal: %v", err)
	}
	c.Close()
}

// TestListenLocalRebindsStaleSocket: a leftover socket file with no listener
// (crash residue) must be removed and rebound, not treated as live.
func TestListenLocalRebindsStaleSocket(t *testing.T) {
	cfg := cfgFor(t)

	ln, _, err := listenLocal(cfg, "devbox")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ln.Close() // leaves the socket file on disk, no listener

	ln2, _, err := listenLocal(cfg, "devbox")
	if err != nil {
		t.Fatalf("listenLocal should rebind a stale socket, got: %v", err)
	}
	ln2.Close()
}
