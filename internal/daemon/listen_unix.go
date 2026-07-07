//go:build unix

package daemon

import (
	"net"
	"os"
	"path/filepath"
	"syscall"

	"github.com/jaxonwang/lopen/internal/config"
)

// listenLocal binds the per-host request endpoint the ssh tunnel forwards to,
// returning the listener and the forward target (the local half of the
// `-R 127.0.0.1:<port>:<target>` spec). On unix it is a 0600 unix socket
// under the state dir; only this user can connect to it locally, so it is the
// last line of defense behind the per-host token.
func listenLocal(cfg *config.Config, label string) (net.Listener, string, error) {
	sock := filepath.Join(cfg.SocketDir(), label+".sock")
	_ = os.Remove(sock)
	old := syscall.Umask(0o077)
	ln, err := net.Listen("unix", sock)
	syscall.Umask(old)
	return ln, sock, err
}
