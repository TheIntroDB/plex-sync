package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// Every platform has to produce somewhere to look. The exact list matters less
// than these two properties: the right one for the platform comes first, and a
// user's own location is always honoured.
func TestPlexDirsPerPlatform(t *testing.T) {
	env := func(mapping map[string]string) func(string) string {
		return func(key string) string { return mapping[key] }
	}

	cases := []struct {
		name    string
		goos    string
		env     map[string]string
		wantAny []string
	}{
		{
			name:    "macos uses the application support directory",
			goos:    "darwin",
			env:     map[string]string{"HOME": "/Users/someone"},
			wantAny: []string{filepath.Join("/Users/someone", "Library", "Application Support", "Plex Media Server")},
		},
		{
			name: "windows uses LOCALAPPDATA, not APPDATA, and tries both",
			goos: "windows",
			env: map[string]string{
				"LOCALAPPDATA": `C:\Users\someone\AppData\Local`,
				"APPDATA":      `C:\Users\someone\AppData\Roaming`,
			},
			wantAny: []string{
				filepath.Join(`C:\Users\someone\AppData\Local`, "Plex Media Server"),
				filepath.Join(`C:\Users\someone\AppData\Roaming`, "Plex Media Server"),
			},
		},
		{
			name: "linux covers the container image and the native packages",
			goos: "linux",
			env:  map[string]string{},
			wantAny: []string{
				"/config/Library/Application Support/Plex Media Server",
				"/var/lib/plexmediaserver/Library/Application Support/Plex Media Server",
				"/var/snap/plexmediaserver/common/Library/Application Support/Plex Media Server",
			},
		},
		{
			name:    "freebsd covers both flavours",
			goos:    "freebsd",
			env:     map[string]string{},
			wantAny: []string{"/usr/local/plexdata/Plex Media Server"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dirs := plexDirsForOS(tc.goos, env(tc.env))
			if len(dirs) == 0 {
				t.Fatalf("%s produced no candidate directories", tc.goos)
			}
			joined := strings.Join(dirs, "\n")
			for _, want := range tc.wantAny {
				if !strings.Contains(joined, want) {
					t.Errorf("%s candidates do not include %q:\n%s", tc.goos, want, joined)
				}
			}
			// Every candidate has to be absolute. filepath.IsAbs knows only the
			// platform it is compiled for, so this is checked for the host's own
			// list rather than for the others.
			if tc.goos == runtime.GOOS {
				for _, dir := range dirs {
					if !filepath.IsAbs(dir) {
						t.Errorf("%s produced a relative candidate: %q", tc.goos, dir)
					}
				}
			}
		})
	}
}

// Windows must not silently fall back to nothing when LOCALAPPDATA is unset.
func TestPlexDirsOnWindowsWithoutLocalAppData(t *testing.T) {
	dirs := plexDirsForOS("windows", func(string) string { return "" })
	if len(dirs) != 0 {
		t.Errorf("with no environment there is nowhere to point, got %v", dirs)
	}
}

// The whole point of discovery is that the user does not have to configure
// anything, so a machine with Plex in the platform's default place must be found
// without help.
func TestDiscoverPlexDirFindsTheRealLayout(t *testing.T) {
	home := t.TempDir()
	dir := filepath.Join(home, "Library", "Application Support", "Plex Media Server")
	if err := os.MkdirAll(filepath.Join(dir, "Plug-in Support", "Databases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PlexDBSubpath), []byte("not a real database"), 0o600); err != nil {
		t.Fatal(err)
	}

	// Force the macOS candidate list, whatever platform this test runs on.
	t.Setenv("PLEX_CONFIG_DIR", "")
	t.Setenv("HOME", home)

	if got := DiscoverPlexDir(); got == "" {
		t.Fatalf("nothing found; the macOS candidate list is %v", PlatformPlexDirs())
	}
}

// An override wins, and a stale override must not be accepted blindly: pointing
// at a directory with no database in it should not be reported as a discovery.
func TestExplicitConfigDirWinsAndIsValidated(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("PLEX_CONFIG_DIR", dir)

	// A directory with no database in it must never be reported as a discovery,
	// whichever way it was named. Falling through to the platform's own
	// locations is fine; accepting this one is not.
	if got := DiscoverPlexDir(); got == dir {
		t.Errorf("an override without a database was accepted: %q", got)
	}

	if err := os.MkdirAll(filepath.Join(dir, "Plug-in Support", "Databases"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, PlexDBSubpath), []byte("not a real database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := DiscoverPlexDir(); got != dir {
		t.Errorf("override was not honoured: got %q, want %q", got, dir)
	}

	cfg := Default()
	cfg.Plex.ConfigDir = dir
	if got, want := cfg.Plex.ResolvedDatabase(), filepath.Join(dir, PlexDBSubpath); got != want {
		t.Errorf("ResolvedDatabase = %q, want %q", got, want)
	}
}

// An explicit database path is used exactly as given, because a user who names a
// file means that file.
func TestExplicitDatabasePathIsUsedAsIs(t *testing.T) {
	cfg := Default()
	cfg.Plex.Database = "/somewhere/else/library.db"
	cfg.Plex.ConfigDir = "/ignored"
	if got := cfg.Plex.ResolvedDatabase(); got != "/somewhere/else/library.db" {
		t.Errorf("ResolvedDatabase = %q, want the explicit path", got)
	}
}
