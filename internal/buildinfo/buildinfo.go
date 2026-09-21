// Package buildinfo carries the values injected at build time.
package buildinfo

import "runtime"

// These are set with -ldflags at build time; see the Makefile.
var (
	// Version is the release version, or a git description in a local build.
	Version = "dev"
	// Commit is the short commit hash the binary was built from.
	Commit = "none"
	// Date is the build timestamp in RFC 3339.
	Date = "unknown"
)

// String renders the version line printed by `plex-sync version`.
func String() string {
	return "plex-sync " + Version + " (" + Commit + ", " + Date + ", " + runtime.Version() + ")"
}

// UserAgent is sent with outbound requests, so a server operator can tell which
// client is talking to them.
func UserAgent() string {
	return "plex-sync/" + Version
}
