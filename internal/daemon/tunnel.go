package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jaxonwang/lopen/internal/config"
)

// Tunnel supervises one persistent `ssh -N -R` reverse unix-socket forward to
// a directly-enrolled host, restarting it with backoff when it dies (network
// change, laptop wake, remote reboot).
//
// Before each attempt the stale remote socket is removed: sshd only unlinks
// an existing socket if the REMOTE sshd_config sets StreamLocalBindUnlink,
// which we cannot set without root. ExitOnForwardFailure turns a failed bind
// into a fast exit → retry, instead of a silently useless session.
type Tunnel struct {
	Host      config.Host
	LocalSock string // the daemon's per-host request socket to forward to
	Log       *slog.Logger

	// SSHCommand overrides the ssh binary (tests).
	SSHCommand string
}

func (t *Tunnel) ssh() string {
	if t.SSHCommand != "" {
		return t.SSHCommand
	}
	return "ssh"
}

// controlPath returns the ControlMaster socket path for this host. Kept
// short: unix socket paths have a ~104-byte limit on macOS.
func (t *Tunnel) controlPath(ctlDir string) string {
	return filepath.Join(ctlDir, t.Host.Label+".ctl")
}

// baseArgs are the safety-critical ssh options shared by the tunnel and any
// exec over the same connection. Host.Dest is validated against destRe at
// config load, and "--" precedes it everywhere, so it can never be parsed as
// an option.
func (t *Tunnel) baseArgs(ctlDir string) []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
		"-o", "ControlMaster=auto",
		"-o", "ControlPersist=yes",
		"-o", "ControlPath=" + t.controlPath(ctlDir),
	}
}

func (t *Tunnel) Run(ctx context.Context, ctlDir string) {
	backoff := time.Second
	const maxBackoff = 60 * time.Second
	for ctx.Err() == nil {
		start := time.Now()
		err := t.once(ctx, ctlDir)
		if ctx.Err() != nil {
			return
		}
		if time.Since(start) > 2*time.Minute {
			backoff = time.Second // it held for a while; reset
		}
		t.Log.Warn("tunnel down, retrying", "host", t.Host.Label, "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff *= 2; backoff > maxBackoff {
			backoff = maxBackoff
		}
	}
}

func (t *Tunnel) once(ctx context.Context, ctlDir string) error {
	// Pre-clean the stale remote socket, then ensure its parent dir exists.
	// sshd only unlinks an existing -R socket if the remote sshd_config sets
	// StreamLocalBindUnlink, which we can't change without root. RemoteSocket
	// is a config value (never request-derived) and is validated at config
	// load; it is still single-quoted here because it is interpolated into a
	// remote shell string that ssh hands to the login shell.
	rmCmd := fmt.Sprintf("rm -f -- %s && mkdir -p -- %s", shQuote(t.Host.RemoteSocket), shQuote(filepath.Dir(t.Host.RemoteSocket)))
	pre := exec.CommandContext(ctx, t.ssh(), append(t.baseArgs(ctlDir), "--", t.Host.Dest, rmCmd)...)
	pre.Stdout, pre.Stderr = os.Stderr, os.Stderr
	if err := pre.Run(); err != nil {
		return fmt.Errorf("pre-clean: %w", err)
	}

	args := append(t.baseArgs(ctlDir),
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-R", t.Host.RemoteSocket+":"+t.LocalSock,
		"--", t.Host.Dest,
	)
	cmd := exec.CommandContext(ctx, t.ssh(), args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	t.Log.Info("tunnel up", "host", t.Host.Label)
	return cmd.Run()
}

// Exec runs a command on the tunnel's host over the multiplexed connection,
// returning a started *exec.Cmd whose stdout the caller streams. Used for
// pull-mode transfers (`cat` / `tar`). remoteCmd is built by the caller from
// validated components only.
func (t *Tunnel) Exec(ctx context.Context, ctlDir string, remoteCmd string) *exec.Cmd {
	args := append(t.baseArgs(ctlDir), "--", t.Host.Dest, remoteCmd)
	return exec.CommandContext(ctx, t.ssh(), args...)
}

// shQuote single-quotes s for POSIX sh, the only safe generic quoting.
func shQuote(s string) string {
	out := []byte{'\''}
	for i := 0; i < len(s); i++ {
		if s[i] == '\'' {
			out = append(out, `'\''`...)
			continue
		}
		out = append(out, s[i])
	}
	out = append(out, '\'')
	return string(out)
}
