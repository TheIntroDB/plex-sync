// Package httpclient is the single outbound HTTP path for the whole tool.
//
// It wraps Fiber's client (which is fasthttp underneath) so that Plex and
// TheIntroDB requests share one connection pool, one timeout policy and one
// place to record what was sent. Every outbound call in the program goes
// through here.
package httpclient

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	fiberclient "github.com/gofiber/fiber/v3/client"
)

// DefaultTimeout applies when a caller does not set one.
const DefaultTimeout = 20 * time.Second

// Response is a completed HTTP response, with the body already copied out of
// Fiber's pooled buffers.
type Response struct {
	Status  int
	Headers map[string]string
	Body    []byte
	// Elapsed is how long the request took, for the diagnostics screen.
	Elapsed time.Duration
}

// JSON decodes the body into v.
func (r *Response) JSON(v any) error {
	if len(r.Body) == 0 {
		return errors.New("empty response body")
	}
	if err := json.Unmarshal(r.Body, v); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	return nil
}

// String returns the body as a string, trimmed of surrounding whitespace.
func (r *Response) String() string { return strings.TrimSpace(string(r.Body)) }

// Header returns a single header value, or "".
func (r *Response) Header(key string) string { return r.Headers[strings.ToLower(key)] }

// IntHeader returns a header parsed as an integer, and false when absent or
// not a number. TheIntroDB reports its remaining quota this way.
func (r *Response) IntHeader(key string) (int, bool) {
	raw := r.Header(key)
	if raw == "" {
		return 0, false
	}
	var n int
	if _, err := fmt.Sscanf(raw, "%d", &n); err != nil {
		return 0, false
	}
	return n, true
}

// Client performs requests through Fiber's client.
type Client struct {
	fc      *fiberclient.Client
	timeout time.Duration
}

// New builds a client. insecureSkipVerify exists only for a self-signed local
// Plex; it is never enabled by default.
func New(timeout time.Duration, insecureSkipVerify bool, userAgent string) *Client {
	fc := fiberclient.New()
	if timeout <= 0 {
		timeout = DefaultTimeout
	}
	if insecureSkipVerify {
		cfg := &tls.Config{InsecureSkipVerify: true} //nolint:gosec // opt-in for local Plex
		fc.SetTLSConfig(cfg)
	}
	if userAgent != "" {
		fc.SetUserAgent(userAgent)
	}
	return &Client{fc: fc, timeout: timeout}
}

// Close releases idle connections.
func (c *Client) Close() { c.fc.CloseIdleConnections() }

// Fiber exposes the underlying Fiber client, so a caller that needs something
// this wrapper does not cover is not blocked.
func (c *Client) Fiber() *fiberclient.Client { return c.fc }

// Do performs a request and returns the response with its body copied.
func (c *Client) Do(
	ctx context.Context,
	method, rawURL string,
	headers map[string]string,
	query url.Values,
) (*Response, error) {
	if rawURL == "" {
		return nil, errors.New("httpclient: empty URL")
	}
	if ctx == nil {
		ctx = context.Background()
	}

	req := c.fc.R().SetContext(ctx).SetTimeout(c.timeout)
	for k, v := range headers {
		req.SetHeader(k, v)
	}
	for k, values := range query {
		for _, v := range values {
			req.AddParam(k, v)
		}
	}

	started := time.Now()
	var (
		resp *fiberclient.Response
		err  error
	)
	switch strings.ToUpper(method) {
	case "GET", "":
		resp, err = req.Get(rawURL)
	case "POST":
		resp, err = req.Post(rawURL)
	case "PUT":
		resp, err = req.Put(rawURL)
	case "PATCH":
		resp, err = req.Patch(rawURL)
	case "DELETE":
		resp, err = req.Delete(rawURL)
	case "HEAD":
		resp, err = req.Head(rawURL)
	case "OPTIONS":
		resp, err = req.Options(rawURL)
	default:
		return nil, fmt.Errorf("httpclient: unsupported method %q", method)
	}
	if err != nil {
		return nil, fmt.Errorf("%s %s: %w", strings.ToUpper(method), rawURL, err)
	}
	defer resp.Close()

	out := &Response{
		Status:  resp.StatusCode(),
		Headers: map[string]string{},
		Elapsed: time.Since(started),
	}
	for k, v := range resp.Headers() {
		if len(v) > 0 {
			out.Headers[strings.ToLower(k)] = v[0]
		}
	}
	// Copy: the underlying buffer is returned to the pool when resp is closed.
	body := resp.Body()
	out.Body = append([]byte(nil), body...)
	return out, nil
}

// GetJSON performs a GET and decodes a 2xx body into out.
//
// A non-2xx status is not an error here: callers decide what a 404 or a 429
// means, and both are ordinary outcomes for this tool.
func (c *Client) GetJSON(
	ctx context.Context,
	rawURL string,
	headers map[string]string,
	query url.Values,
	out any,
) (*Response, error) {
	resp, err := c.Do(ctx, "GET", rawURL, headers, query)
	if err != nil {
		return nil, err
	}
	if resp.Status >= 200 && resp.Status < 300 && out != nil {
		if err := resp.JSON(out); err != nil {
			return resp, err
		}
	}
	return resp, nil
}

// PostJSON performs a POST with a JSON body.
func (c *Client) PostJSON(
	ctx context.Context,
	rawURL string,
	headers map[string]string,
	payload any,
	out any,
) (*Response, error) {
	req := c.fc.R().SetContext(ctx).SetTimeout(c.timeout)
	for k, v := range headers {
		req.SetHeader(k, v)
	}
	if payload != nil {
		req.SetJSON(payload)
	}
	resp, err := req.Post(rawURL)
	if err != nil {
		return nil, fmt.Errorf("POST %s: %w", rawURL, err)
	}
	defer resp.Close()
	result := &Response{Status: resp.StatusCode(), Headers: map[string]string{}}
	for k, v := range resp.Headers() {
		if len(v) > 0 {
			result.Headers[strings.ToLower(k)] = v[0]
		}
	}
	result.Body = append([]byte(nil), resp.Body()...)
	if out != nil && result.Status >= 200 && result.Status < 300 {
		if err := result.JSON(out); err != nil {
			return result, err
		}
	}
	return result, nil
}
