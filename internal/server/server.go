// Package server wires the MCP server, the authentication layer and the HTTP
// routes of news-mcp.
package server

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/modelcontextprotocol/go-sdk/oauthex"

	"github.com/mrdev023/mcp-nextcloud-news/internal/config"
	"github.com/mrdev023/mcp-nextcloud-news/internal/httpx"
	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud-news/internal/oidc"
	"github.com/mrdev023/mcp-nextcloud-news/internal/standalone"
	"github.com/mrdev023/mcp-nextcloud-news/internal/tools"
)

// NewHandler wires the MCP server, the authentication layer selected by
// cfg.AuthMode and the HTTP routes.
func NewHandler(cfg *config.Config, version string, log *slog.Logger, nc *nextcloud.Client, idp *oidc.Client) http.Handler {
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
	tools.Register(server, nc)

	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return server }, &mcp.StreamableHTTPOptions{
		Logger:         log,
		SessionTimeout: 30 * time.Minute,
		// Every /mcp request must carry a valid access token, which a DNS
		// rebinding attacker cannot obtain. Behind a reverse proxy or gateway
		// the Host header is the public name, which the SDK's localhost check
		// would reject.
		DisableLocalhostProtection: idp != nil || !httpx.IsLoopbackHost(cfg.BaseURL.Hostname()),
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
		return httpx.AccessLog(log, mux)
	}

	as := standalone.New(standalone.Config{
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
	prmDoc.ScopesSupported = []string{standalone.Scope}
	registerPRM()
	bearer := auth.RequireBearerToken(as.VerifyAccessToken, &auth.RequireBearerTokenOptions{
		ResourceMetadataURL: prmURL,
		Scopes:              []string{standalone.Scope},
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
	return httpx.AccessLog(log, mux)
}
