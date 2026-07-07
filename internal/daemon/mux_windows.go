package daemon

// Win32-OpenSSH does not implement ControlMaster (mux needs an AF_UNIX
// listener, which its compat layer rejects — Win32-OpenSSH#1328), so no
// multiplexing options are emitted. The persistent tunnel's foreground -N
// still blocks for the life of the connection, and each pull-mode Exec opens
// its own ssh connection (paying a fresh handshake).
func masterMuxArgs(_, _ string) []string { return nil }
func clientMuxArgs(_, _ string) []string { return nil }
