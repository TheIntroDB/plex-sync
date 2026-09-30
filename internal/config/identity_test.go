package config

import (
	"os"
	"runtime"
	"strings"
	"testing"
)

// identityConfig is a configuration whose state directory is a temporary one.
//
// Every test here resolves an identity, and resolution writes a file. Using
// Default() alone would put that file in the real per-user state directory of
// whoever runs the suite.
func identityConfig(t *testing.T) *Config {
	t.Helper()
	cfg := Default()
	cfg.StateDir = t.TempDir()
	// Both variables are cleared rather than left to the environment, because a
	// developer with PLEX_SYNC_DEVICE_NAME exported would otherwise be testing
	// their own shell.
	t.Setenv(EnvClientID, "")
	t.Setenv(EnvDeviceName, "")
	return cfg
}

func TestDeviceNameResolution(t *testing.T) {
	t.Run("defaults to the product name", func(t *testing.T) {
		cfg := identityConfig(t)
		if got := cfg.Plex.ResolvedDeviceName(); got != DefaultDeviceName {
			t.Errorf("ResolvedDeviceName = %q, want %q", got, DefaultDeviceName)
		}
	})

	t.Run("the setting wins over the default", func(t *testing.T) {
		cfg := identityConfig(t)
		cfg.Plex.DeviceName = "plex-sync (media-nas)"
		if got := cfg.Plex.ResolvedDeviceName(); got != "plex-sync (media-nas)" {
			t.Errorf("ResolvedDeviceName = %q, want the configured name", got)
		}
	})

	t.Run("the environment wins over the setting", func(t *testing.T) {
		cfg := identityConfig(t)
		cfg.Plex.DeviceName = "from-the-file"
		t.Setenv(EnvDeviceName, "  from-the-container  ")
		if got := cfg.Plex.ResolvedDeviceName(); got != "from-the-container" {
			t.Errorf("ResolvedDeviceName = %q, want the environment's name, trimmed", got)
		}
	})

	t.Run("an empty setting is not an empty name", func(t *testing.T) {
		cfg := identityConfig(t)
		cfg.Plex.DeviceName = "   "
		if got := cfg.Plex.ResolvedDeviceName(); got != DefaultDeviceName {
			t.Errorf("ResolvedDeviceName = %q, want %q: Plex always has a name to print",
				got, DefaultDeviceName)
		}
	})
}

// The point of the stored identifier: Plex treats one it has not seen as a new
// device, so a nightly run must present the same one every night.
func TestResolvePlexIdentityKeepsOneIdentifier(t *testing.T) {
	cfg := identityConfig(t)

	if err := cfg.ResolvePlexIdentity(); err != nil {
		t.Fatalf("ResolvePlexIdentity: %v", err)
	}
	first := cfg.Plex.ClientID
	if first == "" {
		t.Fatal("no identifier was resolved")
	}
	if !strings.HasPrefix(first, "plex-sync-") {
		t.Errorf("identifier = %q, want the plex-sync- prefix so Plex's device list is readable", first)
	}

	raw, err := os.ReadFile(cfg.ClientIDPath())
	if err != nil {
		t.Fatalf("the identifier was not stored: %v", err)
	}
	if got := strings.TrimSpace(string(raw)); got != first {
		t.Errorf("stored identifier = %q, want %q", got, first)
	}
	info, err := os.Stat(cfg.ClientIDPath())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if runtime.GOOS != "windows" {
		// Windows has no POSIX modes: os.WriteFile's 0600 there only toggles
		// the read-only attribute, and a file created in the user's own
		// profile is already limited by the directory's ACLs. There is nothing
		// portable to assert, which is the same reason the saved configuration
		// skips this. The identifier's stability, below, is not platform
		// specific and is asserted everywhere.
		if perm := info.Mode().Perm(); perm != 0o600 {
			t.Errorf("stored identifier mode = %v, want 0600", perm)
		}
	}

	// A second run, as a scheduled job would be, over the same state.
	second := Default()
	second.StateDir = cfg.StateDir
	if err := second.ResolvePlexIdentity(); err != nil {
		t.Fatalf("ResolvePlexIdentity (second run): %v", err)
	}
	if second.Plex.ClientID != first {
		t.Errorf("second run used %q, want the stored %q: a new identifier is a new device to Plex",
			second.Plex.ClientID, first)
	}
}

func TestResolvePlexIdentityPrefersTheEnvironment(t *testing.T) {
	cfg := identityConfig(t)
	t.Setenv(EnvClientID, "plex-sync-from-the-container")

	if err := cfg.ResolvePlexIdentity(); err != nil {
		t.Fatalf("ResolvePlexIdentity: %v", err)
	}
	if cfg.Plex.ClientID != "plex-sync-from-the-container" {
		t.Errorf("identifier = %q, want the environment's", cfg.Plex.ClientID)
	}
	// An identity somebody chose is not this tool's to store.
	if _, err := os.Stat(cfg.ClientIDPath()); !os.IsNotExist(err) {
		t.Errorf("a file was written for an identifier that came from the environment (stat err = %v)", err)
	}
}

func TestResolvePlexIdentityLeavesAConfiguredIdentifierAlone(t *testing.T) {
	cfg := identityConfig(t)
	cfg.Plex.ClientID = "chosen-in-the-file"

	if err := cfg.ResolvePlexIdentity(); err != nil {
		t.Fatalf("ResolvePlexIdentity: %v", err)
	}
	if cfg.Plex.ClientID != "chosen-in-the-file" {
		t.Errorf("identifier = %q, want the configured one", cfg.Plex.ClientID)
	}
	if _, err := os.Stat(cfg.ClientIDPath()); !os.IsNotExist(err) {
		t.Errorf("a file was written over a configured identifier (stat err = %v)", err)
	}
}

// The stored value is read from disk and sent as a request header, so what is
// there is validated rather than trusted.
func TestResolvePlexIdentityReplacesAnUnusableFile(t *testing.T) {
	for name, contents := range map[string]string{
		"empty":          "",
		"whitespace":     "   \n\t\n",
		"spaces inside":  "not an identifier",
		"a header break": "plex-sync-ok\r\nX-Plex-Token: injected",
		"far too long":   "plex-sync-" + strings.Repeat("a", maxClientIDLen),
		"a quoted value": `"plex-sync-abc"`,
	} {
		t.Run(name, func(t *testing.T) {
			cfg := identityConfig(t)
			if err := os.WriteFile(cfg.ClientIDPath(), []byte(contents), 0o600); err != nil {
				t.Fatalf("write the file: %v", err)
			}

			if err := cfg.ResolvePlexIdentity(); err != nil {
				t.Fatalf("ResolvePlexIdentity: %v", err)
			}
			if cfg.Plex.ClientID == "" {
				t.Fatal("nothing was resolved")
			}
			if strings.ContainsAny(cfg.Plex.ClientID, " \r\n\t\"") {
				t.Errorf("identifier = %q, which is not usable as a header value", cfg.Plex.ClientID)
			}
			// And the bad value is gone rather than left for the next run.
			raw, err := os.ReadFile(cfg.ClientIDPath())
			if err != nil {
				t.Fatalf("read back: %v", err)
			}
			if got := strings.TrimSpace(string(raw)); got != cfg.Plex.ClientID {
				t.Errorf("stored identifier = %q, want the replacement %q", got, cfg.Plex.ClientID)
			}
		})
	}
}

// A usable stored value is used as it is: an operator who put one there meant it.
func TestResolvePlexIdentityAcceptsAStoredValue(t *testing.T) {
	cfg := identityConfig(t)
	if err := os.WriteFile(cfg.ClientIDPath(), []byte("plex-sync-hand-written\n"), 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	if err := cfg.ResolvePlexIdentity(); err != nil {
		t.Fatalf("ResolvePlexIdentity: %v", err)
	}
	if cfg.Plex.ClientID != "plex-sync-hand-written" {
		t.Errorf("identifier = %q, want the stored one", cfg.Plex.ClientID)
	}
}

// Writing the configuration down must not freeze the identifier: it belongs to
// the install, and a file copied to a second machine would hand both the same
// identity, which Plex would show as one device.
func TestSaveDoesNotFreezeTheResolvedIdentifier(t *testing.T) {
	cfg := identityConfig(t)
	if err := cfg.ResolvePlexIdentity(); err != nil {
		t.Fatalf("ResolvePlexIdentity: %v", err)
	}
	if got := cfg.forWriting().Plex.ClientID; got != "" {
		t.Errorf("the resolved identifier was written to the file: %q", got)
	}

	// One that was chosen is kept, because then it was chosen.
	chosen := identityConfig(t)
	chosen.Plex.ClientID = "chosen-by-hand"
	if got := chosen.forWriting().Plex.ClientID; got != "chosen-by-hand" {
		t.Errorf("a configured identifier was dropped on write: %q", got)
	}
}
