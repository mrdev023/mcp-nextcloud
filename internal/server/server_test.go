package server

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud/internal/config"
	"github.com/mrdev023/mcp-nextcloud/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud/internal/nextcloud/nextcloudtest"
	"github.com/mrdev023/mcp-nextcloud/internal/oidc"
	"github.com/mrdev023/mcp-nextcloud/internal/oidc/oidctest"
)

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"NEXTCLOUD_URL", "NEXTCLOUD_USER", "NEXTCLOUD_APP_PASSWORD", "MCP_AUTH_PASSWORD",
		"MCP_AUTH_MODE", "MCP_BASE_URL", "MCP_PORT", "MCP_OAUTH_CLIENT_ID", "MCP_OAUTH_DCR", "LOG_LEVEL",
		"MCP_OIDC_ISSUER", "MCP_OIDC_AUDIENCE", "MCP_OIDC_REQUIRED_SCOPES", "MCP_OIDC_CLIENT_ID",
		"MCP_OIDC_CLIENT_SECRET", "MCP_OIDC_EXCHANGE_AUDIENCE", "MCP_OIDC_EXCHANGE_SCOPE", "MCP_READ_ONLY"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// ---------- tests ----------

// TestOAuthRoutes checks that only the standard discovery documents are
// served, and that the consent page shows the full redirect URI.
func TestOAuthRoutes(t *testing.T) {
	setEnv(t, map[string]string{
		"NEXTCLOUD_URL": "https://cloud.example", "NEXTCLOUD_USER": "u",
		"NEXTCLOUD_APP_PASSWORD": "p", "MCP_AUTH_PASSWORD": "correct horse battery",
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHandler(cfg, "test", logger, nextcloud.NewClient("https://cloud.example", "u", "p", nil), nil))
	defer srv.Close()

	for path, want := range map[string]int{
		"/.well-known/oauth-authorization-server":   http.StatusOK,
		"/.well-known/oauth-protected-resource/mcp": http.StatusOK,
		"/.well-known/oauth-protected-resource":     http.StatusOK,
		"/.well-known/openid-configuration":         http.StatusNotFound,
		"/mcp/.well-known/oauth-protected-resource": http.StatusNotFound,
		"/healthz": http.StatusOK,
	} {
		resp, err := http.Get(srv.URL + path)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != want {
			t.Errorf("GET %s = %d, want %d", path, resp.StatusCode, want)
		}
	}

	// Register a client, then render the consent page.
	redirect := "http://127.0.0.1:4567/oauth/callback"
	reg, _ := json.Marshal(map[string]any{"redirect_uris": []string{redirect}, "client_name": "Test client"})
	resp, err := http.Post(srv.URL+"/register", "application/json", bytes.NewReader(reg))
	if err != nil {
		t.Fatal(err)
	}
	var out struct {
		ClientID string `json:"client_id"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&out)
	resp.Body.Close()

	q := url.Values{
		"client_id": {out.ClientID}, "redirect_uri": {redirect}, "response_type": {"code"},
		"code_challenge": {strings.Repeat("a", 43)}, "code_challenge_method": {"S256"},
	}
	resp, err = http.Get(srv.URL + "/authorize?" + q.Encode())
	if err != nil {
		t.Fatal(err)
	}
	page, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("authorize page: status %d", resp.StatusCode)
	}
	for _, want := range []string{"Test client", redirect, "loopback"} {
		if !strings.Contains(string(page), want) {
			t.Errorf("consent page does not mention %q", want)
		}
	}
	if resp.Header.Get("X-Frame-Options") != "DENY" {
		t.Error("consent page can be framed")
	}
}

// TestOIDCEndToEnd runs the whole chain: MCP client with a JWT for mcp-nextcloud
// -> validation -> token exchange -> Nextcloud called with the exchanged
// token only. It also checks that no token reaches the logs.
func TestOIDCEndToEnd(t *testing.T) {
	f := oidctest.NewIDP(t)

	var mu sync.Mutex
	var seen []string
	ncSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		a := r.Header.Get("Authorization")
		mu.Lock()
		seen = append(seen, a)
		mu.Unlock()
		if r.Header.Get("OCS-APIRequest") != "true" || !strings.HasPrefix(a, "Bearer nc-token-for-") {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		writeJSON(w, map[string]any{"feeds": []map[string]any{{"id": 1, "title": strings.TrimPrefix(a, "Bearer ")}}, "folders": []any{}})
	}))
	defer ncSrv.Close()

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	setEnv(t, map[string]string{
		"NEXTCLOUD_URL": ncSrv.URL, "MCP_AUTH_MODE": "oidc", "MCP_BASE_URL": "https://news.example",
		"MCP_OIDC_ISSUER": f.URL, "MCP_OIDC_AUDIENCE": oidctest.Audience, "MCP_OIDC_CLIENT_ID": oidctest.ClientID,
		"MCP_OIDC_CLIENT_SECRET": oidctest.ClientSecret, "MCP_OIDC_EXCHANGE_AUDIENCE": oidctest.UpstreamAudience,
	})
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(NewHandler(cfg, "test", logger, nextcloud.NewClient(ncSrv.URL, "", "", ncSrv.Client()), oidc.New(oidc.Config{Issuer: f.URL, Audience: oidctest.Audience, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret, ExchangeAudience: oidctest.UpstreamAudience, HTTPClient: f.Client(), Logger: logger})))
	defer srv.Close()

	// Protected resource metadata points at the external issuer.
	resp, err := http.Get(srv.URL + "/.well-known/oauth-protected-resource/mcp")
	if err != nil {
		t.Fatal(err)
	}
	var prm struct {
		Resource             string   `json:"resource"`
		AuthorizationServers []string `json:"authorization_servers"`
	}
	_ = json.NewDecoder(resp.Body).Decode(&prm)
	resp.Body.Close()
	if prm.Resource != "https://news.example/mcp" || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != f.URL {
		t.Errorf("PRM = %+v", prm)
	}
	// No embedded authorization server.
	for _, p := range []string{"/authorize", "/token", "/register", "/.well-known/oauth-authorization-server"} {
		resp, _ := http.Get(srv.URL + p)
		resp.Body.Close()
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("%s: status %d, want 404", p, resp.StatusCode)
		}
	}

	// 401 challenges.
	post := func(tok string) *http.Response {
		req, _ := http.NewRequest(http.MethodPost, srv.URL+"/mcp", strings.NewReader("{}"))
		if tok != "" {
			req.Header.Set("Authorization", "Bearer "+tok)
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	r := post("")
	if wa := r.Header.Get("WWW-Authenticate"); r.StatusCode != 401 ||
		!strings.Contains(wa, `resource_metadata="https://news.example/.well-known/oauth-protected-resource/mcp"`) || strings.Contains(wa, "error=") {
		t.Errorf("missing token: %d %q", r.StatusCode, wa)
	}
	wrongAud := f.Claims("alice")
	wrongAud["aud"] = oidctest.UpstreamAudience // a Nextcloud token must not be accepted
	r = post(f.Sign(wrongAud))
	if wa := r.Header.Get("WWW-Authenticate"); r.StatusCode != 401 || !strings.Contains(wa, `error="invalid_token"`) {
		t.Errorf("token for another audience: %d %q", r.StatusCode, wa)
	}

	// Real MCP sessions for two users.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	connect := func(tok string) *mcp.ClientSession {
		c := mcp.NewClient(&mcp.Implementation{Name: "c", Version: "0"}, nil)
		s, err := c.Connect(ctx, &mcp.StreamableClientTransport{Endpoint: srv.URL + "/mcp", HTTPClient: &http.Client{Transport: bearerRT{tok}}}, nil)
		if err != nil {
			t.Fatal(err)
		}
		return s
	}
	call := func(s *mcp.ClientSession) string {
		res, err := s.CallTool(ctx, &mcp.CallToolParams{Name: "news_list_feeds", Arguments: map[string]any{}})
		if err != nil {
			t.Fatal(err)
		}
		return res.Content[0].(*mcp.TextContent).Text
	}
	aliceTok, bobTok := f.Sign(f.Claims("alice")), f.Sign(f.Claims("bob"))
	sa, sb := connect(aliceTok), connect(bobTok)
	defer sa.Close()
	defer sb.Close()
	if out := call(sa); !strings.Contains(out, "nc-token-for-alice") {
		t.Errorf("alice saw %s", out)
	}
	if out := call(sb); !strings.Contains(out, "nc-token-for-bob") {
		t.Errorf("bob saw %s", out)
	}
	_ = call(sa)
	if n := f.ExchangeCount(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (one per user, then cached)", n)
	}

	// IDP refusing the exchange gives a clear tool error.
	f.SetExchange(oidctest.Exchange{Status: http.StatusBadRequest, TokenType: "Bearer", ExpiresIn: 300})
	sc := connect(f.Sign(f.Claims("carol")))
	defer sc.Close()
	res, err := sc.CallTool(ctx, &mcp.CallToolParams{Name: "news_list_feeds", Arguments: map[string]any{}})
	if err != nil || !res.IsError || !strings.Contains(res.Content[0].(*mcp.TextContent).Text, "could not obtain a Nextcloud token") {
		t.Errorf("refused exchange: %v %+v", err, res)
	}

	mu.Lock()
	for _, a := range seen {
		if a == "Bearer "+aliceTok || a == "Bearer "+bobTok || !strings.HasPrefix(a, "Bearer nc-token-for-") {
			t.Errorf("Nextcloud received %q instead of an exchanged token", a)
		}
	}
	mu.Unlock()

	for _, secret := range []string{aliceTok, bobTok, "nc-token-for-alice", "nc-token-for-bob", oidctest.ClientSecret} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs leak a secret: %q", secret[:min(len(secret), 20)])
		}
	}
}

var writeJSON = nextcloudtest.WriteJSON
