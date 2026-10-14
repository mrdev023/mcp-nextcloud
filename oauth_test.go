package main

import (
	"bytes"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
)

type bearerRT struct{ tok string }

func (b bearerRT) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

// TestOAuthRoutes checks that only the standard discovery documents are
// served, and that the consent page shows the full redirect URI.
func TestOAuthRoutes(t *testing.T) {
	setEnv(t, map[string]string{
		"NEXTCLOUD_URL": "https://cloud.example", "NEXTCLOUD_USER": "u",
		"NEXTCLOUD_APP_PASSWORD": "p", "MCP_AUTH_PASSWORD": "correct horse battery",
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(buildHandler(cfg, logger, nextcloud.NewClient("https://cloud.example", "u", "p", nil), nil))
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
