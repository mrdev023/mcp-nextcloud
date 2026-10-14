package standalone

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud/nextcloudtest"
	"github.com/mrdev023/mcp-nextcloud-news/internal/tools"
)

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// TestEndToEnd exercises DCR -> authorize -> token -> authenticated MCP calls.
func TestEndToEnd(t *testing.T) {
	ncSrv := nextcloudtest.NewServer(t, map[string]http.HandlerFunc{
		"GET items": func(w http.ResponseWriter, r *http.Request) {
			nextcloudtest.WriteJSON(w, map[string]any{"items": []map[string]any{
				{"id": 5, "title": "Hello", "body": "<p>Hello <b>world</b></p>", "unread": true, "feedId": 1, "pubDate": 1700000000},
			}})
		},
		"POST items/read/multiple": func(w http.ResponseWriter, r *http.Request) {},
	})
	nc := nextcloud.NewClient(ncSrv.URL, nextcloudtest.User, nextcloudtest.AppPassword, ncSrv.Client())

	mux := http.NewServeMux()
	srv := httptest.NewServer(mux)
	defer srv.Close()

	as := New(Config{
		Issuer:        srv.URL,
		Resource:      srv.URL + "/mcp",
		LoginPassword: "correct horse battery",
		AllowDCR:      true,
		SigningKey:    []byte("0123456789abcdef0123456789abcdef"),
		Logger:        slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	server := mcp.NewServer(&mcp.Implementation{Name: "t", Version: "0"}, nil)
	tools.Register(server, nc)
	h := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, nil)
	mux.Handle("/mcp", auth.RequireBearerToken(as.VerifyAccessToken, &auth.RequireBearerTokenOptions{Scopes: []string{Scope}})(h))
	mux.HandleFunc("/register", as.RegisterHandler)
	mux.HandleFunc("/authorize", as.AuthorizeHandler)
	mux.HandleFunc("/token", as.TokenHandler)

	// Unauthenticated MCP request is rejected.
	resp, err := http.Post(srv.URL+"/mcp", "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated: status %d", resp.StatusCode)
	}

	// 1. Dynamic client registration.
	redirect := "http://127.0.0.1:33418/callback"
	reg, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "client_name": "test"})
	resp, err = http.Post(srv.URL+"/register", "application/json", bytes.NewReader(reg))
	if err != nil {
		t.Fatal(err)
	}
	var regOut struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&regOut)
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated || regOut.ClientID == "" {
		t.Fatalf("register: status %d", resp.StatusCode)
	}

	// 2. Authorization with PKCE.
	verifier := strings.Repeat("v", 50)
	sum := sha256.Sum256([]byte(verifier))
	form := url.Values{
		"client_id": {regOut.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"state": {"xyz"}, "code_challenge": {b64.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"resource": {srv.URL + "/mcp"},
	}
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	form.Set("password", "wrong password!")
	resp, _ = noRedirect.PostForm(srv.URL+"/authorize", form)
	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("wrong password: status %d", resp.StatusCode)
	}

	form.Set("password", "correct horse battery")
	resp, err = noRedirect.PostForm(srv.URL+"/authorize", form)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	loc, _ := url.Parse(resp.Header.Get("Location"))
	code := loc.Query().Get("code")
	if resp.StatusCode != http.StatusFound || code == "" || loc.Query().Get("state") != "xyz" {
		t.Fatalf("authorize: status %d, location %q", resp.StatusCode, loc)
	}

	// 3. Token exchange.
	tokenReq := url.Values{
		"grant_type": {"authorization_code"}, "code": {code}, "redirect_uri": {redirect},
		"client_id": {regOut.ClientID}, "code_verifier": {verifier},
	}
	var tok struct {
		AccessToken  string `json:"access_token"`
		RefreshToken string `json:"refresh_token"`
	}
	resp, _ = http.PostForm(srv.URL+"/token", tokenReq)
	_ = json.NewDecoder(resp.Body).Decode(&tok)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || tok.AccessToken == "" {
		t.Fatalf("token: status %d", resp.StatusCode)
	}
	// Code replay must fail.
	resp, _ = http.PostForm(srv.URL+"/token", tokenReq)
	resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("code replay: status %d", resp.StatusCode)
	}
	// Refresh works.
	resp, _ = http.PostForm(srv.URL+"/token", url.Values{
		"grant_type": {"refresh_token"}, "refresh_token": {tok.RefreshToken}, "client_id": {regOut.ClientID},
	})
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("refresh: status %d", resp.StatusCode)
	}
	// A refresh token is not an access token.
	if _, err := as.VerifyAccessToken(context.Background(), tok.RefreshToken, nil); err == nil {
		t.Fatal("refresh token accepted as access token")
	}

	// 4. MCP session with the access token.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	client := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint:   srv.URL + "/mcp",
		HTTPClient: &http.Client{Transport: bearerRT{tok.AccessToken}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer sess.Close()

	list, err := sess.ListTools(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(list.Tools) != 9 {
		t.Fatalf("got %d tools", len(list.Tools))
	}
	for _, tl := range list.Tools {
		write := strings.Contains(tl.Name, "mark") || strings.Contains(tl.Name, "star")
		if tl.Annotations == nil || tl.Annotations.ReadOnlyHint == write {
			t.Errorf("%s: bad annotations %+v", tl.Name, tl.Annotations)
		}
	}

	res, err := sess.CallTool(ctx, &mcp.CallToolParams{Name: "news_list_items", Arguments: map[string]any{}})
	if err != nil || res.IsError {
		t.Fatalf("list items: %v %+v", err, res)
	}
	txt := res.Content[0].(*mcp.TextContent).Text
	if !strings.Contains(txt, `"Hello world"`) || !strings.Contains(txt, "2023-11-14T22:13:20Z") {
		t.Errorf("list items output: %s", txt)
	}

	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "news_mark_read", Arguments: map[string]any{"itemIds": []int{3, 1, 3}}})
	if err != nil || res.IsError {
		t.Fatalf("mark read: %v %+v", err, res)
	}
	if b := ncSrv.LastCall().Body; b != `{"itemIds":[1,3]}` {
		t.Errorf("mark read body %q", b)
	}

	res, err = sess.CallTool(ctx, &mcp.CallToolParams{Name: "news_star", Arguments: map[string]any{"id": 999}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "item 999 not found") {
		t.Errorf("star missing item: %v %+v", err, res)
	}
}

func TestSameResource(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		want bool
	}{
		{"https://mcp.example.com/mcp", "https://mcp.example.com/mcp", true},
		{"HTTPS://MCP.Example.com/mcp/", "https://mcp.example.com/mcp", true},
		{"https://mcp.example.com/MCP", "https://mcp.example.com/mcp", false},
		{"https://mcp.example.com/mcp", "https://other.example.com/mcp", false},
		{"https://mcp.example.com/mcp#x", "https://mcp.example.com/mcp", false},
		{"http://mcp.example.com/mcp", "https://mcp.example.com/mcp", false},
	} {
		if got := sameResource(tc.a, tc.b); got != tc.want {
			t.Errorf("sameResource(%q, %q) = %v, want %v", tc.a, tc.b, got, tc.want)
		}
	}
}
