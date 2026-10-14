package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// Nextcloud News API v1-3 client. Authenticates with HTTP Basic Auth using an
// app-password. The credentials are only ever placed in the Authorization
// header and are never included in errors or logs.

// ItemType selects the scope of an items query.
type ItemType int

const (
	TypeFeed    ItemType = 0
	TypeFolder  ItemType = 1
	TypeStarred ItemType = 2
	TypeAll     ItemType = 3
)

// maxResponseBytes caps how much of a Nextcloud response we are willing to
// buffer, protecting the process's memory budget.
const maxResponseBytes = 16 << 20

var (
	ErrUnauthorized  = errors.New("nextcloud rejected the credentials")
	ErrNoCredentials = errors.New("no Nextcloud credentials for this request")
	ErrNotFound      = errors.New("not found")
)

// APIError is returned for unexpected non-2xx responses.
type APIError struct {
	Status  int
	Message string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("nextcloud news API error (HTTP %d): %s", e.Status, e.Message)
	}
	return fmt.Sprintf("nextcloud news API error (HTTP %d)", e.Status)
}

type Feed struct {
	ID               int64  `json:"id"`
	URL              string `json:"url"`
	Title            string `json:"title"`
	Link             string `json:"link,omitempty"`
	FolderID         *int64 `json:"folderId"`
	UnreadCount      int    `json:"unreadCount"`
	UpdateErrorCount int    `json:"updateErrorCount,omitempty"`
	LastUpdateError  string `json:"lastUpdateError,omitempty"`
}

type FeedsResponse struct {
	Feeds        []Feed `json:"feeds"`
	StarredCount int    `json:"starredCount"`
	NewestItemID *int64 `json:"newestItemId"`
}

type Folder struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

// Item mirrors the News API item object (only the fields we use).
type Item struct {
	ID            int64  `json:"id"`
	GUID          string `json:"guid"`
	URL           string `json:"url"`
	Title         string `json:"title"`
	Author        string `json:"author"`
	PubDate       int64  `json:"pubDate"`
	UpdatedDate   int64  `json:"updatedDate"`
	Body          string `json:"body"`
	EnclosureMime string `json:"enclosureMime"`
	EnclosureLink string `json:"enclosureLink"`
	FeedID        int64  `json:"feedId"`
	Unread        bool   `json:"unread"`
	Starred       bool   `json:"starred"`
	LastModified  int64  `json:"lastModified"`
	Fingerprint   string `json:"fingerprint"`
	ContentHash   string `json:"contentHash"`
}

// The API has historically used both "unread"/"starred" and
// "isUnread"/"isStarred"; accept either.
func (it *Item) UnmarshalJSON(b []byte) error {
	type plain Item
	var aux struct {
		plain
		IsUnread  *bool `json:"isUnread"`
		IsStarred *bool `json:"isStarred"`
	}
	if err := json.Unmarshal(b, &aux); err != nil {
		return err
	}
	*it = Item(aux.plain)
	if aux.IsUnread != nil {
		it.Unread = *aux.IsUnread
	}
	if aux.IsStarred != nil {
		it.Starred = *aux.IsStarred
	}
	return nil
}

// ListItemsParams maps to GET items.
type ListItemsParams struct {
	Type        ItemType
	ID          int64 // feed or folder id, ignored for starred/all
	GetRead     bool
	BatchSize   int   // number of items; -1 means all (avoid)
	Offset      int64 // only items with id < Offset (0 = from the newest)
	OldestFirst bool
	Search      string
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

// bearerFromHeader extracts the token of an "Authorization: Bearer <token>"
// header, or "" if absent or malformed.
func bearerFromHeader(h http.Header) string {
	scheme, tok, ok := strings.Cut(strings.TrimSpace(h.Get("Authorization")), " ")
	if !ok || !strings.EqualFold(scheme, "Bearer") {
		return ""
	}
	tok = strings.TrimSpace(tok)
	if tok == "" || strings.ContainsAny(tok, " \t\r\n") {
		return ""
	}
	return tok
}

type NewsClient struct {
	base string // ends with "/index.php/apps/news/api/v1-3/"
	user string
	pass string
	http *http.Client
}

func NewNewsClient(nextcloudURL, user, appPassword string, hc *http.Client) *NewsClient {
	if hc == nil {
		hc = &http.Client{Timeout: 30 * time.Second}
	}
	return &NewsClient{
		base: strings.TrimRight(nextcloudURL, "/") + "/index.php/apps/news/api/v1-3/",
		user: user,
		pass: appPassword,
		http: hc,
	}
}

func (c *NewsClient) do(ctx context.Context, method, path string, q url.Values, body any, out any) error {
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

func (c *NewsClient) ListFeeds(ctx context.Context) (*FeedsResponse, error) {
	var r FeedsResponse
	if err := c.do(ctx, http.MethodGet, "feeds", nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *NewsClient) ListFolders(ctx context.Context) ([]Folder, error) {
	var r struct {
		Folders []Folder `json:"folders"`
	}
	if err := c.do(ctx, http.MethodGet, "folders", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.Folders, nil
}

func (c *NewsClient) ListItems(ctx context.Context, p ListItemsParams) ([]Item, error) {
	q := url.Values{}
	q.Set("type", strconv.Itoa(int(p.Type)))
	q.Set("id", strconv.FormatInt(p.ID, 10))
	q.Set("getRead", strconv.FormatBool(p.GetRead))
	bs := p.BatchSize
	if bs == 0 {
		bs = 20
	}
	q.Set("batchSize", strconv.Itoa(bs))
	q.Set("offset", strconv.FormatInt(p.Offset, 10))
	q.Set("oldestFirst", strconv.FormatBool(p.OldestFirst))
	if p.Search != "" {
		q.Set("search", p.Search)
	}
	var r struct {
		Items []Item `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "items", q, nil, &r); err != nil {
		return nil, err
	}
	return r.Items, nil
}

// GetItem fetches one item by id. The v1-3 API has no single-item endpoint,
// so we request the newest item (all feeds, read included) with an id lower
// than id+1, i.e. the item itself if it exists.
func (c *NewsClient) GetItem(ctx context.Context, id int64) (*Item, error) {
	items, err := c.ListItems(ctx, ListItemsParams{
		Type:      TypeAll,
		GetRead:   true,
		BatchSize: 1,
		Offset:    id + 1,
	})
	if err != nil {
		return nil, err
	}
	if len(items) == 0 || items[0].ID != id {
		return nil, ErrNotFound
	}
	return &items[0], nil
}

// GetUpdated returns all items modified since lastModified (unix seconds).
func (c *NewsClient) GetUpdated(ctx context.Context, lastModified int64, t ItemType, id int64) ([]Item, error) {
	q := url.Values{}
	q.Set("lastModified", strconv.FormatInt(lastModified, 10))
	q.Set("type", strconv.Itoa(int(t)))
	q.Set("id", strconv.FormatInt(id, 10))
	var r struct {
		Items []Item `json:"items"`
	}
	if err := c.do(ctx, http.MethodGet, "items/updated", q, nil, &r); err != nil {
		return nil, err
	}
	return r.Items, nil
}

func (c *NewsClient) MarkRead(ctx context.Context, ids ...int64) error {
	if len(ids) == 1 {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/read", ids[0]), nil, nil, nil)
	}
	return c.do(ctx, http.MethodPost, "items/read/multiple", nil, map[string][]int64{"itemIds": ids}, nil)
}

func (c *NewsClient) MarkUnread(ctx context.Context, ids ...int64) error {
	if len(ids) == 1 {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/unread", ids[0]), nil, nil, nil)
	}
	return c.do(ctx, http.MethodPost, "items/unread/multiple", nil, map[string][]int64{"itemIds": ids}, nil)
}

// MarkAllRead marks every item with id <= newestItemID as read.
func (c *NewsClient) MarkAllRead(ctx context.Context, newestItemID int64) error {
	q := url.Values{"newestItemId": {strconv.FormatInt(newestItemID, 10)}}
	return c.do(ctx, http.MethodPost, "items/read", q, nil, nil)
}

func (c *NewsClient) Star(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/star", id), nil, nil, nil)
}

func (c *NewsClient) Unstar(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/unstar", id), nil, nil, nil)
}
