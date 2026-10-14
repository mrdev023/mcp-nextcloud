// Package oidc implements the external OIDC provider mode (MCP_AUTH_MODE=oidc).
//
// news-mcp is an OAuth 2.1 resource server for an external authorization
// server, as the MCP authorization spec describes:
//   - it validates every bearer JWT itself (signature via the issuer's JWKS,
//     iss, aud, exp/nbf, asymmetric algorithms only) and only accepts tokens
//     issued for it;
//   - it advertises the issuer in its RFC 9728 protected resource metadata;
//   - to call Nextcloud it never forwards the token it received: it acts as
//     an OAuth client and obtains a distinct Nextcloud-audience token through
//     an RFC 8693 token exchange, which is cached per incoming token.
package oidc

import (
	"context"
	"crypto/ecdsa"
	"crypto/ed25519"
	"crypto/elliptic"
	"crypto/rsa"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/mrdev023/mcp-nextcloud-news/internal/httpx"
	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
)

// Only asymmetric algorithms: "none" and HMAC (HS*) are never accepted.
var allowedJWTAlgs = []string{"RS256", "RS384", "RS512", "PS256", "PS384", "PS512", "ES256", "ES384", "ES512", "EdDSA"}

const (
	jwtLeeway          = 30 * time.Second
	jwksMaxAge         = time.Hour // periodic refresh, for key rotation
	discoveryRetry     = 10 * time.Second
	exchangeMargin     = 30 * time.Second // renew exchanged tokens this early
	exchangeCacheLimit = 256
	idpBodyLimit       = 1 << 20

	tokenTypeAccessToken = "urn:ietf:params:oauth:token-type:access_token"
	grantTokenExchange   = "urn:ietf:params:oauth:grant-type:token-exchange"
)

// jwksMinRefresh rate-limits JWKS refreshes triggered by unknown kids (a
// variable so tests can disable it).
var jwksMinRefresh = 30 * time.Second

var errNoKey = errors.New("no matching signing key")

var b64 = base64.RawURLEncoding

// Config configures the OIDC resource server and token exchange.
type Config struct {
	Issuer           string   // MCP_OIDC_ISSUER
	Audience         string   // expected aud of incoming tokens
	RequiredScopes   []string // optional
	ClientID         string   // news-mcp's own client at the issuer
	ClientSecret     string
	ExchangeAudience string // audience requested for Nextcloud tokens
	ExchangeScope    string // optional scope for the exchange
	HTTPClient       *http.Client
	Logger           *slog.Logger
}

// Client validates incoming access tokens and exchanges them for Nextcloud tokens.
type Client struct {
	cfg Config

	mu          sync.Mutex
	discovered  *oidcDiscovery
	lastDiscErr time.Time
	keys        map[string]any // kid -> public key
	keysAt      time.Time      // last successful JWKS fetch
	keysTried   time.Time      // last JWKS fetch attempt
	cache       map[[32]byte]cachedToken
}

type oidcDiscovery struct {
	Issuer        string `json:"issuer"`
	JWKSURI       string `json:"jwks_uri"`
	TokenEndpoint string `json:"token_endpoint"`
}

type cachedToken struct {
	token string
	exp   time.Time
}

// New returns a Client. Discovery and JWKS are fetched lazily.
func New(cfg Config) *Client {
	if cfg.HTTPClient == nil {
		cfg.HTTPClient = &http.Client{Timeout: 10 * time.Second}
	}
	return &Client{cfg: cfg, cache: map[[32]byte]cachedToken{}}
}

// ---------- discovery and JWKS ----------

func (o *Client) getJSON(ctx context.Context, u string, out any) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
	if err != nil {
		return err
	}
	req.Header.Set("Accept", "application/json")
	resp, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("GET %s: HTTP %d", u, resp.StatusCode)
	}
	return json.NewDecoder(io.LimitReader(resp.Body, idpBodyLimit)).Decode(out)
}

// discovery returns the issuer's OpenID Provider metadata, fetched once and
// retried at most every discoveryRetry on failure.
func (o *Client) discovery(ctx context.Context) (*oidcDiscovery, error) {
	o.mu.Lock()
	if d := o.discovered; d != nil {
		o.mu.Unlock()
		return d, nil
	}
	if time.Since(o.lastDiscErr) < discoveryRetry {
		o.mu.Unlock()
		return nil, errors.New("OIDC discovery unavailable (recent failure)")
	}
	o.mu.Unlock()

	var d oidcDiscovery
	err := o.getJSON(ctx, strings.TrimRight(o.cfg.Issuer, "/")+"/.well-known/openid-configuration", &d)
	if err == nil && d.Issuer != o.cfg.Issuer {
		err = fmt.Errorf("discovery document issuer %q does not match MCP_OIDC_ISSUER", d.Issuer)
	}
	if err == nil && (d.JWKSURI == "" || d.TokenEndpoint == "") {
		err = errors.New("discovery document lacks jwks_uri or token_endpoint")
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if err != nil {
		o.lastDiscErr = time.Now()
		o.cfg.Logger.Error("OIDC discovery failed", "issuer", o.cfg.Issuer, "error", err)
		return nil, err
	}
	o.discovered = &d
	return &d, nil
}

type jwk struct {
	Kty string `json:"kty"`
	Kid string `json:"kid"`
	Use string `json:"use"`
	Crv string `json:"crv"`
	N   string `json:"n"`
	E   string `json:"e"`
	X   string `json:"x"`
	Y   string `json:"y"`
}

func (k *jwk) publicKey() (any, error) {
	dec := b64.DecodeString
	switch k.Kty {
	case "RSA":
		n, err1 := dec(k.N)
		e, err2 := dec(k.E)
		if err1 != nil || err2 != nil || len(e) == 0 || len(e) > 4 {
			return nil, errors.New("bad RSA key")
		}
		return &rsa.PublicKey{N: new(big.Int).SetBytes(n), E: int(new(big.Int).SetBytes(e).Int64())}, nil
	case "EC":
		var curve elliptic.Curve
		switch k.Crv {
		case "P-256":
			curve = elliptic.P256()
		case "P-384":
			curve = elliptic.P384()
		case "P-521":
			curve = elliptic.P521()
		default:
			return nil, fmt.Errorf("unsupported curve %q", k.Crv)
		}
		x, err1 := dec(k.X)
		y, err2 := dec(k.Y)
		size := (curve.Params().BitSize + 7) / 8
		if err1 != nil || err2 != nil || len(x) > size || len(y) > size {
			return nil, errors.New("bad EC key")
		}
		pt := make([]byte, 1+2*size)
		pt[0] = 4
		copy(pt[1+size-len(x):1+size], x)
		copy(pt[1+2*size-len(y):], y)
		return ecdsa.ParseUncompressedPublicKey(curve, pt)
	case "OKP":
		x, err := dec(k.X)
		if k.Crv != "Ed25519" || err != nil || len(x) != ed25519.PublicKeySize {
			return nil, errors.New("bad OKP key")
		}
		return ed25519.PublicKey(x), nil
	}
	return nil, fmt.Errorf("unsupported key type %q", k.Kty)
}

func (o *Client) refreshKeys(ctx context.Context) error {
	d, err := o.discovery(ctx)
	if err != nil {
		return err
	}
	var set struct {
		Keys []jwk `json:"keys"`
	}
	if err := o.getJSON(ctx, d.JWKSURI, &set); err != nil {
		return err
	}
	keys := map[string]any{}
	for i := range set.Keys {
		k := &set.Keys[i]
		if k.Use != "" && k.Use != "sig" {
			continue
		}
		pk, err := k.publicKey()
		if err != nil {
			continue
		}
		keys[k.Kid] = pk
	}
	if len(keys) == 0 {
		return errors.New("JWKS contains no usable signing key")
	}
	o.mu.Lock()
	o.keys, o.keysAt = keys, time.Now()
	o.mu.Unlock()
	return nil
}

// key returns the verification key for kid, refreshing the JWKS when it is
// stale or when an unknown kid shows up (key rotation), rate-limited.
func (o *Client) key(ctx context.Context, kid string) (any, error) {
	lookup := func() (any, bool) {
		if k, ok := o.keys[kid]; ok {
			return k, true
		}
		if kid == "" && len(o.keys) == 1 {
			for _, k := range o.keys {
				return k, true
			}
		}
		return nil, false
	}
	o.mu.Lock()
	k, found := lookup()
	stale := time.Since(o.keysAt) > jwksMaxAge
	canTry := time.Since(o.keysTried) > jwksMinRefresh
	if (stale || !found) && canTry {
		o.keysTried = time.Now()
	} else {
		canTry = false
	}
	o.mu.Unlock()

	if canTry {
		if err := o.refreshKeys(ctx); err != nil {
			o.cfg.Logger.Warn("JWKS refresh failed", "error", err)
		}
		o.mu.Lock()
		k, found = lookup()
		o.mu.Unlock()
	}
	if !found {
		return nil, errNoKey
	}
	return k, nil
}

// ---------- incoming token validation ----------

// verify validates a JWT access token issued for this server.
func (o *Client) verify(ctx context.Context, raw string) (*auth.TokenInfo, error) {
	parser := jwt.NewParser(
		jwt.WithValidMethods(allowedJWTAlgs),
		jwt.WithIssuer(o.cfg.Issuer),
		jwt.WithAudience(o.cfg.Audience),
		jwt.WithExpirationRequired(),
		jwt.WithLeeway(jwtLeeway),
	)
	claims := jwt.MapClaims{}
	_, err := parser.ParseWithClaims(raw, claims, func(t *jwt.Token) (any, error) {
		kid, _ := t.Header["kid"].(string)
		return o.key(ctx, kid)
	})
	if err != nil {
		return nil, fmt.Errorf("%w: %w", auth.ErrInvalidToken, err)
	}
	exp, _ := claims.GetExpirationTime()
	sub, _ := claims.GetSubject()
	var scopes []string
	if s, ok := claims["scope"].(string); ok {
		scopes = strings.Fields(s)
	}
	return &auth.TokenInfo{
		Scopes:     scopes,
		Expiration: exp.Time,
		// Binds MCP sessions to the user (SDK session-hijacking protection).
		UserID: o.cfg.Issuer + "|" + sub,
	}, nil
}

type verifiedKey struct{}

type verified struct {
	raw  string
	info *auth.TokenInfo
}

// Middleware protects /mcp. It answers 401/403 with RFC 6750 challenges that
// point to the protected resource metadata, then hands over to the SDK's
// RequireBearerToken (which reuses the validation result) so that the SDK
// gets the TokenInfo it uses to bind sessions to users.
func (o *Client) Middleware(resourceMetadataURL string) func(http.Handler) http.Handler {
	challenge := func(w http.ResponseWriter, status int, errCode, desc string) {
		params := []string{fmt.Sprintf("resource_metadata=%q", resourceMetadataURL)}
		if len(o.cfg.RequiredScopes) > 0 {
			params = append(params, fmt.Sprintf("scope=%q", strings.Join(o.cfg.RequiredScopes, " ")))
		}
		if errCode != "" {
			params = append(params, fmt.Sprintf("error=%q", errCode), fmt.Sprintf("error_description=%q", desc))
		}
		w.Header().Set("WWW-Authenticate", "Bearer "+strings.Join(params, ", "))
		http.Error(w, desc, status)
	}
	sdk := auth.RequireBearerToken(func(ctx context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
		if v, ok := ctx.Value(verifiedKey{}).(*verified); ok && v.raw == tok {
			return v.info, nil
		}
		return o.verify(ctx, tok)
	}, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: resourceMetadataURL,
		Scopes:              o.cfg.RequiredScopes,
		ClockSkew:           jwtLeeway,
	})
	return func(next http.Handler) http.Handler {
		inner := sdk(next)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			raw := httpx.BearerFromHeader(r.Header)
			if raw == "" {
				challenge(w, http.StatusUnauthorized, "", "missing bearer token")
				return
			}
			info, err := o.verify(r.Context(), raw)
			if err != nil {
				o.cfg.Logger.Debug("rejected access token", "reason", err)
				challenge(w, http.StatusUnauthorized, "invalid_token", "the access token is invalid or was not issued for this server")
				return
			}
			for _, s := range o.cfg.RequiredScopes {
				if !slices.Contains(info.Scopes, s) {
					challenge(w, http.StatusForbidden, "insufficient_scope", "the access token lacks a required scope")
					return
				}
			}
			ctx := context.WithValue(r.Context(), verifiedKey{}, &verified{raw: raw, info: info})
			inner.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

// ---------- token exchange (RFC 8693) ----------

type exchangeError struct {
	Code string `json:"error"`
	Desc string `json:"error_description"`
}

// Exchange returns a Nextcloud-audience access token for the incoming
// subject token, from cache when possible. subjectExp bounds the cache
// lifetime so that an exchanged token never outlives the token it came from.
func (o *Client) Exchange(ctx context.Context, subject string, subjectExp time.Time) (string, error) {
	key := sha256.Sum256([]byte(subject))
	now := time.Now()
	o.mu.Lock()
	if c, ok := o.cache[key]; ok && now.Before(c.exp) {
		o.mu.Unlock()
		return c.token, nil
	}
	o.mu.Unlock()

	d, err := o.discovery(ctx)
	if err != nil {
		return "", fmt.Errorf("%w: %w", nextcloud.ErrTokenExchange, err)
	}
	form := url.Values{
		"grant_type":           {grantTokenExchange},
		"subject_token":        {subject},
		"subject_token_type":   {tokenTypeAccessToken},
		"requested_token_type": {tokenTypeAccessToken},
		"audience":             {o.cfg.ExchangeAudience},
	}
	if o.cfg.ExchangeScope != "" {
		form.Set("scope", o.cfg.ExchangeScope)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, d.TokenEndpoint, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Accept", "application/json")
	// client_secret_basic (RFC 6749 section 2.3.1): form-urlencode both parts.
	req.SetBasicAuth(url.QueryEscape(o.cfg.ClientID), url.QueryEscape(o.cfg.ClientSecret))
	resp, err := o.cfg.HTTPClient.Do(req)
	if err != nil {
		return "", fmt.Errorf("%w: token endpoint unreachable", nextcloud.ErrTokenExchange)
	}
	defer resp.Body.Close()
	body := io.LimitReader(resp.Body, idpBodyLimit)

	if resp.StatusCode != http.StatusOK {
		var e exchangeError
		_ = json.NewDecoder(body).Decode(&e)
		o.cfg.Logger.Warn("token exchange refused", "status", resp.StatusCode, "error", e.Code, "error_description", e.Desc)
		if e.Code == "" {
			e.Code = fmt.Sprintf("HTTP %d", resp.StatusCode)
		}
		return "", fmt.Errorf("%w: %s", nextcloud.ErrTokenExchange, e.Code)
	}
	var tr struct {
		AccessToken     string `json:"access_token"`
		TokenType       string `json:"token_type"`
		IssuedTokenType string `json:"issued_token_type"`
		ExpiresIn       int64  `json:"expires_in"`
	}
	if err := json.NewDecoder(body).Decode(&tr); err != nil || tr.AccessToken == "" {
		return "", fmt.Errorf("%w: malformed token response", nextcloud.ErrTokenExchange)
	}
	// RFC 8693 requires token_type; "N_A" means the result is not an access
	// token. Tolerate a missing value, which some providers omit.
	if tr.TokenType != "" && !strings.EqualFold(tr.TokenType, "Bearer") {
		return "", fmt.Errorf("%w: unexpected token_type %q", nextcloud.ErrTokenExchange, tr.TokenType)
	}
	if tr.IssuedTokenType != "" && tr.IssuedTokenType != tokenTypeAccessToken {
		return "", fmt.Errorf("%w: unexpected issued_token_type", nextcloud.ErrTokenExchange)
	}

	exp := subjectExp
	if tr.ExpiresIn > 0 {
		if e := now.Add(time.Duration(tr.ExpiresIn) * time.Second); exp.IsZero() || e.Before(exp) {
			exp = e
		}
	}
	if !exp.IsZero() {
		exp = exp.Add(-exchangeMargin)
	}
	if exp.After(now) {
		o.mu.Lock()
		o.storeLocked(key, cachedToken{token: tr.AccessToken, exp: exp}, now)
		o.mu.Unlock()
	}
	return tr.AccessToken, nil
}

// storeLocked inserts into the bounded cache: expired entries go first, then
// the entry closest to expiry.
func (o *Client) storeLocked(key [32]byte, v cachedToken, now time.Time) {
	if len(o.cache) >= exchangeCacheLimit {
		for k, c := range o.cache {
			if !now.Before(c.exp) {
				delete(o.cache, k)
			}
		}
	}
	for len(o.cache) >= exchangeCacheLimit {
		var victim [32]byte
		var soonest time.Time
		for k, c := range o.cache {
			if soonest.IsZero() || c.exp.Before(soonest) {
				victim, soonest = k, c.exp
			}
		}
		delete(o.cache, victim)
	}
	o.cache[key] = v
}

// ExchangeMiddleware gives each MCP request a lazy Nextcloud credential: the
// exchange only happens if a tool actually calls Nextcloud.
func (o *Client) ExchangeMiddleware() mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if extra := req.GetExtra(); extra != nil {
				if raw := httpx.BearerFromHeader(extra.Header); raw != "" {
					var exp time.Time
					if extra.TokenInfo != nil {
						exp = extra.TokenInfo.Expiration
					}
					ctx = nextcloud.WithTokenSource(ctx, func(ctx context.Context) (string, error) {
						return o.Exchange(ctx, raw, exp)
					})
				}
			}
			return next(ctx, method, req)
		}
	}
}
