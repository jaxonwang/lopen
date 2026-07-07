package tarstream

import (
	"archive/tar"
	"bytes"
	"os"
	"path/filepath"
	"testing"
)

func TestPackUnpackRoundTrip(t *testing.T) {
	src := t.TempDir()
	mustWrite(t, filepath.Join(src, "a.txt"), "hello")
	mustWrite(t, filepath.Join(src, "sub", "b with space.txt"), "world")
	mustWrite(t, filepath.Join(src, "sub", "uni-日本語.txt"), "こんにちは")
	if err := os.Symlink("a.txt", filepath.Join(src, "link")); err != nil {
		t.Fatal(err)
	}

	var buf bytes.Buffer
	skipped, err := Pack(src, &buf)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1 (the symlink)", skipped)
	}

	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(&buf, dst, 1<<30, 100000); err != nil {
		t.Fatal(err)
	}
	for rel, want := range map[string]string{
		"a.txt":                "hello",
		"sub/b with space.txt": "world",
		"sub/uni-日本語.txt":      "こんにちは",
	} {
		got, err := os.ReadFile(filepath.Join(dst, rel))
		if err != nil {
			t.Fatalf("%s: %v", rel, err)
		}
		if string(got) != want {
			t.Fatalf("%s = %q, want %q", rel, got, want)
		}
	}
	if _, err := os.Lstat(filepath.Join(dst, "link")); !os.IsNotExist(err) {
		t.Fatal("symlink should not have been materialized")
	}
}

func TestUnpackRejectsTraversal(t *testing.T) {
	// NUL-in-name cannot be constructed through archive/tar's writer (it
	// rejects the header), so that vector is covered by TestSafeJoin only.
	for _, name := range []string{"../evil", "/abs/evil", "a/../../evil", ".."} {
		var buf bytes.Buffer
		tw := tar.NewWriter(&buf)
		if err := tw.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Size: 0, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
		tw.Close()
		dst := filepath.Join(t.TempDir(), "out")
		if _, err := Unpack(&buf, dst, 1<<30, 100000); err == nil {
			t.Errorf("entry %q: expected rejection", name)
		}
	}
}

func TestUnpackSkipsSymlinkEntries(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	// A symlink pointing outside, then a file "through" it — the classic
	// two-step link attack. The link must be skipped so the file write goes
	// to a plain path (and then fails confinement or lands inside dst).
	if err := tw.WriteHeader(&tar.Header{Name: "escape", Typeflag: tar.TypeSymlink, Linkname: "/tmp"}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{Name: "escape/pwned", Typeflag: tar.TypeReg, Size: 2, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	tw.Write([]byte("hi"))
	tw.Close()

	dst := filepath.Join(t.TempDir(), "out")
	skipped, err := Unpack(&buf, dst, 1<<30, 100000)
	if err != nil {
		t.Fatal(err)
	}
	if skipped != 1 {
		t.Fatalf("skipped = %d, want 1", skipped)
	}
	if _, err := os.Stat("/tmp/pwned"); err == nil {
		os.Remove("/tmp/pwned")
		t.Fatal("file escaped through symlink")
	}
	// The file lands harmlessly inside dst as escape/pwned (escape is a
	// plain directory created by MkdirAll).
	if _, err := os.Stat(filepath.Join(dst, "escape", "pwned")); err != nil {
		t.Fatalf("expected file confined inside dst: %v", err)
	}
}

func TestUnpackByteBudget(t *testing.T) {
	// An entry declaring more bytes than the budget must be rejected before
	// any bytes are synthesized — this is the sparse-expansion / disk-fill
	// defense.
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	if err := tw.WriteHeader(&tar.Header{Name: "big", Typeflag: tar.TypeReg, Size: 100, Mode: 0o644}); err != nil {
		t.Fatal(err)
	}
	tw.Write(make([]byte, 100))
	tw.Close()

	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(&buf, dst, 50, 100); err == nil {
		t.Fatal("expected byte-budget rejection")
	}
}

func TestUnpackEntryCap(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	for i := 0; i < 10; i++ {
		if err := tw.WriteHeader(&tar.Header{Name: "f" + string(rune('0'+i)), Typeflag: tar.TypeReg, Size: 0, Mode: 0o644}); err != nil {
			t.Fatal(err)
		}
	}
	tw.Close()
	dst := filepath.Join(t.TempDir(), "out")
	if _, err := Unpack(&buf, dst, 1<<30, 3); err == nil {
		t.Fatal("expected entry-count rejection")
	}
}

func TestSafeJoin(t *testing.T) {
	good := map[string]string{
		"a/b.txt":  filepath.FromSlash("/root/a/b.txt"),
		"a/./b":    filepath.FromSlash("/root/a/b"),
		"a/c/../b": filepath.FromSlash("/root/a/b"),
	}
	for name, want := range good {
		got, err := SafeJoin("/root", name)
		if err != nil || got != want {
			t.Errorf("SafeJoin(%q) = %q, %v; want %q", name, got, err, want)
		}
	}
	// Confinement is checked in slash space regardless of host OS. Backslash
	// is rejected so a `..\..\x` name cannot escape root on Windows, where
	// filepath treats '\' as a separator.
	for _, name := range []string{"", "/abs", "../x", "a/../../x", "..", "a\x00",
		`..\..\x`, `a\b`, `\abs`} {
		if _, err := SafeJoin("/root", name); err == nil {
			t.Errorf("SafeJoin(%q): expected error", name)
		}
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}
