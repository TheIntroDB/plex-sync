package plexdb

import (
	"encoding/json"
	"testing"

	"github.com/TheIntroDB/plex-integration/internal/model"
)

// These rows are copied verbatim out of a real Plex database (PMS 1.43.4), one
// per shape Plex writes: a film part with chapters, a Plex-generated optimized
// rendition, and a per-tag blob. They are the ground truth for the encoder,
// which until now was only checked against its own round trip.
const (
	// A movie's own file: chapters present but empty, no markers yet.
	realMoviePart = `{"ma:container":"mp4","ma:has64bitOffsets":"0","ma:optimizedForStreaming":"1",` +
		`"ma:videoProfile":"high","pv:chapters":"{\"Chapters\":{}}",` +
		`"url":"ma%3Acontainer=mp4&ma%3Ahas64bitOffsets=0&ma%3AoptimizedForStreaming=1&ma%3AvideoProfile=high&pv%3Achapters=%7B%22Chapters%22%3A%7B%7D%7D"}`

	// A Plex-generated optimized version, where the value contains characters
	// that must be escaped: a slash, a question mark, an ampersand and an
	// equals sign.
	realOptimizedPart = `{"at:key":"/services/iva/assets/715262/video.mp4?fmt=4&bitrate=5000",` +
		`"ma:container":"mp4",` +
		`"url":"at%3Akey=%2Fservices%2Fiva%2Fassets%2F715262%2Fvideo%2Emp4%3Ffmt%3D4%26bitrate%3D5000&ma%3Acontainer=mp4"}`

	// A streaming profile tag, where the encoded value contains encoded equals
	// signs and an encoded ampersand of its own.
	realStreamProfileTag = `{"sr:deviceProfile":"Universal Mobile",` +
		`"sr:mediaSettings":"advancedSubtitles=burn&autoAdjustQuality=0",` +
		`"url":"sr%3AdeviceProfile=Universal%20Mobile&sr%3AmediaSettings=advancedSubtitles%3Dburn%26autoAdjustQuality%3D0"}`
)

// coreMembers returns the non-url members of a real row as string values, which
// is exactly what EncodeExtra takes.
func coreMembers(t *testing.T, raw string) map[string]string {
	t.Helper()
	var members map[string]json.RawMessage
	if err := json.Unmarshal([]byte(raw), &members); err != nil {
		t.Fatalf("the captured row is not valid JSON: %v", err)
	}
	core := map[string]string{}
	for name, value := range members {
		if name == "url" {
			continue
		}
		var asString string
		if err := json.Unmarshal(value, &asString); err == nil {
			core[name] = asString
			continue
		}
		core[name] = string(value)
	}
	return core
}

// TestEncodeExtraReproducesRealPlexBytes is the check that matters: given the
// members of a row Plex itself wrote, the encoder must produce that row's bytes
// exactly, including the url it built from them.
func TestEncodeExtraReproducesRealPlexBytes(t *testing.T) {
	for name, raw := range map[string]string{
		"movie part":        realMoviePart,
		"optimized part":    realOptimizedPart,
		"streaming profile": realStreamProfileTag,
	} {
		t.Run(name, func(t *testing.T) {
			got := EncodeExtra(coreMembers(t, raw))
			if got != raw {
				t.Errorf("the encoder does not reproduce what Plex wrote.\n plex: %s\n ours: %s", raw, got)
			}
		})
	}
}

// The rule is that only A-Z a-z 0-9 underscore and hyphen stay literal. This
// pins every character class the captured rows exercise, so a regression in the
// encoder is caught here rather than in someone's library.
func TestEncodeExtraEscapesExactlyWhatPlexDoes(t *testing.T) {
	cases := map[string]struct{ value, want string }{
		"space is %20, never a plus": {
			value: "Universal Mobile",
			want:  "Universal%20Mobile",
		},
		"slashes and colons in a path": {
			value: "/services/iva/assets/715262/video.mp4",
			want:  "%2Fservices%2Fiva%2Fassets%2F715262%2Fvideo%2Emp4",
		},
		"question mark and ampersand": {
			value: "video.mp4?fmt=4&bitrate=5000",
			want:  "video%2Emp4%3Ffmt%3D4%26bitrate%3D5000",
		},
		"braces and quotes from a nested blob": {
			value: `{"Chapters":{}}`,
			want:  "%7B%22Chapters%22%3A%7B%7D%7D",
		},
		"only these stay literal": {
			value: "AZaz09_-",
			want:  "AZaz09_-",
		},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			if got := percentEncodeExtra(tc.value); got != tc.want {
				t.Errorf("percentEncodeExtra(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

// Rewriting a real row must keep every member Plex put there, add ours, and
// leave the row's other data intact.
func TestRewriteExtraKeepsEverythingPlexStored(t *testing.T) {
	intros := []model.Marker{{Text: model.MarkerIntro, StartMS: 0, EndMS: 37_000}}
	credits := []model.Marker{{Text: model.MarkerCredits, StartMS: 5_614_000, EndMS: 6_098_400, Final: true}}

	out, ok := RewriteExtra(realMoviePart, []string{"intro", "credits"}, intros, credits)
	if !ok {
		t.Fatal("a row Plex wrote must be rewritable")
	}

	var rewritten map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &rewritten); err != nil {
		t.Fatalf("the rewritten row is not valid JSON: %v\n%s", err, out)
	}

	// Plex's own members survive untouched.
	for _, name := range []string{"ma:container", "ma:has64bitOffsets", "ma:optimizedForStreaming", "ma:videoProfile", "pv:chapters"} {
		before := coreMembers(t, realMoviePart)[name]
		var after string
		if err := json.Unmarshal(rewritten[name], &after); err != nil {
			t.Errorf("member %q is missing or not a string after rewriting", name)
			continue
		}
		if after != before {
			t.Errorf("member %q changed: %q became %q", name, before, after)
		}
	}

	// Ours are added.
	for _, name := range []string{"pv:intros", "pv:credits"} {
		if _, present := rewritten[name]; !present {
			t.Errorf("member %q was not added", name)
		}
	}

	// The url is rebuilt rather than kept: it has to describe every member.
	var rebuilt string
	if err := json.Unmarshal(rewritten["url"], &rebuilt); err != nil {
		t.Fatalf("url is not a string: %v", err)
	}
	if want := EncodeExtra(coreMembers(t, out)); want != out {
		t.Errorf("the rewritten row does not round trip through the encoder:\n want %s\n got  %s", want, out)
	}
	if rebuilt == "" {
		t.Error("url was left empty")
	}

	// The final flag has to survive into the credits blob, because it is what
	// raises Up Next.
	var creditsBlob string
	if err := json.Unmarshal(rewritten["pv:credits"], &creditsBlob); err != nil {
		t.Fatalf("pv:credits is not a string: %v", err)
	}
	if !contains(creditsBlob, "final") {
		t.Errorf("the final credits marker lost its final flag: %s", creditsBlob)
	}
	var introBlob string
	if err := json.Unmarshal(rewritten["pv:intros"], &introBlob); err != nil {
		t.Fatalf("pv:intros is not a string: %v", err)
	}
	if !contains(introBlob, "MediaPartMarker") {
		t.Errorf("pv:intros does not carry a marker array: %s", introBlob)
	}
}

func contains(haystack, needle string) bool {
	for i := 0; i+len(needle) <= len(haystack); i++ {
		if haystack[i:i+len(needle)] == needle {
			return true
		}
	}
	return false
}

// A credits marker the planner did not flag must not come back final. Getting
// this wrong tells Plex an item ends early, which is what raises Up Next, so it
// is pinned here.
func TestRewriteExtraDoesNotInventAFinalMarker(t *testing.T) {
	credits := []model.Marker{
		{Text: model.MarkerCredits, StartMS: 1000, EndMS: 2000},
		{Text: model.MarkerCredits, StartMS: 3000, EndMS: 4000},
	}
	out, ok := RewriteExtra(realMoviePart, []string{"credits"}, nil, credits)
	if !ok {
		t.Fatal("expected a rewrite")
	}
	var decoded map[string]json.RawMessage
	if err := json.Unmarshal([]byte(out), &decoded); err != nil {
		t.Fatalf("rewritten row is not valid JSON: %v", err)
	}
	blob := memberString(t, decoded["pv:credits"])
	if contains(blob, "final") {
		t.Errorf("no marker was flagged final, but the payload carries one: %s", blob)
	}
}

// memberString unwraps one member of an extra_data object.
//
// Plex stores nested values as JSON strings, so the value has to be unquoted
// before it can be parsed. A member that is not a string means the encoder
// produced something Plex has never written.
func memberString(t *testing.T, raw json.RawMessage) string {
	t.Helper()
	var asString string
	if err := json.Unmarshal(raw, &asString); err != nil {
		t.Fatalf("a member is not stored as a JSON string, which is what Plex writes: %v (%s)", err, raw)
	}
	return asString
}
