package daemon

import (
	"net"

	"github.com/jaxonwang/lopen/internal/config"
)

// listenLocal binds the per-host request endpoint the ssh tunnel forwards to.
//
// Windows' bundled ssh.exe cannot dial a local AF_UNIX socket for -R (Win32-
// OpenSSH emulates unix sockets as named pipes and fails on real ones —
// PowerShell/Win32-OpenSSH#1564), so on Windows the local half is an
// ephemeral loopback TCP port: the forward becomes pure TCP↔TCP
// (`-R 127.0.0.1:<remote>:127.0.0.1:<local>`). Loopback TCP is reachable by
// other local users on a shared machine, but the per-host token (checked in
// handle) gates every request regardless of transport.
func listenLocal(_ *config.Config, _ string) (net.Listener, string, error) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return nil, "", err
	}
	return ln, ln.Addr().String(), nil
}
