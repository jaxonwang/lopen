// Package protocol defines the wire format spoken over the lopen unix socket.
//
// A conversation is:
//
//	client → daemon: one JSON request header, terminated by '\n'
//	client → daemon: payload as length-prefixed chunks (4-byte big-endian
//	                 uint32 length, then that many bytes; length 0 ends the
//	                 stream)
//	daemon → client: a stream of JSON events, one per line; the final event
//	                 has Event == "done"
//
// Every field that crosses this boundary is untrusted by the daemon: any host
// on the ssh chain can write to the forwarded socket.
package protocol

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"regexp"
)

const (
	Version = 1

	// MaxHeaderBytes bounds the JSON request line so a hostile peer cannot
	// make the daemon buffer unbounded input before validation.
	MaxHeaderBytes = 64 * 1024

	// MaxChunkBytes bounds a single payload chunk.
	MaxChunkBytes = 1 << 20

	// DefaultMaxPayload is the default cap on total payload bytes accepted
	// for one request (file content or tar stream).
	DefaultMaxPayload = 500 << 20
)

const (
	OpOpen   = "open"
	OpReveal = "reveal"
)

const (
	ModeInline = "inline"
	ModePull   = "pull"
)

// Origin identifies the host a request came from. With chained socket
// forwarding the socket a request arrives on no longer identifies the source,
// so the client declares it. Label is validated and used as a path component
// of the local mirror.
type Origin struct {
	Host  string `json:"host"`
	Label string `json:"label"`
}

// Request is the header the client sends. Path must be absolute on the
// origin host.
type Request struct {
	V      int    `json:"v"`
	Op     string `json:"op"`
	Origin Origin `json:"origin"`
	Path   string `json:"path"`
	Dir    bool   `json:"dir"`
	// Wait is informational: the client alone decides whether to block on the
	// daemon's "done" event. The daemon streams events regardless.
	Wait  bool   `json:"wait"`
	Force bool   `json:"force,omitempty"`
	App   string `json:"app,omitempty"`
	Size  int64  `json:"size"` // declared payload size; advisory, re-checked on receive
	Mode  string `json:"mode"`
	// Token authenticates the request. The remote transport is a loopback TCP
	// port reachable by any local user on the remote host (unlike a 0600 UNIX
	// socket), so the daemon requires a per-host secret that only a client able
	// to read the 0600 remote agent config can supply. Compared in constant
	// time against the daemon's stored token for the tunnel's host.
	Token string `json:"token,omitempty"`
}

// AgentConfig is the small file the daemon provisions on each remote host
// (~/.lopen/agent.json, mode 0600). The remote `lopen` client reads it to learn
// the label the daemon knows this host by, the loopback port to dial, and the
// token to authenticate with. Because the transport is a shared-loopback TCP
// port rather than a per-user socket, the 0600 mode on this file is what keeps
// another local user from learning the token and pushing files as you.
type AgentConfig struct {
	Label string `json:"label"`
	Port  int    `json:"port"`
	Token string `json:"token"`
}

// Event is a daemon → client message.
type Event struct {
	Event  string `json:"event"`            // "received" | "progress" | "done"
	Status string `json:"status,omitempty"` // for "done": "ok" | "error"
	Error  string `json:"error,omitempty"`
	Detail string `json:"detail,omitempty"`
}

var (
	// labelRe: mirror path component. No leading dash or dot (dash could be
	// mistaken for an option by downstream tools; dot avoids "." / ".."
	// shenanigans before we even reach path joining).
	labelRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	// appRe: app names handed to `open -a`. Never allowed to start with a
	// dash, no path separators.
	appRe = regexp.MustCompile(`^[A-Za-z0-9 ._][A-Za-z0-9 ._-]{0,127}$`)
)

func ValidLabel(s string) bool { return labelRe.MatchString(s) }
func ValidApp(s string) bool   { return appRe.MatchString(s) }

// Validate checks structural invariants of a request. It does not check
// filesystem state.
func (r *Request) Validate(maxPayload int64) error {
	if r.V != Version {
		return fmt.Errorf("unsupported protocol version %d", r.V)
	}
	switch r.Op {
	case OpOpen, OpReveal:
	default:
		return fmt.Errorf("unknown op %q", r.Op)
	}
	switch r.Mode {
	case ModeInline, ModePull:
	default:
		return fmt.Errorf("unknown mode %q", r.Mode)
	}
	if !ValidLabel(r.Origin.Label) {
		return fmt.Errorf("invalid origin label %q", r.Origin.Label)
	}
	if len(r.Path) == 0 || r.Path[0] != '/' {
		return errors.New("path must be absolute")
	}
	if len(r.Path) > 4096 {
		return errors.New("path too long")
	}
	for _, c := range r.Path {
		if c == 0 || c == '\n' || c == '\r' {
			return errors.New("path contains forbidden control character")
		}
	}
	if r.App != "" && !ValidApp(r.App) {
		return fmt.Errorf("invalid app name %q", r.App)
	}
	if r.Size < 0 || r.Size > maxPayload {
		return fmt.Errorf("declared size %d exceeds limit %d", r.Size, maxPayload)
	}
	return nil
}

// WriteRequest encodes the header line.
func WriteRequest(w io.Writer, r *Request) error {
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxHeaderBytes {
		return errors.New("request header too large")
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// ReadRequest reads and decodes a header line from a *bufio.Reader whose
// buffer must be at least MaxHeaderBytes. ReadSlice (not ReadBytes) is
// load-bearing: ReadBytes grows without bound, ReadSlice fails with
// ErrBufferFull once the buffer is exhausted, which is the enforcement of
// MaxHeaderBytes.
func ReadRequest(br *bufio.Reader) (*Request, error) {
	line, err := br.ReadSlice('\n')
	if err != nil {
		if errors.Is(err, bufio.ErrBufferFull) {
			return nil, errors.New("request header exceeds limit")
		}
		return nil, err
	}
	var r Request
	dec := json.NewDecoder(bytes.NewReader(line))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&r); err != nil {
		return nil, fmt.Errorf("bad request header: %w", err)
	}
	return &r, nil
}

func WriteEvent(w io.Writer, e Event) error {
	b, err := json.Marshal(e)
	if err != nil {
		return err
	}
	b = append(b, '\n')
	_, err = w.Write(b)
	return err
}

// MaxEventBytes bounds a single event line. Events are tiny; this just keeps
// the read symmetric with the request side so a misbehaving peer can't make
// the reader buffer unboundedly.
const MaxEventBytes = 64 * 1024

// readLine reads up to and including a '\n', failing once max bytes have been
// read without one. Unlike ReadRequest's ReadSlice (which requires the
// reader's buffer to be pre-sized to the cap), this works with a
// default-sized bufio.Reader by accumulating fragments.
func readLine(br *bufio.Reader, max int) ([]byte, error) {
	var buf []byte
	for {
		frag, err := br.ReadSlice('\n')
		if len(buf)+len(frag) > max {
			return nil, errors.New("line exceeds limit")
		}
		if err == bufio.ErrBufferFull {
			buf = append(buf, frag...)
			continue
		}
		if err != nil {
			return nil, err
		}
		return append(buf, frag...), nil
	}
}

func ReadEvent(br *bufio.Reader) (*Event, error) {
	line, err := readLine(br, MaxEventBytes)
	if err != nil {
		return nil, err
	}
	var e Event
	if err := json.Unmarshal(line, &e); err != nil {
		return nil, fmt.Errorf("bad event: %w", err)
	}
	return &e, nil
}

// ChunkWriter frames written bytes as length-prefixed chunks. Close writes
// the terminating zero-length chunk.
type ChunkWriter struct {
	w io.Writer
}

func NewChunkWriter(w io.Writer) *ChunkWriter { return &ChunkWriter{w: w} }

func (cw *ChunkWriter) Write(p []byte) (int, error) {
	total := 0
	for len(p) > 0 {
		n := len(p)
		if n > MaxChunkBytes {
			n = MaxChunkBytes
		}
		var hdr [4]byte
		binary.BigEndian.PutUint32(hdr[:], uint32(n))
		if _, err := cw.w.Write(hdr[:]); err != nil {
			return total, err
		}
		if _, err := cw.w.Write(p[:n]); err != nil {
			return total, err
		}
		p = p[n:]
		total += n
	}
	return total, nil
}

func (cw *ChunkWriter) Close() error {
	var hdr [4]byte
	_, err := cw.w.Write(hdr[:])
	return err
}

// ChunkReader unframes a chunk stream. It enforces both the per-chunk bound
// and a total-bytes cap, returning ErrPayloadTooLarge when the cap is
// exceeded (the caller must abort the request; the peer may still be
// sending).
type ChunkReader struct {
	r         io.Reader
	remaining int // bytes left in current chunk
	done      bool
	total     int64
	maxTotal  int64
}

var ErrPayloadTooLarge = errors.New("payload exceeds size limit")

func NewChunkReader(r io.Reader, maxTotal int64) *ChunkReader {
	return &ChunkReader{r: r, maxTotal: maxTotal}
}

func (cr *ChunkReader) Total() int64 { return cr.total }

func (cr *ChunkReader) Read(p []byte) (int, error) {
	if cr.done {
		return 0, io.EOF
	}
	if cr.remaining == 0 {
		var hdr [4]byte
		if _, err := io.ReadFull(cr.r, hdr[:]); err != nil {
			return 0, fmt.Errorf("reading chunk header: %w", err)
		}
		n := binary.BigEndian.Uint32(hdr[:])
		if n == 0 {
			cr.done = true
			return 0, io.EOF
		}
		if n > MaxChunkBytes {
			return 0, fmt.Errorf("chunk of %d bytes exceeds limit", n)
		}
		cr.remaining = int(n)
	}
	if int64(cr.remaining)+cr.total > cr.maxTotal {
		return 0, ErrPayloadTooLarge
	}
	if len(p) > cr.remaining {
		p = p[:cr.remaining]
	}
	n, err := cr.r.Read(p)
	cr.remaining -= n
	cr.total += int64(n)
	if err == io.EOF && cr.remaining > 0 {
		err = io.ErrUnexpectedEOF
	}
	return n, err
}
