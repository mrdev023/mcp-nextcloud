package news

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud/internal/nextcloud"
	newsapi "github.com/mrdev023/mcp-nextcloud/internal/nextcloud/news"
	"github.com/mrdev023/mcp-nextcloud/internal/tools"
)

// fakeNews is an in-memory API that records what the tools asked for.
type fakeNews struct {
	mu sync.Mutex

	feeds   newsapi.FeedsResponse
	folders []newsapi.Folder
	items   []newsapi.Item // served by ListItems, GetItem and GetUpdated
	err     error          // returned by every call when set

	calls  []string
	params newsapi.ListItemsParams // last ListItems call
	ids    []int64                 // ids of the last write call
}

func (f *fakeNews) record(call string, ids ...int64) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, call)
	f.ids = ids
	return f.err
}

func (f *fakeNews) lastCall() (string, []int64) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.calls) == 0 {
		return "", nil
	}
	return f.calls[len(f.calls)-1], f.ids
}

func (f *fakeNews) ListFeeds(context.Context) (*newsapi.FeedsResponse, error) {
	if err := f.record("ListFeeds"); err != nil {
		return nil, err
	}
	return &f.feeds, nil
}

func (f *fakeNews) ListFolders(context.Context) ([]newsapi.Folder, error) {
	return f.folders, f.record("ListFolders")
}

func (f *fakeNews) ListItems(_ context.Context, p newsapi.ListItemsParams) ([]newsapi.Item, error) {
	f.mu.Lock()
	f.params = p
	f.mu.Unlock()
	if err := f.record("ListItems"); err != nil {
		return nil, err
	}
	return f.items, nil
}

func (f *fakeNews) GetItem(_ context.Context, id int64) (*newsapi.Item, error) {
	if err := f.record("GetItem", id); err != nil {
		return nil, err
	}
	for i := range f.items {
		if f.items[i].ID == id {
			return &f.items[i], nil
		}
	}
	return nil, nextcloud.ErrNotFound
}

func (f *fakeNews) GetUpdated(_ context.Context, lastModified int64, t newsapi.ItemType, id int64) ([]newsapi.Item, error) {
	if err := f.record(fmt.Sprintf("GetUpdated(%d,%d,%d)", lastModified, t, id)); err != nil {
		return nil, err
	}
	return append([]newsapi.Item(nil), f.items...), nil
}

func (f *fakeNews) MarkRead(_ context.Context, ids ...int64) error {
	return f.record("MarkRead", ids...)
}
func (f *fakeNews) MarkUnread(_ context.Context, ids ...int64) error {
	return f.record("MarkUnread", ids...)
}
func (f *fakeNews) MarkAllRead(_ context.Context, newest int64) error {
	return f.record("MarkAllRead", newest)
}
func (f *fakeNews) Star(_ context.Context, id int64) error   { return f.record("Star", id) }
func (f *fakeNews) Unstar(_ context.Context, id int64) error { return f.record("Unstar", id) }

// call runs a tool and returns its JSON text, or the error text with isErr.
func call(t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) (text string, isErr bool) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	res, err := cs.CallTool(context.Background(), &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("%s: protocol error: %v", name, err)
	}
	if len(res.Content) == 0 {
		t.Fatalf("%s: empty result", name)
	}
	return res.Content[0].(*mcp.TextContent).Text, res.IsError
}

// decode runs a tool that must succeed and decodes its structured output.
func decode[T any](t *testing.T, cs *mcp.ClientSession, name string, args map[string]any) T {
	t.Helper()
	text, isErr := call(t, cs, name, args)
	if isErr {
		t.Fatalf("%s: unexpected tool error: %s", name, text)
	}
	var out T
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		t.Fatalf("%s: %v in %s", name, err, text)
	}
	return out
}

func ptr[T any](v T) *T { return &v }

func TestListFeedsTool(t *testing.T) {
	f := &fakeNews{
		feeds: newsapi.FeedsResponse{
			Feeds:        []newsapi.Feed{{ID: 1, Title: "Go blog", URL: "https://go.dev/blog/feed.atom", FolderID: ptr(int64(3)), UnreadCount: 2, LastUpdateError: "timeout"}},
			StarredCount: 4,
			NewestItemID: ptr(int64(99)),
		},
		folders: []newsapi.Folder{{ID: 3, Name: "Tech"}},
	}
	out := decode[ListFeedsOut](t, connect(t, f), "news_list_feeds", nil)
	if len(out.Feeds) != 1 || out.Feeds[0].Title != "Go blog" || *out.Feeds[0].FolderID != 3 || out.Feeds[0].UpdateError != "timeout" {
		t.Errorf("feeds = %+v", out.Feeds)
	}
	if len(out.Folders) != 1 || out.Folders[0].Name != "Tech" || out.StarredCount != 4 || out.NewestItemID != 99 {
		t.Errorf("out = %+v", out)
	}
}

func TestListItemsTool(t *testing.T) {
	items := []newsapi.Item{
		{ID: 30, Title: "Go 1.26 released", Body: "<p>Generic <b>methods</b></p>", FeedID: 1, Unread: true, PubDate: 1700000000},
		{ID: 20, Title: "Rust news", Body: "borrow checker", FeedID: 2},
	}
	for _, tc := range []struct {
		name       string
		args       map[string]any
		wantParams newsapi.ListItemsParams
		wantIDs    []int64
		wantNext   int64
		wantErr    string
	}{
		{name: "defaults", args: nil,
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeAll, BatchSize: defaultListLimit}, wantIDs: []int64{30, 20}},
		{name: "feed wins over folder and starred", args: map[string]any{"feedId": 7, "folderId": 8, "starredOnly": true},
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeFeed, ID: 7, BatchSize: defaultListLimit}, wantIDs: []int64{30, 20}},
		{name: "folder wins over starred", args: map[string]any{"folderId": 8, "starredOnly": true},
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeFolder, ID: 8, BatchSize: defaultListLimit}, wantIDs: []int64{30, 20}},
		{name: "starred", args: map[string]any{"starredOnly": true, "includeRead": true, "oldestFirst": true},
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeStarred, GetRead: true, OldestFirst: true, BatchSize: defaultListLimit}, wantIDs: []int64{30, 20}},
		{name: "limit clamped, full page gives a cursor", args: map[string]any{"limit": 2, "offset": 50},
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeAll, BatchSize: 2, Offset: 50}, wantIDs: []int64{30, 20}, wantNext: 20},
		{name: "limit above max", args: map[string]any{"limit": 5000},
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeAll, BatchSize: maxListLimit}, wantIDs: []int64{30, 20}},
		{name: "search filters locally too", args: map[string]any{"search": "  GO methods "},
			wantParams: newsapi.ListItemsParams{Type: newsapi.TypeAll, BatchSize: defaultListLimit, Search: "GO methods"}, wantIDs: []int64{30}},
		{name: "negative offset", args: map[string]any{"offset": -1}, wantErr: "offset must be >= 0"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := &fakeNews{items: items}
			cs := connect(t, f)
			if tc.wantErr != "" {
				if text, isErr := call(t, cs, "news_list_items", tc.args); !isErr || !strings.Contains(text, tc.wantErr) {
					t.Fatalf("got %q (error %v), want error %q", text, isErr, tc.wantErr)
				}
				return
			}
			out := decode[ListItemsOut](t, cs, "news_list_items", tc.args)
			if f.params != tc.wantParams {
				t.Errorf("params = %+v, want %+v", f.params, tc.wantParams)
			}
			var ids []int64
			for _, it := range out.Items {
				ids = append(ids, it.ID)
			}
			if fmt.Sprint(ids) != fmt.Sprint(tc.wantIDs) || out.Count != len(tc.wantIDs) || out.NextOffset != tc.wantNext {
				t.Errorf("ids = %v, count = %d, nextOffset = %d; want %v, %d", ids, out.Count, out.NextOffset, tc.wantIDs, tc.wantNext)
			}
		})
	}

	t.Run("excerpt or body", func(t *testing.T) {
		cs := connect(t, &fakeNews{items: items[:1]})
		out := decode[ListItemsOut](t, cs, "news_list_items", nil)
		if it := out.Items[0]; it.Excerpt != "Generic methods" || it.Body != "" || it.PublishedAt != "2023-11-14T22:13:20Z" {
			t.Errorf("default item = %+v", it)
		}
		out = decode[ListItemsOut](t, cs, "news_list_items", map[string]any{"includeBody": true})
		if it := out.Items[0]; it.Body != "Generic methods" || it.Excerpt != "" {
			t.Errorf("item with body = %+v", it)
		}
	})
}

func TestGetItemTool(t *testing.T) {
	f := &fakeNews{items: []newsapi.Item{{ID: 7, Title: "Long", Body: "<p>" + strings.Repeat("é", 50) + "</p>"}}}
	cs := connect(t, f)

	out := decode[ItemOut](t, cs, "news_get_item", map[string]any{"id": 7})
	if out.Body != strings.Repeat("é", 50) || out.BodyTruncated || out.BodyLength != 50 {
		t.Errorf("text body = %+v", out)
	}
	out = decode[ItemOut](t, cs, "news_get_item", map[string]any{"id": 7, "format": "HTML", "maxBodyChars": 10})
	if !strings.HasPrefix(out.Body, "<p>") || !out.BodyTruncated || out.BodyLength != 57 {
		t.Errorf("html body = %+v", out)
	}

	for args, want := range map[string]string{
		`{"id": 0}`:                    "id must be a positive item id",
		`{"id": 7, "format": "pdf"}`:   `format must be "text" or "html"`,
		`{"id": 8}`:                    "item 8 not found",
		`{"id": 7, "maxBodyChars": 1}`: "",
	} {
		var a map[string]any
		_ = json.Unmarshal([]byte(args), &a)
		text, isErr := call(t, cs, "news_get_item", a)
		if want == "" {
			if isErr {
				t.Errorf("%s: unexpected error %s", args, text)
			}
			continue
		}
		if !isErr || !strings.Contains(text, want) {
			t.Errorf("%s: got %q (error %v), want %q", args, text, isErr, want)
		}
	}
}

func TestWriteTools(t *testing.T) {
	tooMany := make([]int64, maxBatchIDs+1)
	for i := range tooMany {
		tooMany[i] = int64(i + 1)
	}
	for _, tc := range []struct {
		tool     string
		args     map[string]any
		wantCall string
		wantIDs  []int64
		wantMsg  string
	}{
		{"news_mark_read", map[string]any{"itemIds": []int{3, 1, 3}, "itemId": 2}, "MarkRead", []int64{1, 2, 3}, "3 item(s) marked as read"},
		{"news_mark_unread", map[string]any{"itemId": 5}, "MarkUnread", []int64{5}, "1 item(s) marked as unread"},
		{"news_mark_read", map[string]any{}, "", nil, "provide itemId or itemIds"},
		{"news_mark_read", map[string]any{"itemIds": []int{4, -1}}, "", nil, "item ids must be positive"},
		{"news_mark_unread", map[string]any{"itemIds": tooMany}, "", nil, "too many ids (1001 > 1000)"},
		{"news_star", map[string]any{"id": 9}, "Star", []int64{9}, "item starred"},
		{"news_unstar", map[string]any{"id": 9}, "Unstar", []int64{9}, "star removed"},
		{"news_star", map[string]any{"id": 0}, "", nil, "id must be a positive item id"},
		{"news_mark_all_read", map[string]any{"newestItemId": 42}, "MarkAllRead", []int64{42}, "all items up to id 42 marked as read"},
		{"news_mark_all_read", map[string]any{}, "MarkAllRead", []int64{99}, "all items up to id 99 marked as read"},
		{"news_mark_all_read", map[string]any{"newestItemId": -3}, "", nil, "newestItemId must be positive"},
	} {
		t.Run(tc.tool+"/"+tc.wantMsg, func(t *testing.T) {
			f := &fakeNews{feeds: newsapi.FeedsResponse{NewestItemID: ptr(int64(99))}}
			text, isErr := call(t, connect(t, f), tc.tool, tc.args)
			gotCall, gotIDs := f.lastCall()
			if tc.wantCall == "" {
				if !isErr || !strings.Contains(text, tc.wantMsg) || (gotCall != "" && gotCall != "ListFeeds") {
					t.Fatalf("got %q (error %v, call %s), want error %q and no write", text, isErr, gotCall, tc.wantMsg)
				}
				return
			}
			var out ActionOut
			if err := json.Unmarshal([]byte(text), &out); isErr || err != nil {
				t.Fatalf("got %q (error %v)", text, isErr)
			}
			if gotCall != tc.wantCall || fmt.Sprint(gotIDs) != fmt.Sprint(tc.wantIDs) || !out.OK || out.Message != tc.wantMsg {
				t.Errorf("call %s%v, out %+v; want %s%v %q", gotCall, gotIDs, out, tc.wantCall, tc.wantIDs, tc.wantMsg)
			}
		})
	}

	t.Run("mark all read with no items", func(t *testing.T) {
		f := &fakeNews{}
		out := decode[ActionOut](t, connect(t, f), "news_mark_all_read", nil)
		if c, _ := f.lastCall(); c != "ListFeeds" || !out.OK || out.Message != "no items: nothing to mark" {
			t.Errorf("last call %s, out %+v", c, out)
		}
	})
}

func TestGetUpdatedTool(t *testing.T) {
	f := &fakeNews{items: []newsapi.Item{
		{ID: 3, LastModified: 300}, {ID: 1, LastModified: 100}, {ID: 4, LastModified: 300}, {ID: 2, LastModified: 200},
	}}
	cs := connect(t, f)

	out := decode[UpdatedOut](t, cs, "news_get_updated", map[string]any{"lastModified": 50, "folderId": 6, "limit": 3})
	if c, _ := f.lastCall(); c != "GetUpdated(50,1,6)" {
		t.Errorf("call = %s", c)
	}
	var ids []int64
	for _, it := range out.Items {
		ids = append(ids, it.ID)
	}
	if fmt.Sprint(ids) != "[1 2 3]" || out.Count != 3 || out.Total != 4 || !out.Truncated || out.NextLastModified != 300 {
		t.Errorf("ids %v, out %+v", ids, out)
	}

	out = decode[UpdatedOut](t, cs, "news_get_updated", map[string]any{"lastModified": 500})
	if out.Count != 4 || out.Truncated || out.NextLastModified != 500 {
		t.Errorf("never moves backwards: %+v", out)
	}

	if text, isErr := call(t, cs, "news_get_updated", map[string]any{"lastModified": -1}); !isErr || !strings.Contains(text, "lastModified must be >= 0") {
		t.Errorf("negative lastModified: %q", text)
	}
}

func TestErrorMapping(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nextcloud.ErrUnauthorized, "Nextcloud authentication failed"},
		{nextcloud.ErrNoCredentials, "missing Nextcloud credentials"},
		{fmt.Errorf("%w: invalid_grant", nextcloud.ErrTokenExchange), "could not obtain a Nextcloud token from the identity provider (token exchange failed: invalid_grant)"},
		{nextcloud.ErrNotFound, "item 5 not found"},
		{fmt.Errorf("nextcloud request failed: %w", context.DeadlineExceeded), "Nextcloud did not answer in time"},
		{&nextcloud.APIError{Status: 503, Message: "maintenance"}, "HTTP 503): maintenance"},
	} {
		cs := connect(t, &fakeNews{err: tc.err})
		if text, isErr := call(t, cs, "news_star", map[string]any{"id": 5}); !isErr || !strings.Contains(text, tc.want) {
			t.Errorf("%v: got %q (error %v), want %q", tc.err, text, isErr, tc.want)
		}
	}
	if err := tools.MapErr(nil, "x"); err != nil {
		t.Errorf("tools.MapErr(nil) = %v", err)
	}
}
