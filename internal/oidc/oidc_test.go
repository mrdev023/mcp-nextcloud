package oidc

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"

	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud-news/internal/oidc/oidctest"
)

func newTestClient(f *oidctest.IDP, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	return New(Config{
		Issuer: f.URL, Audience: oidctest.Audience, ClientID: oidctest.ClientID, ClientSecret: oidctest.ClientSecret,
		ExchangeAudience: oidctest.UpstreamAudience, HTTPClient: f.Client(), Logger: logger,
	})
}

func TestOIDCVerify(t *testing.T) {
	f := oidctest.NewIDP(t)
	o := newTestClient(f, nil)
	ctx := context.Background()

	info, err := o.verify(ctx, f.Sign(f.Claims("alice")))
	if err != nil {
		t.Fatalf("valid token rejected: %v", err)
	}
	if info.UserID != f.URL+"|alice" || len(info.Scopes) != 2 || info.Expiration.IsZero() {
		t.Errorf("token info = %+v", info)
	}

	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	forged := jwt.NewWithClaims(jwt.SigningMethodRS256, f.Claims("alice"))
	forged.Header["kid"] = "k1"
	forgedS, _ := forged.SignedString(otherKey)
	hs, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, f.Claims("alice")).SignedString([]byte("whatever-secret"))
	none, _ := jwt.NewWithClaims(jwt.SigningMethodNone, f.Claims("alice")).SignedString(jwt.UnsafeAllowNoneSignatureType)
	mut := func(k string, v any) string {
		c := f.Claims("alice")
		if v == nil {
			delete(c, k)
		} else {
			c[k] = v
		}
		return f.Sign(c)
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
	ecS := f.SignEC(f.Claims("bob"))
	if _, err := o.verify(ctx, ecS); err != nil {
		t.Errorf("ES256 token rejected: %v", err)
	}
}

func TestOIDCKeyRotation(t *testing.T) {
	old := jwksMinRefresh
	jwksMinRefresh = 0
	t.Cleanup(func() { jwksMinRefresh = old })

	f := oidctest.NewIDP(t)
	o := newTestClient(f, nil)
	if _, err := o.verify(context.Background(), f.Sign(f.Claims("alice"))); err != nil {
		t.Fatal(err)
	}
	f.RotateKey("k2")
	if _, err := o.verify(context.Background(), f.Sign(f.Claims("alice"))); err != nil {
		t.Fatalf("token signed with rotated key rejected: %v", err)
	}
}

// TestJWKSRefreshRateLimited checks that tokens signed with unknown keys
// cannot make news-mcp refetch the JWKS on every request.
func TestJWKSRefreshRateLimited(t *testing.T) {
	f := oidctest.NewIDP(t)
	o := newTestClient(f, nil)
	ctx := context.Background()
	if _, err := o.verify(ctx, f.Sign(f.Claims("alice"))); err != nil {
		t.Fatal(err)
	}
	otherKey, _ := rsa.GenerateKey(rand.Reader, 2048)
	for i := range 20 {
		forged := jwt.NewWithClaims(jwt.SigningMethodRS256, f.Claims("mallory"))
		forged.Header["kid"] = fmt.Sprintf("unknown-%d", i)
		s, _ := forged.SignedString(otherKey)
		if _, err := o.verify(ctx, s); err == nil {
			t.Fatal("forged token accepted")
		}
	}
	if n := f.JWKSCount(); n != 1 {
		t.Errorf("JWKS fetched %d times, want 1", n)
	}
	// Valid tokens keep working from the cached keys.
	if _, err := o.verify(ctx, f.Sign(f.Claims("bob"))); err != nil {
		t.Errorf("valid token rejected after forged ones: %v", err)
	}
}

func TestOIDCDiscoveryIssuerMismatch(t *testing.T) {
	f := oidctest.NewIDP(t)
	o := New(Config{
		Issuer: f.URL + "/", Audience: oidctest.Audience, HTTPClient: f.Client(),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if _, err := o.discovery(context.Background()); err == nil {
		t.Fatal("discovery accepted a document whose issuer differs from the configured one")
	}
}

func TestTokenExchange(t *testing.T) {
	f := oidctest.NewIDP(t)
	o := newTestClient(f, nil)
	ctx := context.Background()
	alice := f.Sign(f.Claims("alice"))
	exp := time.Now().Add(5 * time.Minute)

	tok, err := o.Exchange(ctx, alice, exp)
	if err != nil || tok != "nc-token-for-alice" {
		t.Fatalf("exchange = %q, %v", tok, err)
	}
	if f.LastForm().Get("subject_token") != alice || f.LastForm().Get("requested_token_type") != tokenTypeAccessToken {
		t.Errorf("unexpected exchange form: %v", f.LastForm())
	}
	// Cached.
	if tok, _ := o.Exchange(ctx, alice, exp); tok != "nc-token-for-alice" || f.ExchangeCount() != 1 {
		t.Errorf("second exchange not served from cache (exchanges=%d)", f.ExchangeCount())
	}
	// Another subject gets its own token.
	if tok, _ := o.Exchange(ctx, f.Sign(f.Claims("bob")), exp); tok != "nc-token-for-bob" {
		t.Errorf("bob got %q", tok)
	}

	// Not cached when the exchanged token expires within the safety margin.
	f.SetExchange(oidctest.Exchange{Status: http.StatusOK, TokenType: "Bearer", ExpiresIn: 20})
	carol := f.Sign(f.Claims("carol"))
	_, _ = o.Exchange(ctx, carol, exp)
	_, _ = o.Exchange(ctx, carol, exp)
	if n := f.ExchangeCount(); n != 4 {
		t.Errorf("short-lived token was cached (exchanges=%d, want 4)", n)
	}

	// Missing token_type is tolerated, N_A is not.
	f.SetExchange(oidctest.Exchange{Status: http.StatusOK, ExpiresIn: 300})
	if _, err := o.Exchange(ctx, f.Sign(f.Claims("dave")), exp); err != nil {
		t.Errorf("missing token_type: %v", err)
	}
	f.SetExchange(oidctest.Exchange{Status: http.StatusOK, TokenType: "N_A", ExpiresIn: 300})
	if _, err := o.Exchange(ctx, f.Sign(f.Claims("erin")), exp); !errors.Is(err, nextcloud.ErrTokenExchange) {
		t.Errorf("token_type N_A: err = %v", err)
	}

	// IDP refusal surfaces as ErrTokenExchange with the OAuth error code.
	f.SetExchange(oidctest.Exchange{Status: http.StatusBadRequest, TokenType: "Bearer", ExpiresIn: 300})
	_, err = o.Exchange(ctx, f.Sign(f.Claims("frank")), exp)
	if !errors.Is(err, nextcloud.ErrTokenExchange) || !strings.Contains(err.Error(), "invalid_grant") {
		t.Errorf("refused exchange: err = %v", err)
	}
}

func TestExchangeCacheBounded(t *testing.T) {
	o := New(Config{Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
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
