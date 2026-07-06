package daemon

import (
	"bufio"
	"context"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/jaxonwang/lopen/internal/client"
	"github.com/jaxonwang/lopen/internal/config"
	"github.com/jaxonwang/lopen/internal/protocol"
)

// newTestServer wires a Server with a fake `open` (records its argv to a
// file) and returns it plus the loopback TCP address clients dial (mirroring
// the 127.0.0.1:<port> that sshd binds in production), the "devbox" host
// token, and the open-log path.
func newTestServer(t *testing.T) (srv *Server, addr, token, openLog string) {
	t.Helper()
	base := t.TempDir()
	openLog = filepath.Join(base, "open.log")
	fakeOpen := filepath.Join(base, "fake-open")
	script := "#!/bin/sh\necho \"$@\" >> " + openLog + "\n"
	if err := os.WriteFile(fakeOpen, []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		MirrorDir: filepath.Join(base, "mirror"),
		StateDir:  filepath.Join(base, "state"),
		Hosts:     []config.Host{{Label: "devbox", Dest: "devbox.example.com"}},
	}
	// Trigger defaults without reading a file.
	b := true
	cfg.AllowInline = &b
	cfg.TTLDays = config.DefaultTTLDays
	cfg.MaxMirrorBytes = config.DefaultMaxMirror
	cfg.MaxPayloadBytes = 10 << 20

	var err error
	srv, err = NewServer(cfg, slog.New(slog.NewTextHandler(os.Stderr, nil)))
	if err != nil {
		t.Fatal(err)
	}
	srv.OpenCommand = fakeOpen

	token, err = srv.tokens.ensure("devbox")
	if err != nil {
		t.Fatal(err)
	}

	// The daemon accepts on any net.Listener; in production sshd forwards a
	// loopback TCP port to the daemon's local UNIX socket, so a TCP listener
	// here exercises the same accept path the client dials.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	addr = ln.Addr().String()
	tun := &Tunnel{Host: cfg.Hosts[0], Token: token, Log: srv.Log}
	srv.tunnels["devbox"] = tun
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go srv.acceptLoop(ctx, ln, tun)
	return srv, addr, token, openLog
}

func TestInlineFileEndToEnd(t *testing.T) {
	srv, addr, token, openLog := newTestServer(t)

	src := filepath.Join(t.TempDir(), "report.pdf")
	if err := os.WriteFile(src, []byte("PDFDATA"), 0o644); err != nil {
		t.Fatal(err)
	}
	err := client.Open(src, client.Options{
		Addr: addr, Token: token, Label: "devbox", Wait: true, Retry: time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}

	// The mirror copy exists with the right content under <label>/<abs>.
	mirrored := filepath.Join(srv.Cfg.MirrorDir, "devbox", src)
	got, err := os.ReadFile(mirrored)
	if err != nil || string(got) != "PDFDATA" {
		t.Fatalf("mirror content = %q, %v", got, err)
	}
	// open was invoked on it.
	logB, err := os.ReadFile(openLog)
	if err != nil {
		t.Fatal(err)
	}
	if want := "-- " + mirrored + "\n"; string(logB) != want {
		t.Fatalf("open argv = %q, want %q", logB, want)
	}
	// Index recorded it.
	rel := filepath.Join("devbox", src[1:])
	if srv.Mir.Idx.Get(rel) == nil {
		t.Fatal("index entry missing")
	}
}

func TestInlineDirEndToEnd(t *testing.T) {
	srv, addr, token, _ := newTestServer(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "x.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(filepath.Join(src, "d"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(src, "d", "y.txt"), []byte("y"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true}); err != nil {
		t.Fatal(err)
	}
	mirrored := filepath.Join(srv.Cfg.MirrorDir, "devbox", src)
	if got, err := os.ReadFile(filepath.Join(mirrored, "d", "y.txt")); err != nil || string(got) != "y" {
		t.Fatalf("dir mirror content = %q, %v", got, err)
	}
}

func TestOverwriteReplacesOldVersion(t *testing.T) {
	srv, addr, token, _ := newTestServer(t)
	src := filepath.Join(t.TempDir(), "doc.txt")

	if err := os.WriteFile(src, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true}); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(src, []byte("v2-longer"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true}); err != nil {
		t.Fatal(err)
	}
	mirrored := filepath.Join(srv.Cfg.MirrorDir, "devbox", src)
	if got, _ := os.ReadFile(mirrored); string(got) != "v2-longer" {
		t.Fatalf("mirror = %q, want v2-longer", got)
	}
}

func TestLocallyModifiedGuard(t *testing.T) {
	srv, addr, token, _ := newTestServer(t)
	src := filepath.Join(t.TempDir(), "doc.txt")
	if err := os.WriteFile(src, []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true}); err != nil {
		t.Fatal(err)
	}

	// Simulate a local edit on the Mac copy: bump mtime past the guard
	// window and change size.
	mirrored := filepath.Join(srv.Cfg.MirrorDir, "devbox", src)
	if err := os.WriteFile(mirrored, []byte("local edits!"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(mirrored, future, future); err != nil {
		t.Fatal(err)
	}

	err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true})
	if err == nil {
		t.Fatal("expected locally-modified refusal")
	}
	// Local copy untouched.
	if got, _ := os.ReadFile(mirrored); string(got) != "local edits!" {
		t.Fatalf("guarded copy was overwritten: %q", got)
	}

	// --force overrides.
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(mirrored); string(got) != "v1" {
		t.Fatalf("force overwrite failed: %q", got)
	}
}

// TestDirOverwriteGuardsModifiedChild: opening a directory must not clobber a
// file the user edited inside the previously-synced copy, even though editing
// a child does not bump the parent directory's mtime.
func TestDirOverwriteGuardsModifiedChild(t *testing.T) {
	srv, addr, token, _ := newTestServer(t)
	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("v1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true}); err != nil {
		t.Fatal(err)
	}

	// Edit a file inside the mirrored directory; leave the dir mtime alone.
	child := filepath.Join(srv.Cfg.MirrorDir, "devbox", src, "notes.txt")
	if err := os.WriteFile(child, []byte("my local edits"), 0o600); err != nil {
		t.Fatal(err)
	}
	future := time.Now().Add(10 * time.Second)
	if err := os.Chtimes(child, future, future); err != nil {
		t.Fatal(err)
	}

	// Re-opening the parent dir must be refused without --force.
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true}); err == nil {
		t.Fatal("expected directory overwrite to be guarded by modified child")
	}
	if got, _ := os.ReadFile(child); string(got) != "my local edits" {
		t.Fatalf("child was clobbered: %q", got)
	}

	// --force overwrites and clears the stale child index entry.
	if err := os.WriteFile(filepath.Join(src, "notes.txt"), []byte("v2"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := client.Open(src, client.Options{Addr: addr, Token: token, Label: "devbox", Wait: true, Force: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := os.ReadFile(child); string(got) != "v2" {
		t.Fatalf("force dir overwrite failed: %q", got)
	}
}

func TestPullRefusedForMismatchedOrigin(t *testing.T) {
	_, addr, _, _ := newTestServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := &protocol.Request{
		V: protocol.Version, Op: protocol.OpOpen,
		Origin: protocol.Origin{Host: "other", Label: "otherhost"},
		Path:   "/etc/passwd", Mode: protocol.ModePull, Wait: true,
	}
	if err := protocol.WriteRequest(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	// otherhost is not a configured label, so it is refused up front with a
	// "done"/error and no payload is ever fetched.
	for {
		ev, err := protocol.ReadEvent(br)
		if err != nil {
			t.Fatalf("connection error before verdict: %v", err)
		}
		if ev.Event == "done" {
			if ev.Status != "error" {
				t.Fatal("cross-origin pull was allowed")
			}
			return
		}
	}
}

// TestInlineOriginSpoofRefused: a request arriving on host A's tunnel that
// claims to be host B must be rejected, even in inline mode, so a compromised
// host cannot forge content under another host's mirror label.
func TestInlineOriginSpoofRefused(t *testing.T) {
	_, addr, _, _ := newTestServer(t) // configures only "devbox"
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := &protocol.Request{
		V: protocol.Version, Op: protocol.OpOpen,
		Origin: protocol.Origin{Host: "b", Label: "otherlabel"},
		Path:   "/home/u/f.txt", Mode: protocol.ModeInline, Wait: true, Size: 1,
	}
	if err := protocol.WriteRequest(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	ev, err := protocol.ReadEvent(br)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != "done" || ev.Status != "error" {
		t.Fatalf("origin spoof was not refused: %+v", ev)
	}
}

func TestRejectsRootPath(t *testing.T) {
	_, addr, token, _ := newTestServer(t)
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	req := &protocol.Request{
		V: protocol.Version, Op: protocol.OpOpen,
		Origin: protocol.Origin{Host: "devbox", Label: "devbox"},
		Path:   "/", Mode: protocol.ModeInline, Wait: true, Token: token,
	}
	if err := protocol.WriteRequest(conn, req); err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(conn)
	ev, err := protocol.ReadEvent(br)
	if err != nil {
		t.Fatal(err)
	}
	if ev.Event != "done" || ev.Status != "error" {
		t.Fatalf("mirroring / was not refused: %+v", ev)
	}
}

// TestWrongTokenRefused: the loopback TCP transport is reachable by any local
// user on the remote host, so a request with the correct label but a wrong (or
// empty) token — what an unauthorized local user could send — must be rejected
// before any payload is read.
func TestWrongTokenRefused(t *testing.T) {
	for _, tok := range []string{"", "deadbeef"} {
		_, addr, _, _ := newTestServer(t)
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			t.Fatal(err)
		}
		req := &protocol.Request{
			V: protocol.Version, Op: protocol.OpOpen,
			Origin: protocol.Origin{Host: "devbox", Label: "devbox"},
			Path:   "/home/u/f.txt", Mode: protocol.ModeInline, Wait: true, Size: 1,
			Token: tok,
		}
		if err := protocol.WriteRequest(conn, req); err != nil {
			t.Fatal(err)
		}
		br := bufio.NewReader(conn)
		ev, err := protocol.ReadEvent(br)
		if err != nil {
			conn.Close()
			t.Fatal(err)
		}
		if ev.Event != "done" || ev.Status != "error" {
			conn.Close()
			t.Fatalf("token %q was not refused: %+v", tok, ev)
		}
		conn.Close()
	}
}

func TestShQuote(t *testing.T) {
	cases := map[string]string{
		`plain`:       `'plain'`,
		`with space`:  `'with space'`,
		`has'quote`:   `'has'\''quote'`,
		`$(rm -rf /)`: `'$(rm -rf /)'`,
		"back`tick":   "'back`tick'",
	}
	for in, want := range cases {
		if got := ShQuote(in); got != want {
			t.Errorf("ShQuote(%q) = %s, want %s", in, got, want)
		}
	}
}
