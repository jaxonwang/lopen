package daemon

import (
	"context"

	"github.com/jaxonwang/lopen/internal/protocol"
)

func openPlatform(ctx context.Context, req *protocol.Request, abs string) error {
	return runOpen(ctx, nil, "/usr/bin/open", macOpenArgs(req, abs)...)
}
