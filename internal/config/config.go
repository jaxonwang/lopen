// Package config loads lopend's daemon configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

// Host is one enrolled remote. Label doubles as the mirror subdirectory and
// the socket filename, so it is validated with the same rule the protocol
// uses for origin labels.
type Host struct {
	// Label names this host locally (mirror dir, socket name).
	Label string `json:"label"`
	// Dest is the ssh destination ([user@]host, may be an ssh_config alias).
	Dest string `json:"dest"`
	// RemoteSocket is the socket path created on the remote host.
	// Defaults to ".lopen/lopen.sock" (relative to the remote $HOME).
	RemoteSocket string `json:"remote_socket,omitempty"`
	// Keep pins this host's mirror: GC never evicts it.
	Keep bool `json:"keep,omitempty"`
}

type Config struct {
	Hosts []Host `json:"hosts"`

	// MirrorDir is where fetched files land. Default ~/lopen.
	MirrorDir string `json:"mirror_dir,omitempty"`
	// StateDir holds sockets, index, logs. Default:
	// darwin ~/Library/Application Support/lopen, else ~/.local/state/lopen.
	StateDir string `json:"state_dir,omitempty"`

	TTLDays         int   `json:"ttl_days,omitempty"`          // default 7
	MaxMirrorBytes  int64 `json:"max_mirror_bytes,omitempty"`  // default 2 GiB
	MaxPayloadBytes int64 `json:"max_payload_bytes,omitempty"` // default 500 MiB
	// AllowInline permits inline payloads (needed for chained hosts the Mac
	// cannot ssh to). Default true.
	AllowInline *bool `json:"allow_inline,omitempty"`
}

const (
	DefaultTTLDays       = 7
	DefaultMaxMirror     = 2 << 30
	DefaultMaxPayload    = 500 << 20
	DefaultRemoteSocket  = ".lopen/lopen.sock"
	defaultConfigRelUnix = ".config/lopen/config.json"
)

// destRe rejects ssh destinations that could be parsed as options and shell
// metacharacters. Allows [user@]host where both parts are alnum/dot/dash/
// underscore and do not start with a dash.
var destRe = regexp.MustCompile(`^(?:[A-Za-z0-9._][A-Za-z0-9._-]*@)?[A-Za-z0-9._][A-Za-z0-9._-]*$`)

func ValidDest(s string) bool { return destRe.MatchString(s) }

func DefaultPath() string {
	home, _ := os.UserHomeDir()
	return filepath.Join(home, defaultConfigRelUnix)
}

func Load(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := json.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.fill(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

var labelRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)

func (c *Config) fill() error {
	home, err := os.UserHomeDir()
	if err != nil {
		return err
	}
	if c.MirrorDir == "" {
		c.MirrorDir = filepath.Join(home, "lopen")
	}
	if c.StateDir == "" {
		c.StateDir = defaultStateDir(home)
	}
	if c.TTLDays == 0 {
		c.TTLDays = DefaultTTLDays
	}
	if c.MaxMirrorBytes == 0 {
		c.MaxMirrorBytes = DefaultMaxMirror
	}
	if c.MaxPayloadBytes == 0 {
		c.MaxPayloadBytes = DefaultMaxPayload
	}
	seen := map[string]bool{}
	for i := range c.Hosts {
		h := &c.Hosts[i]
		if !labelRe.MatchString(h.Label) {
			return fmt.Errorf("invalid host label %q", h.Label)
		}
		if seen[h.Label] {
			return fmt.Errorf("duplicate host label %q", h.Label)
		}
		seen[h.Label] = true
		if !ValidDest(h.Dest) {
			return fmt.Errorf("host %q: invalid ssh destination %q", h.Label, h.Dest)
		}
		if h.RemoteSocket == "" {
			h.RemoteSocket = DefaultRemoteSocket
		}
		if !validRemoteSocket(h.RemoteSocket) {
			return fmt.Errorf("host %q: invalid remote_socket %q", h.Label, h.RemoteSocket)
		}
	}
	return nil
}

// validRemoteSocket accepts a relative path (bound under the remote $HOME by
// ssh -R), rejecting anything that could break the `-R remote:local` spec
// (a ':' splits the forward spec) or inject into the remote shell used for
// pre-cleaning. Absolute paths, '..', and control/shell metacharacters are
// disallowed.
func validRemoteSocket(s string) bool {
	if s == "" || strings.HasPrefix(s, "/") || strings.HasPrefix(s, "~") {
		return false
	}
	if strings.Contains(s, "..") {
		return false
	}
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9':
		case r == '.' || r == '_' || r == '-' || r == '/':
		default:
			return false
		}
	}
	return true
}

func (c *Config) InlineAllowed() bool {
	return c.AllowInline == nil || *c.AllowInline
}

func (c *Config) HostByLabel(label string) *Host {
	for i := range c.Hosts {
		if c.Hosts[i].Label == label {
			return &c.Hosts[i]
		}
	}
	return nil
}

func (c *Config) SocketDir() string  { return filepath.Join(c.StateDir, "hosts") }
func (c *Config) IndexPath() string  { return filepath.Join(c.StateDir, "index.json") }
func (c *Config) ControlDir() string { return filepath.Join(c.StateDir, "ctl") }

func defaultStateDir(home string) string {
	if dirDarwin := filepath.Join(home, "Library", "Application Support"); isDir(dirDarwin) {
		return filepath.Join(dirDarwin, "lopen")
	}
	return filepath.Join(home, ".local", "state", "lopen")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
