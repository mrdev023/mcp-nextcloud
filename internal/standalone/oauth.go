// Package standalone is a minimal embedded OAuth 2.1 authorization server
// for the MCP authorization spec (revision 2026-07-28):
//   - RFC 9728 protected resource metadata (served via the go-sdk helper)
//   - RFC 8414 authorization server metadata
//   - Authorization Code + PKCE (S256 mandatory) and refresh tokens
//   - RFC 8707 resource indicators: tokens are bound to the /mcp audience
//   - RFC 9207 iss parameter in authorization responses
//   - Client registration: a pre-registered client and/or RFC 7591 dynamic
//     registration. DCR is deprecated by the spec in favour of Client ID
//     Metadata Documents, which are not implemented (yet); it is kept
//     because it is what current MCP clients use.
//
// All artefacts (client ids issued by DCR, authorization codes, access and
// refresh tokens) are self-contained HMAC-SHA256 signed blobs, so the server
// keeps almost no state (only a small set of already-redeemed codes) and
// tokens survive restarts as long as the signing key is stable.
//
// The resource owner authenticates on the /authorize page with
// MCP_AUTH_PASSWORD. The Nextcloud app-password is never involved.
package standalone

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"html/template"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
)

const (
	// Scope is the only scope issued and required in standalone mode.
	Scope           = "news"
	accessTokenTTL  = time.Hour
	refreshTokenTTL = 30 * 24 * time.Hour
	authCodeTTL     = 5 * time.Minute
	ownerUserID     = "owner"

	maxLoginFailures   = 10
	loginFailureWindow = 15 * time.Minute
)

// Config configures the embedded authorization server.
type Config struct {
	Issuer             string // public base URL, no trailing slash
	Resource           string // canonical MCP endpoint URL
	LoginPassword      string // MCP_AUTH_PASSWORD
	StaticClientID     string // optional pre-registered client
	StaticClientSecret string // optional; empty = public client
	StaticRedirectURIs []string
	AllowDCR           bool
	SigningKey         []byte
	Logger             *slog.Logger
}

// Server is the embedded authorization server. Its handlers serve RFC 8414
// metadata, /authorize, /token and, optionally, /register.
type Server struct {
	cfg Config

	mu        sync.Mutex
	usedCodes map[string]time.Time // jti -> expiry
	failures  []time.Time
}

// New returns a Server for cfg.
func New(cfg Config) *Server {
	return &Server{cfg: cfg, usedCodes: map[string]time.Time{}}
}

// ---------- signed tokens ----------

type claims struct {
	Typ   string   `json:"t"`
	Cid   string   `json:"c,omitempty"`
	Aud   string   `json:"a,omitempty"`
	Scope string   `json:"s,omitempty"`
	Exp   int64    `json:"e,omitempty"`
	Iat   int64    `json:"i,omitempty"`
	Jti   string   `json:"j,omitempty"`
	Redir string   `json:"r,omitempty"` // code: redirect_uri
	Chal  string   `json:"p,omitempty"` // code: PKCE challenge
	URIs  []string `json:"u,omitempty"` // client: registered redirect URIs
	Name  string   `json:"n,omitempty"` // client: client_name
}

const (
	typAccess  = "at"
	typRefresh = "rt"
	typCode    = "ac"
	typClient  = "cl"
)

var b64 = base64.RawURLEncoding

func randomID() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return b64.EncodeToString(b)
}

func (a *Server) mac(payload string) []byte {
	m := hmac.New(sha256.New, a.cfg.SigningKey)
	m.Write([]byte("news-mcp.v1."))
	m.Write([]byte(payload))
	return m.Sum(nil)
}

func (a *Server) sign(c claims) string {
	if c.Iat == 0 {
		c.Iat = time.Now().Unix()
	}
	if c.Jti == "" {
		c.Jti = randomID()
	}
	j, _ := json.Marshal(c)
	p := b64.EncodeToString(j)
	return p + "." + b64.EncodeToString(a.mac(p))
}

var errBadToken = errors.New("invalid or expired token")

func (a *Server) parse(tok, typ string) (*claims, error) {
	p, s, ok := strings.Cut(tok, ".")
	if !ok || len(tok) > 8192 {
		return nil, errBadToken
	}
	sig, err := b64.DecodeString(s)
	if err != nil || !hmac.Equal(sig, a.mac(p)) {
		return nil, errBadToken
	}
	j, err := b64.DecodeString(p)
	if err != nil {
		return nil, errBadToken
	}
	var c claims
	if err := json.Unmarshal(j, &c); err != nil || c.Typ != typ {
		return nil, errBadToken
	}
	if c.Exp != 0 && time.Now().Unix() >= c.Exp {
		return nil, errBadToken
	}
	return &c, nil
}

// VerifyAccessToken is the auth.TokenVerifier used on the MCP endpoint.
func (a *Server) VerifyAccessToken(_ context.Context, tok string, _ *http.Request) (*auth.TokenInfo, error) {
	c, err := a.parse(tok, typAccess)
	if err != nil || c.Aud != a.cfg.Resource {
		return nil, auth.ErrInvalidToken
	}
	return &auth.TokenInfo{
		Scopes:     strings.Fields(c.Scope),
		Expiration: time.Unix(c.Exp, 0),
		UserID:     ownerUserID,
	}, nil
}

// ---------- clients ----------

type client struct {
	ID           string
	Name         string
	RedirectURIs []string
	Secret       string // non-empty => confidential client
	// loopbackAnyPort allows any http://localhost|127.0.0.1|[::1] redirect.
	loopbackAnyPort bool
}

// Redirect URIs accepted for the static client when MCP_OAUTH_REDIRECT_URIS
// is not set (in addition to any loopback URI).
var defaultStaticRedirects = []string{
	"https://claude.ai/api/mcp/auth_callback",
	"https://claude.com/api/mcp/auth_callback",
}

func (a *Server) lookupClient(id string) (*client, bool) {
	if id == "" {
		return nil, false
	}
	if a.cfg.StaticClientID != "" && hmac.Equal([]byte(id), []byte(a.cfg.StaticClientID)) {
		c := &client{ID: id, Name: "pre-registered client", Secret: a.cfg.StaticClientSecret}
		if len(a.cfg.StaticRedirectURIs) > 0 {
			c.RedirectURIs = a.cfg.StaticRedirectURIs
		} else {
			c.RedirectURIs = defaultStaticRedirects
			c.loopbackAnyPort = true
		}
		return c, true
	}
	if !a.cfg.AllowDCR {
		return nil, false
	}
	cl, err := a.parse(id, typClient)
	if err != nil {
		return nil, false
	}
	return &client{ID: id, Name: cl.Name, RedirectURIs: cl.URIs}, true
}

func isLoopbackHTTP(u *url.URL) bool {
	if u.Scheme != "http" {
		return false
	}
	h := u.Hostname()
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

func (c *client) redirectAllowed(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" {
		return false
	}
	if c.loopbackAnyPort && isLoopbackHTTP(u) {
		return true
	}
	for _, r := range c.RedirectURIs {
		if r == raw {
			return true
		}
		// RFC 8252 §7.3: loopback redirect ports may vary.
		ru, err := url.Parse(r)
		if err == nil && isLoopbackHTTP(ru) && isLoopbackHTTP(u) &&
			ru.Hostname() == u.Hostname() && ru.Path == u.Path && ru.RawQuery == u.RawQuery {
			return true
		}
	}
	return false
}

func validRegistrationRedirect(raw string) bool {
	u, err := url.Parse(raw)
	if err != nil || u.Fragment != "" || u.Scheme == "" {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "https":
		return u.Host != ""
	case "http":
		return isLoopbackHTTP(u)
	case "javascript", "data", "file", "vbscript", "blob":
		return false
	}
	return true // private-use scheme for native apps (cursor://, vscode://…)
}

// ---------- helpers ----------

func setCORS(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Access-Control-Allow-Origin", "*")
	h.Set("Access-Control-Allow-Methods", "GET, POST, OPTIONS")
	h.Set("Access-Control-Allow-Headers", "Authorization, Content-Type, MCP-Protocol-Version")
}

func writeJSONResp(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Pragma", "no-cache")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthErr(w http.ResponseWriter, status int, code, desc string) {
	if status == http.StatusUnauthorized {
		w.Header().Set("WWW-Authenticate", `Basic realm="news-mcp"`)
	}
	writeJSONResp(w, status, map[string]string{"error": code, "error_description": desc})
}

func sameSecret(a, b string) bool {
	ha, hb := sha256.Sum256([]byte(a)), sha256.Sum256([]byte(b))
	return hmac.Equal(ha[:], hb[:])
}

// sameResource compares two resource identifiers (RFC 8707) with the
// tolerance the MCP spec asks of servers: case-insensitive scheme and host,
// optional trailing slash.
func sameResource(a, b string) bool {
	ua, err1 := url.Parse(a)
	ub, err2 := url.Parse(b)
	if err1 != nil || err2 != nil || ua.Fragment != "" || ub.Fragment != "" {
		return false
	}
	return strings.EqualFold(ua.Scheme, ub.Scheme) && strings.EqualFold(ua.Host, ub.Host) &&
		strings.TrimRight(ua.Path, "/") == strings.TrimRight(ub.Path, "/") && ua.RawQuery == ub.RawQuery
}

// ---------- metadata ----------

type asMetadata struct {
	Issuer                            string   `json:"issuer"`
	AuthorizationEndpoint             string   `json:"authorization_endpoint"`
	TokenEndpoint                     string   `json:"token_endpoint"`
	RegistrationEndpoint              string   `json:"registration_endpoint,omitempty"`
	ScopesSupported                   []string `json:"scopes_supported"`
	ResponseTypesSupported            []string `json:"response_types_supported"`
	ResponseModesSupported            []string `json:"response_modes_supported"`
	GrantTypesSupported               []string `json:"grant_types_supported"`
	TokenEndpointAuthMethodsSupported []string `json:"token_endpoint_auth_methods_supported"`
	CodeChallengeMethodsSupported     []string `json:"code_challenge_methods_supported"`
	IssParameterSupported             bool     `json:"authorization_response_iss_parameter_supported"`
}

func (a *Server) MetadataHandler(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	m := asMetadata{
		Issuer:                            a.cfg.Issuer,
		AuthorizationEndpoint:             a.cfg.Issuer + "/authorize",
		TokenEndpoint:                     a.cfg.Issuer + "/token",
		ScopesSupported:                   []string{Scope},
		ResponseTypesSupported:            []string{"code"},
		ResponseModesSupported:            []string{"query"},
		GrantTypesSupported:               []string{"authorization_code", "refresh_token"},
		TokenEndpointAuthMethodsSupported: []string{"none", "client_secret_basic", "client_secret_post"},
		CodeChallengeMethodsSupported:     []string{"S256"},
		IssParameterSupported:             true,
	}
	if a.cfg.AllowDCR {
		m.RegistrationEndpoint = a.cfg.Issuer + "/register"
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(m)
}

// ---------- dynamic client registration (RFC 7591) ----------

func (a *Server) RegisterHandler(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var req struct {
		RedirectURIs            []string `json:"redirect_uris"`
		ClientName              string   `json:"client_name"`
		TokenEndpointAuthMethod string   `json:"token_endpoint_auth_method"`
		GrantTypes              []string `json:"grant_types"`
		ResponseTypes           []string `json:"response_types"`
		Scope                   string   `json:"scope"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 16<<10)).Decode(&req); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "malformed JSON body")
		return
	}
	if len(req.RedirectURIs) == 0 || len(req.RedirectURIs) > 10 {
		oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "between 1 and 10 redirect_uris are required")
		return
	}
	for _, u := range req.RedirectURIs {
		if len(u) > 512 || !validRegistrationRedirect(u) {
			oauthErr(w, http.StatusBadRequest, "invalid_redirect_uri", "redirect_uri must be https, http on loopback, or a private-use scheme: "+u)
			return
		}
	}
	for _, g := range req.GrantTypes {
		if g != "authorization_code" && g != "refresh_token" {
			oauthErr(w, http.StatusBadRequest, "invalid_client_metadata", "unsupported grant_type "+g)
			return
		}
	}
	if m := req.TokenEndpointAuthMethod; m != "" && m != "none" {
		// We only issue public clients via DCR; the client must use PKCE.
		req.TokenEndpointAuthMethod = "none"
	}
	name := req.ClientName
	if len(name) > 100 {
		name = name[:100]
	}
	now := time.Now()
	id := a.sign(claims{Typ: typClient, URIs: req.RedirectURIs, Name: name, Iat: now.Unix()})
	a.cfg.Logger.Info("oauth client registered", "client_name", name)
	writeJSONResp(w, http.StatusCreated, map[string]any{
		"client_id":                  id,
		"client_id_issued_at":        now.Unix(),
		"client_name":                name,
		"redirect_uris":              req.RedirectURIs,
		"token_endpoint_auth_method": "none",
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"scope":                      Scope,
	})
}

// ---------- authorization endpoint ----------

var authorizeTmpl = template.Must(template.New("authorize").Parse(`<!doctype html>
<html lang="en"><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>Authorize – Nextcloud News MCP</title>
<style>
body{font-family:system-ui,sans-serif;background:#f4f5f7;color:#222;display:flex;justify-content:center;padding:4rem 1rem;margin:0}
main{background:#fff;max-width:26rem;width:100%;padding:2rem;border-radius:10px;box-shadow:0 2px 10px #0002}
h1{font-size:1.25rem;margin-top:0}code{background:#eee;padding:0 .25rem;border-radius:3px;word-break:break-all}
input[type=password]{width:100%;box-sizing:border-box;padding:.6rem;margin:.75rem 0;font-size:1rem}
button{padding:.6rem 1.2rem;font-size:1rem;border:0;border-radius:6px;background:#0082c9;color:#fff;cursor:pointer}
.err{color:#b00020}.warn{color:#8a5a00;font-size:.9rem}.muted{color:#666;font-size:.9rem}
@media(prefers-color-scheme:dark){body{background:#181a1b;color:#ddd}main{background:#24272a}code{background:#333}.muted{color:#aaa}}
</style></head><body><main>
<h1>Authorize access to Nextcloud News</h1>
<p><strong>{{.ClientName}}</strong> wants to read your Nextcloud News articles and change their read / starred state.</p>
<p class="muted">After approval you will be redirected to:<br><code>{{.RedirectURI}}</code></p>
{{if .Loopback}}<p class="warn">This client uses a local (loopback) redirect: any program on that machine listening on this port could receive the authorization. Only approve if you just started this sign-in from a client you trust.</p>{{end}}
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<form method="post" action="{{.Action}}">
{{range $k, $v := .Params}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<label for="pw">Server password</label>
<input id="pw" type="password" name="password" autocomplete="current-password" autofocus required>
<button type="submit">Authorize</button>
</form></main></body></html>`))

type authzRequest struct {
	client    *client
	redirect  string
	state     string
	challenge string
	scope     string
	resource  string
}

// validateAuthz checks an authorization request. If the client or the
// redirect URI are invalid, it returns fatal=true: the error must be shown to
// the user and NOT redirected (RFC 6749 §4.1.2.1).
func (a *Server) validateAuthz(q url.Values) (req *authzRequest, errCode, errDesc string, fatal bool) {
	c, ok := a.lookupClient(q.Get("client_id"))
	if !ok {
		return nil, "invalid_client", "unknown client_id", true
	}
	redirect := q.Get("redirect_uri")
	if redirect == "" && len(c.RedirectURIs) == 1 && !c.loopbackAnyPort {
		redirect = c.RedirectURIs[0]
	}
	if redirect == "" || !c.redirectAllowed(redirect) {
		return nil, "invalid_request", "redirect_uri is not registered for this client", true
	}
	req = &authzRequest{client: c, redirect: redirect, state: q.Get("state")}
	if q.Get("response_type") != "code" {
		return req, "unsupported_response_type", "only response_type=code is supported", false
	}
	req.challenge = q.Get("code_challenge")
	if req.challenge == "" || q.Get("code_challenge_method") != "S256" {
		return req, "invalid_request", "PKCE with code_challenge_method=S256 is required", false
	}
	if l := len(req.challenge); l < 43 || l > 128 {
		return req, "invalid_request", "invalid code_challenge", false
	}
	if res := q.Get("resource"); res != "" {
		if !sameResource(res, a.cfg.Resource) && !sameResource(res, a.cfg.Issuer) {
			return req, "invalid_target", "unknown resource", false
		}
	}
	req.resource = a.cfg.Resource
	req.scope = Scope
	if s := q.Get("scope"); s != "" && !slices.Contains(strings.Fields(s), Scope) {
		return req, "invalid_scope", "the only supported scope is " + Scope, false
	}
	return req, "", "", false
}

func (a *Server) redirectWith(w http.ResponseWriter, r *http.Request, req *authzRequest, params url.Values) {
	u, _ := url.Parse(req.redirect)
	q := u.Query()
	for k, v := range params {
		q[k] = v
	}
	if req.state != "" {
		q.Set("state", req.state)
	}
	q.Set("iss", a.cfg.Issuer)
	u.RawQuery = q.Encode()
	http.Redirect(w, r, u.String(), http.StatusFound)
}

func (a *Server) AuthorizeHandler(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Frame-Options", "DENY")
	h.Set("Content-Security-Policy", "default-src 'none'; style-src 'unsafe-inline'; frame-ancestors 'none'")
	h.Set("Cache-Control", "no-store")
	h.Set("Referrer-Policy", "no-referrer")

	if r.Method != http.MethodGet && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	params := r.Form
	if r.Method == http.MethodPost {
		params = r.PostForm
	}

	req, code, desc, fatal := a.validateAuthz(params)
	if fatal {
		http.Error(w, "OAuth error: "+code+": "+desc, http.StatusBadRequest)
		return
	}
	if code != "" {
		a.redirectWith(w, r, req, url.Values{"error": {code}, "error_description": {desc}})
		return
	}

	var formErr string
	if r.Method == http.MethodPost {
		if !a.allowLoginAttempt() {
			formErr = "Too many failed attempts. Try again later."
		} else if sameSecret(params.Get("password"), a.cfg.LoginPassword) {
			a.issueCode(w, r, req)
			return
		} else {
			a.recordLoginFailure()
			a.cfg.Logger.Warn("oauth authorize: wrong password", "remote", r.RemoteAddr)
			time.Sleep(500 * time.Millisecond)
			formErr = "Wrong password."
		}
	}

	hidden := map[string]string{}
	for _, k := range []string{"client_id", "redirect_uri", "response_type", "state", "code_challenge", "code_challenge_method", "scope", "resource"} {
		if v := params.Get(k); v != "" {
			hidden[k] = v
		}
	}
	ru, _ := url.Parse(req.redirect)
	name := req.client.Name
	if name == "" {
		name = "An MCP client"
	}
	h.Set("Content-Type", "text/html; charset=utf-8")
	if formErr != "" {
		w.WriteHeader(http.StatusUnauthorized)
	}
	_ = authorizeTmpl.Execute(w, map[string]any{
		"ClientName":  name,
		"RedirectURI": req.redirect,
		"Loopback":    isLoopbackHTTP(ru),
		"Error":       formErr,
		"Params":      hidden,
		"Action":      a.cfg.Issuer + "/authorize",
	})
}

func (a *Server) allowLoginAttempt() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	cutoff := time.Now().Add(-loginFailureWindow)
	a.failures = slices.DeleteFunc(a.failures, func(t time.Time) bool { return t.Before(cutoff) })
	return len(a.failures) < maxLoginFailures
}

func (a *Server) recordLoginFailure() {
	a.mu.Lock()
	a.failures = append(a.failures, time.Now())
	a.mu.Unlock()
}

func (a *Server) issueCode(w http.ResponseWriter, r *http.Request, req *authzRequest) {
	code := a.sign(claims{
		Typ:   typCode,
		Cid:   req.client.ID,
		Aud:   req.resource,
		Scope: req.scope,
		Redir: req.redirect,
		Chal:  req.challenge,
		Exp:   time.Now().Add(authCodeTTL).Unix(),
	})
	a.cfg.Logger.Info("oauth authorization granted", "client_name", req.client.Name)
	a.redirectWith(w, r, req, url.Values{"code": {code}})
}

// redeemCode marks a code as used; it returns false if it was already used.
func (a *Server) redeemCode(jti string, exp int64) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	now := time.Now()
	for k, e := range a.usedCodes {
		if now.After(e) {
			delete(a.usedCodes, k)
		}
	}
	if _, used := a.usedCodes[jti]; used {
		return false
	}
	a.usedCodes[jti] = time.Unix(exp, 0).Add(time.Minute)
	return true
}

// ---------- token endpoint ----------

func (a *Server) TokenHandler(w http.ResponseWriter, r *http.Request) {
	setCORS(w)
	if r.Method == http.MethodOptions {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, 32<<10)
	if err := r.ParseForm(); err != nil {
		oauthErr(w, http.StatusBadRequest, "invalid_request", "malformed form body")
		return
	}
	f := r.PostForm

	// Client authentication.
	clientID, secret, hasBasic := r.BasicAuth()
	if hasBasic {
		if id, err := url.QueryUnescape(clientID); err == nil {
			clientID = id
		}
		if s, err := url.QueryUnescape(secret); err == nil {
			secret = s
		}
	} else {
		clientID, secret = f.Get("client_id"), f.Get("client_secret")
	}
	c, ok := a.lookupClient(clientID)
	if !ok {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "unknown client")
		return
	}
	if c.Secret != "" && !sameSecret(secret, c.Secret) {
		oauthErr(w, http.StatusUnauthorized, "invalid_client", "client authentication failed")
		return
	}

	switch f.Get("grant_type") {
	case "authorization_code":
		cl, err := a.parse(f.Get("code"), typCode)
		if err != nil || cl.Cid != c.ID {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "invalid or expired authorization code")
			return
		}
		if ru := f.Get("redirect_uri"); ru != "" && ru != cl.Redir {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "redirect_uri mismatch")
			return
		}
		v := f.Get("code_verifier")
		sum := sha256.Sum256([]byte(v))
		if len(v) < 43 || len(v) > 128 || b64.EncodeToString(sum[:]) != cl.Chal {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "PKCE verification failed")
			return
		}
		if res := f.Get("resource"); res != "" && !sameResource(res, cl.Aud) && !sameResource(res, a.cfg.Issuer) {
			oauthErr(w, http.StatusBadRequest, "invalid_target", "resource mismatch")
			return
		}
		if !a.redeemCode(cl.Jti, cl.Exp) {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "authorization code already used")
			return
		}
		a.issueTokens(w, c.ID, cl.Scope)

	case "refresh_token":
		cl, err := a.parse(f.Get("refresh_token"), typRefresh)
		if err != nil || cl.Cid != c.ID || cl.Aud != a.cfg.Resource {
			oauthErr(w, http.StatusBadRequest, "invalid_grant", "invalid or expired refresh token")
			return
		}
		a.issueTokens(w, c.ID, cl.Scope)

	default:
		oauthErr(w, http.StatusBadRequest, "unsupported_grant_type", "supported: authorization_code, refresh_token")
	}
}

func (a *Server) issueTokens(w http.ResponseWriter, clientID, scope string) {
	now := time.Now()
	at := a.sign(claims{Typ: typAccess, Cid: clientID, Aud: a.cfg.Resource, Scope: scope, Exp: now.Add(accessTokenTTL).Unix()})
	rt := a.sign(claims{Typ: typRefresh, Cid: clientID, Aud: a.cfg.Resource, Scope: scope, Exp: now.Add(refreshTokenTTL).Unix()})
	writeJSONResp(w, http.StatusOK, map[string]any{
		"access_token":  at,
		"token_type":    "Bearer",
		"expires_in":    int(accessTokenTTL.Seconds()),
		"refresh_token": rt,
		"scope":         scope,
	})
}
