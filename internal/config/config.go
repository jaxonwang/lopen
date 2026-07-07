// Package config loads lopend's daemon configuration.
package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
)

// Host is one enrolled remote. Label doubles as the mirror subdirectory and
// the socket filename, so it is validated with the same rule the protocol
// uses for origin labels.
type Host struct {
	// Label names this host locally (mirror dir, socket name).
	Label string `json:"label"`
	// Dest is the ssh destination ([user@]host, may be an ssh_config alias).
	Dest string `json:"dest"`
	// RemotePort is the loopback TCP port the daemon binds on the remote host
	// (via ssh -R 127.0.0.1:<port>:<local unix socket>). Amazon-managed sshd
	// refuses UNIX-socket reverse forwards (AllowStreamLocalForwarding), so the
	// remote end of the tunnel is a loopback TCP port; access is gated by a
	// per-host token in the 0600 remote agent config, not by socket file mode.
	// Defaults to DefaultRemotePort. All hosts may share one port value — each
	// binds on its own machine; the only collision is another local user on the
	// same host, in which case set a distinct port here.
	RemotePort int `json:"remote_port,omitempty"`
	// Keep pins this host's mirror: GC never evicts it.
	Keep bool `json:"keep,omitempty"`
}

type Config struct {
	Hosts []Host `json:"hosts"`

	// MirrorDir is where fetched files land. Default ~/lopen.
	MirrorDir string `json:"mirror_dir,omitempty"`
	// StateDir holds sockets, index, logs. Default: darwin ~/Library/
	// Application Support/lopen, windows %LOCALAPPDATA%\lopen, else
	// $XDG_STATE_HOME/lopen or ~/.local/state/lopen.
	StateDir string `json:"state_dir,omitempty"`

	TTLDays         int   `json:"ttl_days,omitempty"`          // default 7
	MaxMirrorBytes  int64 `json:"max_mirror_bytes,omitempty"`  // default 2 GiB
	MaxPayloadBytes int64 `json:"max_payload_bytes,omitempty"` // default 500 MiB
	// AllowInline permits inline payloads (needed for chained hosts the Mac
	// cannot ssh to). Default true.
	AllowInline *bool `json:"allow_inline,omitempty"`

	// path is the file this config was loaded from (for Save). Not serialized.
	path string `json:"-"`
}

const (
	DefaultTTLDays    = 7
	DefaultMaxMirror  = 2 << 30
	DefaultMaxPayload = 500 << 20
	// DefaultRemotePort is the loopback TCP port bound on each remote host.
	// Chosen in the IANA dynamic/private range to avoid well-known services.
	DefaultRemotePort    = 47654
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
	c.path = path
	if err := c.fill(); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// LoadRaw reads a config without filling defaults, for editing (setup). A
// missing file yields an empty config bound to path so a first enrollment can
// create it.
func LoadRaw(path string) (*Config, error) {
	if path == "" {
		path = DefaultPath()
	}
	c := &Config{path: path}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return c, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	c.path = path
	return c, nil
}

// Save writes the config back to the file it was loaded from, atomically.
func (c *Config) Save() error {
	if c.path == "" {
		c.path = DefaultPath()
	}
	if err := os.MkdirAll(filepath.Dir(c.path), 0o700); err != nil {
		return err
	}
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, c.path)
}

// AddHost appends a host after validating its label and destination are usable
// and not already enrolled.
func (c *Config) AddHost(h Host) error {
	if !labelRe.MatchString(h.Label) {
		return fmt.Errorf("invalid host label %q", h.Label)
	}
	if !ValidDest(h.Dest) {
		return fmt.Errorf("invalid ssh destination %q", h.Dest)
	}
	for _, e := range c.Hosts {
		if e.Label == h.Label {
			return fmt.Errorf("host label %q already enrolled", h.Label)
		}
	}
	c.Hosts = append(c.Hosts, h)
	return nil
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
		if h.RemotePort == 0 {
			h.RemotePort = DefaultRemotePort
		}
		if h.RemotePort < 1 || h.RemotePort > 65535 {
			return fmt.Errorf("host %q: invalid remote_port %d", h.Label, h.RemotePort)
		}
	}
	return nil
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

// TokensPath holds the persisted per-host auth tokens (0600). Tokens survive
// daemon restarts so the remote agent config provisioned on each host stays
// valid across reconnects.
func (c *Config) TokensPath() string { return filepath.Join(c.StateDir, "tokens.json") }

func defaultStateDir(home string) string {
	if runtime.GOOS == "windows" {
		if d := os.Getenv("LOCALAPPDATA"); d != "" {
			return filepath.Join(d, "lopen")
		}
		return filepath.Join(home, "AppData", "Local", "lopen")
	}
	if dirDarwin := filepath.Join(home, "Library", "Application Support"); isDir(dirDarwin) {
		return filepath.Join(dirDarwin, "lopen")
	}
	if d := os.Getenv("XDG_STATE_HOME"); d != "" {
		return filepath.Join(d, "lopen")
	}
	return filepath.Join(home, ".local", "state", "lopen")
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}
