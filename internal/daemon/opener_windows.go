package daemon

import (
	"context"
	"errors"
	"os/exec"

	"github.com/jaxonwang/lopen/internal/protocol"
)

func openPlatform(ctx context.Context, req *protocol.Request, abs string) error {
	if req.App != "" {
		return errors.New("-a <App> is only supported when the local machine is a Mac")
	}
	if req.Op == protocol.OpReveal {
		// explorer.exe exits nonzero even on success, so run it fire-and-
		// forget: reaching Start means the request was handed to the shell.
		// Not bound to ctx — the caller cancels the request context right
		// after this returns, which would otherwise kill explorer before the
		// shell has taken over the /select handoff.
		cmd := exec.Command("explorer.exe", "/select,"+abs)
		if err := cmd.Start(); err != nil {
			return err
		}
		go cmd.Wait()
		return nil
	}
	// rundll32 FileProtocolHandler resolves the default handler via the
	// shell, takes the path as a plain argv element (no cmd.exe quoting
	// hazards), and needs no console window.
	return runOpen(ctx, nil, "rundll32", "url.dll,FileProtocolHandler", abs)
}
