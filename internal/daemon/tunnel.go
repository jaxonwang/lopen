package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"time"

	"github.com/jaxonwang/lopen/internal/config"
	"github.com/jaxonwang/lopen/internal/protocol"
)

// Tunnel supervises one persistent `ssh -N -R` reverse forward to a
// directly-enrolled host, restarting it with backoff when it dies (network
// change, laptop wake, remote reboot).
//
// The remote end is a loopback TCP port (127.0.0.1:<port>), not a UNIX socket:
// Amazon-managed sshd refuses UNIX-socket reverse forwards
// (AllowStreamLocalForwarding), but forwarding a remote TCP port to a local
// UNIX socket is permitted. ExitOnForwardFailure turns a failed bind into a
// fast exit → retry, instead of a silently useless session. sshd cleans up its
// own loopback listener on disconnect, so no remote pre-clean is needed.
//
// Because a loopback TCP port is reachable by any local user on the remote host
// (unlike a 0600 socket), the daemon provisions a 0600 agent config on the
// remote holding a per-host token; the client must present it to be honored.
type Tunnel struct {
	Host      config.Host
	LocalSock string // the daemon's per-host request socket to forward to
	Token     string // per-host auth token written into the remote agent config
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

// connArgs are the safety-critical ssh options common to every connection.
// Host.Dest is validated against destRe at config load, and "--" precedes it
// everywhere, so it can never be parsed as an option.
func (t *Tunnel) connArgs() []string {
	return []string{
		"-o", "BatchMode=yes",
		"-o", "ServerAliveInterval=30",
		"-o", "ServerAliveCountMax=3",
	}
}

// masterArgs make this connection the ControlMaster: the persistent tunnel
// (ssh -N -R) forces ControlMaster=yes and holds the connection open in the
// foreground, so pull-mode Exec can multiplex over it via the same ControlPath.
//
// ControlPersist is deliberately NOT set: with it, ssh backgrounds the master
// and the foreground -N returns immediately (nil error), which the supervisor
// misreads as the tunnel dying — an endless up/down flap while a detached
// master silently holds the forward. Without persist, the foreground -N blocks
// for the life of the connection, and when it dies the master dies with it.
func (t *Tunnel) masterArgs(ctlDir string) []string {
	return append(t.connArgs(),
		"-o", "ControlMaster=yes",
		"-o", "ControlPath="+t.controlPath(ctlDir),
	)
}

// clientArgs multiplex a short-lived command over the tunnel's master if it is
// up, without ever promoting themselves to master (ControlMaster=no).
func (t *Tunnel) clientArgs(ctlDir string) []string {
	return append(t.connArgs(),
		"-o", "ControlMaster=no",
		"-o", "ControlPath="+t.controlPath(ctlDir),
	)
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
	// Provision the remote agent config (~/.lopen/agent.json, 0600) over the
	// multiplexed connection before bringing up the forward. The client reads
	// it to learn its label, the loopback port to dial, and the token to
	// authenticate with. Writing it atomically (temp + mv) and chmod 0600 keeps
	// the token unreadable by other local users. All interpolated values are
	// daemon-controlled (validated config + our own token), but we still
	// single-quote everything handed to the remote shell.
	if err := t.provision(ctx); err != nil {
		return fmt.Errorf("provision: %w", err)
	}

	// Forward a remote loopback TCP port to our local UNIX socket. Binding on
	// 127.0.0.1 keeps the listener off the network; only processes on the
	// remote host can reach it, and the token gates them.
	fwd := fmt.Sprintf("127.0.0.1:%d:%s", t.Host.RemotePort, t.LocalSock)
	args := append(t.masterArgs(ctlDir),
		"-N",
		"-o", "ExitOnForwardFailure=yes",
		"-R", fwd,
		"--", t.Host.Dest,
	)
	cmd := exec.CommandContext(ctx, t.ssh(), args...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	t.Log.Info("tunnel up", "host", t.Host.Label, "remote_port", t.Host.RemotePort)
	return cmd.Run()
}

// provision writes the remote agent config atomically at ~/.lopen/agent.json
// with mode 0600. The remote shell command is built entirely from
// daemon-controlled values (validated label, config port, our minted token),
// each single-quoted.
//
// It uses a standalone connection (connArgs, no ControlMaster): a multiplexed
// provision would create the master and cause the tunnel's -N to return
// immediately, flapping the tunnel.
func (t *Tunnel) provision(ctx context.Context) error {
	agent := protocol.AgentConfig{Label: t.Host.Label, Port: t.Host.RemotePort, Token: t.Token}
	blob, err := json.Marshal(agent)
	if err != nil {
		return err
	}
	// Write via a heredoc-free, injection-safe pipeline: printf the
	// single-quoted JSON into a temp file, chmod, then atomically rename.
	// chmod 700 ~/.lopen is explicit (not just umask): if the directory
	// pre-exists with looser perms, a group/other-writable ~/.lopen would let
	// another local user pre-create agent.json.tmp as a symlink or read the
	// token — the 0600 file mode is only as strong as the directory holding it.
	remoteCmd := fmt.Sprintf(
		"umask 077 && mkdir -p ~/.lopen && chmod 700 ~/.lopen && printf %%s %s > ~/.lopen/agent.json.tmp && chmod 600 ~/.lopen/agent.json.tmp && mv -f ~/.lopen/agent.json.tmp ~/.lopen/agent.json",
		shQuote(string(blob)),
	)
	cmd := exec.CommandContext(ctx, t.ssh(), append(t.connArgs(), "--", t.Host.Dest, remoteCmd)...)
	cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
	return cmd.Run()
}

// Exec runs a command on the tunnel's host over the multiplexed connection,
// returning a started *exec.Cmd whose stdout the caller streams. Used for
// pull-mode transfers (`cat` / `tar`). remoteCmd is built by the caller from
// validated components only.
func (t *Tunnel) Exec(ctx context.Context, ctlDir string, remoteCmd string) *exec.Cmd {
	args := append(t.clientArgs(ctlDir), "--", t.Host.Dest, remoteCmd)
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
