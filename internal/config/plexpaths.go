package config

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// plexDirsForOS lists the application-support directories Plex uses on one
// platform, most likely first.
//
// The directory is not the same everywhere, and on Linux it depends on how Plex
// was installed, so the list covers the distributions rather than guessing at
// one. Every entry is only a candidate: discovery returns the first one that
// actually holds a database, so a wrong entry costs nothing.
func plexDirsForOS(goos string, env func(string) string) []string {
	home := env("HOME")
	localAppData := env("LOCALAPPDATA")
	appData := env("APPDATA")

	switch goos {
	case "darwin":
		var dirs []string
		if home != "" {
			dirs = append(dirs, filepath.Join(home, "Library", "Application Support", "Plex Media Server"))
		}
		return dirs

	case "windows":
		var dirs []string
		// Plex keeps its data under LOCALAPPDATA, not APPDATA.
		if localAppData != "" {
			dirs = append(dirs, filepath.Join(localAppData, "Plex Media Server"))
		}
		if appData != "" {
			dirs = append(dirs, filepath.Join(appData, "Plex Media Server"))
		}
		return dirs

	case "freebsd", "openbsd", "netbsd":
		return []string{
			"/usr/local/plexdata-plexpass/Plex Media Server",
			"/usr/local/plexdata/Plex Media Server",
		}
	}

	// Linux, and anything else unix-like.
	return []string{
		// The container images, official and linuxserver.io alike.
		"/config/Library/Application Support/Plex Media Server",
		// Native packages: .deb and .rpm.
		"/var/lib/plexmediaserver/Library/Application Support/Plex Media Server",
		// Snap.
		"/var/snap/plexmediaserver/common/Library/Application Support/Plex Media Server",
		// Docker with the data directory mounted at /opt, and older builds.
		"/opt/plex/Library/Application Support/Plex Media Server",
		"/usr/lib/plexmediaserver/Library/Application Support/Plex Media Server",
	}
}

// PlatformPlexDirs returns the candidate directories for the running platform.
func PlatformPlexDirs() []string {
	return plexDirsForOS(runtime.GOOS, os.Getenv)
}

// plexDatabaseIn reports whether a directory holds a Plex database.
func plexDatabaseIn(dir string) bool {
	if strings.TrimSpace(dir) == "" {
		return false
	}
	st, err := os.Stat(filepath.Join(dir, PlexDBSubpath))
	return err == nil && !st.IsDir()
}
