package daemon

import (
	"context"
	"fmt"
	"os/exec"
	"strings"

	"github.com/jaxonwang/lopen/internal/protocol"
)

// open runs the platform opener on the mirrored path. abs is
// daemon-constructed (mirror root + validated rel); req.App passed appRe.
// Everything is argv, never a shell.
func (s *Server) open(ctx context.Context, req *protocol.Request, abs string) error {
	if s.OpenCommand != "" {
		// Test hook: emulate the macOS argv shape regardless of host OS.
		return runOpen(ctx, nil, s.OpenCommand, macOpenArgs(req, abs)...)
	}
	return openPlatform(ctx, req, abs)
}

// macOpenArgs builds the argv tail for macOS /usr/bin/open (also used by the
// OpenCommand test hook on every platform).
func macOpenArgs(req *protocol.Request, abs string) []string {
	var args []string
	if req.Op == protocol.OpReveal {
		args = append(args, "-R")
	}
	if req.App != "" {
		args = append(args, "-a", req.App)
	}
	return append(args, "--", abs)
}

// runOpen executes an opener command, surfacing its output on failure.
// env == nil inherits the daemon's environment.
func runOpen(ctx context.Context, env []string, bin string, args ...string) error {
	cmd := exec.CommandContext(ctx, bin, args...)
	cmd.Env = env
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("open failed: %v: %s", err, strings.TrimSpace(string(out)))
	}
	return nil
}
