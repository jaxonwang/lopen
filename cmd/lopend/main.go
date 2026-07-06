// lopend is the local (macOS) daemon and management CLI. With no subcommand it
// runs the daemon: it holds reverse TCP tunnels to enrolled hosts, receives
// open requests, mirrors the content locally, and runs `open`.
//
// Subcommands:
//
//	lopend               run the daemon (also `lopend run`)
//	lopend setup <dest>  enroll a host (push binaries, add to config)
//	lopend install       install/refresh the launchd agent (from-source installs)
//	lopend uninstall     remove the launchd agent
//	lopend doctor        print config + connectivity diagnostics
package main

import (
	"context"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"syscall"

	"github.com/jaxonwang/lopen/internal/config"
	"github.com/jaxonwang/lopen/internal/daemon"
	"github.com/jaxonwang/lopen/internal/launchd"
	"github.com/jaxonwang/lopen/internal/setup"
)

func main() {
	if len(os.Args) < 2 {
		runDaemon(nil)
		return
	}
	switch os.Args[1] {
	case "run":
		runDaemon(os.Args[2:])
	case "setup":
		cmdSetup(os.Args[2:])
	case "install":
		cmdInstall(os.Args[2:])
	case "uninstall":
		cmdUninstall(os.Args[2:])
	case "doctor":
		cmdDoctor(os.Args[2:])
	case "-h", "--help", "help":
		usage()
	default:
		// Back-compat: `lopend -config ...` (flags, no subcommand) runs the daemon.
		if os.Args[1][0] == '-' {
			runDaemon(os.Args[1:])
			return
		}
		fmt.Fprintf(os.Stderr, "lopend: unknown subcommand %q\n", os.Args[1])
		usage()
		os.Exit(2)
	}
}

func usage() {
	fmt.Fprint(os.Stderr, `usage: lopend [subcommand]

  (none) | run       run the daemon (default; used by brew services / launchd)
  setup <dest>       enroll a host: push binaries and add it to the config
  install            install/refresh the launchd agent (from-source installs)
  uninstall          remove the launchd agent
  doctor             print config and connectivity diagnostics

Run 'lopend <subcommand> -h' for subcommand flags.
`)
}

func fatal(err error) {
	fmt.Fprintf(os.Stderr, "lopend: %v\n", err)
	os.Exit(1)
}

func runDaemon(args []string) {
	fs := flag.NewFlagSet("run", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default ~/.config/lopen/config.json)")
	_ = fs.Parse(args)

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	cfg, err := config.Load(*cfgPath)
	if err != nil {
		fatal(err)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Fprintln(os.Stderr, "lopend: no hosts configured; enroll one with `lopend setup <ssh-host>`")
		os.Exit(1)
	}

	srv, err := daemon.NewServer(cfg, log)
	if err != nil {
		fatal(err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := srv.Run(ctx); err != nil {
		fatal(err)
	}
}

func cmdSetup(args []string) {
	fs := flag.NewFlagSet("setup", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default ~/.config/lopen/config.json)")
	label := fs.String("label", "", "local label for this host (default: derived from destination)")
	port := fs.Int("port", 0, "remote loopback port (default: 47654)")
	keep := fs.Bool("keep", false, "pin this host's mirror so GC never evicts it")
	fs.Usage = func() {
		fmt.Fprint(os.Stderr, "usage: lopend setup [flags] <ssh-destination>\n\nflags:\n")
		fs.PrintDefaults()
	}
	_ = fs.Parse(args)
	if fs.NArg() != 1 {
		fs.Usage()
		os.Exit(2)
	}

	assets, err := setup.FindAssets()
	if err != nil {
		fatal(err)
	}
	cfg, err := config.LoadRaw(*cfgPath)
	if err != nil {
		fatal(err)
	}
	h, err := setup.Enroll(context.Background(), cfg, assets, setup.Options{
		Dest: fs.Arg(0), Label: *label, RemotePort: *port, Keep: *keep,
	})
	if err != nil {
		fatal(err)
	}
	if err := cfg.Save(); err != nil {
		fatal(err)
	}
	fmt.Printf("Saved to config. Restart the daemon to bring up the tunnel to %q:\n", h.Label)
	fmt.Println("  brew services restart lopen      # Homebrew")
	fmt.Println("  lopend install                   # from-source (launchd)")
}

func cmdInstall(args []string) {
	fs := flag.NewFlagSet("install", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file the agent will use (default ~/.config/lopen/config.json)")
	_ = fs.Parse(args)

	exe, err := os.Executable()
	if err != nil {
		fatal(err)
	}
	logPath, err := defaultLogPath()
	if err != nil {
		fatal(err)
	}
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		fatal(err)
	}
	cp := *cfgPath
	if cp == "" {
		cp = config.DefaultPath()
	}
	if err := launchd.Install(exe, cp, logPath); err != nil {
		fatal(err)
	}
	fmt.Printf("Installed and loaded the lopend LaunchAgent (%s).\n", launchd.Label)
	fmt.Printf("Logs: %s\n", logPath)
}

func cmdUninstall(args []string) {
	_ = flag.NewFlagSet("uninstall", flag.ExitOnError).Parse(args)
	if err := launchd.Uninstall(); err != nil {
		fatal(err)
	}
	fmt.Println("Removed the lopend LaunchAgent.")
}

func cmdDoctor(args []string) {
	fs := flag.NewFlagSet("doctor", flag.ExitOnError)
	cfgPath := fs.String("config", "", "config file (default ~/.config/lopen/config.json)")
	_ = fs.Parse(args)

	path := *cfgPath
	if path == "" {
		path = config.DefaultPath()
	}
	fmt.Printf("config: %s\n", path)
	cfg, err := config.Load(path)
	if err != nil {
		fmt.Printf("  ERROR: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("  mirror_dir: %s\n", cfg.MirrorDir)
	fmt.Printf("  state_dir:  %s\n", cfg.StateDir)
	fmt.Printf("  hosts: %d\n", len(cfg.Hosts))
	for _, h := range cfg.Hosts {
		fmt.Printf("    - %-16s dest=%s port=%d keep=%v\n", h.Label, h.Dest, h.RemotePort, h.Keep)
	}
	if len(cfg.Hosts) == 0 {
		fmt.Println("  (no hosts; enroll with `lopend setup <ssh-host>`)")
	}
}

func defaultLogPath() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, "Library", "Logs", "lopen", "lopend.log"), nil
}
