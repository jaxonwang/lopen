// Package client implements the remote-side `lopen` logic: resolve a path,
// connect to the forwarded unix socket, send the request + payload, wait for
// the daemon's verdict.
package client

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/jaxonwang/lopen/internal/protocol"
	"github.com/jaxonwang/lopen/internal/tarstream"
)

type Options struct {
	Agent   string // path to the agent config; default ~/.lopen/agent.json
	Addr    string // dial address override (host:port); default from agent config
	Label   string // origin label override; default from agent config
	Token   string // token override; default from agent config
	Wait    bool
	Force   bool
	Reveal  bool
	App     string
	Retry   time.Duration // how long to retry a missing/refusing endpoint
	MaxSize int64         // refuse to send more than this many bytes
	Stderr  io.Writer
}

// DefaultAgentPath is the remote agent config the daemon provisions.
func DefaultAgentPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, ".lopen", "agent.json")
}

// loadAgent reads the daemon-provisioned agent config.
func loadAgent(path string) (*protocol.AgentConfig, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, fmt.Errorf(
				"no lopen agent config at %s\nThis host is not enrolled, or the tunnel from your Mac is not up.\nIf this is a chained hop, reconnect it with `lssh`; if it is a direct host, check `lopend` on your Mac", path)
		}
		return nil, err
	}
	var a protocol.AgentConfig
	if err := json.Unmarshal(b, &a); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if a.Label == "" || a.Token == "" {
		return nil, fmt.Errorf("%s: incomplete agent config", path)
	}
	if a.Port < 1 || a.Port > 65535 {
		return nil, fmt.Errorf("%s: invalid port %d in agent config", path, a.Port)
	}
	return &a, nil
}

// Open sends one path. Returns nil once the daemon reports the local open
// succeeded (or, with Wait=false, once the daemon acknowledged receipt).
func Open(path string, o Options) error {
	// Resolve label/addr/token from the daemon-provisioned agent config unless
	// explicitly overridden. Reading the config only when a value is missing
	// lets tests inject everything directly.
	if o.Label == "" || o.Addr == "" || o.Token == "" {
		agentPath := o.Agent
		if agentPath == "" {
			agentPath = DefaultAgentPath()
		}
		a, err := loadAgent(agentPath)
		if err != nil {
			return err
		}
		if o.Label == "" {
			o.Label = a.Label
		}
		if o.Addr == "" {
			o.Addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(a.Port))
		}
		if o.Token == "" {
			o.Token = a.Token
		}
	}
	if !protocol.ValidLabel(o.Label) {
		return fmt.Errorf("label %q from agent config is invalid", o.Label)
	}
	if o.Stderr == nil {
		o.Stderr = os.Stderr
	}
	if o.MaxSize == 0 {
		o.MaxSize = protocol.DefaultMaxPayload
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	// Resolve symlinks so the daemon's mirror key is the real path — two
	// links to the same file share one mirror slot, and a dangling link
	// fails here with a clear message instead of on the Mac.
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("cannot resolve %s: %w", path, err)
	}
	fi, err := os.Stat(abs)
	if err != nil {
		return err
	}
	size := fi.Size()
	if fi.IsDir() {
		if size, err = dirSize(abs); err != nil {
			return err
		}
	}
	if size > o.MaxSize && !o.Force {
		return fmt.Errorf("%s is %d bytes (limit %d); re-run with --force", path, size, o.MaxSize)
	}

	conn, err := dialRetry(o.Addr, o.Retry)
	if err != nil {
		return err
	}
	defer conn.Close()

	host, _ := os.Hostname()
	op := protocol.OpOpen
	if o.Reveal {
		op = protocol.OpReveal
	}
	req := &protocol.Request{
		V:      protocol.Version,
		Op:     op,
		Origin: protocol.Origin{Host: host, Label: o.Label},
		Path:   abs,
		Dir:    fi.IsDir(),
		Wait:   o.Wait,
		Force:  o.Force,
		App:    o.App,
		Size:   size,
		Mode:   protocol.ModeInline,
		Token:  o.Token,
	}
	if err := protocol.WriteRequest(conn, req); err != nil {
		return err
	}

	br := bufio.NewReader(conn)
	// The daemon validates before accepting payload; surface an early error
	// (e.g. locally-modified guard) before we ship bytes.
	ev, err := protocol.ReadEvent(br)
	if err != nil {
		return fmt.Errorf("daemon: %w", err)
	}
	if ev.Event == "done" {
		return doneToErr(ev)
	}

	cw := protocol.NewChunkWriter(conn)
	if fi.IsDir() {
		skipped, err := tarstream.Pack(abs, cw)
		if err != nil {
			return fmt.Errorf("packing %s: %w", path, err)
		}
		if skipped > 0 {
			fmt.Fprintf(o.Stderr, "lopen: skipped %d non-regular entries (symlinks, sockets, ...)\n", skipped)
		}
	} else {
		f, err := os.Open(abs)
		if err != nil {
			return err
		}
		_, err = io.Copy(cw, f)
		f.Close()
		if err != nil {
			return err
		}
	}
	if err := cw.Close(); err != nil {
		return err
	}

	if !o.Wait {
		return nil
	}
	for {
		ev, err := protocol.ReadEvent(br)
		if err != nil {
			return fmt.Errorf("connection lost waiting for open confirmation: %w", err)
		}
		if ev.Event == "done" {
			return doneToErr(ev)
		}
		if ev.Detail != "" {
			fmt.Fprintf(o.Stderr, "lopen: %s\n", ev.Detail)
		}
	}
}

func doneToErr(ev *protocol.Event) error {
	if ev.Status == "ok" {
		return nil
	}
	return errors.New(ev.Error)
}

// dialRetry retries for the configured window: right after laptop wake the
// tunnel needs a few seconds to re-establish.
func dialRetry(addr string, window time.Duration) (net.Conn, error) {
	if window == 0 {
		window = 10 * time.Second
	}
	deadline := time.Now().Add(window)
	var lastErr error
	for {
		conn, err := net.DialTimeout("tcp", addr, 2*time.Second)
		if err == nil {
			return conn, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			break
		}
		time.Sleep(500 * time.Millisecond)
	}
	return nil, fmt.Errorf(
		"cannot reach lopen daemon at %s (%v)\nThe reverse tunnel from your Mac may be down.\nIf this is a chained hop, reconnect it with `lssh`; if it is a direct host, check `lopend` on your Mac", addr, lastErr)
}

func dirSize(root string) (int64, error) {
	var total int64
	err := filepath.WalkDir(root, func(_ string, d os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.Type().IsRegular() {
			if fi, err := d.Info(); err == nil {
				total += fi.Size()
			}
		}
		return nil
	})
	return total, err
}
