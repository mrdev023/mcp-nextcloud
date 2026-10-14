package nextcloud

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/mrdev023/mcp-nextcloud/internal/nextcloud/nextcloudtest"
)

var writeJSON = nextcloudtest.WriteJSON

// listFolders makes one authenticated call through Do.
func listFolders(ctx context.Context, c *Client) error {
	return c.Do(ctx, http.MethodGet, "index.php/apps/news/api/v1-3/folders", nil, nil, nil)
}

func TestClientTokenSource(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Authorization")
		writeJSON(w, map[string]any{"folders": []any{}})
	}))
	defer srv.Close()

	anon := NewClient(srv.URL, "", "", srv.Client())
	if err := listFolders(context.Background(), anon); !errors.Is(err, ErrNoCredentials) {
		t.Fatalf("no credentials: err = %v", err)
	}
	src := func(context.Context) (string, error) { return "tok-1", nil }
	basic := NewClient(srv.URL, "u", "p", srv.Client())
	if err := listFolders(WithTokenSource(context.Background(), src), basic); err != nil || got != "Bearer tok-1" {
		t.Fatalf("token source must win over basic: Authorization = %q, err = %v", got, err)
	}
	failing := func(context.Context) (string, error) { return "", fmt.Errorf("%w: boom", ErrTokenExchange) }
	if err := listFolders(WithTokenSource(context.Background(), failing), anon); !errors.Is(err, ErrTokenExchange) {
		t.Fatalf("failing source: err = %v", err)
	}
}

func TestUserAgent(t *testing.T) {
	var got []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = append(got, r.Header.Get("User-Agent"))
		writeJSON(w, map[string]any{})
	}))
	defer srv.Close()
	ctx := context.Background()
	_ = listFolders(ctx, NewClient(srv.URL, "u", "p", srv.Client()))
	_ = listFolders(ctx, NewClient(srv.URL, "u", "p", srv.Client(), WithUserAgent("MCP Nextcloud/1.2.3")))
	if len(got) != 2 || got[0] != "MCP Nextcloud" || got[1] != "MCP Nextcloud/1.2.3" {
		t.Errorf("User-Agent = %q", got)
	}
}
