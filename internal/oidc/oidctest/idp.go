// Package oidctest provides a fake OpenID Connect provider for tests: OIDC
// discovery, a JWKS with RSA and EC keys, and an RFC 8693 token endpoint.
package oidctest

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/rsa"
	"encoding/base64"
	"encoding/json"
	"math/big"
	"net/http"
	"net/http/httptest"
	"net/url"
	"sync"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
)

// Settings the fake provider expects from news-mcp.
const (
	ClientID         = "news-mcp"
	ClientSecret     = "s3cret:with%specials"
	Audience         = "news-mcp"
	UpstreamAudience = "nextcloud"
	// ECKeyID is the kid of the EC P-256 key published in the JWKS.
	ECKeyID = "ec1"
)

const (
	grantTokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
)

var b64 = base64.RawURLEncoding

// Exchange tunes the answers of the token endpoint.
type Exchange struct {
	Status    int    // HTTP status; anything but 200 answers invalid_grant
	TokenType string // token_type of the response; "" omits it
	ExpiresIn int64  // expires_in of the response
}

// IdP is a fake identity provider. Exchanged tokens are "nc-token-for-<sub>",
// so tests can tell users apart.
type IdP struct {
	*httptest.Server
	t *testing.T

	mu        sync.Mutex
	key       *rsa.PrivateKey
	kid       string
	ecKey     *ecdsa.PrivateKey
	exchange  Exchange
	exchanges int
	lastForm  url.Values
}

// NewIdP starts a fake provider. It is closed when the test ends.
func NewIdP(t *testing.T) *IdP {
	t.Helper()
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	ec, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	f := &IdP{t: t, key: k, kid: "k1", ecKey: ec,
		exchange: Exchange{Status: http.StatusOK, TokenType: "Bearer", ExpiresIn: 300}}
	mux := http.NewServeMux()
	mux.HandleFunc("/.well-known/openid-configuration", func(w http.ResponseWriter, r *http.Request) {
		writeJSON(w, map[string]string{
			"issuer":         f.URL,
			"jwks_uri":       f.URL + "/jwks",
			"token_endpoint": f.URL + "/token",
		})
	})
	mux.HandleFunc("/jwks", f.jwks)
	mux.HandleFunc("/token", f.token)
	f.Server = httptest.NewServer(mux)
	t.Cleanup(f.Close)
	return f
}

func (f *IdP) jwks(w http.ResponseWriter, _ *http.Request) {
	f.mu.Lock()
	defer f.mu.Unlock()
	pub := f.key.PublicKey
	ecPub := f.ecKey.PublicKey
	writeJSON(w, map[string]any{"keys": []map[string]string{
		{"kty": "RSA", "kid": f.kid, "use": "sig", "alg": "RS256",
			"n": b64.EncodeToString(pub.N.Bytes()), "e": b64.EncodeToString(big.NewInt(int64(pub.E)).Bytes())},
		{"kty": "EC", "kid": ECKeyID, "crv": "P-256",
			"x": b64.EncodeToString(ecPub.X.FillBytes(make([]byte, 32))), "y": b64.EncodeToString(ecPub.Y.FillBytes(make([]byte, 32)))},
		{"kty": "RSA", "kid": "enc", "use": "enc", "n": "AQAB", "e": "AQAB"},
	}})
}

func (f *IdP) token(w http.ResponseWriter, r *http.Request) {
	id, secret, ok := r.BasicAuth()
	id, _ = url.QueryUnescape(id)
	secret, _ = url.QueryUnescape(secret)
	if !ok || id != ClientID || secret != ClientSecret {
		w.WriteHeader(http.StatusUnauthorized)
		writeJSON(w, map[string]string{"error": "invalid_client"})
		return
	}
	_ = r.ParseForm()
	f.mu.Lock()
	f.exchanges++
	f.lastForm = r.PostForm
	ex := f.exchange
	f.mu.Unlock()
	if ex.Status != http.StatusOK {
		w.WriteHeader(ex.Status)
		writeJSON(w, map[string]string{"error": "invalid_grant", "error_description": "subject token rejected"})
		return
	}
	if r.PostForm.Get("grant_type") != grantTokenExchange || r.PostForm.Get("audience") != UpstreamAudience ||
		r.PostForm.Get("subject_token_type") != tokenTypeAccessToken {
		w.WriteHeader(http.StatusBadRequest)
		writeJSON(w, map[string]string{"error": "invalid_request"})
		return
	}
	sub, _, _ := jwt.NewParser().ParseUnverified(r.PostForm.Get("subject_token"), jwt.MapClaims{})
	s, _ := sub.Claims.GetSubject()
	resp := map[string]any{"access_token": "nc-token-for-" + s, "expires_in": ex.ExpiresIn,
		"issued_token_type": tokenTypeAccessToken}
	if ex.TokenType != "" {
		resp["token_type"] = ex.TokenType
	}
	writeJSON(w, resp)
}

// Claims returns valid access-token claims for sub, issued for Audience.
func (f *IdP) Claims(sub string) jwt.MapClaims {
	return jwt.MapClaims{
		"iss": f.URL, "aud": []string{Audience, "account"}, "sub": sub,
		"exp": time.Now().Add(5 * time.Minute).Unix(), "iat": time.Now().Unix(), "scope": "openid news",
	}
}

// Sign signs c with the current RSA key (RS256).
func (f *IdP) Sign(c jwt.MapClaims) string {
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

// SignEC signs c with the EC key (ES256, kid ECKeyID).
func (f *IdP) SignEC(c jwt.MapClaims) string {
	f.mu.Lock()
	defer f.mu.Unlock()
	tok := jwt.NewWithClaims(jwt.SigningMethodES256, c)
	tok.Header["kid"] = ECKeyID
	s, err := tok.SignedString(f.ecKey)
	if err != nil {
		f.t.Fatal(err)
	}
	return s
}

// RotateKey replaces the RSA signing key with a new one published as kid.
func (f *IdP) RotateKey(kid string) {
	k, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		f.t.Fatal(err)
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key, f.kid = k, kid
}

// SetExchange changes the answers of the token endpoint.
func (f *IdP) SetExchange(ex Exchange) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.exchange = ex
}

// ExchangeCount returns the number of authenticated token requests.
func (f *IdP) ExchangeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.exchanges
}

// LastForm returns the form of the last authenticated token request.
func (f *IdP) LastForm() url.Values {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.lastForm
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}
