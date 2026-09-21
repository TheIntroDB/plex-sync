package tui

import (
	"strings"
	"testing"

	"github.com/TheIntroDB/plex-sync/internal/buildinfo"
)

// The version has to be on the screen, on the first line, because the first
// question about any binary is which one it is.
func TestTheHeaderShowsTheVersion(t *testing.T) {
	original := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = original })

	m := newTestModel(t)
	first := strings.SplitN(m.View(), "\n", 2)[0]

	if !strings.Contains(first, shortVersion()) {
		t.Errorf("the first line is %q, which does not carry the version %q",
			first, shortVersion())
	}
}

func TestTheHeaderNamesARelease(t *testing.T) {
	original := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = original })

	for _, tc := range []struct {
		injected string
		want     string
	}{
		{"0.1.0", "v0.1.0"},
		{"v0.1.0", "v0.1.0"},
		{"0.1.0-beta.1", "v0.1.0-beta.1"},
		{"dev", "dev build"},
		{"", "dev build"},
		{"  ", "dev build"},
	} {
		buildinfo.Version = tc.injected
		if got := shortVersion(); got != tc.want {
			t.Errorf("injected %q showed %q, want %q", tc.injected, got, tc.want)
		}

		// And it reaches the screen.
		m := newTestModel(t)
		if first := strings.SplitN(m.View(), "\n", 2)[0]; !strings.Contains(first, tc.want) {
			t.Errorf("injected %q: the first line is %q, want it to contain %q",
				tc.injected, first, tc.want)
		}
	}
}

// A build nobody stamped must not claim a version it does not have. A binary
// compiled by hand saying v0.1.0 would be worse than saying nothing.
func TestAnUnstampedBuildSaysSo(t *testing.T) {
	original := buildinfo.Version
	t.Cleanup(func() { buildinfo.Version = original })

	buildinfo.Version = "dev"
	if got := shortVersion(); strings.ContainsAny(got, "0123456789") {
		t.Errorf("an unstamped build reported %q, which looks like a version", got)
	}
}
