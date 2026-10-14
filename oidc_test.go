package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud/nextcloudtest"
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

// ---------- fake identity provider ----------

const (
	testClientID     = "news-mcp"
	testClientSecret = "s3cret:with%specials"
	testAudience     = "news-mcp"
	testUpstreamAud  = "nextcloud"
)

type fakeIdP struct {
	t   *testing.T
	srv *httptest.Server

	mu        sync.Mutex
	key       *rsa.PrivateKey
	kid       string
	ecKey     *ecdsa.PrivateKey
	exchanges int
	// exchange response knobs
	exchangeStatus int
	tokenType      string
	expiresIn      int64
	lastForm       url.Values
}

func newFakeIdP(t *testing.T) *fakeIdP {
	t.Helper()
	k, _ := rsa.GenerateKey(rand.Reader, 2048)
	ec, _ := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	f := &fakeIdP{t: t, key: k, kid: "k1", ecKey: ec, exchangeStatus: http.StatusOK, tokenType: "Bearer", expiresIn: 300}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{
			"issuer":         f.srv.URL,
			"jwks_uri":       f.srv.URL + "/jwks",
			"token_endpoint": f.srv.URL + "/token",
		})
	})
	mux.HandleFunc("/jwks", func(w http.ResponseWriter, r *http.Request) {
		f.mu.Lock()
		defer f.mu.Unlock()
		pub := f.key.PublicKey
		ecPub := f.ecKey.PublicKey
		writeJSON(w, map[string]any{"keys": []map[string]string{
			{"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
				"n": b64.EncodeToString(pub.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes())},
			{"kty": "EC", "kid": "ec1", "crv": "P-256",
				"x": b64.EncodeToString(ecPub.X.FillBytes(make([]byte, 32))), "y": b64.EncodeToString(ecPub.Y.FillBytes(make([]byte, 32)))},
			{"kty": "RSA", "kid": "enc", "use": "enc", "n": "AQAB", "e": "AQAB"},
		}})
	})
	mux.HandleFunc("/token", func(w http.ResponseWriter, r *http.Request) {
		id, secret, ok := r.BasicAuth()
		id, _ = url.QueryUnescape(id)
		secret, _ = url.QueryUnescape(secret)
		if !ok || id != testClientID || secret != testClientSecret {
			w.WriteHeader(http.StatusUnauthorized)
			writeJSON(w, map[string]string{"error": "invalid_client"})
			return
		}
		_ = r.ParseForm()
		f.mu.Lock()
		f.exchanges++
		f.lastForm = r.PostForm
		status, tt, exp := f.exchangeStatus, f.tokenType, f.expiresIn
		f.mu.Unlock()
		if status != http.StatusOK {
			w.WriteHeader(status)
			writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": "subject token rejected"})
			return
		}
		if r.PostForm.Get("grant_type") != grantTokenExchange || r.PostForm.Get("audience") != testUpstreamAud ||
			r.PostForm.Get("subject_token_type") != tokenTypeAccessToken {
			w.WriteHeader(http.StatusBadRequest)
			writeJSON(w, map[string]string{"error": "invalid_request"})
			return
		}
		// The exchanged token encodes the subject so tests can tell users apart.
		sub, _, _ := jwt.NewParser().ParseUnverified(r.PostForm.Get("subject_token"), jwt.MapClaims{})
		s, _ := sub.Claims.GetSubject()
		resp := map[string]any{"access_token": "nc-token-for-" + s, "expires_in": exp,
			"issued_token_type": tokenTypeAccessToken}
		if tt != "" {
			resp["token_type"] = tt
		}
		writeJSON(w, resp)
	})
	f.srv = httptest.NewServer(mux)
	t.Cleanup(f.srv.Close)
	return f
}

func (f *fakeIdP) claims(sub string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": f.srv.URL, "aud": []string{testAudience, "account"}, "sub": sub,
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(), "scope": "openid news",
	}
}

func (f *fakeIdP) sign(c jwt.MapClaims) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := jwt.NewWithClaims(jwt.SigningMethodRS256, c)
	tok.Header["kid"] = f.kid
	s, err := tok.SignedString(f.key)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

func (f *fakeIdP) exchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

func newTestOIDC(f *fakeIdP, logger *slog.Logger) *OIDC {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return NewOIDC(OIDCConfig{
		Issuer: f.srv.URL, Audience: testAudience, ClientID: testClientID, ClientSecret: testClientSecret,
		ExchangeAudience: testUpstreamAud, HTTPClient: f.srv.Client(), Logger: logger,
	})
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

func TestOIDCVerify(t *testing.T) {
	f := newFakeIdP(t)
	o := newTestOIDC(f, nil)
	ctx := context.Background()

	info, err := o.verify(ctx, f.sign(f.claims("alice")))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if info.UserID != f.srv.URL+"|alice" || len(info.Scopes) != 2 || info.Expiration.IsZero() {
		t.Errorf("token info = %+v", info)
	}

	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, f.claims("alice"))
	forged.Header["kid"] = "k1"
	forgedS, _ := forged.SignedString(otherKey)
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, f.claims("alice")).SignedString([]byte("whatever-secret"))
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, f.claims("alice")).SignedString(jwt.UnsafeAllowNoneSignatureType)
	mut := func(k string, v any) string {
		c := f.claims("alice")
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return f.sign(c)
	}
	for name, tok := range map[string]string{
		"wrong audience": mut("aud", "nextcloud"),
		"wrong issuer":   mut("iss", "https://evil.example"),
		"expired":        mut("exp", time.Now().Add(-2*time.Minute).Unix()),
		"not yet valid":  mut("nbf", time.Now().Add(5*time.Minute).Unix()),
		"missing exp":    mut("exp", nil),
		"bad signature":  forgedS,
		"HS256":          hs,
		"alg none":       none,
		"garbage":        "not.a.jwt",
	} {
		if _, err := o.verify(ctx, tok); err == nil {
			t.Errorf("%s: token accepted", name)
		}
	}

	// Leeway: expired 10 s ago is still within the 30 s tolerance.
	if _, err := o.verify(ctx, mut("exp", time.Now().Add(-10*time.Second).Unix())); err != nil {
		t.Errorf("token within leeway rejected: %v", err)
	}

	// EC keys from the JWKS work too.
	ec := jwt.NewWithClaims(jwt.SigningMethodES256, f.claims("bob"))
	ec.Header["kid"] = "ec1"
	ecS, _ := ec.SignedString(f.ecKey)
	if _, err := o.verify(ctx, ecS); err != nil {
		t.Errorf("ES256 token rejected: %v", err)
	}
}

func TestOIDCKeyRotation(t *testing.T) {
	old := jwksMinRefresh
	jwksMinRefresh = 0
	t.Cleanup(func() { jwksMinRefresh = old })

	f := newFakeIdP(t)
	o := newTestOIDC(f, nil)
	if _, err := o.verify(context.Background(), f.sign(f.claims("alice"))); err != nil {
		t.Fatal(err)
	}
	newKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	f.mu.Lock()
	f.key, f.kid = newKey, "k2"
	f.mu.Unlock()
	if _, err := o.verify(context.Background(), f.sign(f.claims("alice"))); err != nil {
		t.Fatalf("token signed with rotated key rejected: %v", err)
	}
}

func TestOIDCDiscoveryIssuerMismatch(t *testing.T) {
	f := newFakeIdP(t)
	o := NewOIDC(OIDCConfig{
		Issuer: f.srv.URL + "/", Audience: testAudience, HTTPClient: f.srv.Client(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := o.discovery(context.Background()); err == nil {
		t.Fatal("discovery accepted a document whose issuer differs from the configured one")
	}
}

func TestTokenExchange(t *testing.T) {
	f := newFakeIdP(t)
	o := newTestOIDC(f, nil)
	ctx := context.Background()
	alice := f.sign(f.claims("alice"))
	exp := time.Now().Add(5 * time.Minute)

	tok, err := o.Exchange(ctx, alice, exp)
	if err != nil || tok != "nc-token-for-alice" {
		t.Fatalf("exchange = %q, %v", tok, err)
	}
	if f.lastForm.Get("subject_token") != alice || f.lastForm.Get("requested_token_type") != tokenTypeAccessToken {
		t.Errorf("unexpected exchange form: %v", f.lastForm)
	}
	// Cached.
	if tok, _ := o.Exchange(ctx, alice, exp); tok != "nc-token-for-alice" || f.exchangeCount() != 1 {
		t.Errorf("second exchange not served from cache (exchanges=%d)", f.exchangeCount())
	}
	// Another subject gets its own token.
	if tok, _ := o.Exchange(ctx, f.sign(f.claims("bob")), exp); tok != "nc-token-for-bob" {
		t.Errorf("bob got %q", tok)
	}

	// Not cached when the exchanged token expires within the safety margin.
	f.mu.Lock()
	f.expiresIn = 20
	f.mu.Unlock()
	carol := f.sign(f.claims("carol"))
	_, _ = o.Exchange(ctx, carol, exp)
	_, _ = o.Exchange(ctx, carol, exp)
	if n := f.exchangeCount(); n != 4 {
		t.Errorf("short-lived token was cached (exchanges=%d, want 4)", n)
	}

	// Missing token_type is tolerated, N_A is not.
	f.mu.Lock()
	f.expiresIn, f.tokenType = 300, ""
	f.mu.Unlock()
	if _, err := o.Exchange(ctx, f.sign(f.claims("dave")), exp); err != nil {
		t.Errorf("missing token_type: %v", err)
	}
	f.mu.Lock()
	f.tokenType = "N_A"
	f.mu.Unlock()
	if _, err := o.Exchange(ctx, f.sign(f.claims("erin")), exp); !errors.Is(err, nextcloud.ErrTokenExchange) {
		t.Errorf("token_type N_A: err = %v", err)
	}

	// IdP refusal surfaces as nextcloud.ErrTokenExchange with the OAuth error code.
	f.mu.Lock()
	f.tokenType, f.exchangeStatus = "Bearer", http.StatusBadRequest
	f.mu.Unlock()
	_, err = o.Exchange(ctx, f.sign(f.claims("frank")), exp)
	if !errors.Is(err, nextcloud.ErrTokenExchange) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("refused exchange: err = %v", err)
	}
}

func TestExchangeCacheBounded(t *testing.T) {
	o := NewOIDC(OIDCConfig{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
	now := time.Now()
	for i := range exchangeCacheLimit + 50 {
		var k [32]byte
		copy(k[:], fmt.Sprintf("key-%d", i))
		o.storeLocked(k, cachedToken{token: "t", exp: now.Add(time.Duration(i+1) * time.Second)}, now)
	}
	if len(o.cache) != exchangeCacheLimit {
		t.Fatalf("cache size = %d, want %d", len(o.cache), exchangeCacheLimit)
	}
	var newest [32]byte
	copy(newest[:], fmt.Sprintf("key-%d", exchangeCacheLimit+49))
	if _, ok := o.cache[newest]; !ok {
		t.Error("newest entry was evicted")
	}
}

// TestOIDCEndToEnd runs the whole chain: MCP client with a JWT for news-mcp
// -> validation -> token exchange -> Nextcloud called with the exchanged
// token only. It also checks that no token reaches the logs.
func TestOIDCEndToEnd(t *testing.T) {
	f := newFakeIdP(t)

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
		"MCP_OIDC_ISSUER": f.srv.URL, "MCP_OIDC_AUDIENCE": testAudience, "MCP_OIDC_CLIENT_ID": testClientID,
		"MCP_OIDC_CLIENT_SECRET": testClientSecret, "MCP_OIDC_EXCHANGE_AUDIENCE": testUpstreamAud,
	})
	cfg, err := loadConfig()
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(buildHandler(cfg, logger, nextcloud.NewClient(ncSrv.URL, "", "", ncSrv.Client()), newTestOIDC(f, logger)))
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
	if prm.Resource != "https://news.example/mcp" || len(prm.AuthorizationServers) != 1 || prm.AuthorizationServers[0] != f.srv.URL {
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
	wrongAud := f.claims("alice")
	wrongAud["aud"] = testUpstreamAud // a Nextcloud token must not be accepted
	r = post(f.sign(wrongAud))
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
	aliceTok, bobTok := f.sign(f.claims("alice")), f.sign(f.claims("bob"))
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
	if n := f.exchangeCount(); n != 2 {
		t.Errorf("exchanges = %d, want 2 (one per user, then cached)", n)
	}

	// IdP refusing the exchange gives a clear tool error.
	f.mu.Lock()
	f.exchangeStatus = http.StatusBadRequest
	f.mu.Unlock()
	sc := connect(f.sign(f.claims("carol")))
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

	for _, secret := range []string{aliceTok, bobTok, "nc-token-for-alice", "nc-token-for-bob", testClientSecret} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs leak a secret: %q", secret[:min(len(secret), 20)])
		}
	}
}

var writeJSON = nextcloudtest.WriteJSON
