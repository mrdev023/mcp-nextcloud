// Package tools defines the MCP tools exposed by news-mcp.
package tools

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"html"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
)

const (
	defaultListLimit    = 20
	maxListLimit        = 100
	defaultUpdatedLimit = 50
	maxUpdatedLimit     = 200
	defaultBodyChars    = 20000
	maxBodyChars        = 200000
	listBodyChars       = 2000
	excerptChars        = 280
	maxBatchIDs         = 1000
)

// ---------- output types ----------

type FeedOut struct {
	ID          int64  `json:"id"`
	Title       string `json:"title"`
	URL         string `json:"url"`
	Link        string `json:"link,omitempty"`
	FolderID    *int64 `json:"folderId,omitempty"`
	UnreadCount int    `json:"unreadCount"`
	UpdateError string `json:"updateError,omitempty"`
}

type FolderOut struct {
	ID   int64  `json:"id"`
	Name string `json:"name"`
}

type ListFeedsOut struct {
	Feeds        []FeedOut   `json:"feeds"`
	Folders      []FolderOut `json:"folders"`
	StarredCount int         `json:"starredCount"`
	NewestItemID int64       `json:"newestItemId,omitempty"`
}

type ItemOut struct {
	ID            int64  `json:"id"`
	Title         string `json:"title"`
	URL           string `json:"url,omitempty"`
	Author        string `json:"author,omitempty"`
	FeedID        int64  `json:"feedId"`
	PublishedAt   string `json:"publishedAt,omitempty"`
	Unread        bool   `json:"unread"`
	Starred       bool   `json:"starred"`
	LastModified  int64  `json:"lastModified,omitempty"`
	Excerpt       string `json:"excerpt,omitempty"`
	Body          string `json:"body,omitempty"`
	BodyTruncated bool   `json:"bodyTruncated,omitempty"`
	BodyLength    int    `json:"bodyLength,omitempty"`
	Enclosure     string `json:"enclosure,omitempty"`
}

type ListItemsOut struct {
	Items []ItemOut `json:"items"`
	Count int       `json:"count"`
	// NextOffset is the cursor to pass as `offset` to get the next page; 0 when
	// there are no more items.
	NextOffset int64 `json:"nextOffset"`
}

type UpdatedOut struct {
	Items     []ItemOut `json:"items"`
	Count     int       `json:"count"`
	Total     int       `json:"total"`
	Truncated bool      `json:"truncated"`
	// NextLastModified is the value to pass as `lastModified` on the next call.
	NextLastModified int64 `json:"nextLastModified"`
}

type ActionOut struct {
	OK      bool    `json:"ok"`
	ItemIDs []int64 `json:"itemIds,omitempty"`
	Message string  `json:"message"`
}

// ---------- input types ----------

type ListFeedsIn struct{}

type ListItemsIn struct {
	FeedID      *int64 `json:"feedId,omitempty" jsonschema:"only items of this feed"`
	FolderID    *int64 `json:"folderId,omitempty" jsonschema:"only items of this folder (ignored if feedId is set)"`
	StarredOnly bool   `json:"starredOnly,omitempty" jsonschema:"only starred items (ignored if feedId or folderId is set)"`
	IncludeRead bool   `json:"includeRead,omitempty" jsonschema:"include already-read items (default false: unread only)"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max number of items to return (default 20, max 100)"`
	Offset      int64  `json:"offset,omitempty" jsonschema:"pagination cursor: pass nextOffset from the previous page (an item id, not a count). 0 = start"`
	OldestFirst bool   `json:"oldestFirst,omitempty" jsonschema:"return oldest items first"`
	Search      string `json:"search,omitempty" jsonschema:"full-text search terms (server-side when supported, also filtered on title/body)"`
	IncludeBody bool   `json:"includeBody,omitempty" jsonschema:"include the article body as plain text (truncated to 2000 chars). Default: a short excerpt only"`
}

type GetItemIn struct {
	ID           int64  `json:"id" jsonschema:"item id"`
	Format       string `json:"format,omitempty" jsonschema:"body format: text (default, HTML stripped) or html"`
	MaxBodyChars int    `json:"maxBodyChars,omitempty" jsonschema:"truncate the body to this many characters (default 20000, max 200000)"`
}

type IDsIn struct {
	ItemID  *int64  `json:"itemId,omitempty" jsonschema:"a single item id"`
	ItemIDs []int64 `json:"itemIds,omitempty" jsonschema:"several item ids (batch, max 1000)"`
}

type MarkAllReadIn struct {
	NewestItemID *int64 `json:"newestItemId,omitempty" jsonschema:"mark every item with id <= this as read. Defaults to the newest item currently known (i.e. everything)"`
}

type ItemIDIn struct {
	ID int64 `json:"id" jsonschema:"item id"`
}

type GetUpdatedIn struct {
	LastModified int64  `json:"lastModified" jsonschema:"unix timestamp (seconds or microseconds); returns items changed since then. Use nextLastModified from a previous call"`
	FeedID       *int64 `json:"feedId,omitempty" jsonschema:"restrict to this feed"`
	FolderID     *int64 `json:"folderId,omitempty" jsonschema:"restrict to this folder"`
	Limit        int    `json:"limit,omitempty" jsonschema:"max items returned, oldest changes first (default 50, max 200)"`
	IncludeBody  bool   `json:"includeBody,omitempty" jsonschema:"include plain-text bodies (truncated to 2000 chars)"`
}

// ---------- registration ----------

// NewsAPI is the part of the Nextcloud News client the tools use. It is
// declared here, by its consumer, so tests can substitute an in-memory fake.
type NewsAPI interface {
	ListFeeds(ctx context.Context) (*nextcloud.FeedsResponse, error)
	ListFolders(ctx context.Context) ([]nextcloud.Folder, error)
	ListItems(ctx context.Context, p nextcloud.ListItemsParams) ([]nextcloud.Item, error)
	GetItem(ctx context.Context, id int64) (*nextcloud.Item, error)
	GetUpdated(ctx context.Context, lastModified int64, t nextcloud.ItemType, id int64) ([]nextcloud.Item, error)
	MarkRead(ctx context.Context, ids ...int64) error
	MarkUnread(ctx context.Context, ids ...int64) error
	MarkAllRead(ctx context.Context, newestItemID int64) error
	Star(ctx context.Context, id int64) error
	Unstar(ctx context.Context, id int64) error
}

var _ NewsAPI = (*nextcloud.Client)(nil)

type handlers struct{ nc NewsAPI }

// mapErr turns client errors into clean, user-facing tool errors.
func mapErr(err error, what string) error {
	switch {
	case err == nil:
		return nil
	case errors.Is(err, nextcloud.ErrUnauthorized):
		return errors.New("Nextcloud authentication failed: Nextcloud rejected the credentials (app-password or exchanged token)")
	case errors.Is(err, nextcloud.ErrNoCredentials):
		return errors.New("missing Nextcloud credentials for this request")
	case errors.Is(err, nextcloud.ErrTokenExchange):
		return fmt.Errorf("could not obtain a Nextcloud token from the identity provider (%v)", err)
	case errors.Is(err, nextcloud.ErrNotFound):
		return fmt.Errorf("%s not found", what)
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("Nextcloud did not answer in time")
	}
	return err
}

func (t *handlers) listFeeds(ctx context.Context, _ *mcp.CallToolRequest, _ ListFeedsIn) (*mcp.CallToolResult, ListFeedsOut, error) {
	var out ListFeedsOut
	fr, err := t.nc.ListFeeds(ctx)
	if err != nil {
		return nil, out, mapErr(err, "feeds")
	}
	folders, err := t.nc.ListFolders(ctx)
	if err != nil {
		return nil, out, mapErr(err, "folders")
	}
	out.Feeds = make([]FeedOut, 0, len(fr.Feeds))
	for _, f := range fr.Feeds {
		out.Feeds = append(out.Feeds, FeedOut{
			ID: f.ID, Title: f.Title, URL: f.URL, Link: f.Link, FolderID: f.FolderID,
			UnreadCount: f.UnreadCount, UpdateError: f.LastUpdateError,
		})
	}
	out.Folders = make([]FolderOut, 0, len(folders))
	for _, f := range folders {
		out.Folders = append(out.Folders, FolderOut(f))
	}
	out.StarredCount = fr.StarredCount
	if fr.NewestItemID != nil {
		out.NewestItemID = *fr.NewestItemID
	}
	return nil, out, nil
}

func clamp(v, def, max int) int {
	if v <= 0 {
		return def
	}
	return min(v, max)
}

func (t *handlers) listItems(ctx context.Context, _ *mcp.CallToolRequest, in ListItemsIn) (*mcp.CallToolResult, ListItemsOut, error) {
	out := ListItemsOut{Items: []ItemOut{}}
	if in.Offset < 0 {
		return nil, out, errors.New("offset must be >= 0")
	}
	limit := clamp(in.Limit, defaultListLimit, maxListLimit)
	p := nextcloud.ListItemsParams{
		Type:        nextcloud.TypeAll,
		GetRead:     in.IncludeRead,
		BatchSize:   limit,
		Offset:      in.Offset,
		OldestFirst: in.OldestFirst,
		Search:      strings.TrimSpace(in.Search),
	}
	var scoped bool
	p.Type, p.ID, scoped = scope(in.FeedID, in.FolderID)
	if !scoped && in.StarredOnly {
		p.Type = nextcloud.TypeStarred
	}
	items, err := t.nc.ListItems(ctx, p)
	if err != nil {
		return nil, out, mapErr(err, "feed or folder")
	}
	if len(items) == limit {
		out.NextOffset = items[len(items)-1].ID
	}
	terms := strings.Fields(strings.ToLower(p.Search))
	for i := range items {
		if len(terms) > 0 && !matches(&items[i], terms) {
			continue
		}
		out.Items = append(out.Items, toItemOut(&items[i], in.IncludeBody, listBodyChars))
	}
	out.Count = len(out.Items)
	return nil, out, nil
}

// matches is a local safety net for News versions that ignore `search`.
func matches(it *nextcloud.Item, terms []string) bool {
	hay := strings.ToLower(it.Title + " " + it.Author + " " + it.Body)
	for _, t := range terms {
		if !strings.Contains(hay, t) {
			return false
		}
	}
	return true
}

// scope maps the feedId / folderId filters to a News API item type and id.
// feedId wins over folderId; scoped is false when neither is set.
func scope(feedID, folderID *int64) (typ nextcloud.ItemType, id int64, scoped bool) {
	switch {
	case feedID != nil:
		return nextcloud.TypeFeed, *feedID, true
	case folderID != nil:
		return nextcloud.TypeFolder, *folderID, true
	}
	return nextcloud.TypeAll, 0, false
}

func positiveID(id int64) error {
	if id <= 0 {
		return errors.New("id must be a positive item id")
	}
	return nil
}

func (t *handlers) getItem(ctx context.Context, _ *mcp.CallToolRequest, in GetItemIn) (*mcp.CallToolResult, ItemOut, error) {
	if err := positiveID(in.ID); err != nil {
		return nil, ItemOut{}, err
	}
	format := strings.ToLower(in.Format)
	if format != "" && format != "text" && format != "html" {
		return nil, ItemOut{}, errors.New(`format must be "text" or "html"`)
	}
	it, err := t.nc.GetItem(ctx, in.ID)
	if err != nil {
		return nil, ItemOut{}, mapErr(err, fmt.Sprintf("item %d", in.ID))
	}
	max := clamp(in.MaxBodyChars, defaultBodyChars, maxBodyChars)
	out := toItemOut(it, false, 0)
	body := it.Body
	if format != "html" {
		body = htmlToText(body)
	}
	out.BodyLength = utf8.RuneCountInString(body)
	out.Body, out.BodyTruncated = truncate(body, max)
	return nil, out, nil
}

func collectIDs(in IDsIn) ([]int64, error) {
	ids := slices.Clone(in.ItemIDs)
	if in.ItemID != nil {
		ids = append(ids, *in.ItemID)
	}
	if len(ids) == 0 {
		return nil, errors.New("provide itemId or itemIds")
	}
	slices.Sort(ids)
	ids = slices.Compact(ids)
	if len(ids) > maxBatchIDs {
		return nil, fmt.Errorf("too many ids (%d > %d)", len(ids), maxBatchIDs)
	}
	if ids[0] <= 0 {
		return nil, errors.New("item ids must be positive")
	}
	return ids, nil
}

// setRead returns the handler of news_mark_read (read) or news_mark_unread.
func (t *handlers) setRead(read bool) mcp.ToolHandlerFor[IDsIn, ActionOut] {
	apply, state := NewsAPI.MarkUnread, "unread"
	if read {
		apply, state = NewsAPI.MarkRead, "read"
	}
	return func(ctx context.Context, _ *mcp.CallToolRequest, in IDsIn) (*mcp.CallToolResult, ActionOut, error) {
		ids, err := collectIDs(in)
		if err != nil {
			return nil, ActionOut{}, err
		}
		if err := apply(t.nc, ctx, ids...); err != nil {
			return nil, ActionOut{}, mapErr(err, "item")
		}
		return nil, ActionOut{OK: true, ItemIDs: ids, Message: fmt.Sprintf("%d item(s) marked as %s", len(ids), state)}, nil
	}
}

func (t *handlers) markAllRead(ctx context.Context, _ *mcp.CallToolRequest, in MarkAllReadIn) (*mcp.CallToolResult, ActionOut, error) {
	var newest int64
	if in.NewestItemID != nil {
		newest = *in.NewestItemID
	} else {
		fr, err := t.nc.ListFeeds(ctx)
		if err != nil {
			return nil, ActionOut{}, mapErr(err, "feeds")
		}
		if fr.NewestItemID == nil {
			return nil, ActionOut{OK: true, Message: "no items: nothing to mark"}, nil
		}
		newest = *fr.NewestItemID
	}
	if newest <= 0 {
		return nil, ActionOut{}, errors.New("newestItemId must be positive")
	}
	if err := t.nc.MarkAllRead(ctx, newest); err != nil {
		return nil, ActionOut{}, mapErr(err, "item")
	}
	return nil, ActionOut{OK: true, Message: fmt.Sprintf("all items up to id %d marked as read", newest)}, nil
}

// setStar returns the handler of news_star (starred) or news_unstar.
func (t *handlers) setStar(starred bool) mcp.ToolHandlerFor[ItemIDIn, ActionOut] {
	apply, msg := NewsAPI.Unstar, "star removed"
	if starred {
		apply, msg = NewsAPI.Star, "item starred"
	}
	return func(ctx context.Context, _ *mcp.CallToolRequest, in ItemIDIn) (*mcp.CallToolResult, ActionOut, error) {
		if err := positiveID(in.ID); err != nil {
			return nil, ActionOut{}, err
		}
		if err := apply(t.nc, ctx, in.ID); err != nil {
			return nil, ActionOut{}, mapErr(err, fmt.Sprintf("item %d", in.ID))
		}
		return nil, ActionOut{OK: true, ItemIDs: []int64{in.ID}, Message: msg}, nil
	}
}

func (t *handlers) getUpdated(ctx context.Context, _ *mcp.CallToolRequest, in GetUpdatedIn) (*mcp.CallToolResult, UpdatedOut, error) {
	out := UpdatedOut{Items: []ItemOut{}, NextLastModified: in.LastModified}
	if in.LastModified < 0 {
		return nil, out, errors.New("lastModified must be >= 0")
	}
	typ, id, _ := scope(in.FeedID, in.FolderID)
	items, err := t.nc.GetUpdated(ctx, in.LastModified, typ, id)
	if err != nil {
		return nil, out, mapErr(err, "feed or folder")
	}
	// Oldest changes first so that NextLastModified is a valid resume point
	// even when the result is truncated.
	slices.SortFunc(items, func(a, b nextcloud.Item) int {
		if c := cmp.Compare(a.LastModified, b.LastModified); c != 0 {
			return c
		}
		return cmp.Compare(a.ID, b.ID)
	})
	limit := clamp(in.Limit, defaultUpdatedLimit, maxUpdatedLimit)
	out.Total = len(items)
	if len(items) > limit {
		items, out.Truncated = items[:limit], true
	}
	for i := range items {
		out.Items = append(out.Items, toItemOut(&items[i], in.IncludeBody, listBodyChars))
		out.NextLastModified = max(out.NextLastModified, items[i].LastModified)
	}
	out.Count = len(out.Items)
	return nil, out, nil
}

// ---------- helpers ----------

func toItemOut(it *nextcloud.Item, withBody bool, bodyChars int) ItemOut {
	o := ItemOut{
		ID: it.ID, Title: it.Title, URL: it.URL, Author: it.Author, FeedID: it.FeedID,
		Unread: it.Unread, Starred: it.Starred, LastModified: it.LastModified,
		Enclosure: it.EnclosureLink,
	}
	if it.PubDate > 0 {
		o.PublishedAt = time.Unix(it.PubDate, 0).UTC().Format(time.RFC3339)
	}
	if withBody {
		o.Body, o.BodyTruncated = truncate(htmlToText(it.Body), bodyChars)
	} else if it.Body != "" {
		o.Excerpt, _ = truncate(htmlToText(it.Body), excerptChars)
	}
	return o
}

// truncate cuts s to at most n runes, appending an ellipsis when cut.
func truncate(s string, n int) (string, bool) {
	if n <= 0 || utf8.RuneCountInString(s) <= n {
		return s, false
	}
	i, count := 0, 0
	for i = range s {
		if count == n {
			break
		}
		count++
	}
	return s[:i] + "…", true
}

var (
	reDropBlocks = regexp.MustCompile(`(?is)<(script|style|noscript|iframe)\b.*?</(script|style|noscript|iframe)\s*>`)
	reBreaks     = regexp.MustCompile(`(?i)<\s*(br|/p|/div|/li|/h[1-6]|/tr|/blockquote|hr)\b[^>]*>`)
	reListItem   = regexp.MustCompile(`(?i)<\s*li\b[^>]*>`)
	reTags       = regexp.MustCompile(`(?s)<[^>]*>`)
	reSpaces     = regexp.MustCompile(`[ \t\r\f\v\x{00a0}]+`)
	reNewlines   = regexp.MustCompile(`\s*\n\s*(\n\s*)+`)
)

// htmlToText is a small, dependency-free HTML to plain-text converter good
// enough for feed article bodies.
func htmlToText(s string) string {
	if !strings.ContainsAny(s, "<&") {
		return strings.TrimSpace(s)
	}
	s = reDropBlocks.ReplaceAllString(s, "")
	s = reBreaks.ReplaceAllString(s, "\n")
	s = reListItem.ReplaceAllString(s, "\n- ")
	s = reTags.ReplaceAllString(s, "")
	s = html.UnescapeString(s)
	s = reSpaces.ReplaceAllString(s, " ")
	s = reNewlines.ReplaceAllString(s, "\n\n")
	lines := strings.Split(s, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimSpace(l)
	}
	return strings.TrimSpace(strings.Join(lines, "\n"))
}
