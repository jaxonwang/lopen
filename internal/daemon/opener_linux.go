package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jaxonwang/lopen/internal/protocol"
)

func openPlatform(ctx context.Context, req *protocol.Request, abs string) error {
	if req.App != "" {
		return errors.New("-a <App> is only supported when the local machine is a Mac")
	}
	env := guiEnv()
	if req.Op == protocol.OpReveal {
		if err := revealDBus(ctx, env, abs); err == nil {
			return nil
		}
		// No usable FileManager1 (no implementation on the bus, no gdbus, or
		// the call failed): open the containing directory instead of failing.
		abs = filepath.Dir(abs)
	}
	argv, err := openerArgv()
	if err != nil {
		return err
	}
	return runOpen(ctx, env, argv[0], append(argv[1:], abs)...)
}

// openerArgv returns the opener command as an argv prefix (the target path is
// appended by the caller). gio takes an "open" subcommand; xdg-open takes the
// path directly.
func openerArgv() ([]string, error) {
	// gio resolves the handler via GAppInfo/DBus activation and does not
	// depend on XDG_CURRENT_DESKTOP; xdg-open is the portable fallback.
	if p, err := exec.LookPath("gio"); err == nil {
		return []string{p, "open"}, nil
	}
	if p, err := exec.LookPath("xdg-open"); err == nil {
		return []string{p}, nil
	}
	return nil, errors.New("no opener found: install glib2 (gio) or xdg-utils (xdg-open)")
}

// guiEnv returns the daemon's environment with the GUI-session variables the
// opener needs (DISPLAY, WAYLAND_DISPLAY, DBUS_SESSION_BUS_ADDRESS, ...)
// resolved from the user manager's current environment block. A daemon
// started by `systemd --user` does not inherit these, and they can change
// across logins, so they are resolved lazily at open time. The resolved
// values overwrite any inherited copy (rather than being appended, where
// POSIX getenv would return the stale inherited value shadowing ours).
func guiEnv() []string {
	base := os.Environ()
	out, err := exec.Command("systemctl", "--user", "show-environment").Output()
	if err != nil {
		return base
	}
	wanted := map[string]bool{
		"DISPLAY":                  true,
		"WAYLAND_DISPLAY":          true,
		"DBUS_SESSION_BUS_ADDRESS": true,
		"XDG_CURRENT_DESKTOP":      true,
		"XAUTHORITY":               true,
		"XDG_RUNTIME_DIR":          true,
	}
	resolved := map[string]string{}
	for _, line := range strings.Split(string(out), "\n") {
		k, v, ok := strings.Cut(line, "=")
		if !ok || !wanted[k] {
			continue
		}
		// show-environment shell-quotes some values; strip one quote layer.
		resolved[k] = strings.Trim(v, `"'`)
	}
	env := make([]string, 0, len(base)+len(resolved))
	for _, kv := range base {
		k, _, _ := strings.Cut(kv, "=")
		if _, override := resolved[k]; override {
			continue // drop the inherited copy; the resolved one is appended below
		}
		env = append(env, kv)
	}
	for k, v := range resolved {
		env = append(env, k+"="+v)
	}
	return env
}

// revealDBus asks the session file manager to highlight abs, via the
// org.freedesktop.FileManager1 interface (implemented by Nautilus, Dolphin,
// Thunar, Nemo, ...).
func revealDBus(ctx context.Context, env []string, abs string) error {
	gdbus, err := exec.LookPath("gdbus")
	if err != nil {
		return err
	}
	uri := (&url.URL{Scheme: "file", Path: abs}).String()
	// url.URL leaves ' unescaped in paths, but the URI is embedded in a
	// GVariant string literal below; %27 keeps the literal well-formed and
	// decodes back to ' on the file-manager side.
	uri = strings.ReplaceAll(uri, "'", "%27")
	cmd := exec.CommandContext(ctx, gdbus, "call", "--session",
		"--dest", "org.freedesktop.FileManager1",
		"--object-path", "/org/freedesktop/FileManager1",
		"--method", "org.freedesktop.FileManager1.ShowItems",
		"['"+uri+"']", "")
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("FileManager1.ShowItems: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
