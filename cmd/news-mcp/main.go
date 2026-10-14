// Command news-mcp is an MCP server (Streamable HTTP + OAuth) exposing a
// Nextcloud News account as tools.
package main

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/mrdev023/mcp-nextcloud-news/internal/config"
	"github.com/mrdev023/mcp-nextcloud-news/internal/nextcloud"
	"github.com/mrdev023/mcp-nextcloud-news/internal/oidc"
	"github.com/mrdev023/mcp-nextcloud-news/internal/server"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "news-mcp:", err)
		os.Exit(1)
	}
}

func run() error {
	// Keep the heap small unless the operator decided otherwise.
	if os.Getenv("GOMEMLIMIT") == "" {
		debug.SetMemoryLimit(32 << 20)
	}

	cfg, err := config.Load()
	if err != nil {
		return err
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: cfg.LogLevel}))

	user, pass := cfg.NextcloudUser, cfg.AppPassword
	var idp *oidc.Client
	if cfg.AuthMode == config.AuthModeOIDC {
		// Never fall back to a shared identity: every Nextcloud call uses a
		// token exchanged for the calling user.
		if user != "" || pass != "" {
			log.Warn("oidc mode: NEXTCLOUD_USER / NEXTCLOUD_APP_PASSWORD are ignored")
		}
		user, pass = "", ""
		idp = oidc.New(oidc.Config{
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
	nc := nextcloud.NewClient(cfg.NextcloudURL, user, pass, &http.Client{
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
		Handler:           server.NewHandler(cfg, version, log, nc, idp),
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		MaxHeaderBytes:    64 << 10,
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	errc := make(chan error, 1)
	go func() { errc <- srv.ListenAndServe() }()
	attrs := []any{"addr", srv.Addr, "auth_mode", cfg.AuthMode, "read_only", cfg.ReadOnly, "mcp_endpoint", cfg.BaseURL.String() + "/mcp", "nextcloud", cfg.NextcloudURL}
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
