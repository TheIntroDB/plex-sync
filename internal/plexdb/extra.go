package plexdb

import (
	"bytes"
	"encoding/json"
	"io"
	"sort"
	"strconv"
	"strings"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// The bytes Plex expects in taggings.extra_data for a marker row. These are
// byte-exact literals: Plex compares extra_data to decide whether an existing
// row still carries its marker payload, and it leaves a row alone (or hides it
// in the UI) when the payload is missing. "pv:version" is the payload schema
// version of the row itself and the url member is the same information Plex
// encodes for media_parts.extra_data.
const (
	// extraIntro is an intro marker at payload version 5.
	extraIntro = `{"pv:version":"5","url":"pv%3Aversion=5"}`

	// extraCredits is a non-final credits marker at payload version 4.
	extraCredits = `{"pv:version":"4","url":"pv%3Aversion=4"}`

	// extraCreditsFinal is the credits marker Plex treats as the end of the
	// item, which is what raises the Up Next prompt. It carries pv:final 1.
	extraCreditsFinal = `{"pv:final":"1","pv:version":"4","url":"pv%3Afinal=1&pv%3Aversion=4"}`
)

// extraURLMember is the member of media_parts.extra_data that holds every other
// member re-encoded as a query string.
const extraURLMember = "url"

// extraIntroMember and extraCreditsMember are the media_parts.extra_data
// members that describe the markers Plex discovered for a file.
const (
	extraIntroMember   = "pv:intros"
	extraCreditsMember = "pv:credits"
)

// EncodeExtra builds the JSON object Plex stores in media_parts.extra_data from
// already-stringified members: each member is joined as key=value into the url
// member and then the members are emitted alongside it in sorted order.
func EncodeExtra(core map[string]string) string {
	members := make(map[string]any, len(core))
	for name, value := range core {
		if name == extraURLMember {
			continue
		}
		members[name] = value
	}
	if len(members) == 0 {
		return ""
	}
	return encodeExtraMembers(members)
}

// RewriteExtra returns the media_parts.extra_data value for a part after the
// given marker types changed, and whether the row needs writing at all.
//
// types names the marker kinds this write touched ("intro", "credits"); a kind
// that is not named is left exactly as it was. intros and credits are the
// marker set the item should end up with, so the rewritten members describe the
// final state rather than the delta.
//
// A part whose extra_data is absent, unparseable, or carries no url member (the
// pre-1.40 legacy format) is returned unchanged with false: Plex rewrites those
// itself on its next pass and touching them is what corrupts a library.
func RewriteExtra(raw string, types []string, intros, credits []model.Marker) (string, bool) {
	members, ok := decodeExtraMembers(raw)
	if !ok {
		return raw, false
	}

	touched := false
	for _, t := range types {
		switch strings.ToLower(strings.TrimSpace(t)) {
		case string(model.MarkerIntro):
			members[extraIntroMember] = introExtraMember(intros)
			touched = true
		case string(model.MarkerCredits):
			members[extraCreditsMember] = creditsExtraMember(credits)
			touched = true
		}
	}
	if !touched {
		return raw, false
	}
	return encodeExtraMembers(members), true
}

// MarkerExtraData returns the byte-exact taggings.extra_data payload for a
// marker that is about to be written.
func MarkerExtraData(m model.Marker) string {
	if m.Text == model.MarkerIntro {
		return extraIntro
	}
	if m.Final {
		return extraCreditsFinal
	}
	return extraCredits
}

// decodeExtraMembers parses a media_parts.extra_data blob. It reports false for
// an empty value, for anything that is not a JSON object, and for the legacy
// pre-1.40 format that carries no url member.
//
// Numbers are decoded as json.Number so a value such as a version is emitted
// again byte for byte instead of being reformatted through a float.
func decodeExtraMembers(raw string) (map[string]any, bool) {
	if strings.TrimSpace(raw) == "" {
		return nil, false
	}
	dec := json.NewDecoder(strings.NewReader(raw))
	dec.UseNumber()
	var members map[string]any
	if err := dec.Decode(&members); err != nil {
		return nil, false
	}
	// Reject trailing data: a blob that decodes to an object followed by
	// anything else is not something Plex wrote.
	if _, err := dec.Token(); err != io.EOF {
		return nil, false
	}
	if members == nil {
		return nil, false
	}
	if _, ok := members[extraURLMember]; !ok {
		return nil, false
	}
	return members, true
}

// introExtraMember is the pv:intros member: the intro markers Plex shows on the
// scrubber. With nothing to show the marker list encodes as an empty string,
// which is how Plex records "this file has no intro markers".
func introExtraMember(intros []model.Marker) map[string]any {
	array := map[string]any{
		"attributeName": "intros",
		"version":       5,
	}
	if len(intros) == 0 {
		array["MediaPartMarker"] = ""
	} else {
		list := make([]any, 0, len(intros))
		for _, m := range intros {
			list = append(list, map[string]any{
				"startTimeOffset": m.StartMS,
				"endTimeOffset":   m.EndMS,
			})
		}
		array["MediaPartMarker"] = list
	}
	return map[string]any{"MediaPartMarkersArray": array}
}

// creditsExtraMember is the pv:credits member. With no credits markers the
// array carries only its name and version.
//
// Exactly one credits marker is marked final: the one the plan flagged, which
// is the marker Plex treats as the end of the item. When a plan marks none, the
// last credits marker by start time is treated as the final one, because a
// credits array that never ends the item does not raise Up Next.
func creditsExtraMember(credits []model.Marker) map[string]any {
	array := map[string]any{
		"attributeName": "credits",
		"version":       4,
	}
	if len(credits) == 0 {
		return map[string]any{"MediaPartMarkersArray": array}
	}

	sorted := make([]model.Marker, len(credits))
	copy(sorted, credits)
	sort.SliceStable(sorted, func(i, j int) bool { return sorted[i].StartMS < sorted[j].StartMS })

	// The final flag is the planner's decision, not the writer's. Marking the
	// last credits marker final by default would tell Plex an item ends where
	// it does not, and would disagree with the payload written to taggings for
	// the same marker.
	finalIndex := -1
	for i, m := range sorted {
		if m.Final {
			finalIndex = i
			break
		}
	}

	list := make([]any, 0, len(sorted))
	for i, m := range sorted {
		entry := map[string]any{
			"startTimeOffset": m.StartMS,
			"endTimeOffset":   m.EndMS,
		}
		if i == finalIndex {
			entry["final"] = true
		}
		list = append(list, entry)
	}
	array["MediaPartMarker"] = list
	return map[string]any{"MediaPartMarkersArray": array}
}

// encodeExtraMembers emits the media_parts.extra_data JSON object from its
// members.
//
// Plex keeps the same information twice: once as real members and once as a
// query string in the url member. The query string is every member except url,
// joined as key=value with an ampersand, where both the key and the value are
// percent-encoded so that only A-Z a-z 0-9 underscore and hyphen survive
// unescaped. Nested objects and arrays are JSON-encoded into their value;
// scalars are stringified.
//
// The final object carries the members plus url, with all names in sorted order.
func encodeExtraMembers(members map[string]any) string {
	names := make([]string, 0, len(members))
	for name := range members {
		if name == extraURLMember {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	var pairs bytes.Buffer
	for i, name := range names {
		if i > 0 {
			pairs.WriteByte('&')
		}
		pairs.WriteString(percentEncodeExtra(name))
		pairs.WriteByte('=')
		pairs.WriteString(percentEncodeExtra(stringifyMember(members[name])))
	}

	out := make([]string, 0, len(names)+1)
	out = append(out, names...)
	out = append(out, extraURLMember)
	sort.Strings(out)

	var b bytes.Buffer
	b.WriteByte('{')
	for i, name := range out {
		if i > 0 {
			b.WriteByte(',')
		}
		key, err := jsonEncode(name)
		if err != nil {
			continue
		}
		b.Write(key)
		b.WriteByte(':')

		value := members[name]
		if name == extraURLMember {
			value = pairs.String()
		} else if stringifyNested(value) {
			// Plex stores nested values as JSON strings, not as nested objects:
			// a real row holds "pv:chapters":"{\"Chapters\":{}}". Emitting an
			// object here would produce a row that no version of Plex has ever
			// written. Scalars are left as they are, so a value Plex stored as a
			// number does not silently become a string.
			value = stringifyMember(value)
		}
		encoded, err := jsonEncode(value)
		if err != nil {
			continue
		}
		b.Write(encoded)
	}
	b.WriteByte('}')
	return b.String()
}

// jsonEncode marshals a value the way Plex writes it: with HTML escaping turned
// off, so the ampersand that joins the url member's pairs stays an ampersand
// instead of becoming \u0026.
func jsonEncode(v any) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(v); err != nil {
		return nil, err
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

// stringifyNested reports whether a member value has to be stored as a JSON
// string rather than as itself.
//
// Plex writes nested values as strings. Copying a real row out of a database
// shows "pv:chapters" holding the characters {"Chapters":{}} rather than a
// nested object, and rebuilding that row has to produce the same thing or the
// row stops looking like anything Plex ever wrote.
func stringifyNested(v any) bool {
	switch t := v.(type) {
	case map[string]any, []any, map[string]string, []string:
		return true
	case json.RawMessage:
		trimmed := bytes.TrimSpace(t)
		return len(trimmed) > 0 && (trimmed[0] == '{' || trimmed[0] == '[')
	default:
		return false
	}
}

// stringifyMember renders one member value for the url query string: a string
// is itself, an object or array is JSON-encoded, and a scalar is printed the
// way JSON prints it.
func stringifyMember(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	case json.Number:
		return t.String()
	case bool:
		return strconv.FormatBool(t)
	case float64:
		return strconv.FormatFloat(t, 'f', -1, 64)
	case json.RawMessage:
		return string(t)
	default:
		encoded, err := jsonEncode(v)
		if err != nil {
			return ""
		}
		return string(encoded)
	}
}

// percentEncodeExtra escapes everything that is not an unreserved character.
//
// net/url.QueryEscape is not enough: it leaves ".", "~", "!", "*", "'" and the
// parentheses alone, all of which Plex escapes. net/url.Values.Encode is worse
// still: it spells a space as "+" and sorts by a different rule. The encoding is
// therefore spelled out here, one byte at a time, with uppercase hex.
func percentEncodeExtra(s string) string {
	const hexDigits = "0123456789ABCDEF"
	var b strings.Builder
	b.Grow(len(s) + len(s)/4)
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9', c == '_', c == '-':
			b.WriteByte(c)
		default:
			b.WriteByte('%')
			b.WriteByte(hexDigits[c>>4])
			b.WriteByte(hexDigits[c&0x0f])
		}
	}
	return b.String()
}
