package config

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
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

	// clientIDWaitAttempts and clientIDWaitInterval bound the wait for another
	// process to finish writing an identifier it has created but not yet
	// written. Two processes racing here is a startup collision, not a
	// workload, so the window is a fraction of a second and the wait is over
	// rather than indefinite.
	clientIDWaitAttempts = 50
	clientIDWaitInterval = 2 * time.Millisecond
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
		c.PlexClientIDResolved = true
		return nil
	}

	id := NewClientID()
	if err := os.MkdirAll(c.StateDir, 0o755); err != nil {
		return fmt.Errorf("create state directory %s: %w", c.StateDir, err)
	}
	// O_EXCL, so that two processes starting against the same state directory
	// agree on one identifier: the one that creates the file wins, and the other
	// adopts what it wrote. Without it both generate one, both write, each keeps
	// its own in memory -- two devices to Plex from one install -- and the file
	// holds whichever finished last.
	f, err := os.OpenFile(c.ClientIDPath(), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	switch {
	case err == nil:
		_, writeErr := f.WriteString(id + "\n")
		closeErr := f.Close()
		if writeErr != nil {
			return fmt.Errorf("write %s: %w", c.ClientIDPath(), writeErr)
		}
		if closeErr != nil {
			return fmt.Errorf("write %s: %w", c.ClientIDPath(), closeErr)
		}
		c.Plex.ClientID = id
		c.PlexClientIDResolved = true
		return nil
	case errors.Is(err, os.ErrExist):
		// Another process created it between the read above and this open, so
		// its identifier is the install's and ours is not. The file is created
		// before it is written, so an immediate read can catch it still empty;
		// waiting for it is bounded, and running without one is worse than
		// waiting a moment.
		for attempt := 0; attempt < clientIDWaitAttempts; attempt++ {
			if stored := c.StoredClientID(); stored != "" {
				c.Plex.ClientID = stored
				c.PlexClientIDResolved = true
				return nil
			}
			time.Sleep(clientIDWaitInterval)
		}
		// It exists and holds nothing usable: a crash between the create and
		// the write, or a file that was truncated or edited. Replacing it is
		// the only way out, because otherwise every run waits and then uses an
		// identifier that is not the one on disk.
		if err := writeClientID(c.ClientIDPath(), id); err != nil {
			return err
		}
		c.Plex.ClientID = id
		c.PlexClientIDResolved = true
		return nil
	default:
		return fmt.Errorf("write %s: %w", c.ClientIDPath(), err)
	}
}

// writeClientID replaces the stored identifier through a temporary file, so a
// reader arriving during the write sees either the old value or the new one and
// never half of either.
func writeClientID(path, id string) error {
	dir := filepath.Dir(path)
	temp, err := os.CreateTemp(dir, filepath.Base(path)+".tmp*")
	if err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	name := temp.Name()
	defer func() { _ = os.Remove(name) }() // a no-op once renamed

	if _, err := temp.WriteString(id + "\n"); err != nil {
		_ = temp.Close()
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := temp.Close(); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Chmod(name, 0o600); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	if err := os.Rename(name, path); err != nil {
		return fmt.Errorf("replace %s: %w", path, err)
	}
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
