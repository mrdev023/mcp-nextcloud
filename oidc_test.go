package main

import (
	"bytes"
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud/nextcloudtest"
	"github.com/mrdev023/mcp-nextcloud-news/internal/oidc"
	"github.com/mrdev023/mcp-nextcloud-news/internal/oidc/oidctest"
)

func setEnv(t *testing.T, kv map[string]string) {
	t.Helper()
	for _, k := range []string{"NEXTCLOUD_URL", "NEXTCLOUD_USER", "NEXTCLOUD_APP_PASSWORD", "MCP_AUTH_PASSWORD",
		"MCP_AUTH_MODE", "MCP_BASE_URL", "MCP_PORT", "MCP_OAUTH_CLIENT_ID", "MCP_OAUTH_DCR", "LOG_LEVEL",
		"MCP_OIDC_ISSUER", "MCP_OIDC_AUDIENCE", "MCP_OIDC_REQUIRED_SCOPES", "MCP_OIDC_CLIENT_ID",
		"MCP_OIDC_CLIENT_SECRET", "MCP_OIDC_EXCHANGE_AUDIENCE", "MCP_OIDC_EXCHANGE_SCOPE"} {
		t.Setenv(k, "")
	}
	for k, v := range kv {
		t.Setenv(k, v)
	}
}

// ---------- tests ----------

func TestLoadConfigModes(t *testing.T) {
	oidcEnv := map[string]string{
		"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oidc",
		"MCP_OIDC_ISSUER": "https://id.example/realms/r", "MCP_OIDC_CLIENT_ID": "news-mcp",
		"MCP_OIDC_CLIENT_SECRET": "x", "MCP_OIDC_EXCHANGE_AUDIENCE": "nextcloud",
		"MCP_BASE_URL": "https://news.example",
	}
	with := func(kv map[string]string) map[string]string {
		m := map[string]string{}
		for k, v := range oidcEnv {
			m[k] = v
		}
		for k, v := range kv {
			m[k] = v
		}
		return m
	}
	cases := []struct {
		name     string
		env      map[string]string
		wantErr  string
		wantMode string
		wantAud  string
	}{
		{"oidc defaults audience to resource URI", oidcEnv, "", authModeOIDC, "https://news.example/mcp"},
		{"oidc explicit audience", with(map[string]string{"MCP_OIDC_AUDIENCE": "news-mcp"}), "", authModeOIDC, "news-mcp"},
		{"oidc missing settings", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oidc"},
			"MCP_OIDC_CLIENT_ID, MCP_OIDC_CLIENT_SECRET, MCP_OIDC_EXCHANGE_AUDIENCE, MCP_OIDC_ISSUER", "", ""},
		{"oidc plain http issuer refused", with(map[string]string{"MCP_OIDC_ISSUER": "http://id.example"}), "https", "", ""},
		{"oidc http loopback issuer allowed", with(map[string]string{"MCP_OIDC_ISSUER": "http://127.0.0.1:9000/realms/r"}), "", authModeOIDC, ""},
		{"standalone still needs secrets", map[string]string{"NEXTCLOUD_URL": "https://cloud.example"}, "MCP_AUTH_PASSWORD", "", ""},
		{"oauth is an alias of standalone", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "oauth",
			"NEXTCLOUD_USER": "u", "NEXTCLOUD_APP_PASSWORD": "p", "MCP_AUTH_PASSWORD": "correct horse battery"}, "", authModeStandalone, ""},
		{"gateway mode removed", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "gateway"}, "use MCP_AUTH_MODE=oidc", "", ""},
		{"passthrough mode removed", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "passthrough"}, "use MCP_AUTH_MODE=oidc", "", ""},
		{"unknown mode", map[string]string{"NEXTCLOUD_URL": "https://cloud.example", "MCP_AUTH_MODE": "magic"}, "MCP_AUTH_MODE", "", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			setEnv(t, tc.env)
			cfg, err := loadConfig()
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to mention %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if cfg.AuthMode != tc.wantMode {
				t.Errorf("mode = %q, want %q", cfg.AuthMode, tc.wantMode)
			}
			if tc.wantAud != "" && cfg.OIDCAudience != tc.wantAud {
				t.Errorf("audience = %q, want %q", cfg.OIDCAudience, tc.wantAud)
			}
		})
	}
}

// TestOIDCEndToEnd runs the whole chain: MCP client with a JWT for news-mcp
// -> validation -> token exchange -> Nextcloud called with the exchanged
// token only. It also checks that no token reaches the logs.
func TestOIDCEndToEnd(t *testing.T) {
	f := oidctest.NewIdP(t)

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
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(buildHandler(cfg, logger, nextcloud.NewClient(ncSrv.URL, "", "", ncSrv.Client()), oidc.New(oidc.Config{Issuer: f.URL, Audience: oidctest.Audience, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret, ExchangeAudience: oidctest.UpstreamAudience, HTTPClient: f.Client(), Logger: logger})))
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

	// IdP refusing the exchange gives a clear tool error.
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
