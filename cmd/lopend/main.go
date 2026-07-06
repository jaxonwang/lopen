// lopend is the local (macOS) daemon: it holds reverse unix-socket tunnels
// to enrolled hosts, receives open requests, mirrors the content locally,
// and runs `open`.
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/jaxonwang/lopen/internal/config"
	"github.com/jaxonwang/lopen/internal/daemon"
)

func main() {
	cfgPath := flag.String("config", "", "config file (default ~/.config/lopen/config.json)")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lopend: %v\n", err)
		os.Exit(1)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(os.Stderr, "lopend: no hosts configured; add hosts to the config file")
		os.Exit(1)
	}

	srv, err := daemon.NewServer(cfg, log)
	if err != nil {
		fmt.Fprintf(os.Stderr, "lopend: %v\n", err)
		os.Exit(1)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		fmt.Fprintf(os.Stderr, "lopend: %v\n", err)
		os.Exit(1)
	}
}
