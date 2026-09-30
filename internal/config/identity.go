package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

// Plex device identity: the client identifier Plex keys this install's device
// entry on, and the name it shows for it.
//
// Both exist because of one report. A scheduled run produced a "used a new
// device to access your server" notification every night, with nothing in the
// brackets where the device name goes. The identifier was generated per process,
// so Plex saw a new device on every run; no device name was sent at all, so its
// notification had nothing to print. Neither is visible from inside Plex: the
// tool worked, and the only symptom was mail the user could not place.
const (
	// EnvClientID overrides the stored client identifier.
	EnvClientID = "PLEX_SYNC_CLIENT_ID"
	// EnvDeviceName overrides the name Plex shows for this tool.
	EnvDeviceName = "PLEX_SYNC_DEVICE_NAME"
	// ClientIDFile is the stored identifier inside the state directory.
	ClientIDFile = "client-id"
	// DefaultDeviceName is what Plex shows when nothing names this tool.
	DefaultDeviceName = "plex-sync"
	// clientIDPrefix marks a generated identifier as this tool's.
	clientIDPrefix = "plex-sync-"
	// maxClientIDLen bounds a stored identifier. Plex does not publish a limit;
	// this one is small enough to be obviously sane and far larger than the
	// identifiers real clients send.
	maxClientIDLen = 128
)

// ResolvedDeviceName is the name Plex shows for this tool: the environment, then
// the file, then "plex-sync".
//
// The environment is read first because a container's name is the one thing it
// should be told rather than discover: its state directory is often all it has,
// and the name a person recognises in a Plex device list is usually the container
// or host they put it on.
func (p Plex) ResolvedDeviceName() string {
	if v := strings.TrimSpace(os.Getenv(EnvDeviceName)); v != "" {
		return v
	}
	if v := strings.TrimSpace(p.DeviceName); v != "" {
		return v
	}
	return DefaultDeviceName
}

// ResolvePlexIdentity fills in the client identifier when nothing has chosen one,
// storing a generated one in the state directory.
//
// A stored identifier is the whole point: Plex treats an identifier it has not
// seen as a new device, so one that changes between runs turns a nightly timer
// into a nightly notification, and fills the device list with a device per run.
// Keeping it in the state directory rather than in the configuration file is
// deliberate -- it belongs to the install, not to a person, so it follows the
// state and not a config that gets copied between machines.
func (c *Config) ResolvePlexIdentity() error {
	if v := strings.TrimSpace(os.Getenv(EnvClientID)); v != "" {
		c.Plex.ClientID = v
		return nil
	}
	if v := strings.TrimSpace(c.Plex.ClientID); v != "" {
		return nil
	}
	if stored := c.StoredClientID(); stored != "" {
		c.Plex.ClientID = stored
		return nil
	}

	id := NewClientID()
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		return fmt.Errorf("create state directory %s: %w", c.StateDir, err)
	}
	// 0600: the identifier is not a secret, but it names this install to a
	// server, and nothing else on the machine has any business rewriting it.
	if err := os.WriteFile(c.ClientIDPath(), []byte(id+"\n"), 0o600); err != nil {
		return fmt.Errorf("write %s: %w", c.ClientIDPath(), err)
	}
	c.Plex.ClientID = id
	return nil
}

// ClientIDPath is where the identifier is stored.
func (c *Config) ClientIDPath() string {
	return filepath.Join(c.StateDir, ClientIDFile)
}

// StoredClientID returns the identifier already stored for this state directory,
// or "" when there is not a usable one.
//
// The file is read from disk and its contents end up in a request header, so what
// is there is validated rather than trusted. A file that was truncated, edited
// or written as something else entirely must be replaced rather than sent.
func (c *Config) StoredClientID() string {
	raw, err := os.ReadFile(c.ClientIDPath())
	if err != nil {
		return ""
	}
	return usableClientID(string(raw))
}

// NewClientID returns a fresh identifier in the shape Plex expects: its own
// prefix, so an operator reading Plex's device list can tell what the device is
// even before its name says so.
func NewClientID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on any platform this ships to. The
		// fallback is not a good identifier, but a run is not worth failing
		// over one, and a timestamp is still better than nothing at all.
		return clientIDPrefix + strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return clientIDPrefix + hex.EncodeToString(buf[:])
}

// usableClientID returns the identifier held in a file's contents, or "" when
// what is there is not one.
func usableClientID(raw string) string {
	id := strings.TrimSpace(raw)
	if id == "" || len(id) > maxClientIDLen {
		return ""
	}
	for _, r := range id {
		switch {
		case r >= 'a' && r <= 'z',
			r >= 'A' && r <= 'Z',
			r >= '0' && r <= '9',
			r == '-', r == '_', r == '.':
		default:
			return ""
		}
	}
	return id
}
