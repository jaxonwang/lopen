package daemon

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/jaxonwang/lopen/internal/config"
	"github.com/jaxonwang/lopen/internal/index"
	"github.com/jaxonwang/lopen/internal/mirror"
	"github.com/jaxonwang/lopen/internal/protocol"
	"github.com/jaxonwang/lopen/internal/tarstream"
)

// Server is the daemon core: it owns the per-host request sockets, the
// mirror, and the tunnels.
type Server struct {
	Cfg *config.Config
	Log *slog.Logger

	Mir     *mirror.Mirror
	tunnels map[string]*Tunnel
	tokens  *tokenStore

	// OpenCommand overrides the platform opener (tests).
	OpenCommand string

	// requestTimeout bounds one whole request (header, payload, open).
	requestTimeout time.Duration

	// sem bounds concurrent in-flight requests so a hostile peer cannot
	// exhaust local disk with many parallel max-size staging transfers.
	sem chan struct{}

	slotMu keyedMutex // serializes stage+promote+open for a given mirror slot
}

// keyedMutex gives one mutex per string key so concurrent requests for
// different mirror slots don't block each other, but two requests for the
// same slot serialize (avoiding a Promote race). Entries are reference
// counted and removed on final unlock, so the map cannot grow without bound
// as a hostile peer streams requests with unique paths.
type keyedMutex struct {
	mu    sync.Mutex
	locks map[string]*refMutex
}

type refMutex struct {
	mu   sync.Mutex
	refs int
}

func (k *keyedMutex) Lock(key string) func() {
	k.mu.Lock()
	if k.locks == nil {
		k.locks = map[string]*refMutex{}
	}
	rm, ok := k.locks[key]
	if !ok {
		rm = &refMutex{}
		k.locks[key] = rm
	}
	rm.refs++
	k.mu.Unlock()

	rm.mu.Lock()
	return func() {
		rm.mu.Unlock()
		k.mu.Lock()
		if rm.refs--; rm.refs == 0 {
			delete(k.locks, key)
		}
		k.mu.Unlock()
	}
}

func NewServer(cfg *config.Config, log *slog.Logger) (*Server, error) {
	for _, d := range []string{cfg.MirrorDir, cfg.StateDir, cfg.SocketDir(), cfg.ControlDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	idx, err := index.Load(cfg.IndexPath())
	if err != nil {
		return nil, err
	}
	tokens, err := loadTokens(cfg.TokensPath())
	if err != nil {
		return nil, err
	}
	return &Server{
		Cfg:            cfg,
		Log:            log,
		Mir:            &mirror.Mirror{Root: cfg.MirrorDir, Idx: idx},
		tunnels:        map[string]*Tunnel{},
		tokens:         tokens,
		requestTimeout: 10 * time.Minute,
		sem:            make(chan struct{}, maxConcurrentRequests),
	}, nil
}

// maxConcurrentRequests bounds in-flight requests across all hosts. Excess
// connections wait for a slot; combined with the per-request payload cap this
// bounds transient staging disk to maxConcurrentRequests * MaxPayloadBytes.
const maxConcurrentRequests = 8

// maxArchiveEntries caps the number of members in a directory tar so a
// hostile archive of millions of empty files cannot exhaust inodes.
const maxArchiveEntries = 200_000

// Run starts one listener + tunnel per configured host and blocks until ctx
// is done.
func (s *Server) Run(ctx context.Context) error {
	var wg sync.WaitGroup

	// Build every tunnel and listener up front so s.tunnels is fully
	// populated before any accept loop starts — the map is then read-only
	// for the daemon's lifetime and needs no lock.
	type bound struct {
		t  *Tunnel
		ln net.Listener
	}
	var bounds []bound
	for _, h := range s.Cfg.Hosts {
		ln, fwd, err := listenLocal(s.Cfg, h.Label)
		if err != nil {
			return fmt.Errorf("host %s: %w", h.Label, err)
		}
		token, err := s.tokens.ensure(h.Label)
		if err != nil {
			return fmt.Errorf("host %s: %w", h.Label, err)
		}
		t := &Tunnel{Host: h, LocalSock: fwd, Token: token, Log: s.Log}
		s.tunnels[h.Label] = t
		bounds = append(bounds, bound{t: t, ln: ln})
	}

	for _, b := range bounds {
		b := b
		wg.Add(2)
		go func() { defer wg.Done(); b.t.Run(ctx, s.Cfg.ControlDir()) }()
		go func() {
			defer wg.Done()
			<-ctx.Done()
			b.ln.Close()
		}()
		go s.acceptLoop(ctx, b.ln, b.t)
	}

	// Daily GC plus one at startup.
	wg.Add(1)
	go func() {
		defer wg.Done()
		s.runGC()
		tick := time.NewTicker(24 * time.Hour)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				s.runGC()
			}
		}
	}()

	<-ctx.Done()
	wg.Wait()
	return nil
}

func (s *Server) runGC() {
	pinned := map[string]bool{}
	for _, h := range s.Cfg.Hosts {
		if h.Keep {
			pinned[h.Label] = true
		}
	}
	res := s.Mir.GC(time.Duration(s.Cfg.TTLDays)*24*time.Hour, s.Cfg.MaxMirrorBytes, pinned)
	if len(res.Removed) > 0 || len(res.Retained) > 0 {
		s.Log.Info("gc", "removed", len(res.Removed), "freed_bytes", res.Bytes, "retained_modified", res.Retained)
	}
}

func (s *Server) acceptLoop(ctx context.Context, ln net.Listener, t *Tunnel) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		select {
		case s.sem <- struct{}{}:
		case <-ctx.Done():
			conn.Close()
			return
		}
		go func() {
			defer func() { <-s.sem }()
			defer conn.Close()
			cctx, cancel := context.WithTimeout(ctx, s.requestTimeout)
			defer cancel()
			if err := s.handle(cctx, conn, t); err != nil {
				s.Log.Warn("request failed", "tunnel", t.Host.Label, "err", err)
			}
		}()
	}
}

// handle processes one request. t is the tunnel the connection arrived
// through — with chaining the true origin may differ, but a request is only
// honored when its origin label matches this tunnel's host, so the daemon
// only ever fetches from the host it reached through that socket.
func (s *Server) handle(ctx context.Context, conn net.Conn, t *Tunnel) error {
	tunnelLabel := t.Host.Label
	if dl, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(dl)
	}
	br := bufio.NewReaderSize(conn, protocol.MaxHeaderBytes)
	req, err := protocol.ReadRequest(br)
	if err != nil {
		return err
	}
	fail := func(err error) error {
		_ = protocol.WriteEvent(conn, protocol.Event{Event: "done", Status: "error", Error: err.Error()})
		return err
	}
	if err := req.Validate(s.Cfg.MaxPayloadBytes); err != nil {
		return fail(err)
	}

	// The origin label is attacker-controlled. It must (a) name a host we
	// actually enrolled — so it can't mint arbitrary mirror directories —
	// and (b) match the tunnel the request arrived on. Without (b) a
	// compromised host A could set origin.label="B" and write forged content
	// into host B's mirror slot, spoofing a host the user trusts. A request
	// from a chained inner host still arrives via the nearest direct host's
	// tunnel and is attributed to that host, which is the honest identity we
	// can actually attest to.
	if s.Cfg.HostByLabel(req.Origin.Label) == nil {
		return fail(fmt.Errorf("unknown origin label %q", req.Origin.Label))
	}
	if req.Origin.Label != tunnelLabel {
		return fail(fmt.Errorf("origin %q does not match tunnel host %q", req.Origin.Label, tunnelLabel))
	}

	// The remote transport is a loopback TCP port any local user on the remote
	// host can connect to, so socket file mode no longer gates access. Require
	// the per-host token, which only a client that can read the 0600 remote
	// agent config can supply.
	if !s.tokens.valid(tunnelLabel, req.Token) {
		return fail(fmt.Errorf("authentication failed for host %q", tunnelLabel))
	}

	rel, err := mirror.Rel(req.Origin.Label, req.Path)
	if err != nil {
		return fail(err)
	}

	// Serialize everything touching this mirror slot: two requests for the
	// same rel must not race in Promote.
	unlock := s.slotMu.Lock(rel)
	defer unlock()

	if err := s.Mir.CheckOverwrite(rel, req.Force); err != nil {
		return fail(err)
	}
	_ = protocol.WriteEvent(conn, protocol.Event{Event: "received"})

	staged, err := s.Mir.StagingDir()
	if err != nil {
		return fail(err)
	}
	defer os.RemoveAll(staged)
	stagedPath := filepath.Join(staged, "payload")

	switch req.Mode {
	case protocol.ModeInline:
		if !s.Cfg.InlineAllowed() {
			return fail(errors.New("inline transfers disabled by daemon config"))
		}
		err = s.receiveInline(br, req, stagedPath)
	case protocol.ModePull:
		err = s.pull(ctx, t, req, stagedPath)
	}
	if err != nil {
		return fail(err)
	}

	if err := s.Mir.Promote(stagedPath, rel, req.Dir); err != nil {
		return fail(err)
	}

	if err := s.open(ctx, req, s.Mir.Abs(rel)); err != nil {
		return fail(err)
	}
	_ = s.Mir.Idx.Touch(rel)
	s.Log.Info("opened", "origin", req.Origin.Label, "path", req.Path, "mode", req.Mode, "dir", req.Dir)
	return protocol.WriteEvent(conn, protocol.Event{Event: "done", Status: "ok"})
}

func (s *Server) receiveInline(br *bufio.Reader, req *protocol.Request, stagedPath string) error {
	cr := protocol.NewChunkReader(br, s.Cfg.MaxPayloadBytes)
	if req.Dir {
		skipped, err := tarstream.Unpack(cr, stagedPath, s.Cfg.MaxPayloadBytes, maxArchiveEntries)
		if err != nil {
			return fmt.Errorf("unpacking directory: %w", err)
		}
		if skipped > 0 {
			s.Log.Info("skipped non-regular entries during unpack", "count", skipped)
		}
		return nil
	}
	f, err := os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	_, err = io.Copy(f, cr)
	if cerr := f.Close(); err == nil {
		err = cerr
	}
	return err
}

// pull fetches over the daemon's own multiplexed ssh connection to t's host:
// `cat` for files, `tar -cf -` for directories. The remote path is
// single-quoted with ShQuote; it has already passed protocol validation
// (absolute, no NUL/CR/LF).
func (s *Server) pull(ctx context.Context, t *Tunnel, req *protocol.Request, stagedPath string) error {
	// Own cancelable context: if we stop reading early (payload cap or
	// budget hit), we must kill the remote cat/tar, which would otherwise
	// keep writing, fill the ssh pipe, and block cmd.Wait until the request
	// timeout — holding a sem slot and the per-slot lock the whole time.
	pctx, cancel := context.WithCancel(ctx)
	defer cancel()

	q := ShQuote(req.Path)
	var remoteCmd string
	if req.Dir {
		remoteCmd = "tar -C " + q + " -cf - ."
	} else {
		remoteCmd = "cat -- " + q
	}
	cmd := t.Exec(pctx, s.Cfg.ControlDir(), remoteCmd)
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return err
	}
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return err
	}
	lr := &io.LimitedReader{R: stdout, N: s.Cfg.MaxPayloadBytes + 1}

	var copyErr error
	if req.Dir {
		_, copyErr = tarstream.Unpack(lr, stagedPath, s.Cfg.MaxPayloadBytes, maxArchiveEntries)
	} else {
		var f *os.File
		if f, copyErr = os.OpenFile(stagedPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600); copyErr == nil {
			_, copyErr = io.Copy(f, lr)
			if cerr := f.Close(); copyErr == nil {
				copyErr = cerr
			}
		}
	}
	// Stop the remote producer before waiting: on early stop it is still
	// streaming; cancel kills it so Wait returns promptly.
	if copyErr != nil || lr.N == 0 {
		cancel()
	}
	waitErr := cmd.Wait()
	if copyErr != nil {
		return copyErr
	}
	if lr.N == 0 {
		return protocol.ErrPayloadTooLarge
	}
	return waitErr
}
