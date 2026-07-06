// Package setup implements Mac-side host enrollment: push the remote `lopen`
// binary and `lssh` wrapper to a host, and record it in the config.
package setup

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/jaxonwang/lopen/internal/config"
)

// Assets locates the files pushed to remote hosts. The linux `lopen` binaries
// are named lopen-linux-<arch>; lssh is a shell script.
type Assets struct {
	// Dir holds lopen-linux-amd64, lopen-linux-arm64, and lssh. Resolved from
	// $LOPEN_LIBEXEC, else the directory of the running executable, else ./dist.
	Dir string
}

// FindAssets resolves the asset directory. Homebrew sets LOPEN_LIBEXEC; a
// from-source run falls back to the executable's own directory or ./dist.
func FindAssets() (*Assets, error) {
	if d := os.Getenv("LOPEN_LIBEXEC"); d != "" {
		return &Assets{Dir: d}, nil
	}
	if exe, err := os.Executable(); err == nil {
		dir := filepath.Dir(exe)
		if hasAssets(dir) {
			return &Assets{Dir: dir}, nil
		}
		// From-source layout: binaries land in ./dist next to the repo.
		if dist := filepath.Join(dir, "dist"); hasAssets(dist) {
			return &Assets{Dir: dist}, nil
		}
	}
	if hasAssets("dist") {
		return &Assets{Dir: "dist"}, nil
	}
	return nil, fmt.Errorf("cannot locate lopen assets; set LOPEN_LIBEXEC to the directory holding lopen-linux-* and lssh")
}

func hasAssets(dir string) bool {
	_, err := os.Stat(filepath.Join(dir, "lssh"))
	return err == nil
}

// remoteArch maps `uname -m` output to our binary arch suffix.
func remoteArch(m string) (string, error) {
	switch strings.TrimSpace(m) {
	case "x86_64", "amd64":
		return "amd64", nil
	case "aarch64", "arm64":
		return "arm64", nil
	default:
		return "", fmt.Errorf("unsupported remote architecture %q", m)
	}
}

// Options controls one enrollment.
type Options struct {
	Dest       string // ssh destination
	Label      string // local label; default derived from Dest
	RemotePort int    // 0 → config default
	Keep       bool
	SSH        string // ssh binary override (tests)
	SCP        string // scp binary override (tests)
	Stdout     *os.File
}

// Enroll pushes the assets to the host and adds it to cfg (caller saves).
func Enroll(ctx context.Context, cfg *config.Config, a *Assets, o Options) (config.Host, error) {
	var h config.Host
	if o.SSH == "" {
		o.SSH = "ssh"
	}
	if o.SCP == "" {
		o.SCP = "scp"
	}
	if o.Stdout == nil {
		o.Stdout = os.Stdout
	}
	if !config.ValidDest(o.Dest) {
		return h, fmt.Errorf("invalid ssh destination %q", o.Dest)
	}
	label := o.Label
	if label == "" {
		label = deriveLabel(o.Dest)
	}

	fmt.Fprintf(o.Stdout, "Detecting remote architecture on %s ...\n", o.Dest)
	unameOut, err := runOut(ctx, o.SSH, sshBatch("--", o.Dest, "uname -m; mkdir -p ~/bin")...)
	if err != nil {
		return h, fmt.Errorf("ssh to %s failed (need passwordless ssh): %w", o.Dest, err)
	}
	arch, err := remoteArch(firstLine(unameOut))
	if err != nil {
		return h, err
	}

	bin := filepath.Join(a.Dir, "lopen-linux-"+arch)
	if _, err := os.Stat(bin); err != nil {
		return h, fmt.Errorf("missing binary %s: %w", bin, err)
	}
	lssh := filepath.Join(a.Dir, "lssh")

	fmt.Fprintf(o.Stdout, "Pushing lopen (%s) and lssh to %s:~/bin ...\n", arch, o.Dest)
	// scp to ~/bin; the shell on the far side expands ~.
	if err := run(ctx, o.SCP, "-q", "-o", "BatchMode=yes", bin, o.Dest+":bin/lopen"); err != nil {
		return h, fmt.Errorf("scp lopen: %w", err)
	}
	if err := run(ctx, o.SCP, "-q", "-o", "BatchMode=yes", lssh, o.Dest+":bin/lssh"); err != nil {
		return h, fmt.Errorf("scp lssh: %w", err)
	}
	if err := run(ctx, o.SSH, sshBatch("--", o.Dest, "chmod +x ~/bin/lopen ~/bin/lssh")...); err != nil {
		return h, fmt.Errorf("chmod remote binaries: %w", err)
	}

	h = config.Host{Label: label, Dest: o.Dest, RemotePort: o.RemotePort, Keep: o.Keep}
	if err := cfg.AddHost(h); err != nil {
		return h, err
	}
	fmt.Fprintf(o.Stdout, "Enrolled %s as label %q.\n", o.Dest, label)
	return h, nil
}

// deriveLabel turns an ssh destination into a usable label: strip any user@,
// take the first dot-component, and replace stray characters.
func deriveLabel(dest string) string {
	s := dest
	if i := strings.IndexByte(s, '@'); i >= 0 {
		s = s[i+1:]
	}
	if i := strings.IndexByte(s, '.'); i >= 0 {
		s = s[:i]
	}
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('-')
		}
	}
	out := b.String()
	if out == "" || !(out[0] >= 'A' && out[0] <= 'Z' || out[0] >= 'a' && out[0] <= 'z' || out[0] >= '0' && out[0] <= '9' || out[0] == '_') {
		out = "host" + out
	}
	if len(out) > 64 {
		out = out[:64]
	}
	return out
}

func sshBatch(args ...string) []string {
	return append([]string{"-o", "BatchMode=yes"}, args...)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}

func run(ctx context.Context, name string, args ...string) error {
	cmd := exec.CommandContext(ctx, name, args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

func runOut(ctx context.Context, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	var sb strings.Builder
	cmd.Stdout = &sb
	cmd.Stderr = os.Stderr
	err := cmd.Run()
	return sb.String(), err
}
