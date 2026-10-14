// Command news-mcp is an MCP server (Streamable HTTP + OAuth) exposing a
// Nextcloud News account as tools.
package main

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"runtime/debug"
	"slices"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"
)

var version = "dev"

type config struct {
	AuthMode      string // authModeStandalone or authModeOIDC
	NextcloudURL  string
	NextcloudUser string
	AppPassword   string
	Port          int
	BaseURL       *url.URL
	AuthPassword  string
	ClientID      string
	ClientSecret  string
	RedirectURIs  []string
	AllowDCR      bool
	SigningKey    []byte
	LogLevel      slog.Level

	// OIDC mode.
	OIDCIssuer           string
	OIDCAudience         string
	OIDCRequiredScopes   []string
	OIDCClientID         string
	OIDCClientSecret     string
	OIDCExchangeAudience string
	OIDCExchangeScope    string
}

func env(k, def string) string {
	if v, ok := os.LookupEnv(k); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return def
}

func loadConfig() (*config, error) {
	c := &config{
		NextcloudURL:  strings.TrimRight(env("NEXTCLOUD_URL", ""), "/"),
		NextcloudUser: env("NEXTCLOUD_USER", ""),
		AppPassword:   os.Getenv("NEXTCLOUD_APP_PASSWORD"),
		AuthPassword:  os.Getenv("MCP_AUTH_PASSWORD"),
		ClientID:      env("MCP_OAUTH_CLIENT_ID", ""),
		ClientSecret:  os.Getenv("MCP_OAUTH_CLIENT_SECRET"),
	}
	switch c.AuthMode = strings.ToLower(env("MCP_AUTH_MODE", authModeStandalone)); c.AuthMode {
	case authModeStandalone, authModeOIDC:
	case "oauth": // former name of standalone
		c.AuthMode = authModeStandalone
	case "gateway", "passthrough":
		return nil, errors.New("MCP_AUTH_MODE=" + c.AuthMode + " was removed: it forwarded tokens not issued for this server; use MCP_AUTH_MODE=oidc")
	default:
		return nil, errors.New("MCP_AUTH_MODE must be standalone or oidc")
	}
	oidc := c.AuthMode == authModeOIDC
	if oidc {
		c.OIDCIssuer = env("MCP_OIDC_ISSUER", "")
		c.OIDCAudience = env("MCP_OIDC_AUDIENCE", "")
		c.OIDCRequiredScopes = strings.FieldsFunc(env("MCP_OIDC_REQUIRED_SCOPES", ""), func(r rune) bool { return r == ' ' || r == ',' })
		c.OIDCClientID = env("MCP_OIDC_CLIENT_ID", "")
		c.OIDCClientSecret = os.Getenv("MCP_OIDC_CLIENT_SECRET")
		c.OIDCExchangeAudience = env("MCP_OIDC_EXCHANGE_AUDIENCE", "")
		c.OIDCExchangeScope = env("MCP_OIDC_EXCHANGE_SCOPE", "")
	}

	required := map[string]string{"NEXTCLOUD_URL": c.NextcloudURL}
	if oidc {
		required["MCP_OIDC_ISSUER"] = c.OIDCIssuer
		required["MCP_OIDC_CLIENT_ID"] = c.OIDCClientID
		required["MCP_OIDC_CLIENT_SECRET"] = c.OIDCClientSecret
		required["MCP_OIDC_EXCHANGE_AUDIENCE"] = c.OIDCExchangeAudience
	} else {
		required["NEXTCLOUD_USER"] = c.NextcloudUser
		required["NEXTCLOUD_APP_PASSWORD"] = c.AppPassword
		required["MCP_AUTH_PASSWORD"] = c.AuthPassword
	}
	var missing []string
	for k, v := range required {
		if v == "" {
			missing = append(missing, k)
		}
	}
	slices.Sort(missing)
	if len(missing) > 0 {
		return nil, fmt.Errorf("missing required environment variables: %s", strings.Join(missing, ", "))
	}
	if u, err := url.Parse(c.NextcloudURL); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		return nil, errors.New("NEXTCLOUD_URL must be an absolute http(s) URL")
	}
	if !oidc && len(c.AuthPassword) < 12 {
		return nil, errors.New("MCP_AUTH_PASSWORD must be at least 12 characters")
	}

	port, err := strconv.Atoi(env("MCP_PORT", "8080"))
	if err != nil || port <= 0 || port > 65535 {
		return nil, errors.New("MCP_PORT must be a valid port number")
	}
	c.Port = port

	base := strings.TrimRight(env("MCP_BASE_URL", fmt.Sprintf("http://localhost:%d", port)), "/")
	c.BaseURL, err = url.Parse(base)
	if err != nil || (c.BaseURL.Scheme != "https" && c.BaseURL.Scheme != "http") || c.BaseURL.Host == "" ||
		c.BaseURL.RawQuery != "" || c.BaseURL.Fragment != "" {
		return nil, errors.New("MCP_BASE_URL must be an absolute http(s) URL without query or fragment")
	}
	if oidc {
		u, err := url.Parse(c.OIDCIssuer)
		if err != nil || u.Host == "" || u.RawQuery != "" || u.Fragment != "" ||
			(u.Scheme != "https" && !(u.Scheme == "http" && isLoopbackHost(u.Hostname()))) {
			return nil, errors.New("MCP_OIDC_ISSUER must be an https URL (http is only allowed on loopback)")
		}
		if c.OIDCAudience == "" {
			// Canonical resource URI, as RFC 8707 audience binding expects.
			c.OIDCAudience = base + "/mcp"
		}
		return c, c.parseLogLevel()
	}

	for _, u := range strings.Split(env("MCP_OAUTH_REDIRECT_URIS", ""), ",") {
		if u = strings.TrimSpace(u); u != "" {
			c.RedirectURIs = append(c.RedirectURIs, u)
		}
	}
	c.AllowDCR, err = strconv.ParseBool(env("MCP_OAUTH_DCR", "true"))
	if err != nil {
		return nil, errors.New("MCP_OAUTH_DCR must be true or false")
	}
	if !c.AllowDCR && c.ClientID == "" {
		return nil, errors.New("MCP_OAUTH_DCR=false requires MCP_OAUTH_CLIENT_ID")
	}

	if k := os.Getenv("MCP_OAUTH_SIGNING_KEY"); k != "" {
		if len(k) < 32 {
			return nil, errors.New("MCP_OAUTH_SIGNING_KEY must be at least 32 characters")
		}
		c.SigningKey = []byte(k)
	} else {
		// Deterministic key derived from the server secrets: tokens survive
		// restarts, and changing any secret revokes all issued tokens.
		m := hmac.New(sha256.New, []byte("news-mcp signing key v1"))
		for _, s := range []string{c.AppPassword, c.AuthPassword, c.ClientSecret, base} {
			m.Write([]byte(s))
			m.Write([]byte{0})
		}
		c.SigningKey = m.Sum(nil)
	}

	return c, c.parseLogLevel()
}

func (c *config) parseLogLevel() error {
	switch strings.ToLower(env("LOG_LEVEL", "info")) {
	case "debug":
		c.LogLevel = slog.LevelDebug
	case "info":
		c.LogLevel = slog.LevelInfo
	case "warn", "warning":
		c.LogLevel = slog.LevelWarn
	case "error":
		c.LogLevel = slog.LevelError
	default:
		return errors.New("LOG_LEVEL must be debug, info, warn or error")
	}
	return nil
}

func isLoopbackHost(h string) bool {
	if h == "localhost" {
		return true
	}
	ip := net.ParseIP(h)
	return ip != nil && ip.IsLoopback()
}

// accessLog logs requests without any header (and thus without tokens).
func accessLog(log *slog.Logger, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(sw, r)
		log.Debug("http", "method", r.Method, "path", r.URL.Path, "status", sw.status, "duration", time.Since(start))
	})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (s *statusWriter) WriteHeader(code int) { s.status = code; s.ResponseWriter.WriteHeader(code) }
func (s *statusWriter) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}
func (s *statusWriter) Unwrap() http.ResponseWriter { return s.ResponseWriter }

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "news-mcp:", err)
		os.Exit(1)
	}
}

const (
	authModeStandalone = "standalone"
	authModeOIDC       = "oidc"
)

// buildHandler wires the MCP server, the authentication layer selected by
// cfg.AuthMode and the HTTP routes.
func buildHandler(cfg *config, log *slog.Logger, nc *NewsClient, idp *OIDC) http.Handler {
	issuer := cfg.BaseURL.String()
	basePath := strings.TrimRight(cfg.BaseURL.Path, "/")
	resource := issuer + "/mcp"
	prmURL := cfg.BaseURL.Scheme + "://" + cfg.BaseURL.Host + "/.well-known/oauth-protected-resource" + basePath + "/mcp"

	server := mcp.NewServer(&mcp.Implementation{Name: "nextcloud-news", Title: "Nextcloud News", Version: version}, &mcp.ServerOptions{
		Instructions: "Tools to browse Nextcloud News articles and manage their read/unread and starred state. " +
			"Start with news_list_items (unread only by default) or news_list_feeds; use news_get_item for full text.",
		Logger: log,
	})
	if idp != nil {
		server.AddReceivingMiddleware(idp.ExchangeMiddleware())
	}
	registerTools(server, nc)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: 30 * time.Minute,
		// Every /mcp request must carry a valid access token, which a DNS
		// rebinding attacker cannot obtain. Behind a reverse proxy or gateway
		// the Host header is the public name, which the SDK's localhost check
		// would reject.
		DisableLocalhostProtection: idp != nil || !isLoopbackHost(cfg.BaseURL.Hostname()),
	})

	mux := http.NewServeMux()
	mux.Handle("GET /healthz", http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("ok\n"))
	}))

	prmDoc := &oauthex.ProtectedResourceMetadata{
		Resource:               resource,
		BearerMethodsSupported: []string{"header"},
		ResourceName:           "Nextcloud News MCP",
	}
	// RFC 9728 section 3: well-known suffix inserted before the resource
	// path. The root document is the fallback MCP clients try when
	// WWW-Authenticate carries no resource_metadata.
	registerPRM := func() {
		prm := auth.ProtectedResourceMetadataHandler(prmDoc)
		mux.Handle("/.well-known/oauth-protected-resource"+basePath+"/mcp", prm)
		mux.Handle("/.well-known/oauth-protected-resource", prm)
	}

	if idp != nil {
		prmDoc.AuthorizationServers = []string{cfg.OIDCIssuer}
		prmDoc.ScopesSupported = cfg.OIDCRequiredScopes
		registerPRM()
		mux.Handle(basePath+"/mcp", idp.Middleware(prmURL)(mcpHandler))
		return accessLog(log, mux)
	}

	as := NewAuthServer(OAuthConfig{
		Issuer:             issuer,
		Resource:           resource,
		LoginPassword:      cfg.AuthPassword,
		StaticClientID:     cfg.ClientID,
		StaticClientSecret: cfg.ClientSecret,
		StaticRedirectURIs: cfg.RedirectURIs,
		AllowDCR:           cfg.AllowDCR,
		SigningKey:         cfg.SigningKey,
		Logger:             log,
	})
	prmDoc.AuthorizationServers = []string{issuer}
	prmDoc.ScopesSupported = []string{scopeNews}
	registerPRM()
	bearer := auth.RequireBearerToken(as.VerifyAccessToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: prmURL,
		Scopes:              []string{scopeNews},
		ClockSkew:           30 * time.Second,
	})
	// RFC 8414 section 3: well-known suffix inserted before the issuer path.
	mux.Handle("/.well-known/oauth-authorization-server"+basePath, http.HandlerFunc(as.MetadataHandler))
	mux.Handle(basePath+"/authorize", http.HandlerFunc(as.AuthorizeHandler))
	mux.Handle(basePath+"/token", http.HandlerFunc(as.TokenHandler))
	if cfg.AllowDCR {
		mux.Handle(basePath+"/register", http.HandlerFunc(as.RegisterHandler))
	}
	mux.Handle(basePath+"/mcp", bearer(mcpHandler))
	return accessLog(log, mux)
}

func run() error {
	// Keep the heap small unless the operator decided otherwise.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(32 << 20)
	}

	cfg, err := loadConfig()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	user, pass := cfg.NextcloudUser, cfg.AppPassword
	var idp *OIDC
	if cfg.AuthMode == authModeOIDC {
		// Never fall back to a shared identity: every Nextcloud call uses a
		// token exchanged for the calling user.
		if user != "" || pass != "" {
			log.Warn("oidc mode: NEXTCLOUD_USER / NEXTCLOUD_APP_PASSWORD are ignored")
		}
		user, pass = "", ""
		idp = NewOIDC(OIDCConfig{
			Issuer:           cfg.OIDCIssuer,
			Audience:         cfg.OIDCAudience,
			RequiredScopes:   cfg.OIDCRequiredScopes,
			ClientID:         cfg.OIDCClientID,
			ClientSecret:     cfg.OIDCClientSecret,
			ExchangeAudience: cfg.OIDCExchangeAudience,
			ExchangeScope:    cfg.OIDCExchangeScope,
			Logger:           log,
		})
	}
	nc := NewNewsClient(cfg.NextcloudURL, user, pass, &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			Proxy:               http.ProxyFromEnvironment,
			MaxIdleConns:        4,
			MaxIdleConnsPerHost: 4,
			IdleConnTimeout:     90 * time.Second,
			TLSHandshakeTimeout: 10 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	})

	srv := &http.Server{
		Addr:              fmt.Sprintf(":%d", cfg.Port),
		Handler:           buildHandler(cfg, log, nc, idp),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	attrs := []any{"addr", srv.Addr, "auth_mode", cfg.AuthMode, "mcp_endpoint", cfg.BaseURL.String() + "/mcp", "nextcloud", cfg.NextcloudURL}
	if idp != nil {
		attrs = append(attrs, "oidc_issuer", cfg.OIDCIssuer, "oidc_audience", cfg.OIDCAudience,
			"exchange_audience", cfg.OIDCExchangeAudience, "oidc_client_id", cfg.OIDCClientID)
	} else {
		attrs = append(attrs, "dcr", cfg.AllowDCR, "static_client", cfg.ClientID != "")
	}
	log.Info("news-mcp listening", attrs...)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(sctx)
}
