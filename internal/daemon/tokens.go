package daemon

import (
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"os"
	"sync"
)

// tokenStore holds one secret per host label, persisted to a 0600 file so the
// remote agent configs provisioned by the daemon stay valid across restarts.
// The token is the access-control mechanism for the loopback TCP transport:
// see protocol.AgentConfig.
type tokenStore struct {
	mu     sync.Mutex
	path   string
	Tokens map[string]string `json:"tokens"`
}

func loadTokens(path string) (*tokenStore, error) {
	ts := &tokenStore{path: path, Tokens: map[string]string{}}
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return ts, nil
	}
	if err != nil {
		return nil, err
	}
	// A corrupt token file just means we mint fresh tokens (and re-provision
	// the remote agent configs), so recover rather than wedge the daemon.
	_ = json.Unmarshal(b, ts)
	if ts.Tokens == nil {
		ts.Tokens = map[string]string{}
	}
	return ts, nil
}

func (ts *tokenStore) save() error {
	b, err := json.MarshalIndent(ts, "", " ")
	if err != nil {
		return err
	}
	tmp := ts.path + ".tmp"
	if err := os.WriteFile(tmp, b, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, ts.path)
}

// ensure returns the token for label, minting and persisting a fresh one if it
// does not yet exist.
func (ts *tokenStore) ensure(label string) (string, error) {
	ts.mu.Lock()
	defer ts.mu.Unlock()
	if t, ok := ts.Tokens[label]; ok && t != "" {
		return t, nil
	}
	t, err := newToken()
	if err != nil {
		return "", err
	}
	ts.Tokens[label] = t
	return t, ts.save()
}

// valid reports whether presented matches the stored token for label, using a
// constant-time comparison.
func (ts *tokenStore) valid(label, presented string) bool {
	ts.mu.Lock()
	want := ts.Tokens[label]
	ts.mu.Unlock()
	if want == "" || presented == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(want), []byte(presented)) == 1
}

func newToken() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}
