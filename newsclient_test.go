package main

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

const testPrefix = "/index.php/apps/news/api/v1-3/"

type recorded struct {
	method, path, query, body string
}

func newTestServer(t *testing.T, routes map[string]func(w http.ResponseWriter, r *http.Request)) (*NewsClient, *[]recorded) {
	t.Helper()
	var calls []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		u, p, ok := r.BasicAuth()
		if !ok || u != "alice" || p != "secret-app-pw" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		b, _ := io.ReadAll(r.Body)
		path := strings.TrimPrefix(r.URL.Path, testPrefix)
		calls = append(calls, recorded{r.Method, path, r.URL.RawQuery, string(b)})
		if h, ok := routes[r.Method+" "+path]; ok {
			h(w, r)
			return
		}
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	return NewNewsClient(srv.URL+"/", "alice", "secret-app-pw", srv.Client()), &calls
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

func TestListFeeds(t *testing.T) {
	c, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET feeds": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{
				"feeds":        []map[string]any{{"id": 1, "url": "https://ex.org/rss", "title": "Ex", "folderId": nil, "unreadCount": 3}},
				"starredCount": 2,
				"newestItemId": 42,
			})
		},
	})
	r, err := c.ListFeeds(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Feeds) != 1 || r.Feeds[0].Title != "Ex" || r.Feeds[0].UnreadCount != 3 || r.NewestItemID == nil || *r.NewestItemID != 42 {
		t.Fatalf("unexpected: %+v", r)
	}
}

func TestListItemsQueryAndDecoding(t *testing.T) {
	c, calls := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET items": func(w http.ResponseWriter, r *http.Request) {
			writeJSON(w, map[string]any{"items": []map[string]any{
				{"id": 10, "title": "A", "unread": true, "starred": false, "feedId": 1},
				{"id": 9, "title": "B", "isUnread": false, "isStarred": true, "feedId": 1},
			}})
		},
	})
	items, err := c.ListItems(context.Background(), ListItemsParams{Type: TypeFeed, ID: 1, BatchSize: 5, Offset: 11, Search: "go"})
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 || !items[0].Unread || items[1].Unread || !items[1].Starred {
		t.Fatalf("unexpected items: %+v", items)
	}
	q := (*calls)[0].query
	for _, want := range []string{"type=0", "id=1", "batchSize=5", "offset=11", "getRead=false", "search=go"} {
		if !strings.Contains(q, want) {
			t.Errorf("query %q missing %q", q, want)
		}
	}
}

func TestGetItem(t *testing.T) {
	c, calls := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET items": func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Query().Get("offset") == "8" {
				writeJSON(w, map[string]any{"items": []map[string]any{{"id": 7, "title": "x"}}})
				return
			}
			writeJSON(w, map[string]any{"items": []map[string]any{{"id": 5, "title": "x"}}})
		},
	})
	it, err := c.GetItem(context.Background(), 7)
	if err != nil || it.ID != 7 {
		t.Fatalf("GetItem(7) = %+v, %v", it, err)
	}
	q := (*calls)[0].query
	if !strings.Contains(q, "type=3") || !strings.Contains(q, "getRead=true") || !strings.Contains(q, "batchSize=1") {
		t.Errorf("unexpected query %q", q)
	}
	if _, err := c.GetItem(context.Background(), 6); !errors.Is(err, ErrNotFound) {
		t.Fatalf("GetItem(6) err = %v, want ErrNotFound", err)
	}
}

func TestMarkAndStar(t *testing.T) {
	ok := func(w http.ResponseWriter, r *http.Request) {}
	c, calls := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"POST items/3/read":          ok,
		"POST items/read/multiple":   ok,
		"POST items/3/unread":        ok,
		"POST items/unread/multiple": ok,
		"POST items/read":            ok,
		"POST items/3/star":          ok,
		"POST items/3/unstar":        ok,
	})
	ctx := context.Background()
	steps := []func() error{
		func() error { return c.MarkRead(ctx, 3) },
		func() error { return c.MarkRead(ctx, 1, 2) },
		func() error { return c.MarkUnread(ctx, 3) },
		func() error { return c.MarkUnread(ctx, 4, 5) },
		func() error { return c.MarkAllRead(ctx, 99) },
		func() error { return c.Star(ctx, 3) },
		func() error { return c.Unstar(ctx, 3) },
	}
	for i, s := range steps {
		if err := s(); err != nil {
			t.Fatalf("step %d: %v", i, err)
		}
	}
	if b := (*calls)[1].body; b != `{"itemIds":[1,2]}` {
		t.Errorf("read/multiple body = %q", b)
	}
	if q := (*calls)[4].query; q != "newestItemId=99" {
		t.Errorf("mark all read query = %q", q)
	}
}

func TestErrors(t *testing.T) {
	c, _ := newTestServer(t, map[string]func(http.ResponseWriter, *http.Request){
		"GET folders": func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			writeJSON(w, map[string]string{"message": "boom"})
		},
	})
	ctx := context.Background()
	if err := c.Star(ctx, 123); !errors.Is(err, ErrNotFound) {
		t.Errorf("404: got %v", err)
	}
	var apiErr *APIError
	if _, err := c.ListFolders(ctx); !errors.As(err, &apiErr) || apiErr.Status != 500 || apiErr.Message != "boom" {
		t.Errorf("500: got %v", err)
	}

	bad := NewNewsClient(c.base[:len(c.base)-len(testPrefix)], "alice", "wrong", c.http)
	_, err := bad.ListFeeds(ctx)
	if !errors.Is(err, ErrUnauthorized) {
		t.Fatalf("401: got %v", err)
	}
	if strings.Contains(err.Error(), "wrong") {
		t.Errorf("error leaks password: %v", err)
	}
}
