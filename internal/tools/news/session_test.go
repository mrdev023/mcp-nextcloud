package news

import (
	"context"
	"slices"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/tools"
)

// connect registers every tool on a fresh server and returns a client
// session connected to it through in-memory transports.
func connect(t *testing.T, api API) *mcp.ClientSession {
	t.Helper()
	return connectWith(t, api, tools.Options{})
}

// connectWith is connect with registration options.
func connectWith(t *testing.T, api API, opts tools.Options) *mcp.ClientSession {
	t.Helper()
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	tools.Register(s, opts, Toolset(api))
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "test-client", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func TestAnnotations(t *testing.T) {
	type hints struct{ readOnly, destructive bool }
	want := map[string]hints{
		"news_list_feeds":    {readOnly: true},
		"news_list_items":    {readOnly: true},
		"news_get_item":      {readOnly: true},
		"news_get_updated":   {readOnly: true},
		"news_mark_read":     {},
		"news_mark_unread":   {},
		"news_star":          {},
		"news_unstar":        {},
		"news_mark_all_read": {destructive: true},
	}
	res, err := connect(t, nil).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Tools) != len(want) {
		t.Fatalf("got %d tools, want %d", len(res.Tools), len(want))
	}
	for _, tl := range res.Tools {
		w, ok := want[tl.Name]
		a := tl.Annotations
		switch {
		case !ok:
			t.Errorf("unexpected tool %s", tl.Name)
		case a == nil || a.Title == "":
			t.Errorf("%s: missing annotations or title", tl.Name)
		case a.ReadOnlyHint != w.readOnly:
			t.Errorf("%s: readOnlyHint = %v, want %v", tl.Name, a.ReadOnlyHint, w.readOnly)
		case a.OpenWorldHint == nil || *a.OpenWorldHint:
			t.Errorf("%s: openWorldHint must be false", tl.Name)
		case !w.readOnly && (a.DestructiveHint == nil || *a.DestructiveHint != w.destructive || !a.IdempotentHint):
			// destructiveHint defaults to true when absent, so write tools
			// must always set it.
			t.Errorf("%s: destructiveHint = %v, idempotentHint = %v, want %v, true", tl.Name, a.DestructiveHint, a.IdempotentHint, w.destructive)
		}
	}
}

func TestReadOnlyOption(t *testing.T) {
	res, err := connectWith(t, nil, tools.Options{ReadOnly: true}).ListTools(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tl := range res.Tools {
		names = append(names, tl.Name)
		if !tl.Annotations.ReadOnlyHint {
			t.Errorf("%s is not read-only", tl.Name)
		}
	}
	slices.Sort(names)
	if want := []string{"news_get_item", "news_get_updated", "news_list_feeds", "news_list_items"}; !slices.Equal(names, want) {
		t.Errorf("tools = %v, want %v", names, want)
	}
}
