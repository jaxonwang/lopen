//go:build unix

package daemon

import (
	"fmt"
	"net"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/jaxonwang/lopen/internal/config"
)

// listenLocal binds the per-host request endpoint the ssh tunnel forwards to,
// returning the listener and the forward target (the local half of the
// `-R 127.0.0.1:<port>:<target>` spec). On unix it is a 0600 unix socket
// under the state dir; only this user can connect to it locally, so it is the
// last line of defense behind the per-host token.
func listenLocal(cfg *config.Config, label string) (net.Listener, string, error) {
	sock := filepath.Join(cfg.SocketDir(), label+".sock")
	// Defense in depth beyond the instance lock: never unlink a socket that a
	// process is actively listening on. If a dial succeeds, someone is serving
	// it — refuse rather than steal the path (which would orphan the running
	// daemon's fd and break every open with "connection reset"). A refused or
	// failed dial means the socket is stale (or absent), so it is safe to
	// remove and rebind.
	if c, err := net.DialTimeout("unix", sock, 200*time.Millisecond); err == nil {
		c.Close()
		return nil, "", fmt.Errorf("socket %s is already in use by a live listener", sock)
	}
	_ = os.Remove(sock)
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", sock)
	syscall.Umask(old)
	return ln, sock, err
}
