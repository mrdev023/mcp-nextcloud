// Package nextcloud is the core HTTP client shared by the Nextcloud app
// modules (News, …): credential selection, request building, response size
// limit and error mapping. It knows nothing about the apps themselves; each
// app package (nextcloud/news, …) builds its API on top of Client.Do.
//
// Credentials are only ever placed in the Authorization header and are never
// included in errors or logs.
package nextcloud

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// maxResponseBytes caps how much of a Nextcloud response we are willing to
// buffer, protecting the process's memory budget.
const maxResponseBytes = 16 << 20

var (
	ErrUnauthorized  = errors.New("nextcloud rejected the credentials")
	ErrNoCredentials = errors.New("no Nextcloud credentials for this request")
	ErrNotFound      = errors.New("not found")
	// ErrTokenExchange is wrapped by a TokenSource that could not obtain a
	// Nextcloud token from the identity provider.
	ErrTokenExchange = errors.New("token exchange failed")
)

// APIError is returned for unexpected non-2xx responses. Message is the
// "message" field of a JSON error body, when there is one.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("nextcloud API error (HTTP %d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("nextcloud API error (HTTP %d)", e.Status)
}

// TokenSource returns the bearer token to use for Nextcloud calls.
type TokenSource func(context.Context) (string, error)

type tokenSourceKey struct{}

// WithTokenSource returns a context whose Nextcloud calls authenticate with a
// bearer token obtained from src (OIDC mode) instead of the app-password.
// Nothing is stored on the client, so credentials never leak across requests.
func WithTokenSource(ctx context.Context, src TokenSource) context.Context {
	return context.WithValue(ctx, tokenSourceKey{}, src)
}

// Client calls Nextcloud with either a per-request bearer token (see
// WithTokenSource) or a server-wide app-password. One Client serves every app
// module.
type Client struct {
	base string // Nextcloud URL, ends with "/"
	user string
	pass string
	http *http.Client
	ua   string
}

// DefaultUserAgent is the User-Agent sent to Nextcloud unless WithUserAgent
// overrides it. Nextcloud shows it as the name of the session in Settings ->
// Security -> Devices & sessions.
const DefaultUserAgent = "MCP Nextcloud"

// Option configures a Client.
type Option func(*Client)

// WithUserAgent sets the User-Agent sent to Nextcloud.
func WithUserAgent(ua string) Option {
	return func(c *Client) { c.ua = ua }
}

// NewClient returns a Client for the Nextcloud instance at nextcloudURL.
// user and appPassword may be empty when every call carries a TokenSource.
func NewClient(nextcloudURL, user, appPassword string, hc *http.Client, opts ...Option) *Client {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	c := &Client{
		base: strings.TrimRight(nextcloudURL, "/") + "/",
		user: user,
		pass: appPassword,
		http: hc,
		ua:   DefaultUserAgent,
	}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Do sends a request to path, relative to the Nextcloud URL (for instance
// "index.php/apps/news/api/v1-3/feeds"), with query q and, when body is not
// nil, body as JSON. It decodes a 2xx JSON response into out (if not nil) and
// maps 401 to ErrUnauthorized, 404 to ErrNotFound and other errors to
// *APIError.
func (c *Client) Do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
	u := c.base + path
	if len(q) > 0 {
		u += "?" + q.Encode()
	}
	var rb io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rb = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, u, rb)
	if err != nil {
		return err
	}
	// A per-request token source (OIDC mode) takes precedence over the
	// server-wide app-password.
	if src, _ := ctx.Value(tokenSourceKey{}).(TokenSource); src != nil {
		tok, err := src(ctx)
		if err != nil {
			return err
		}
		req.Header.Set("Authorization", "Bearer "+tok)
	} else if c.user != "" && c.pass != "" {
		req.SetBasicAuth(c.user, c.pass)
	} else {
		return ErrNoCredentials
	}
	req.Header.Set("User-Agent", c.ua)
	req.Header.Set("Accept", "application/json")
	// Required: the News API routes are #[CORS]. Nextcloud's CORSMiddleware
	// logs the session out and demands Basic auth unless the request passes
	// the CSRF check, which this header satisfies. Without it, a Bearer token
	// (OIDC mode) is rejected with 401 "CORS requires basic auth".
	req.Header.Set("OCS-APIRequest", "true")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	resp, err := c.http.Do(req)
	if err != nil {
		// Strip the URL from transport errors to keep messages short; it never
		// contains credentials but may contain query strings.
		var uerr *url.Error
		if errors.As(err, &uerr) {
			return fmt.Errorf("nextcloud request failed: %w", uerr.Err)
		}
		return fmt.Errorf("nextcloud request failed: %w", err)
	}
	defer resp.Body.Close()
	lr := io.LimitReader(resp.Body, maxResponseBytes)

	switch {
	case resp.StatusCode == http.StatusUnauthorized:
		_, _ = io.Copy(io.Discard, lr)
		return ErrUnauthorized
	case resp.StatusCode == http.StatusNotFound:
		_, _ = io.Copy(io.Discard, lr)
		return ErrNotFound
	case resp.StatusCode < 200 || resp.StatusCode > 299:
		var e struct {
			Message string `json:"message"`
		}
		b, _ := io.ReadAll(io.LimitReader(lr, 4096))
		_ = json.Unmarshal(b, &e)
		return &APIError{Status: resp.StatusCode, Message: e.Message}
	}

	if out == nil {
		_, _ = io.Copy(io.Discard, lr)
		return nil
	}
	if err := json.NewDecoder(lr).Decode(out); err != nil {
		return fmt.Errorf("decoding nextcloud response: %w", err)
	}
	return nil
}
