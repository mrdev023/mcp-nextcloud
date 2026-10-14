package tools

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
)

type empty struct{}

func handler(context.Context, *mcp.CallToolRequest, empty) (*mcp.CallToolResult, empty, error) {
	return nil, empty{}, nil
}

func listTools(t *testing.T, opts Options, sets ...Toolset) []*mcp.Tool {
	t.Helper()
	ctx := context.Background()
	s := mcp.NewServer(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	Register(s, opts, sets...)
	st, ct := mcp.NewInMemoryTransports()
	ss, err := s.Connect(ctx, st, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ss.Close() })
	cs, err := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil).Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	res, err := cs.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	return res.Tools
}

func TestRegister(t *testing.T) {
	a := Toolset{Name: "a", Tools: []ServerTool{
		New("a_read", "Read", "reads", ReadOnly, handler),
		New("a_toggle", "Toggle", "toggles", Reversible, handler),
	}}
	b := Toolset{Name: "b", Tools: []ServerTool{
		New("b_wipe", "Wipe", "wipes", Destructive, handler),
	}}

	names := func(ts []*mcp.Tool) []string {
		var n []string
		for _, t := range ts {
			n = append(n, t.Name)
		}
		slices.Sort(n)
		return n
	}
	if got := names(listTools(t, Options{}, a, b)); !slices.Equal(got, []string{"a_read", "a_toggle", "b_wipe"}) {
		t.Errorf("all tools: %v", got)
	}
	if got := names(listTools(t, Options{ReadOnly: true}, a, b)); !slices.Equal(got, []string{"a_read"}) {
		t.Errorf("read-only: %v", got)
	}

	for _, tl := range listTools(t, Options{}, a, b) {
		an := tl.Annotations
		got := fmt.Sprintf("ro=%v destructive=%v idempotent=%v openWorld=%v", an.ReadOnlyHint, deref(an.DestructiveHint), an.IdempotentHint, deref(an.OpenWorldHint))
		want := map[string]string{
			"a_read":   "ro=true destructive=<nil> idempotent=false openWorld=false",
			"a_toggle": "ro=false destructive=false idempotent=true openWorld=false",
			"b_wipe":   "ro=false destructive=true idempotent=true openWorld=false",
		}[tl.Name]
		if got != want || an.Title == "" {
			t.Errorf("%s: %s (title %q), want %s", tl.Name, got, an.Title, want)
		}
	}
}

func deref(b *bool) any {
	if b == nil {
		return nil
	}
	return *b
}

func TestMapErr(t *testing.T) {
	for _, tc := range []struct {
		err  error
		want string
	}{
		{nil, ""},
		{nextcloud.ErrUnauthorized, "Nextcloud authentication failed"},
		{nextcloud.ErrNoCredentials, "missing Nextcloud credentials"},
		{fmt.Errorf("%w: invalid_grant", nextcloud.ErrTokenExchange), "identity provider (token exchange failed: invalid_grant)"},
		{nextcloud.ErrNotFound, "note 3 not found"},
		{fmt.Errorf("request failed: %w", context.DeadlineExceeded), "Nextcloud did not answer in time"},
		{&nextcloud.APIError{Status: 500}, "nextcloud API error (HTTP 500)"},
	} {
		err := MapErr(tc.err, "note 3")
		if tc.want == "" {
			if err != nil {
				t.Errorf("MapErr(nil) = %v", err)
			}
			continue
		}
		if err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("MapErr(%v) = %v, want %q", tc.err, err, tc.want)
		}
		if errors.Is(tc.err, nextcloud.ErrTokenExchange) && !errors.Is(err, nextcloud.ErrTokenExchange) {
			t.Errorf("MapErr must keep ErrTokenExchange wrapped")
		}
	}
}
