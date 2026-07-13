//go:build unix

package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jaxonwang/lopen/internal/config"
)

// TestControlPathOptQuotedForSpaces guards against a regression where a state
// dir containing a space (the default macOS "~/Library/Application Support")
// broke the ssh `-o ControlPath=...` argument: ssh parses the option value like
// a config line and splits on whitespace unless the value is double-quoted,
// failing with "keyword controlpath extra arguments at end of line" and
// flapping the tunnel. Multiplexing (and thus ControlPath) is unix-only.
func TestControlPathOptQuotedForSpaces(t *testing.T) {
	tun := &Tunnel{Host: config.Host{Label: "devbox"}}
	ctlDir := "/Users/me/Library/Application Support/lopen/ctl"
	opt := controlPathOpt(ctlDir, tun.Host.Label)

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

// TestProvisionTimesOutOnHungSSH guards against the hang that wedged the daemon
// for days: provision runs synchronously at the top of every tunnel attempt, so
// if its ssh blocks (network gone after laptop sleep/wake), an unbounded
// provision stops the retry loop from ever bringing the tunnel up. The
// provisionTimeout ceiling must bound a genuinely-hung ssh under a LIVE context
// (not merely honor an already-cancelled one).
//
// `cat` with no closed stdin blocks forever, standing in for a hung ssh
// connection; the injected short ceiling must kill it and return an error well
// before the test's own deadline.
func TestProvisionTimesOutOnHungSSH(t *testing.T) {
	tun := &Tunnel{
		Host:                     config.Host{Label: "devbox", Dest: "example.com", RemotePort: 47654},
		SSHCommand:               "cat",
		provisionTimeoutOverride: 500 * time.Millisecond,
	}

	done := make(chan error, 1)
	start := time.Now()
	go func() { done <- tun.provision(context.Background()) }()

	select {
	case err := <-done:
		if err == nil {
			t.Fatal("expected a timeout error from the hung provision")
		}
		if elapsed := time.Since(start); elapsed > 5*time.Second {
			t.Fatalf("provision took %v — the timeout ceiling did not bound the hung ssh", elapsed)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("provision did not return — a hung ssh can wedge the retry loop")
	}
}

// TestConnArgsHasConnectTimeout locks in the ConnectTimeout safety option:
// without it a stalled connect after sleep/wake hangs instead of failing fast.
func TestConnArgsHasConnectTimeout(t *testing.T) {
	tun := &Tunnel{Host: config.Host{Label: "devbox"}}
	args := strings.Join(tun.connArgs(), " ")
	if !strings.Contains(args, "ConnectTimeout=") {
		t.Fatalf("connArgs missing ConnectTimeout: %s", args)
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
