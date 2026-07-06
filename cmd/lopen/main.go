// lopen is the remote-side command: `lopen <path>` opens that file or
// directory on your local Mac.
package main

import (
	"flag"
	"fmt"
	"os"

	"github.com/jaxonwang/lopen/internal/client"
)

func main() {
	var o client.Options
	var noWait bool
	flag.BoolVar(&noWait, "n", false, "fire and forget: return once the daemon accepts the request")
	flag.BoolVar(&o.Force, "force", false, "overwrite a locally-modified mirror copy / exceed the size cap")
	flag.BoolVar(&o.Reveal, "reveal", false, "reveal in Finder instead of opening")
	flag.StringVar(&o.App, "a", "", "open with a specific macOS application")
	flag.StringVar(&o.Label, "label", "", "origin label (default: from agent config)")
	flag.StringVar(&o.Agent, "agent", "", "agent config path (default: ~/.lopen/agent.json)")
	flag.StringVar(&o.Addr, "addr", "", "daemon address host:port (default: from agent config)")
	flag.Usage = func() {
		fmt.Fprintf(os.Stderr, `usage: lopen [flags] <path>

Opens <path> (file or directory) on your local Mac.
Blocks until the file has opened locally; -n to fire and forget.

flags:
`)
		flag.PrintDefaults()
	}
	flag.Parse()
	if flag.NArg() != 1 {
		flag.Usage()
		os.Exit(2)
	}
	o.Wait = !noWait

	if err := client.Open(flag.Arg(0), o); err != nil {
		fmt.Fprintf(os.Stderr, "lopen: %v\n", err)
		os.Exit(1)
	}
}
