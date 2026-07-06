package daemon

import (
	"strings"
	"testing"

	"github.com/jaxonwang/lopen/internal/config"
)

// TestControlPathOptQuotedForSpaces guards against a regression where a state
// dir containing a space (the default macOS "~/Library/Application Support")
// broke the ssh `-o ControlPath=...` argument: ssh parses the option value like
// a config line and splits on whitespace unless the value is double-quoted,
// failing with "keyword controlpath extra arguments at end of line" and
// flapping the tunnel.
func TestControlPathOptQuotedForSpaces(t *testing.T) {
	tun := &Tunnel{Host: config.Host{Label: "devbox"}}
	ctlDir := "/Users/me/Library/Application Support/lopen/ctl"
	opt := tun.controlPathOpt(ctlDir)

	if !strings.HasPrefix(opt, `ControlPath="`) || !strings.HasSuffix(opt, `"`) {
		t.Fatalf("ControlPath value not double-quoted: %s", opt)
	}
	// The quoted value must contain the full path including the space, so ssh
	// treats it as a single token.
	if !strings.Contains(opt, "Application Support") {
		t.Fatalf("ControlPath lost the spaced path: %s", opt)
	}
	if !strings.HasSuffix(opt, `.ctl"`) {
		t.Fatalf("ControlPath does not end at the .ctl socket: %s", opt)
	}

	// Master and client must rendezvous on the same ControlPath token, or
	// pull-mode Exec would open a second connection instead of multiplexing.
	master := tun.masterArgs(ctlDir)
	client := tun.clientArgs(ctlDir)
	if cp(master) != cp(client) {
		t.Fatalf("master/client ControlPath differ: %q vs %q", cp(master), cp(client))
	}
	if cp(master) != opt {
		t.Fatalf("masterArgs ControlPath %q != controlPathOpt %q", cp(master), opt)
	}
}

// cp returns the value following the ControlPath -o flag in an ssh arg list.
func cp(args []string) string {
	for _, a := range args {
		if strings.HasPrefix(a, "ControlPath=") {
			return a
		}
	}
	return ""
}
