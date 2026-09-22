// Package plexapi talks to Plex Media Server over its HTTP API.
//
// It is read-only by necessity: Plex has no write API for intro or credits
// markers, so this package only ever enumerates libraries, reads ids, chapters
// and existing markers, and asks whether Plex is busy. Writing markers is the
// job of the SQLite package.
//
// Every request goes through internal/httpclient (Fiber's client underneath),
// so the whole tool shares one connection pool and one timeout policy. A
// non-2xx status is not an error at this layer: the methods decide what a 404
// from an endpoint means.
package plexapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/TheIntroDB/plex-sync/internal/buildinfo"
	"github.com/TheIntroDB/plex-sync/internal/config"
	"github.com/TheIntroDB/plex-sync/internal/httpclient"
	"github.com/TheIntroDB/plex-sync/internal/model"
)

const (
	// Product is the X-Plex-Product value sent with every request.
	Product = "plex-sync"
	// DefaultURL applies when the configuration leaves plex.url empty.
	DefaultURL = "http://127.0.0.1:32400"
	// ItemWindow is how many items a paged enumeration asks for per request.
	ItemWindow = 200
	// PreferencesFile is the Plex preferences file inside the config dir.
	PreferencesFile = "Preferences.xml"

	// metadataTypeMovie and metadataTypeEpisode are Plex's metadata_type values,
	// which the /all endpoint takes as ?type=.
	metadataTypeMovie   = 1
	metadataTypeEpisode = 4
)

// Version and UserAgent report the real build rather than a constant that has to
// be remembered at release time, so a server operator reading their logs sees
// which build is talking to them.
var (
	// Version is the X-Plex-Version value sent with every request.
	Version = buildinfo.Version
	// UserAgent is the HTTP User-Agent of the shared client.
	UserAgent = Product + "/" + Version
)

// HTTPError is a non-2xx answer that the caller did not treat as a fallback.
type HTTPError struct {
	Status int
	Method string
	URL    string
	Body   string
}

// Error renders the failure with a short sample of the body.
func (e *HTTPError) Error() string {
	msg := fmt.Sprintf("plex: %s %s: HTTP %d", e.Method, e.URL, e.Status)
	if body := strings.TrimSpace(e.Body); body != "" {
		if len(body) > 200 {
			body = body[:200] + "..."
		}
		msg += ": " + body
	}
	return msg
}

// Section is one Plex library section.
type Section struct {
	Key   int    `json:"key"`
	Title string `json:"title"`
	Type  string `json:"type"` // movie, show, artist, photo
}

// Client reads from one Plex server.
type Client struct {
	cfg        config.Plex
	hc         *httpclient.Client
	identifier string
	owned      bool
}

// NewClient builds a client over an existing HTTP client.
//
// When hc is nil the client builds its own from cfg.TimeoutS and
// cfg.InsecureSkipVerify, and Close releases it. A caller-supplied client is
// never closed here, because it is usually shared with TheIntroDB.
func NewClient(cfg config.Plex, hc *httpclient.Client) *Client {
	base := strings.TrimSpace(cfg.URL)
	if base == "" {
		base = DefaultURL
	}
	if !strings.Contains(base, "://") {
		base = "http://" + base
	}
	cfg.URL = strings.TrimRight(base, "/")

	owned := false
	if hc == nil {
		timeout := time.Duration(cfg.TimeoutS * float64(time.Second))
		if timeout <= 0 {
			timeout = httpclient.DefaultTimeout
		}
		hc = httpclient.New(timeout, cfg.InsecureSkipVerify, UserAgent)
		owned = true
	}
	return &Client{cfg: cfg, hc: hc, identifier: clientIdentifier(), owned: owned}
}

// Close releases the connection pool, but only when this client built it.
func (c *Client) Close() {
	if c.owned && c.hc != nil {
		c.hc.Close()
	}
}

// URL returns the base URL this client talks to.
func (c *Client) URL() string { return c.cfg.URL }

// clientIdentifier is a stable-per-process X-Plex-Client-Identifier. Plex keys
// its token and activity records on it, so it must not change per request.
func clientIdentifier() string {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err == nil {
		return Product + "-" + hex.EncodeToString(buf[:])
	}
	return Product + "-" + strconv.FormatInt(time.Now().UnixNano(), 16)
}

// headers returns the headers every Plex request needs.
func (c *Client) headers() map[string]string {
	return map[string]string{
		"X-Plex-Token":             c.cfg.Token,
		"Accept":                   "application/json",
		"X-Plex-Client-Identifier": c.identifier,
		"X-Plex-Product":           Product,
		"X-Plex-Version":           Version,
	}
}

// do performs a GET against a server-relative path.
func (c *Client) do(
	ctx context.Context,
	path string,
	query url.Values,
	extra map[string]string,
) (*httpclient.Response, error) {
	hdr := c.headers()
	for k, v := range extra {
		hdr[k] = v
	}
	return c.hc.Do(ctx, "GET", c.cfg.URL+path, hdr, query)
}

// container fetches a path and decodes the MediaContainer envelope.
func (c *Client) container(
	ctx context.Context,
	path string,
	query url.Values,
	extra map[string]string,
) (*wireContainer, error) {
	resp, err := c.do(ctx, path, query, extra)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &HTTPError{Status: resp.Status, Method: "GET", URL: c.cfg.URL + path, Body: resp.String()}
	}
	var out wireContainer
	if err := resp.JSON(&out); err != nil {
		return nil, fmt.Errorf("plex: decode %s: %w", path, err)
	}
	return &out, nil
}

// Identity reports the server's MediaContainer, which proves Plex is up.
func (c *Client) Identity(ctx context.Context) (map[string]any, error) {
	resp, err := c.do(ctx, "/identity", nil, nil)
	if err != nil {
		return nil, err
	}
	if resp.Status < 200 || resp.Status >= 300 {
		return nil, &HTTPError{Status: resp.Status, Method: "GET", URL: c.cfg.URL + "/identity", Body: resp.String()}
	}
	var envelope struct {
		MediaContainer map[string]any `json:"MediaContainer"`
	}
	if err := resp.JSON(&envelope); err != nil {
		return nil, fmt.Errorf("plex: decode /identity: %w", err)
	}
	if envelope.MediaContainer == nil {
		envelope.MediaContainer = map[string]any{}
	}
	return envelope.MediaContainer, nil
}

// Sections lists the server's library sections.
func (c *Client) Sections(ctx context.Context) ([]Section, error) {
	ctr, err := c.container(ctx, "/library/sections", nil, nil)
	if err != nil {
		return nil, err
	}
	return parseSections(*ctr), nil
}

// Items enumerates the movies and episodes of the given sections, or of every
// video section when sectionKeys is empty.
func (c *Client) Items(ctx context.Context, sectionKeys []int) ([]model.LibraryItem, error) {
	sections, err := c.Sections(ctx)
	if err != nil {
		return nil, err
	}
	wanted := make(map[int]bool, len(sectionKeys))
	for _, k := range sectionKeys {
		wanted[k] = true
	}

	var out []model.LibraryItem
	for _, sec := range sections {
		kind, metadataType, ok := sectionKind(sec.Type)
		if !ok {
			continue
		}
		if len(wanted) > 0 && !wanted[sec.Key] {
			continue
		}
		items, err := c.sectionItems(ctx, sec.Key, metadataType, kind)
		if err != nil {
			return out, err
		}
		out = append(out, items...)
	}
	return out, nil
}

// sectionItems reads one section, following Plex's container paging when the
// server reports more items than the first answer carried.
func (c *Client) sectionItems(ctx context.Context, key, metadataType int, kind model.Kind) ([]model.LibraryItem, error) {
	path := "/library/sections/" + strconv.Itoa(key) + "/all"
	query := url.Values{
		"type":         {strconv.Itoa(metadataType)},
		"includeGuids": {"1"},
	}

	ctr, err := c.container(ctx, path, query, nil)
	if err != nil {
		return nil, err
	}
	out := parseItems(*ctr, kind)
	total := ctr.MediaContainer.Total.Int()

	for start := len(out); total > 0 && start < total; {
		extra := map[string]string{
			"X-Plex-Container-Start": strconv.Itoa(start),
			"X-Plex-Container-Size":  strconv.Itoa(ItemWindow),
		}
		page, err := c.container(ctx, path, query, extra)
		if err != nil {
			return out, err
		}
		batch := parseItems(*page, kind)
		if len(batch) == 0 {
			break
		}
		out = append(out, batch...)
		start += len(batch)
	}
	return out, nil
}

// Chapters returns the chapters Plex extracted from an item's file.
func (c *Client) Chapters(ctx context.Context, ratingKey int) ([]model.Chapter, error) {
	path := "/library/metadata/" + strconv.Itoa(ratingKey)
	query := url.Values{"includeChapters": {"1"}}
	ctr, err := c.container(ctx, path, query, nil)
	if err != nil {
		return nil, err
	}
	return parseChapters(*ctr), nil
}

// Markers returns the intro and credits markers Plex already holds for an item.
//
// Plex only grew this endpoint recently: a 400 or 404 means the server is too
// old or the item carries no markers, and both are answered with an empty
// slice and no error, because callers fall back to the database.
func (c *Client) Markers(ctx context.Context, ratingKey int) ([]model.ExistingMarker, error) {
	path := "/library/metadata/" + strconv.Itoa(ratingKey) + "/markers"
	resp, err := c.do(ctx, path, nil, nil)
	if err != nil {
		return nil, err
	}
	switch {
	case resp.Status == 400 || resp.Status == 404:
		return []model.ExistingMarker{}, nil
	case resp.Status < 200 || resp.Status >= 300:
		return nil, &HTTPError{Status: resp.Status, Method: "GET", URL: c.cfg.URL + path, Body: resp.String()}
	}
	var ctr wireContainer
	if err := resp.JSON(&ctr); err != nil {
		return nil, fmt.Errorf("plex: decode %s: %w", path, err)
	}
	return parseMarkers(ctr), nil
}

// ActiveSessions returns how many streams Plex is serving right now. Zero means
// it is safe to write markers without a playback session watching them.
func (c *Client) ActiveSessions(ctx context.Context) (int, error) {
	ctr, err := c.container(ctx, "/status/sessions", nil, nil)
	if err != nil {
		return 0, err
	}
	return ctr.MediaContainer.Size.Int(), nil
}

// Running reports whether the Plex server is answering at all.
//
// It returns an error only when the server genuinely cannot be reached, so
// callers that must not write while Plex holds the database fail closed.
func (c *Client) Running(ctx context.Context) (bool, error) {
	// Any answer at all, including a 401, proves the process is listening.
	if _, err := c.do(ctx, "/identity", nil, nil); err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return false, err
		}
		return false, fmt.Errorf("plex: cannot tell whether Plex is running: %w", err)
	}
	return true, nil
}

// TokenFromPrefs returns the token Plex uses for local API access.
//
// It is the fallback for a setup that has no token in its config file. Modern
// Plex versions write a per-install ".LocalAdminToken" beside the database,
// which is checked first; "Preferences.xml" covers the Linux, Windows and
// container distributions. An unreadable or tokenless setup yields "", never an
// error: a missing token is reported later, when a request actually fails.
func TokenFromPrefs(configDir string) string {
	dir := strings.TrimSpace(configDir)
	if dir == "" {
		dir = config.DiscoverPlexDir()
	}
	return config.ReadPlexToken(dir)
}

// sectionKind maps a Plex section type onto the item kind and the metadata_type
// its /all endpoint takes.
func sectionKind(sectionType string) (model.Kind, int, bool) {
	switch strings.ToLower(strings.TrimSpace(sectionType)) {
	case "movie":
		return model.KindMovie, metadataTypeMovie, true
	case "show":
		return model.KindEpisode, metadataTypeEpisode, true
	default:
		return "", 0, false
	}
}
