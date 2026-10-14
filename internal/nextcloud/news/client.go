// Package news is the Nextcloud News API v1-3, on top of the core
// nextcloud.Client.
package news

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"strconv"

	"github.com/mrdev023/mcp-nextcloud/internal/nextcloud"
)

// APIPath is the path of the News API below the Nextcloud URL.
const APIPath = "index.php/apps/news/api/v1-3/"

// ItemType selects the scope of an items query.
type ItemType int

const (
	TypeFeed    ItemType = 0
	TypeFolder  ItemType = 1
	TypeStarred ItemType = 2
	TypeAll     ItemType = 3
)

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

// Client is the News API of one Nextcloud instance.
type Client struct {
	nc *nextcloud.Client
}

// New returns the News API client using nc for transport and credentials.
func New(nc *nextcloud.Client) *Client {
	return &Client{nc: nc}
}

func (c *Client) do(ctx context.Context, method, path string, q url.Values, body, out any) error {
	return c.nc.Do(ctx, method, APIPath+path, q, body, out)
}

func (c *Client) ListFeeds(ctx context.Context) (*FeedsResponse, error) {
	var r FeedsResponse
	if err := c.do(ctx, http.MethodGet, "feeds", nil, nil, &r); err != nil {
		return nil, err
	}
	return &r, nil
}

func (c *Client) ListFolders(ctx context.Context) ([]Folder, error) {
	var r struct {
		Folders []Folder `json:"folders"`
	}
	if err := c.do(ctx, http.MethodGet, "folders", nil, nil, &r); err != nil {
		return nil, err
	}
	return r.Folders, nil
}

func (c *Client) ListItems(ctx context.Context, p ListItemsParams) ([]Item, error) {
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
func (c *Client) GetItem(ctx context.Context, id int64) (*Item, error) {
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
		return nil, nextcloud.ErrNotFound
	}
	return &items[0], nil
}

// GetUpdated returns all items modified since lastModified (unix seconds).
func (c *Client) GetUpdated(ctx context.Context, lastModified int64, t ItemType, id int64) ([]Item, error) {
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

func (c *Client) MarkRead(ctx context.Context, ids ...int64) error {
	if len(ids) == 1 {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/read", ids[0]), nil, nil, nil)
	}
	return c.do(ctx, http.MethodPost, "items/read/multiple", nil, map[string][]int64{"itemIds": ids}, nil)
}

func (c *Client) MarkUnread(ctx context.Context, ids ...int64) error {
	if len(ids) == 1 {
		return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/unread", ids[0]), nil, nil, nil)
	}
	return c.do(ctx, http.MethodPost, "items/unread/multiple", nil, map[string][]int64{"itemIds": ids}, nil)
}

// MarkAllRead marks every item with id <= newestItemID as read.
func (c *Client) MarkAllRead(ctx context.Context, newestItemID int64) error {
	q := url.Values{"newestItemId": {strconv.FormatInt(newestItemID, 10)}}
	return c.do(ctx, http.MethodPost, "items/read", q, nil, nil)
}

func (c *Client) Star(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/star", id), nil, nil, nil)
}

func (c *Client) Unstar(ctx context.Context, id int64) error {
	return c.do(ctx, http.MethodPost, fmt.Sprintf("items/%d/unstar", id), nil, nil, nil)
}
