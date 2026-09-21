package plexapi

import (
	"encoding/json"
	"strconv"
	"strings"

	"github.com/TheIntroDB/plex-sync/internal/model"
)

// This file is the pure half of the package: it turns Plex's JSON, which varies
// between PMS releases, into model types. Nothing here performs I/O, so every
// field-name variant can be exercised with a table test.

// flexInt decodes a JSON value that Plex emits sometimes as a number and
// sometimes as a string (ratingKey is the usual offender), and tolerates null
// and a trailing ".0".
type flexInt int

// UnmarshalJSON accepts 123, "123", 123.0 and null.
func (f *flexInt) UnmarshalJSON(b []byte) error {
	s := strings.TrimSpace(string(b))
	if s == "" || s == "null" {
		*f = 0
		return nil
	}
	if strings.HasPrefix(s, `"`) {
		var str string
		if err := json.Unmarshal(b, &str); err != nil {
			return err
		}
		s = strings.TrimSpace(str)
		if s == "" {
			*f = 0
			return nil
		}
	}
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		fl, ferr := strconv.ParseFloat(s, 64)
		if ferr != nil {
			return err
		}
		n = int64(fl)
	}
	*f = flexInt(n)
	return nil
}

// Int returns the value as an int.
func (f flexInt) Int() int { return int(f) }

// Int64 returns the value as an int64.
func (f flexInt) Int64() int64 { return int64(f) }

// wireDirectory is one entry of /library/sections.
type wireDirectory struct {
	Key   flexInt `json:"key"`
	Title string  `json:"title"`
	Type  string  `json:"type"`
}

// wireGuid is one entry of an item's Guid list.
type wireGuid struct {
	ID string `json:"id"`
}

// wirePart is one file of a media item, with the chapters Plex extracted.
type wirePart struct {
	Duration *float64      `json:"duration"`
	File     string        `json:"file"`
	Chapter  []wireChapter `json:"Chapter"`
}

// wireMedia is one media version of an item.
type wireMedia struct {
	Duration       *float64   `json:"duration"`
	VideoFrameRate string     `json:"videoFrameRate"`
	Part           []wirePart `json:"Part"`
}

// wireChapter is a chapter range. Plex has shipped at least two spellings of
// every one of these fields, so all of them are read.
type wireChapter struct {
	Tag             *string  `json:"tag"`
	Title           *string  `json:"title"`
	StartTimeOffset *float64 `json:"startTimeOffset"`
	StartTime       *float64 `json:"startTime"`
	EndTimeOffset   *float64 `json:"endTimeOffset"`
	EndTime         *float64 `json:"endTime"`
}

// wireMarker is one existing intro or credits marker.
type wireMarker struct {
	ID              flexInt  `json:"id"`
	Type            string   `json:"type"`
	StartTimeOffset *float64 `json:"startTimeOffset"`
	StartTime       *float64 `json:"startTime"`
	EndTimeOffset   *float64 `json:"endTimeOffset"`
	EndTime         *float64 `json:"endTime"`
	Final           *bool    `json:"final"`
}

// wireMetadata is one library item. Guid and Guids are both kept raw: a newer
// Plex sends an array under "Guid", an older one a single string under "guid",
// and decoding either into the other's Go type would fail the whole item.
type wireMetadata struct {
	RatingKey            flexInt         `json:"ratingKey"`
	Type                 string          `json:"type"`
	Title                string          `json:"title"`
	ParentTitle          string          `json:"parentTitle"`
	GrandparentTitle     string          `json:"grandparentTitle"`
	ParentIndex          *float64        `json:"parentIndex"`
	Index                *float64        `json:"index"`
	Duration             *float64        `json:"duration"`
	AddedAt              *int64          `json:"addedAt"`
	LastViewedAt         *int64          `json:"lastViewedAt"`
	GrandparentRatingKey flexInt         `json:"grandparentRatingKey"`
	ParentRatingKey      flexInt         `json:"parentRatingKey"`
	Guid                 json.RawMessage `json:"guid"`
	Guids                json.RawMessage `json:"Guid"`
	Media                []wireMedia     `json:"Media"`
	Chapter              []wireChapter   `json:"Chapter"`
}

// wireContainer is the MediaContainer envelope every endpoint returns.
type wireContainer struct {
	MediaContainer struct {
		Size      flexInt         `json:"size"`
		Total     flexInt         `json:"total"`
		Directory []wireDirectory `json:"Directory"`
		Metadata  []wireMetadata  `json:"Metadata"`
		Marker    []wireMarker    `json:"Marker"`
	} `json:"MediaContainer"`
}

// parseSections maps the sections of a container, skipping entries without a key.
func parseSections(ctr wireContainer) []Section {
	out := make([]Section, 0, len(ctr.MediaContainer.Directory))
	for _, d := range ctr.MediaContainer.Directory {
		if d.Key.Int() == 0 {
			continue
		}
		out = append(out, Section{Key: d.Key.Int(), Title: d.Title, Type: d.Type})
	}
	return out
}

// parseItems maps every Metadata entry of a container onto a library item.
func parseItems(ctr wireContainer, kind model.Kind) []model.LibraryItem {
	out := make([]model.LibraryItem, 0, len(ctr.MediaContainer.Metadata))
	for _, m := range ctr.MediaContainer.Metadata {
		out = append(out, parseItem(m, kind))
	}
	return out
}

// parseItem reduces one Metadata entry to what a lookup needs.
func parseItem(m wireMetadata, kind model.Kind) model.LibraryItem {
	if k, ok := metadataKind(m.Type); ok {
		kind = k
	}

	item := model.LibraryItem{
		RatingKey: m.RatingKey.Int(),
		Kind:      kind,
		Title:     m.Title,
		IDs:       parseExternalIDs(guidIDs(m.Guid, m.Guids)),
	}
	if m.Duration != nil && *m.Duration > 0 {
		d := int64(*m.Duration)
		item.DurationMS = &d
	}
	if m.AddedAt != nil {
		item.AddedAt = *m.AddedAt
	}
	if m.LastViewedAt != nil {
		item.LastViewedAt = *m.LastViewedAt
	}

	if kind == model.KindEpisode {
		item.Season = indexPtr(m.ParentIndex)
		item.Episode = indexPtr(m.Index)
		// grandparentTitle is the show on every version we have seen;
		// parentTitle is the show only when Plex flattens the hierarchy.
		if m.GrandparentTitle != "" {
			item.ShowTitle = m.GrandparentTitle
		} else {
			item.ShowTitle = m.ParentTitle
		}
		if m.GrandparentRatingKey.Int() != 0 {
			item.ShowRatingKey = m.GrandparentRatingKey.Int()
		} else if m.ParentRatingKey.Int() != 0 {
			item.ShowRatingKey = m.ParentRatingKey.Int()
		}
	}

	for i, media := range m.Media {
		item.PartCount += len(media.Part)
		// The item duration is Plex's own runtime; the part duration is the
		// file's real length, and only an unambiguous single-file item may
		// hand it over as such.
		if i == 0 && len(m.Media) == 1 && len(media.Part) == 1 &&
			media.Part[0].Duration != nil && *media.Part[0].Duration > 0 {
			d := int64(*media.Part[0].Duration)
			item.FileDurationMS = &d
		}
	}

	if len(m.Media) > 0 {
		item.FPS = parseFPS(m.Media[0].VideoFrameRate)
	}
	return item
}

// metadataKind maps a metadata type string onto an item kind.
func metadataKind(metadataType string) (model.Kind, bool) {
	switch strings.ToLower(strings.TrimSpace(metadataType)) {
	case "movie":
		return model.KindMovie, true
	case "episode":
		return model.KindEpisode, true
	default:
		return "", false
	}
}

// indexPtr turns a JSON number into a season or episode pointer, leaving it nil
// when the value is missing or not usable.
func indexPtr(v *float64) *int {
	if v == nil {
		return nil
	}
	n := int(*v)
	if n < 0 {
		return nil
	}
	return &n
}

// parseFPS normalises Plex's frame-rate string, which is one of "24p", "pal",
// "ntsc" or a bare number.
func parseFPS(raw string) *float64 {
	s := strings.ToLower(strings.TrimSpace(raw))
	switch s {
	case "":
		return nil
	case "ntsc":
		f := 23.976
		return &f
	case "pal":
		f := 25.0
		return &f
	}
	s = strings.TrimSuffix(s, "p")
	f, err := strconv.ParseFloat(s, 64)
	if err != nil || f <= 0 {
		return nil
	}
	return &f
}

// guidIDs flattens the Guid spellings an item may carry into a plain id list.
func guidIDs(raws ...json.RawMessage) []string {
	var out []string
	for _, raw := range raws {
		s := strings.TrimSpace(string(raw))
		if s == "" || s == "null" {
			continue
		}
		switch s[0] {
		case '"':
			var v string
			if json.Unmarshal(raw, &v) == nil && v != "" {
				out = append(out, v)
			}
		case '{':
			var g wireGuid
			if json.Unmarshal(raw, &g) == nil && g.ID != "" {
				out = append(out, g.ID)
			}
		case '[':
			var objs []wireGuid
			if err := json.Unmarshal(raw, &objs); err == nil {
				for _, o := range objs {
					if o.ID != "" {
						out = append(out, o.ID)
					}
				}
				continue
			}
			var strs []string
			if err := json.Unmarshal(raw, &strs); err == nil {
				out = append(out, strs...)
			}
		}
	}
	return out
}

// parseExternalIDs picks the provider ids out of Plex's guid strings, which
// look like tmdb://1396, imdb://tt0944947, tvdb://121361, or the legacy agent
// form com.plexapp.agents.themoviedb://1396?lang=en.
func parseExternalIDs(guids []string) model.ExternalIDs {
	var ids model.ExternalIDs
	for _, raw := range guids {
		provider, value, ok := splitGUID(raw)
		if !ok {
			continue
		}
		switch provider {
		case "tmdb":
			if ids.TMDB == nil {
				if n, err := strconv.Atoi(value); err == nil {
					ids.TMDB = &n
				}
			}
		case "imdb":
			if ids.IMDb == nil && value != "" {
				v := value
				ids.IMDb = &v
			}
		case "tvdb":
			if ids.TVDB == nil {
				if n, err := strconv.Atoi(value); err == nil {
					ids.TVDB = &n
				}
			}
		}
	}
	return ids
}

// splitGUID separates "tmdb://1396?lang=en" into its provider and its id.
func splitGUID(raw string) (provider, value string, ok bool) {
	s := strings.TrimSpace(raw)
	if s == "" {
		return "", "", false
	}
	scheme := s
	if i := strings.Index(s, "://"); i >= 0 {
		scheme, value = s[:i], s[i+3:]
	}
	if i := strings.IndexByte(value, '?'); i >= 0 {
		value = value[:i]
	}
	value = strings.Trim(strings.TrimSpace(value), "/")
	if value == "" {
		return "", "", false
	}
	// A legacy agent id is com.plexapp.agents.imdb: the provider is the last
	// dotted segment, not the whole scheme.
	name := scheme
	if i := strings.LastIndex(scheme, "."); i >= 0 {
		name = scheme[i+1:]
	}
	switch strings.ToLower(name) {
	case "tmdb", "themoviedb":
		return "tmdb", value, true
	case "imdb":
		return "imdb", value, true
	case "tvdb", "thetvdb":
		return "tvdb", value, true
	default:
		return "", "", false
	}
}

// parseChapters maps the chapter list of an item, dropping any chapter that is
// not a real range.
func parseChapters(ctr wireContainer) []model.Chapter {
	var out []model.Chapter
	for _, m := range ctr.MediaContainer.Metadata {
		for _, media := range m.Media {
			for _, part := range media.Part {
				for _, ch := range part.Chapter {
					start, end := chapterBounds(ch)
					if end <= start {
						continue
					}
					out = append(out, model.Chapter{
						Name:    chapterName(ch),
						StartMS: start,
						EndMS:   end,
					})
				}
			}
		}
	}
	return out
}

// chapterName reads whichever of the chapter's name fields Plex filled in.
func chapterName(ch wireChapter) string {
	if ch.Tag != nil && strings.TrimSpace(*ch.Tag) != "" {
		return strings.TrimSpace(*ch.Tag)
	}
	if ch.Title != nil {
		return strings.TrimSpace(*ch.Title)
	}
	return ""
}

// chapterBounds reads whichever pair of offset fields Plex filled in.
func chapterBounds(ch wireChapter) (start, end int64) {
	switch {
	case ch.StartTimeOffset != nil:
		start = int64(*ch.StartTimeOffset)
	case ch.StartTime != nil:
		start = int64(*ch.StartTime)
	}
	switch {
	case ch.EndTimeOffset != nil:
		end = int64(*ch.EndTimeOffset)
	case ch.EndTime != nil:
		end = int64(*ch.EndTime)
	}
	return start, end
}

// parseMarkers maps Plex's marker list into the shared ExistingMarker shape,
// keeping the server's order as the index.
func parseMarkers(ctr wireContainer) []model.ExistingMarker {
	out := make([]model.ExistingMarker, 0, len(ctr.MediaContainer.Marker))
	for i, m := range ctr.MediaContainer.Marker {
		start, end := markerBounds(m)
		out = append(out, model.ExistingMarker{
			TagID:   m.ID.Int64(),
			Text:    m.Type,
			StartMS: start,
			EndMS:   end,
			Index:   i,
			Origin:  string(model.OriginPlex),
		})
	}
	return out
}

// markerBounds reads the marker's offsets, tolerating either spelling.
func markerBounds(m wireMarker) (start, end int64) {
	switch {
	case m.StartTimeOffset != nil:
		start = int64(*m.StartTimeOffset)
	case m.StartTime != nil:
		start = int64(*m.StartTime)
	}
	switch {
	case m.EndTimeOffset != nil:
		end = int64(*m.EndTimeOffset)
	case m.EndTime != nil:
		end = int64(*m.EndTime)
	}
	return start, end
}
